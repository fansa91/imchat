import { api, ApiError, getToken, putWithProgress, setToken, setUnauthorizedHandler } from './api'
import { Socket } from './socket'
import { MSG_CALL, MSG_FILE, MSG_IMAGE, MSG_SYSTEM, MSG_TEXT, MSG_VIDEO, MSG_VOICE } from './types'
import type { CallContent, CallKind, ClientConfig, Conversation, FileContent, FriendEntry, FriendRequests, ImageContent, Member, Message, Presence, PublicUser, Reaction, SystemContent, User, VideoContent, VoiceContent } from './types'
import { clearMediaCaches, imageDB, requestPersist, transcriptDB, voiceDB, voiceKey, type Recording } from './voice'
import { prepareImage } from './image'
import { probeVideo, sanitizeFileName, videoMimeOf } from './media'
import { CallManager, type CallUiState } from './call'
import { GroupCallManager, type GroupCallUiState } from './groupcall'
import { displayName, shortPreview } from './util'

export interface Thread {
  items: Message[] // 服务端已确认的消息，按 seq 升序
  pending: Message[] // 本地待确认（发送中 / 失败）
  loaded: boolean
  hasMore: boolean
  loadingMore: boolean
  modSeq: number // 已应用的变更位置（撤回/回应），重连后用 changed_since 补拉
}

/** 语音本地状态：none 未知 / loading 下载中 / ready 本地有 / expired 服务器已过期且本地没有 / error */
export type VoiceState = 'loading' | 'ready' | 'expired' | 'error'

/** 语音转文字：loading 转写中；text 有结果 */
export interface Transcript {
  loading?: boolean
  text?: string
}

/** 图片下载失败：expired 服务器已删除（撤回 / 过期），其他为 error */
export class MediaError extends Error {
  constructor(public kind: 'expired' | 'error') {
    super(kind)
  }
}

/** v0.5.0：会话列表 / 群聊页展示的进行中群通话（由 group_call_* 事件维护） */
export interface GroupRoomInfo {
  callId: string
  kind: CallKind
  count: number
}

export interface GroupInvite {
  callId: string
  conversationId: number
  kind: CallKind
  creator: PublicUser
  ts: number
}

export interface State {
  me: User | null
  booting: boolean
  conn: 'connecting' | 'online' | 'offline'
  config: ClientConfig
  convs: Record<number, Conversation>
  threads: Record<number, Thread>
  reads: Record<number, Record<number, number>> // 会话 -> 成员 -> 已读 seq
  typing: Record<number, { uid: number; ts: number }> // 会话 -> 最近在输入的人
  users: Record<number, PublicUser> // 已知用户（群成员名字/头像，他人视角不含 email）
  members: Record<number, Member[]> // 已加载的群成员
  voice: Record<string, VoiceState> // voiceKey -> 状态
  presence: Record<number, Presence> // 用户 -> 在线状态
  transcripts: Record<string, Transcript> // voiceKey -> 转文字结果
  call: CallUiState | null // v0.4.0：当前通话（无通话为 null）
  // v0.5.0 好友 / 群通话
  friends: Record<number, FriendEntry> // user_id -> 好友条目（含我的备注）
  friendRequests: FriendRequests
  groupRooms: Record<number, GroupRoomInfo> // conversation_id -> 进行中的群通话房间
  groupCall: GroupCallUiState | null // 当前群通话（无通话为 null）
  groupInvite: GroupInvite | null // 收到的群通话邀请横幅
  notice: string // 退出到登录页时的一次性提示（如 session_revoked）
  toast: string
}

const PAGE = 50
const SEND_TIMEOUT = 10000
const DEFAULT_CONFIG: ClientConfig = { recall_seconds: 120, group_max_members: 500, reactions: ['❤️', '👍', '👎', '😂', '‼️', '❓'], voice_enabled: false }

const uuid = () =>
  (crypto as any).randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`

const emptyThread = (): Thread => ({ items: [], pending: [], loaded: false, hasMore: true, loadingMore: false, modSeq: 0 })

export function parseImage(m: Message): ImageContent | null {
  if (m.msg_type !== MSG_IMAGE || m.recalled) return null
  try {
    return JSON.parse(m.content)
  } catch {
    return null
  }
}

export function parseVoice(m: Message): VoiceContent | null {
  if (m.msg_type !== MSG_VOICE || m.recalled) return null
  try {
    return JSON.parse(m.content)
  } catch {
    return null
  }
}

export function parseVideo(m: Message): VideoContent | null {
  if (m.msg_type !== MSG_VIDEO || m.recalled) return null
  try {
    return JSON.parse(m.content)
  } catch {
    return null
  }
}

export function parseFile(m: Message): FileContent | null {
  if (m.msg_type !== MSG_FILE || m.recalled) return null
  try {
    return JSON.parse(m.content)
  } catch {
    return null
  }
}

export function parseCall(m: Message): CallContent | null {
  if (m.msg_type !== MSG_CALL || m.recalled) return null
  try {
    return JSON.parse(m.content)
  } catch {
    return null
  }
}

const initial = (): State => ({
  me: null,
  booting: true,
  conn: 'offline',
  config: DEFAULT_CONFIG,
  convs: {},
  threads: {},
  reads: {},
  typing: {},
  users: {},
  members: {},
  voice: {},
  presence: {},
  transcripts: {},
  call: null,
  friends: {},
  friendRequests: { incoming: [], outgoing: [] },
  groupRooms: {},
  groupCall: null,
  groupInvite: null,
  notice: '',
  toast: '',
})

class Store {
  private state: State = initial()
  private listeners = new Set<() => void>()
  private socket = new Socket(getToken)
  private timers = new Map<string, number>()
  /** 本地语音 Blob：发送中的按 client_msg_id，已确认的按 voiceKey */
  private blobs = new Map<string, Blob>()
  private blobUrls = new Map<string, string>()
  private downloadQueue: { cid: number; seq: number }[] = []
  private downloading = 0
  /** 进行中的图片加载（去重） */
  private imageLoads = new Map<string, Promise<string>>()
  /** v0.4.0：视频封面 Blob（client_msg_id -> 截图），随视频一起上传 */
  private posters = new Map<string, { blob: Blob; width: number; height: number }>()
  /** v0.4.0：进行中的 PUT 上传（client_msg_id -> 可取消句柄） */
  private uploads = new Map<string, { abort: () => void }>()
  /** v0.4.0：通话状态机（信令走同一个 socket，UI 状态同步到 state.call） */
  private callMgr = new CallManager({
    send: (type, data) => this.socket.send(type, data),
    getIce: () => api.getIceServers(),
    onChange: (call) => this.set({ call }),
    toast: (msg) => this.showToast(msg),
    meId: () => this.state.me?.id,
    ringSeconds: () => this.state.config.call_ring_timeout_seconds ?? 60,
  })
  /** v0.5.0：群通话（SFU 代理接口 + group_call_* 事件驱动） */
  private groupCallMgr = new GroupCallManager({
    onChange: (groupCall) => {
      this.set({ groupCall })
      // 兜底维护房间概要：自己发起/加入时服务端事件可能不回显给本人，
      // 列表与群聊页的「通话中」条不能因此缺失
      if (groupCall && !this.state.groupRooms[groupCall.conversationId]) {
        this.set({
          groupRooms: {
            ...this.state.groupRooms,
            [groupCall.conversationId]: { callId: groupCall.callId, kind: groupCall.kind, count: groupCall.participants.length + 1 },
          },
        })
      }
    },
    toast: (msg) => this.showToast(msg),
    meId: () => this.state.me?.id,
  })
  private configReady: Promise<void> = Promise.resolve()
  /** 自己撤回的文本，用于“重新编辑” */
  recalledDrafts = new Map<string, string>()
  activeId: number | null = null

  constructor() {
    setUnauthorizedHandler(() => this.logout())
    this.socket.onStatus = (conn) => this.set({ conn })
    this.socket.onOpen = () => this.sync()
    this.socket.on('send_ack', (d) => this.onAck(d.message))
    this.socket.on('message', (d) => this.onMessage(d.message))
    this.socket.on('read', (d) => this.onRead(d.conversation_id, d.user_id, d.read_seq))
    this.socket.on('typing', (d) => this.onTyping(d.conversation_id, d.user_id))
    this.socket.on('recall', (d) => this.onRecall(d.message))
    this.socket.on('reaction', (d) => this.onReaction(d.conversation_id, d.seq, d.reactions, d.mod_seq))
    this.socket.on('deleted', (d) => this.onDeleted(d.conversation_id, d.seqs))
    this.socket.on('conversation_updated', (d) => this.refreshConversation(d.conversation_id))
    this.socket.on('conversation_removed', (d) => this.dropConversation(d.conversation_id))
    this.socket.on('presence', (d: Presence) => this.applyPresence([d]))
    // v0.4.0：通话信令转发给状态机；资料变更刷新用户缓存；登录被吊销回登录页
    for (const t of ['call_invite', 'call_accept', 'call_reject', 'call_cancel', 'call_hangup', 'call_signal', 'call_unavailable']) {
      this.socket.on(t, (d) => this.callMgr.handle(t, d))
    }
    // v0.5.0：好友事件——刷新好友/申请列表与红点，被删好友不弹提示（SPEC §1.3）
    this.socket.on('friend_request', () => {
      this.loadFriendRequests()
      this.showToast('收到新的好友申请')
    })
    this.socket.on('friend_request_accepted', (d) => {
      this.loadFriends()
      this.loadFriendRequests()
      if (d?.by) this.showToast(`${displayName(d.by)} 已通过你的好友申请`)
    })
    this.socket.on('friend_request_rejected', () => this.loadFriendRequests())
    this.socket.on('friend_removed', () => this.loadFriends())
    // v0.5.0：群通话事件——房间概要（列表/横幅）在这里维护，通话内事件转发给群通话状态机
    for (const t of ['group_call_started', 'group_call_joined', 'group_call_tracks', 'group_call_tracks_closed', 'group_call_left', 'group_call_ended']) {
      this.socket.on(t, (d) => {
        this.onGroupCallEvent(t, d)
        this.groupCallMgr.handle(t, d)
      })
    }
    this.socket.on('user_updated', (d) => this.onUserUpdated(d?.user))
    this.socket.on('session_revoked', async () => {
      // 若是本设备刚改完密码：本地已换上新 token，校验通过则不登出，
      // socket 会在服务端断开后用新 token 自动重连（其他设备已被踢）。
      try {
        await api.me()
        this.showToast('密码已修改，其他设备已退出登录')
      } catch {
        this.logout('你的登录已被撤销（密码已修改），请重新登录')
      }
    })
    this.socket.on('error', (d, id) => {
      if (id && this.timers.has(id)) this.failPending(id)
      else if (id) this.showToast(d?.message || '操作失败')
      console.warn('server error', d)
    })
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && this.activeId) this.markRead(this.activeId)
    })
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }
  get = () => this.state
  private set(patch: Partial<State>) {
    this.state = { ...this.state, ...patch }
    this.listeners.forEach((l) => l())
  }
  private patchConv(id: number, patch: Partial<Conversation>) {
    const c = this.state.convs[id]
    if (!c) return
    this.set({ convs: { ...this.state.convs, [id]: { ...c, ...patch } } })
  }
  private patchThread(id: number, fn: (t: Thread) => Thread) {
    const t = this.state.threads[id] ?? emptyThread()
    this.set({ threads: { ...this.state.threads, [id]: fn(t) } })
  }
  private addUsers(list: (PublicUser | undefined)[]) {
    const users = { ...this.state.users }
    let changed = false
    for (const u of list) {
      if (u && (!users[u.id] || users[u.id].nickname !== u.nickname || users[u.id].avatar_url !== u.avatar_url)) {
        users[u.id] = u
        changed = true
      }
    }
    if (changed) this.set({ users })
  }

  /** v0.4.0：别人（或自己其他设备）改了昵称/头像 */
  private onUserUpdated(u?: User) {
    if (!u?.id) return
    this.addUsers([u])
    if (u.id === this.state.me?.id) this.set({ me: u })
    // 会话列表里的 peer / members_preview 也换成新资料
    const convs = { ...this.state.convs }
    let touched = false
    for (const c of Object.values(convs)) {
      if (c.peer?.id === u.id) {
        convs[c.id] = { ...c, peer: u }
        touched = true
      }
      if (c.members_preview?.some((x) => x.id === u.id)) {
        convs[c.id] = { ...convs[c.id], members_preview: c.members_preview.map((x) => (x.id === u.id ? u : x)) }
        touched = true
      }
    }
    if (touched) this.set({ convs })
  }

  private toastTimer = 0
  showToast(text: string) {
    clearTimeout(this.toastTimer)
    this.set({ toast: text })
    this.toastTimer = window.setTimeout(() => this.set({ toast: '' }), 3000)
  }

  // ---------- 会话生命周期 ----------

  async boot() {
    if (!getToken()) return this.set({ booting: false })
    try {
      const me = await api.me()
      this.set({ me, booting: false })
      this.addUsers([me])
      this.afterLogin()
    } catch {
      this.set({ booting: false })
    }
  }

  private afterLogin() {
    this.socket.start()
    requestPersist()
    this.configReady = api
      .config()
      .then((config) => this.set({ config: { ...DEFAULT_CONFIG, ...config } }))
      .catch(() => {})
    this.loadConversations().catch(() => {})
    this.loadFriends()
    this.loadFriendRequests()
  }

  async login(email: string, password: string, register = false, nickname = '') {
    const r = register ? await api.register(email, password, nickname) : await api.login(email, password)
    setToken(r.token)
    this.set({ ...initial(), booting: false, me: r.user })
    this.addUsers([r.user])
    this.afterLogin()
  }

  logout(notice = '') {
    this.callMgr.dispose()
    this.groupCallMgr.dispose()
    this.socket.stop()
    setToken('')
    this.timers.forEach((t) => clearTimeout(t))
    this.timers.clear()
    this.blobUrls.forEach((u) => URL.revokeObjectURL(u))
    this.blobUrls.clear()
    this.blobs.clear()
    this.imageLoads.clear()
    this.posters.clear()
    this.uploads.forEach((u) => u.abort())
    this.uploads.clear()
    this.set({ ...initial(), booting: false, notice })
  }

  async loadConversations() {
    const list = await api.conversations()
    const convs: Record<number, Conversation> = {}
    for (const c of list) {
      const local = this.state.convs[c.id]
      // 请求期间通过 WebSocket 收到了更新的消息，以本地为准，避免被旧快照覆盖
      convs[c.id] = local && local.last_seq > c.last_seq ? { ...c, ...local, peer: c.peer ?? local.peer, name: c.name, members_preview: c.members_preview } : c
      this.addUsers([c.peer, ...(c.members_preview ?? [])])
    }
    this.set({ convs })
    return list
  }

  async refreshConversation(id: number) {
    try {
      const c = await api.conversation(id)
      const local = this.state.convs[id]
      this.set({ convs: { ...this.state.convs, [id]: local ? { ...local, ...c, last_seq: Math.max(local.last_seq, c.last_seq), last_message: local.last_seq > c.last_seq ? local.last_message : c.last_message } : c } })
      this.addUsers([c.peer, ...(c.members_preview ?? [])])
      if (this.state.members[id]) this.loadMembers(id)
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) this.dropConversation(id)
    }
  }

  private dropConversation(id: number) {
    const convs = { ...this.state.convs }
    const threads = { ...this.state.threads }
    delete convs[id]
    delete threads[id]
    this.set({ convs, threads })
  }

  /** 连接（重连）后：刷新会话列表，补拉已打开会话的新消息和变更，补发未确认的消息 */
  private async sync() {
    try {
      await this.loadConversations()
      this.refreshPresence()
      // v0.5.0：WS 重连后恢复群通话的成员与订阅（媒体本身不走 WS，不受断线影响）
      if (this.state.groupCall) this.groupCallMgr.recover()
      this.loadFriends()
      this.loadFriendRequests()
      for (const [idStr, t] of Object.entries(this.state.threads)) {
        const id = Number(idStr)
        if (!t.loaded) continue
        if (!this.state.convs[id]) {
          this.dropConversation(id)
          continue
        }
        await this.catchUp(id)
        await this.catchUpChanges(id)
        await this.syncHidden(id)
        if (this.activeId === id) this.loadReads(id)
      }
      for (const t of Object.values(this.state.threads)) {
        for (const m of t.pending) if (m.status === 'sending') this.transmit(m)
      }
      if (this.activeId) this.markRead(this.activeId)
    } catch (e) {
      console.warn('sync failed', e)
    }
  }

  private async catchUp(id: number) {
    for (;;) {
      const t = this.state.threads[id]
      const after = t.items.length ? t.items[t.items.length - 1].seq : 0
      const msgs = await api.messages(id, after ? { after, limit: 200 } : { limit: PAGE })
      this.mergeMessages(id, msgs)
      if (!after || msgs.length < 200) break
    }
  }

  /** 离线期间的撤回 / 表情回应变化 */
  private async catchUpChanges(id: number) {
    const conv = this.state.convs[id]
    for (let i = 0; i < 20; i++) {
      const t = this.state.threads[id]
      if (!t || !conv || (conv.mod_seq ?? 0) <= t.modSeq) return
      const msgs = await api.messages(id, { changedSince: t.modSeq || 0.5, limit: 200 })
      if (!msgs.length) {
        this.patchThread(id, (t) => ({ ...t, modSeq: conv.mod_seq ?? t.modSeq }))
        return
      }
      const loaded = new Set(t.items.map((m) => m.seq))
      this.replaceMessages(id, msgs.filter((m) => loaded.has(m.seq)))
      const maxMod = Math.max(...msgs.map((m) => m.mod_seq ?? 0))
      this.patchThread(id, (t) => ({ ...t, modSeq: Math.max(t.modSeq, maxMod) }))
      if (msgs.length < 200) return
    }
  }

  /** 其他设备上删除的消息 */
  private async syncHidden(id: number) {
    const t = this.state.threads[id]
    if (!t?.items.length) return
    const seqs = await api.hidden(id, t.items[0].seq)
    if (seqs.length) this.onDeleted(id, seqs)
  }

  async openConversation(id: number) {
    this.activeId = id
    const conv = this.state.convs[id]
    if (!this.state.threads[id]?.loaded) {
      const modSeq = conv?.mod_seq ?? 0
      const msgs = await api.messages(id, { limit: PAGE })
      this.patchThread(id, (t) => ({ ...t, loaded: true, hasMore: msgs.length === PAGE, modSeq: Math.max(t.modSeq, modSeq) }))
      this.mergeMessages(id, msgs)
    }
    if (conv?.type === 2) this.loadMembers(id)
    this.loadReads(id)
    this.markRead(id)
  }

  closeConversation() {
    this.activeId = null
  }

  async loadMembers(id: number) {
    try {
      const list = await api.members(id)
      this.addUsers(list.map((m) => m.user))
      this.set({ members: { ...this.state.members, [id]: list } })
      this.loadPresence(list.map((m) => m.user.id).filter((u) => !this.state.presence[u]))
      return list
    } catch {
      return []
    }
  }

  private async loadReads(id: number) {
    try {
      const reads = await api.reads(id)
      const cur = { ...(this.state.reads[id] ?? {}) }
      for (const [uid, seq] of Object.entries(reads)) cur[Number(uid)] = Math.max(seq, cur[Number(uid)] ?? 0)
      this.set({ reads: { ...this.state.reads, [id]: cur } })
    } catch {}
  }

  async loadMore(id: number) {
    const t = this.state.threads[id]
    if (!t || !t.hasMore || t.loadingMore || !t.items.length) return
    this.patchThread(id, (t) => ({ ...t, loadingMore: true }))
    try {
      const msgs = await api.messages(id, { before: t.items[0].seq, limit: PAGE })
      this.mergeMessages(id, msgs)
      this.patchThread(id, (t) => ({ ...t, hasMore: msgs.length === PAGE, loadingMore: false }))
    } catch {
      this.patchThread(id, (t) => ({ ...t, loadingMore: false }))
    }
  }

  /** 引用跳转：一直往前翻直到加载到指定 seq（最多 20 页） */
  async ensureLoaded(id: number, seq: number) {
    for (let i = 0; i < 20; i++) {
      const t = this.state.threads[id]
      if (!t || !t.items.length || t.items[0].seq <= seq || !t.hasMore) return t?.items.some((m) => m.seq === seq) ?? false
      await this.loadMore(id)
    }
    return this.state.threads[id]?.items.some((m) => m.seq === seq) ?? false
  }

  async startChatByEmail(email: string) {
    const user = await api.searchUser(email.trim())
    return this.openDirectWith(user)
  }

  /** 与任意用户打开（或新建）单聊，返回会话 ID（v0.5.0 好友页发消息/通话共用） */
  async openDirectWith(user: PublicUser) {
    const { conversation_id } = await api.openDirect(user.id)
    if (!this.state.convs[conversation_id]) {
      await this.loadConversations()
      if (!this.state.convs[conversation_id]) {
        // 新会话还没有消息时，本地先放一条占位
        this.set({
          convs: {
            ...this.state.convs,
            [conversation_id]: { id: conversation_id, type: 1, last_seq: 0, read_seq: 0, unread: 0, updated_at: new Date().toISOString(), peer: user },
          },
        })
      }
    }
    return conversation_id
  }

  async findUser(email: string) {
    const u = await api.searchUser(email.trim())
    this.addUsers([u])
    return u
  }

  async createGroup(name: string, memberIds: number[]) {
    const r = await api.createGroup(name, memberIds)
    if (r.conversation) {
      this.set({ convs: { ...this.state.convs, [r.conversation_id]: r.conversation } })
      this.addUsers(r.conversation.members_preview ?? [])
    } else await this.loadConversations()
    return r.conversation_id
  }

  async renameGroup(id: number, name: string) {
    const c = await api.updateGroup(id, { name })
    this.patchConv(id, { name: c.name })
  }
  async setAnnouncement(id: number, announcement: string) {
    const c = await api.updateGroup(id, { announcement })
    this.patchConv(id, { announcement: c.announcement, announcement_at: c.announcement_at, announcement_by: c.announcement_by })
  }
  async addMembers(id: number, uids: number[]) {
    await api.addMembers(id, uids)
    await this.loadMembers(id)
  }
  async removeMember(id: number, uid: number) {
    await api.removeMember(id, uid)
    await this.loadMembers(id)
  }
  async setRole(id: number, uid: number, role: 'admin' | 'member') {
    await api.setRole(id, uid, role)
    await this.loadMembers(id)
  }
  async leaveGroup(id: number) {
    await api.leave(id)
    this.dropConversation(id)
  }

  // ---------- 消息 ----------

  private mergeMessages(id: number, msgs: Message[]) {
    if (!msgs.length) return
    this.patchThread(id, (t) => {
      const bySeq = new Map(t.items.map((m) => [m.seq, m]))
      for (const m of msgs) bySeq.set(m.seq, { ...m, status: 'sent' })
      const items = [...bySeq.values()].sort((a, b) => a.seq - b.seq)
      const ids = new Set(msgs.filter((m) => m.sender_id === this.state.me?.id).map((m) => m.client_msg_id))
      return { ...t, items, pending: t.pending.filter((p) => !ids.has(p.client_msg_id)) }
    })
    const last = msgs.reduce((a, b) => (b.seq > a.seq ? b : a))
    const c = this.state.convs[id]
    if (c && last.seq >= c.last_seq) {
      this.patchConv(id, {
        last_seq: last.seq,
        last_message: last,
        last_sender_name: this.state.users[last.sender_id]?.nickname ?? c.last_sender_name,
        updated_at: last.created_at,
        unread: Math.max(0, last.seq - c.read_seq),
      })
    }
    for (const m of msgs) if (m.msg_type === MSG_VOICE && !m.recalled) this.ensureVoice(m)
  }

  // ---------- 在线状态 ----------

  /** 登录 / 重连后：对所有已知会话成员批量拉取在线状态 */
  private async refreshPresence() {
    await this.configReady
    if (!this.state.config.presence_enabled) return
    const ids = new Set<number>()
    for (const c of Object.values(this.state.convs)) {
      if (c.peer) ids.add(c.peer.id)
      for (const u of c.members_preview ?? []) ids.add(u.id)
    }
    for (const list of Object.values(this.state.members)) for (const m of list) ids.add(m.user.id)
    for (const f of Object.values(this.state.friends)) ids.add(f.user.id)
    if (this.state.me) ids.delete(this.state.me.id)
    await this.loadPresence([...ids])
  }

  private async loadPresence(ids: number[]) {
    await this.configReady
    if (!this.state.config.presence_enabled) return
    const me = this.state.me?.id
    ids = [...new Set(ids)].filter((i) => i && i !== me)
    for (let i = 0; i < ids.length; i += 200) {
      try {
        this.applyPresence(await api.presence(ids.slice(i, i + 200)))
      } catch (e) {
        console.warn('presence failed', e)
        return
      }
    }
  }

  private applyPresence(list: Presence[]) {
    if (!list?.length) return
    const presence = { ...this.state.presence }
    for (const p of list) if (p && p.user_id) presence[p.user_id] = { user_id: p.user_id, online: !!p.online, last_seen: p.last_seen ?? null }
    this.set({ presence })
  }

  /** 用新版本替换已加载的消息（撤回、回应变化） */
  private replaceMessages(id: number, msgs: Message[]) {
    if (!msgs.length) return
    const by = new Map(msgs.map((m) => [m.seq, m]))
    this.patchThread(id, (t) => ({ ...t, items: t.items.map((m) => (by.has(m.seq) ? { ...by.get(m.seq)!, status: 'sent' } : m)) }))
    const c = this.state.convs[id]
    if (c?.last_message && by.has(c.last_message.seq)) this.patchConv(id, { last_message: by.get(c.last_message.seq) })
    for (const m of msgs) if (m.recalled) this.forgetMedia(id, m.seq)
  }

  send(id: number, text: string, replyToSeq?: number) {
    const content = text.trim()
    if (!content || !this.state.me) return
    this.transmit(this.addPending(id, MSG_TEXT, content, replyToSeq))
  }

  private addPending(id: number, msgType: number, content: string, replyToSeq?: number) {
    const m: Message = {
      conversation_id: id,
      seq: 0,
      sender_id: this.state.me!.id,
      client_msg_id: uuid(),
      msg_type: msgType,
      content,
      created_at: new Date().toISOString(),
      reply_to_seq: replyToSeq || undefined,
      reply_to: replyToSeq ? this.replyPreviewFor(id, replyToSeq) : undefined,
      status: 'sending',
    }
    this.patchThread(id, (t) => ({ ...t, pending: [...t.pending, m] }))
    return m
  }

  private replyPreviewFor(id: number, seq: number) {
    const o = this.state.threads[id]?.items.find((m) => m.seq === seq)
    if (!o) return undefined
    return { seq, sender_id: o.sender_id, msg_type: o.msg_type, preview: shortPreview(o.msg_type, o.content).slice(0, 80), recalled: !!o.recalled }
  }

  /** 语音：先占位显示，上传到对象存储（预签名 PUT）后再通过 WebSocket 发送 */
  async sendVoice(id: number, rec: Recording, replyToSeq?: number) {
    if (!this.state.me) return
    const local: VoiceContent = { key: '', mime: rec.mime, duration_ms: rec.durationMs, size: rec.blob.size, waveform: rec.waveform }
    const m = this.addPending(id, MSG_VOICE, JSON.stringify(local), replyToSeq)
    this.blobs.set(m.client_msg_id, rec.blob)
    await this.uploadAndSend(m)
  }

  /** 图片：本地压缩 → 先占位显示 → 预签名上传 → WebSocket 发送（与语音同一流程） */
  async sendImage(id: number, file: File | Blob, replyToSeq?: number) {
    if (!this.state.me) return
    const cfg = this.state.config
    if (!cfg.image_enabled) return this.showToast('服务器未开启图片消息')
    if (file.type && !file.type.startsWith('image/')) return this.showToast('只能发送图片')
    let img
    try {
      img = await prepareImage(file)
    } catch (e) {
      console.warn('prepare image', e)
      return this.showToast('无法读取这张图片')
    }
    const max = cfg.image_max_bytes ?? 10 * 1024 * 1024
    if (img.blob.size > max) return this.showToast(`图片不能超过 ${Math.round((max / 1048576) * 10) / 10} MB`)
    const local: ImageContent = { key: '', mime: img.mime, size: img.blob.size, width: img.width, height: img.height }
    const m = this.addPending(id, MSG_IMAGE, JSON.stringify(local), replyToSeq)
    this.blobs.set(m.client_msg_id, img.blob)
    await this.uploadAndSend(m)
  }

  /** 视频：读元数据/截首帧 → 先占位显示 → 预签名上传（封面按图片上传）→ WebSocket 发送 */
  async sendVideo(id: number, file: File, replyToSeq?: number) {
    if (!this.state.me) return
    const cfg = this.state.config
    if (!cfg.video_enabled) return this.showToast('服务器未开启视频消息')
    const mime = videoMimeOf(file)
    if (!['video/mp4', 'video/webm', 'video/quicktime'].includes(mime)) return this.showToast('只支持 MP4 / WebM / MOV 视频')
    const max = cfg.video_max_bytes ?? 100 * 1024 * 1024
    if (file.size > max) return this.showToast(`视频不能超过 ${Math.round((max / 1048576) * 10) / 10} MB`)
    const meta = await probeVideo(file)
    // 服务端默认 video_max_seconds=600（/api/config 未下发这个值，本地按默认值先拦）
    if (meta.durationMs > 601 * 1000) return this.showToast('视频时长不能超过 10 分钟')
    const local: VideoContent = { key: '', mime, size: file.size, duration_ms: meta.durationMs, width: meta.width, height: meta.height }
    const m = this.addPending(id, MSG_VIDEO, JSON.stringify(local), replyToSeq)
    this.blobs.set(m.client_msg_id, file)
    if (meta.poster) this.posters.set(m.client_msg_id, meta.poster)
    await this.uploadAndSend(m)
  }

  /** 文件：原样上传，文件名按服务端规则清洗 */
  async sendFile(id: number, file: File, replyToSeq?: number) {
    if (!this.state.me) return
    const cfg = this.state.config
    if (!cfg.file_enabled) return this.showToast('服务器未开启文件消息')
    const max = cfg.file_max_bytes ?? 100 * 1024 * 1024
    if (file.size > max) return this.showToast(`文件不能超过 ${Math.round((max / 1048576) * 10) / 10} MB`)
    const local: FileContent = { key: '', mime: file.type || 'application/octet-stream', size: file.size, name: sanitizeFileName(file.name) }
    const m = this.addPending(id, MSG_FILE, JSON.stringify(local), replyToSeq)
    this.blobs.set(m.client_msg_id, file)
    await this.uploadAndSend(m)
  }

  private patchProgress(m: Message, fraction: number) {
    this.patchThread(m.conversation_id, (t) => ({
      ...t,
      pending: t.pending.map((p) => (p.client_msg_id === m.client_msg_id ? { ...p, progress: fraction } : p)),
    }))
  }

  private async uploadAndSend(m: Message) {
    const blob = this.blobs.get(m.client_msg_id)
    let vc: any
    try {
      vc = JSON.parse(m.content)
    } catch {
      return this.failPending(m.client_msg_id)
    }
    if (!vc.key) {
      if (!blob) return this.failPending(m.client_msg_id)
      const label = { [MSG_VOICE]: '语音', [MSG_IMAGE]: '图片', [MSG_VIDEO]: '视频', [MSG_FILE]: '文件' }[m.msg_type] ?? '文件'
      try {
        // 视频封面先按图片上传，拿到 key 后写进 content.poster_key
        const poster = this.posters.get(m.client_msg_id)
        if (m.msg_type === MSG_VIDEO && poster && !vc.poster_key) {
          const pre = await api.presign({ kind: 'image', mime: 'image/jpeg', size: poster.blob.size, width: poster.width, height: poster.height })
          const up = putWithProgress(pre.url, pre.headers, poster.blob, () => {})
          this.uploads.set(m.client_msg_id, up)
          await up.promise
          vc = { ...vc, poster_key: pre.key }
        }
        const body =
          m.msg_type === MSG_IMAGE
            ? { kind: 'image' as const, mime: vc.mime, size: blob.size, width: vc.width, height: vc.height }
            : m.msg_type === MSG_VIDEO
              ? { kind: 'video' as const, mime: vc.mime, size: blob.size, duration_ms: vc.duration_ms, width: vc.width, height: vc.height }
              : m.msg_type === MSG_FILE
                ? { kind: 'file' as const, mime: vc.mime, size: blob.size, name: vc.name }
                : { kind: 'voice' as const, mime: vc.mime, size: blob.size, duration_ms: vc.duration_ms }
        const pre = await api.presign(body)
        const up = putWithProgress(pre.url, pre.headers, blob, (f) => this.patchProgress(m, f))
        this.uploads.set(m.client_msg_id, up)
        await up.promise
        vc = { ...vc, key: pre.key }
      } catch (e) {
        if ((e as Error)?.name === 'AbortError') return // 用户已取消，pending 已移除
        this.showToast(e instanceof ApiError ? e.message : `${label}上传失败`)
        return this.failPending(m.client_msg_id)
      } finally {
        this.uploads.delete(m.client_msg_id)
      }
      const content = JSON.stringify(vc)
      m = { ...m, content }
      this.patchThread(m.conversation_id, (t) => ({
        ...t,
        pending: t.pending.map((p) => (p.client_msg_id === m.client_msg_id ? { ...p, content, progress: undefined } : p)),
      }))
    }
    this.transmit(m)
  }

  retry(m: Message) {
    this.patchThread(m.conversation_id, (t) => ({
      ...t,
      pending: t.pending.map((p) => (p.client_msg_id === m.client_msg_id ? { ...p, status: 'sending' } : p)),
    }))
    if (m.msg_type === MSG_VOICE || m.msg_type === MSG_IMAGE || m.msg_type === MSG_VIDEO || m.msg_type === MSG_FILE) this.uploadAndSend(m)
    else this.transmit(m)
  }

  /** 发出（或重发同一个 client_msg_id，服务端保证幂等） */
  private transmit(m: Message) {
    clearTimeout(this.timers.get(m.client_msg_id))
    if (m.msg_type === MSG_VOICE || m.msg_type === MSG_IMAGE || m.msg_type === MSG_VIDEO || m.msg_type === MSG_FILE) {
      try {
        if (!JSON.parse(m.content).key) return // 还没上传完
      } catch {}
    }
    const ok = this.socket.send(
      'send',
      { client_msg_id: m.client_msg_id, conversation_id: m.conversation_id, msg_type: m.msg_type, content: m.content, reply_to_seq: m.reply_to_seq || 0 },
      m.client_msg_id,
    )
    if (!ok) return // 离线：保持“发送中”，连上后 sync 会补发
    this.timers.set(m.client_msg_id, window.setTimeout(() => this.failPending(m.client_msg_id), SEND_TIMEOUT))
  }

  private failPending(clientId: string) {
    clearTimeout(this.timers.get(clientId))
    this.timers.delete(clientId)
    for (const [idStr, t] of Object.entries(this.state.threads)) {
      if (t.pending.some((p) => p.client_msg_id === clientId)) {
        this.patchThread(Number(idStr), (t) => ({
          ...t,
          pending: t.pending.map((p) => (p.client_msg_id === clientId ? { ...p, status: 'failed' } : p)),
        }))
      }
    }
  }

  /** 删除本地发送失败的消息；上传中则先中止 PUT（v0.4.0 文件/视频可取消） */
  discardPending(m: Message) {
    clearTimeout(this.timers.get(m.client_msg_id))
    this.uploads.get(m.client_msg_id)?.abort()
    this.uploads.delete(m.client_msg_id)
    this.blobs.delete(m.client_msg_id)
    this.posters.delete(m.client_msg_id)
    const u = this.blobUrls.get(m.client_msg_id)
    if (u) {
      URL.revokeObjectURL(u)
      this.blobUrls.delete(m.client_msg_id)
    }
    this.patchThread(m.conversation_id, (t) => ({ ...t, pending: t.pending.filter((p) => p.client_msg_id !== m.client_msg_id) }))
  }

  private onAck(m: Message) {
    clearTimeout(this.timers.get(m.client_msg_id))
    this.timers.delete(m.client_msg_id)
    // 自己发的语音：本地录音直接存入 IndexedDB，无需再下载
    const blob = this.blobs.get(m.client_msg_id)
    if (blob && m.msg_type === MSG_VOICE) {
      const k = voiceKey(m.conversation_id, m.seq)
      this.blobs.delete(m.client_msg_id)
      this.blobs.set(k, blob)
      this.set({ voice: { ...this.state.voice, [k]: 'ready' } })
      voiceDB.put(k, blob).catch((e) => console.warn('save voice', e))
    }
    // 自己发的视频/文件不做本地永久缓存（太大），确认后释放 Blob 与预览 URL
    if (blob && (m.msg_type === MSG_VIDEO || m.msg_type === MSG_FILE)) {
      this.blobs.delete(m.client_msg_id)
      this.posters.delete(m.client_msg_id)
      const u = this.blobUrls.get(m.client_msg_id)
      if (u) {
        URL.revokeObjectURL(u)
        this.blobUrls.delete(m.client_msg_id)
      }
    }
    // 自己发的图片：压缩后的 Blob 直接存入 IndexedDB
    if (blob && m.msg_type === MSG_IMAGE) {
      const k = voiceKey(m.conversation_id, m.seq)
      this.blobs.delete(m.client_msg_id)
      this.blobs.set(k, blob)
      const u = this.blobUrls.get(m.client_msg_id)
      if (u) {
        this.blobUrls.delete(m.client_msg_id)
        this.blobUrls.set(k, u)
      }
      imageDB.put(k, blob).catch((e) => console.warn('save image', e))
    }
    this.ingest(m, true)
  }

  private onMessage(m: Message) {
    const mine = m.sender_id === this.state.me?.id
    if (m.msg_type === MSG_SYSTEM && this.isMyExit(m)) {
      this.dropConversation(m.conversation_id) // 自己退群 / 被移出：不再请求该会话
      return
    }
    this.ingest(m, mine)
    if (m.msg_type === MSG_SYSTEM && this.state.convs[m.conversation_id]) {
      this.refreshConversation(m.conversation_id)
    }
    if (!mine) {
      const t = this.state.typing[m.conversation_id]
      if (t && t.uid === m.sender_id) this.set({ typing: { ...this.state.typing, [m.conversation_id]: { uid: 0, ts: 0 } } })
      if (this.activeId === m.conversation_id && document.visibilityState === 'visible') this.markRead(m.conversation_id)
    }
  }

  private isMyExit(m: Message) {
    try {
      const p = JSON.parse(m.content) as SystemContent
      const me = this.state.me?.id
      return (p.op === 'leave' && p.actor?.id === me) || (p.op === 'remove' && !!p.targets?.some((t) => t.id === me))
    } catch {
      return false
    }
  }

  private ingest(m: Message, mine: boolean) {
    const id = m.conversation_id
    if (!this.state.convs[id]) {
      // 别人新发起的会话 / 刚被拉进群：先拉会话列表，再重新处理这条消息
      this.loadConversations().then(() => this.state.convs[id] && this.ingest(m, mine)).catch(() => {})
      return
    }
    const t = this.state.threads[id]
    if (t?.loaded) {
      const lastSeq = t.items.length ? t.items[t.items.length - 1].seq : 0
      if (m.seq > lastSeq + 1) {
        this.catchUp(id) // 中间有缺口（或有自己删除的消息），补拉
        return
      }
      this.mergeMessages(id, [m])
    } else {
      const c = this.state.convs[id]
      if (c && m.seq > c.last_seq) {
        this.patchConv(id, {
          last_seq: m.seq,
          last_message: m,
          last_sender_name: this.state.users[m.sender_id]?.nickname ?? c.last_sender_name,
          updated_at: m.created_at,
          unread: mine ? c.unread : Math.max(0, m.seq - c.read_seq),
          read_seq: mine ? m.seq : c.read_seq,
        })
      }
      if (m.msg_type === MSG_VOICE) this.ensureVoice(m) // 收到即下载，保证 7 天后仍能播放
    }
    if (mine) {
      const c = this.state.convs[id]
      if (c && c.read_seq < m.seq) this.patchConv(id, { read_seq: m.seq, unread: 0 })
    }
  }

  markRead(id: number) {
    const c = this.state.convs[id]
    if (!c || c.read_seq >= c.last_seq) return
    this.patchConv(id, { read_seq: c.last_seq, unread: 0 })
    this.socket.send('read', { conversation_id: id, seq: c.last_seq })
  }

  private onRead(cid: number, uid: number, seq: number) {
    const cur = this.state.reads[cid] ?? {}
    this.set({ reads: { ...this.state.reads, [cid]: { ...cur, [uid]: Math.max(seq, cur[uid] ?? 0) } } })
    if (uid === this.state.me?.id) {
      const c = this.state.convs[cid]
      if (c && seq > c.read_seq) this.patchConv(cid, { read_seq: seq, unread: Math.max(0, c.last_seq - seq) })
    }
  }

  private lastTypingSent = 0
  sendTyping(id: number) {
    const now = Date.now()
    if (now - this.lastTypingSent < 2500) return
    this.lastTypingSent = now
    this.socket.send('typing', { conversation_id: id })
  }

  private onTyping(cid: number, uid: number) {
    const ts = Date.now()
    this.set({ typing: { ...this.state.typing, [cid]: { uid, ts } } })
    setTimeout(() => {
      if (this.state.typing[cid]?.ts === ts) this.set({ typing: { ...this.state.typing, [cid]: { uid: 0, ts: 0 } } })
    }, 4000)
  }

  // ---------- 撤回 / 表情回应 / 删除 ----------

  canRecall(m: Message) {
    return (
      m.sender_id === this.state.me?.id &&
      m.seq > 0 &&
      !m.recalled &&
      m.msg_type !== MSG_SYSTEM &&
      m.msg_type !== MSG_CALL && // 通话记录不可撤回（SPEC §1.4，服务端也会拒）
      Date.now() - new Date(m.created_at).getTime() < this.state.config.recall_seconds * 1000 - 1500
    )
  }

  recall(m: Message) {
    if (m.msg_type === MSG_TEXT) this.recalledDrafts.set(`${m.conversation_id}:${m.seq}`, m.content)
    if (!this.socket.send('recall', { conversation_id: m.conversation_id, seq: m.seq }, uuid())) this.showToast('网络未连接')
  }

  private onRecall(m: Message) {
    const id = m.conversation_id
    const t = this.state.threads[id]
    if (t?.loaded && t.items.some((x) => x.seq === m.seq)) this.replaceMessages(id, [m])
    else {
      const c = this.state.convs[id]
      if (c?.last_message?.seq === m.seq) this.patchConv(id, { last_message: m })
      this.forgetMedia(id, m.seq)
    }
    // 引用了这条消息的回复也要更新预览
    if (t?.loaded) {
      this.patchThread(id, (t) => ({
        ...t,
        items: t.items.map((x) => (x.reply_to?.seq === m.seq ? { ...x, reply_to: { ...x.reply_to, recalled: true, preview: '' } } : x)),
      }))
    }
    if (m.mod_seq && t) this.patchThread(id, (t) => ({ ...t, modSeq: Math.max(t.modSeq, m.mod_seq!) }))
    this.patchConv(id, { mod_seq: Math.max(this.state.convs[id]?.mod_seq ?? 0, m.mod_seq ?? 0) })
  }

  react(m: Message, emoji: string) {
    if (!this.socket.send('react', { conversation_id: m.conversation_id, seq: m.seq, emoji }, uuid())) this.showToast('网络未连接')
  }

  private onReaction(cid: number, seq: number, reactions: Reaction[], modSeq: number) {
    const t = this.state.threads[cid]
    if (t?.loaded) {
      this.patchThread(cid, (t) => ({
        ...t,
        items: t.items.map((m) => (m.seq === seq ? { ...m, reactions } : m)),
        modSeq: Math.max(t.modSeq, modSeq),
      }))
    }
    this.patchConv(cid, { mod_seq: Math.max(this.state.convs[cid]?.mod_seq ?? 0, modSeq) })
  }

  deleteForMe(m: Message) {
    if (!m.seq) return this.discardPending(m)
    if (!this.socket.send('delete', { conversation_id: m.conversation_id, seqs: [m.seq] }, uuid())) this.showToast('网络未连接')
  }

  private onDeleted(cid: number, seqs: number[]) {
    const set = new Set(seqs)
    const t = this.state.threads[cid]
    if (t) this.patchThread(cid, (t) => ({ ...t, items: t.items.filter((m) => !set.has(m.seq)) }))
    const c = this.state.convs[cid]
    if (c?.last_message && set.has(c.last_message.seq)) {
      const items = this.state.threads[cid]?.items ?? []
      if (items.length) this.patchConv(cid, { last_message: items[items.length - 1] })
      else this.refreshConversation(cid)
    }
    for (const s of seqs) this.forgetMedia(cid, s)
  }

  // ---------- 语音本地缓存 ----------

  /** 确保语音在本地：IndexedDB 有就标记 ready，否则排队下载 */
  private async ensureVoice(m: Message) {
    const k = voiceKey(m.conversation_id, m.seq)
    this.loadTranscript(k)
    if (this.state.voice[k] === 'ready' || this.state.voice[k] === 'loading') return
    try {
      if (await voiceDB.has(k)) {
        this.set({ voice: { ...this.state.voice, [k]: 'ready' } })
        return
      }
    } catch {}
    this.set({ voice: { ...this.state.voice, [k]: 'loading' } })
    this.downloadQueue.push({ cid: m.conversation_id, seq: m.seq })
    this.pump()
  }

  private pump() {
    while (this.downloading < 3 && this.downloadQueue.length) {
      const job = this.downloadQueue.shift()!
      this.downloading++
      this.download(job.cid, job.seq).finally(() => {
        this.downloading--
        this.pump()
      })
    }
  }

  private async download(cid: number, seq: number) {
    const k = voiceKey(cid, seq)
    let state: VoiceState = 'error'
    try {
      const { url } = await api.media(cid, seq)
      const res = await fetch(url)
      if (res.status === 404 || res.status === 403) state = 'expired'
      else if (!res.ok) throw new Error(String(res.status))
      else {
        const blob = await res.blob()
        await voiceDB.put(k, blob)
        state = 'ready'
      }
    } catch (e) {
      if (e instanceof ApiError && (e.status === 410 || e.status === 404)) state = 'expired'
      else console.warn('voice download failed', e)
    }
    this.set({ voice: { ...this.state.voice, [k]: state } })
  }

  retryVoice(m: Message) {
    const k = voiceKey(m.conversation_id, m.seq)
    const v = { ...this.state.voice }
    delete v[k]
    this.set({ voice: v })
    this.ensureVoice(m)
  }

  /** 播放用的本地 URL：内存 → IndexedDB；都没有返回 null */
  async voiceUrl(m: Message): Promise<string | null> {
    const k = m.seq ? voiceKey(m.conversation_id, m.seq) : m.client_msg_id
    if (this.blobUrls.has(k)) return this.blobUrls.get(k)!
    let blob = this.blobs.get(k)
    if (!blob && m.seq) blob = (await voiceDB.get(k).catch(() => undefined)) ?? undefined
    if (!blob) return null
    const u = URL.createObjectURL(blob)
    this.blobUrls.set(k, u)
    return u
  }

  /** 撤回 / 删除：清理本地语音、图片和转文字缓存 */
  private forgetMedia(cid: number, seq: number) {
    const k = voiceKey(cid, seq)
    const u = this.blobUrls.get(k)
    if (u) URL.revokeObjectURL(u)
    this.blobUrls.delete(k)
    this.blobs.delete(k)
    this.imageLoads.delete(k)
    voiceDB.del(k).catch(() => {})
    imageDB.del(k).catch(() => {})
    transcriptDB.del(k).catch(() => {})
    if (this.state.transcripts[k]) {
      const transcripts = { ...this.state.transcripts }
      delete transcripts[k]
      this.set({ transcripts })
    }
  }

  // ---------- 图片本地缓存 ----------

  /**
   * 显示用的本地 URL：内存 → IndexedDB → 下载（存入 IndexedDB 永久保存）。
   * 失败抛 MediaError（expired：已撤回 / 已删除）。
   */
  imageUrl(m: Message): Promise<string> {
    const k = m.seq ? voiceKey(m.conversation_id, m.seq) : m.client_msg_id
    const cached = this.blobUrls.get(k)
    if (cached) return Promise.resolve(cached)
    let p = this.imageLoads.get(k)
    if (!p) {
      p = this.loadImage(m, k).finally(() => this.imageLoads.delete(k))
      this.imageLoads.set(k, p)
    }
    return p
  }

  private async loadImage(m: Message, k: string): Promise<string> {
    let blob = this.blobs.get(k)
    if (!blob && m.seq) blob = (await imageDB.get(k).catch(() => undefined)) ?? undefined
    if (!blob) {
      if (!m.seq) throw new MediaError('error')
      try {
        const { url } = await api.media(m.conversation_id, m.seq)
        const res = await fetch(url)
        if (res.status === 404 || res.status === 403) throw new MediaError('expired')
        if (!res.ok) throw new MediaError('error')
        blob = await res.blob()
      } catch (e) {
        if (e instanceof MediaError) throw e
        if (e instanceof ApiError && (e.status === 410 || e.status === 404)) throw new MediaError('expired')
        console.warn('image download failed', e)
        throw new MediaError('error')
      }
      imageDB.put(k, blob).catch((e) => console.warn('save image', e))
    }
    const u = URL.createObjectURL(blob)
    this.blobUrls.set(k, u)
    return u
  }

  // ---------- 语音转文字 ----------

  private async loadTranscript(k: string) {
    // 服务端关闭 STT 后仍显示以前转好的结果
    if (this.state.transcripts[k]) return
    try {
      const text = await transcriptDB.get(k)
      if (text && !this.state.transcripts[k]) this.set({ transcripts: { ...this.state.transcripts, [k]: { text } } })
    } catch {}
  }

  async transcribe(m: Message) {
    if (!m.seq || m.msg_type !== MSG_VOICE || m.recalled) return
    const k = voiceKey(m.conversation_id, m.seq)
    const cur = this.state.transcripts[k]
    if (cur?.loading || cur?.text) return
    this.set({ transcripts: { ...this.state.transcripts, [k]: { loading: true } } })
    try {
      const r = await api.transcribe(m.conversation_id, m.seq)
      const text = (r.text ?? '').trim()
      if (!text) {
        this.showToast('没有识别到文字')
        this.dropTranscript(k)
        return
      }
      this.set({ transcripts: { ...this.state.transcripts, [k]: { text } } })
      transcriptDB.put(k, text).catch(() => {})
    } catch (e) {
      this.dropTranscript(k)
      let msg = '转文字失败，请稍后再试'
      if (e instanceof ApiError) {
        if (e.status === 429 || e.code === 'stt_limit') msg = '今日转文字次数已用完'
        else if (e.status === 501 || e.code === 'stt_disabled') msg = '服务器未开启语音转文字'
        else if (e.status === 410 || e.code === 'recalled') msg = '消息已撤回'
        else if (e.status === 0) msg = '网络连接失败'
        else if (e.status === 403 || e.status === 404) msg = e.message || '消息不存在'
      }
      this.showToast(msg)
    }
  }

  private dropTranscript(k: string) {
    const transcripts = { ...this.state.transcripts }
    delete transcripts[k]
    this.set({ transcripts })
  }

  // ---------- v0.4.0：本地媒体预览 / 资料 / 密码 / 通话 ----------

  /** 待发送（未确认）媒体消息的本地预览 URL：只认内存 Blob，不落盘 */
  localMediaUrl(m: Message): string | null {
    const k = m.client_msg_id
    if (this.blobUrls.has(k)) return this.blobUrls.get(k)!
    const blob = this.blobs.get(k)
    if (!blob) return null
    const u = URL.createObjectURL(blob)
    this.blobUrls.set(k, u)
    return u
  }

  async updateProfile(patch: { nickname?: string; avatar_key?: string; allow_find?: boolean }) {
    const user = await api.patchMe(patch)
    this.set({ me: user, users: { ...this.state.users, [user.id]: user } })
    return user
  }

  /**
   * v0.5.0：退出其他设备。换上服务端新 token 后主动重连 WS：
   * 旧连接随后被服务端踢掉时，session_revoked 的校验用新 token 能通过，不会误登出。
   */
  async revokeOtherSessions() {
    const r = await api.revokeOtherSessions()
    setToken(r.token)
    this.socket.stop()
    this.socket.start()
    this.showToast('已退出其他设备')
  }

  /** v0.5.0「我的 → 存储」：清本地媒体缓存并重置内存里的媒体状态 */
  async clearCaches() {
    await clearMediaCaches().catch(() => {})
    this.blobUrls.forEach((u) => URL.revokeObjectURL(u))
    this.blobUrls.clear()
    this.blobs.clear()
    this.imageLoads.clear()
    this.set({ voice: {}, transcripts: {} })
    this.showToast('缓存已清理')
  }

  /** 本地缓存占用估算（浏览器口径，含 IndexedDB 等） */
  async storageUsage(): Promise<number | null> {
    try {
      const est = await navigator.storage?.estimate?.()
      return est?.usage ?? null
    } catch {
      return null
    }
  }

  /** 已登录改密码：用服务端返回的新 token 无感续用（其他设备的旧 token 已被吊销） */
  async changePassword(oldPassword: string, newPassword: string) {
    const r = await api.changePassword(oldPassword, newPassword)
    setToken(r.token)
    this.set({ me: r.user, users: { ...this.state.users, [r.user.id]: r.user } })
    return r
  }

  startCall(conversationId: number, kind: 'audio' | 'video') {
    const conv = this.state.convs[conversationId]
    if (!conv?.peer) return
    if (!this.state.config.call_enabled) return this.showToast('服务器未开启通话')
    void this.callMgr.start(conversationId, conv.peer.id, kind)
  }
  acceptCall() {
    void this.callMgr.accept()
  }
  rejectCall() {
    this.callMgr.reject()
  }
  hangupCall() {
    this.callMgr.hangup()
  }
  toggleCallMute() {
    this.callMgr.toggleMute()
  }
  toggleCallCamera() {
    this.callMgr.toggleCamera()
  }

  // ---------- v0.5.0 好友 ----------

  async loadFriends() {
    try {
      const list = await api.friends()
      const friends: Record<number, FriendEntry> = {}
      for (const f of list) friends[f.user.id] = f
      this.set({ friends })
      this.addUsers(list.map((f) => f.user))
      // presence 服务开启时，好友在线点跟随实时 presence；未开启时用列表的 online 快照
      if (list.length) this.loadPresence(list.map((f) => f.user.id))
    } catch {}
  }

  async loadFriendRequests() {
    try {
      const r = await api.friendRequests()
      this.set({ friendRequests: r })
      this.addUsers([...r.incoming, ...r.outgoing].map((x) => x.user))
    } catch {}
  }

  /** 发送申请；服务端在反向有待处理申请时直接成友（status=accepted） */
  async sendFriendRequest(userId: number, message?: string) {
    const r = await api.sendFriendRequest(userId, message)
    await this.loadFriendRequests()
    if (r.status === 'accepted') {
      await this.loadFriends()
      this.showToast('已成为好友')
    } else {
      this.showToast('好友申请已发送')
    }
    return r
  }

  async acceptFriendRequest(id: number) {
    await api.acceptFriendRequest(id)
    await Promise.all([this.loadFriends(), this.loadFriendRequests()])
    this.showToast('已添加好友')
  }

  async rejectFriendRequest(id: number) {
    await api.rejectFriendRequest(id)
    await this.loadFriendRequests()
  }

  async cancelFriendRequest(id: number) {
    await api.cancelFriendRequest(id)
    await this.loadFriendRequests()
  }

  /** 搜索结果卡上的「接受」：只有 relation 没有请求 id，按用户找 incoming 申请 */
  async acceptFriendByUser(userId: number) {
    let item = this.state.friendRequests.incoming.find((x) => x.user.id === userId)
    if (!item) {
      await this.loadFriendRequests()
      item = this.state.friendRequests.incoming.find((x) => x.user.id === userId)
    }
    if (item) await this.acceptFriendRequest(item.id)
  }

  async setFriendRemark(userId: number, remark: string) {
    await api.patchFriend(userId, remark)
    await this.loadFriends()
    this.showToast(remark ? '备注已保存' : '备注已清除')
  }

  async removeFriend(userId: number) {
    await api.removeFriend(userId)
    await this.loadFriends()
    this.showToast('已删除好友')
  }

  // ---------- v0.5.0 群通话 ----------

  startGroupCall(conversationId: number, kind: CallKind) {
    if (!this.state.config.group_call_enabled) return this.showToast('服务器未开启群通话')
    if (this.state.call) return this.showToast('已在通话中')
    if (this.state.groupRooms[conversationId]) return this.joinGroupCall(conversationId)
    void this.groupCallMgr.start(conversationId, kind)
  }

  joinGroupCall(conversationId: number) {
    const room = this.state.groupRooms[conversationId]
    if (!room) return this.showToast('通话已结束')
    if (this.state.call) return this.showToast('已在通话中')
    void this.groupCallMgr.join(room.callId, conversationId, room.kind)
  }

  async leaveGroupCall() {
    const convId = this.state.groupCall?.conversationId
    await this.groupCallMgr.leave()
    if (convId) {
      const groupRooms = { ...this.state.groupRooms }
      delete groupRooms[convId]
      this.set({ groupRooms })
    }
  }

  toggleGroupMute() {
    this.groupCallMgr.toggleMute()
  }
  toggleGroupCamera() {
    this.groupCallMgr.toggleCamera()
  }

  clearGroupInvite() {
    if (this.state.groupInvite) this.set({ groupInvite: null })
  }

  /** 群通话房间概要维护：人数增减、邀请横幅、结束清理（通话内事件另由 groupCallMgr 处理） */
  private onGroupCallEvent(type: string, d: any) {
    if (!d) return
    const meId = this.state.me?.id
    const rooms = { ...this.state.groupRooms }
    const byCallId = (callId: string) => Object.entries(rooms).find(([, r]) => r.callId === callId)
    switch (type) {
      case 'group_call_started': {
        const convId = Number(d.conversation_id)
        if (!convId) return
        rooms[convId] = { callId: d.call_id, kind: d.kind === 'video' ? 'video' : 'audio', count: Math.max(1, rooms[convId]?.count ?? 1) }
        this.set({ groupRooms: rooms })
        // creator 可能是 user_id 或 PublicUser（SPEC 未写死），两种都兼容
        const creator: PublicUser =
          d.creator && typeof d.creator === 'object'
            ? d.creator
            : (this.state.users[Number(d.creator ?? d.from_user_id)] ?? { id: Number(d.creator ?? d.from_user_id) || 0, nickname: '', avatar_url: '' })
        if (creator.id && creator.id !== meId && !this.state.groupCall && this.state.groupInvite?.callId !== d.call_id) {
          this.set({
            groupInvite: { callId: d.call_id, conversationId: convId, kind: d.kind === 'video' ? 'video' : 'audio', creator, ts: Date.now() },
          })
        }
        return
      }
      case 'group_call_joined': {
        const hit = byCallId(d.call_id)
        if (hit) {
          const [cid, r] = hit
          // 自己在通话里时人数以参与者名单为准，避免事件与名单双计
          const gc = this.state.groupCall
          const count = gc && gc.callId === d.call_id ? gc.participants.length + 1 : r.count + 1
          rooms[Number(cid)] = { ...r, count }
          this.set({ groupRooms: rooms })
        }
        return
      }
      case 'group_call_left': {
        const hit = byCallId(d.call_id)
        if (hit) {
          const [cid, r] = hit
          const gc = this.state.groupCall
          const count = gc && gc.callId === d.call_id ? gc.participants.length + 1 : r.count - 1
          if (count <= 0) delete rooms[Number(cid)]
          else rooms[Number(cid)] = { ...r, count }
          this.set({ groupRooms: rooms })
        }
        return
      }
      case 'group_call_ended': {
        const convId = Number(d.conversation_id)
        if (convId && rooms[convId]?.callId === d.call_id) {
          delete rooms[convId]
          this.set({ groupRooms: rooms })
        } else {
          const hit = byCallId(d.call_id)
          if (hit) {
            delete rooms[Number(hit[0])]
            this.set({ groupRooms: rooms })
          }
        }
        if (this.state.groupInvite?.callId === d.call_id) this.set({ groupInvite: null })
        return
      }
    }
  }
}

export const store = new Store()
