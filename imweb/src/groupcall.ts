import { api, ApiError } from './api'
import type { CallKind, GroupCallJoinResult, PublicUser, TrackKind } from './types'

/**
 * v0.5.0 群通话状态机（SPEC §3.3/§3.7）：媒体走 Cloudflare Realtime SFU，
 * 全部 SFU 操作由 imserver 代理（App Secret 不下发），本人一个 session 一条
 * RTCPeerConnection：发布 = addTrack + offer → tracks/publish 透传 answer；
 * 订阅 = tracks/subscribe（不带 offer）→ 服务端回 SFU 的 offer 并置
 * requires_immediate_renegotiation → 本地回 answer 走 renegotiate 接口。
 * WS 只负责房间事件（group_call_*），媒体不依赖 WS，断线重连后用
 * GET /api/calls/group/{call_id} 恢复成员与订阅（recover）。
 */

/** 发布前把每条轨道在 offer 中对应 m-line 的 mid 附上：Cloudflare 发布
 * 本地轨道必填 mid，缺失回 406 invalid_params「Missing mid in track」
 * （2026-10-22 生产实测，v0.5.2 修）。 */
function attachMids(pc: RTCPeerConnection, tracks: { kind: TrackKind; mid?: string }[]) {
  const byKind = new Map<string, string>()
  for (const t of pc.getTransceivers()) {
    if (t.sender.track && t.mid) byKind.set(t.sender.track.kind, t.mid)
  }
  for (const tr of tracks) {
    const m = byKind.get(tr.kind)
    if (m) tr.mid = m
  }
}

export interface GParticipant {
  user: PublicUser
  stream: MediaStream | null // 该成员音+视频轨合并的流
  hasVideo: boolean
}

export interface GroupCallUiState {
  callId: string
  conversationId: number
  kind: CallKind
  joinedAt: number
  muted: boolean
  cameraOff: boolean
  localStream: MediaStream | null
  participants: GParticipant[]
}

interface Deps {
  onChange: (call: GroupCallUiState | null) => void
  toast: (msg: string) => void
  meId: () => number | undefined
}

interface NormResp {
  desc?: { type: string; sdp: string }
  renegotiate: boolean
  tracks: { trackName: string; mid?: string; kind?: TrackKind }[]
}

/** 服务端透传的字段命名以 SPEC 的 snake_case 为准，同时兼容 Cloudflare 原生 camelCase */
function norm(r: any): NormResp {
  const d = r?.session_description ?? r?.sessionDescription
  return {
    desc: d?.sdp ? { type: d.type || 'answer', sdp: d.sdp } : undefined,
    renegotiate: !!(r?.requires_immediate_renegotiation ?? r?.requiresImmediateRenegotiation),
    tracks: (r?.tracks ?? []).map((t: any) => ({
      trackName: t.track_name ?? t.trackName ?? '',
      mid: t.mid,
      kind: t.kind,
    })),
  }
}

/** track_name 形如 "{user_id}/audio"，解析发布者与类型 */
function parseTrackName(name: string): { ownerId: number; kind: TrackKind } | null {
  const m = /^(\d+)\/(audio|video)$/.exec(name)
  return m ? { ownerId: Number(m[1]), kind: m[2] as TrackKind } : null
}

export class GroupCallManager {
  private pc: RTCPeerConnection | null = null
  private state: GroupCallUiState | null = null
  /** mid -> 轨道归属（subscribe 响应里登记，ontrack 时据此归位） */
  private midMap = new Map<string, { ownerId: number; kind: TrackKind }>()
  private subscribed = new Set<string>()
  /** SFU 操作串行化：避免多路 subscribe 同时改 signaling 状态 */
  private queue: Promise<void> = Promise.resolve()

  constructor(private deps: Deps) {}

  private setState(s: GroupCallUiState | null) {
    this.state = s
    this.deps.onChange(s)
  }
  private patch(p: Partial<GroupCallUiState>) {
    if (this.state) this.setState({ ...this.state, ...p })
  }
  private enqueue(fn: () => Promise<void>): Promise<void> {
    const run = this.queue.then(fn, fn)
    this.queue = run.catch(() => {})
    return run
  }

  // ---------- 进入 / 离开 ----------

  async start(conversationId: number, kind: CallKind) {
    if (this.state) return this.deps.toast('已在通话中')
    try {
      const res = await api.createGroupCall(conversationId, kind)
      await this.setup(res, conversationId, kind)
    } catch (e) {
      this.teardown()
      this.deps.toast(e instanceof ApiError ? e.message : '无法加入群通话')
    }
  }

  async join(callId: string, conversationId: number, kind: CallKind) {
    if (this.state) return this.deps.toast('已在通话中')
    try {
      const res = await api.joinGroupCall(callId)
      await this.setup(res, conversationId, kind)
    } catch (e) {
      this.teardown()
      this.deps.toast(e instanceof ApiError ? e.message : '无法加入群通话')
    }
  }

  private async setup(res: GroupCallJoinResult, conversationId: number, kind: CallKind) {
    const me = this.deps.meId()
    this.midMap.clear()
    this.subscribed.clear()
    this.setState({
      callId: res.call_id,
      conversationId,
      kind,
      joinedAt: Date.now(),
      muted: false,
      cameraOff: false,
      localStream: null,
      participants: (res.participants ?? []).filter((u) => u.id !== me).map((u) => ({ user: u, stream: null, hasVideo: false })),
    })
    const pc = new RTCPeerConnection({ iceServers: res.ice_servers?.iceServers ?? [] })
    this.pc = pc
    pc.ontrack = (e) => this.onTrack(e)

    let stream: MediaStream
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: true,
        video: kind === 'video' ? { facingMode: 'user' } : false,
      })
    } catch {
      await api.leaveGroupCall(res.call_id).catch(() => {})
      this.teardown()
      return this.deps.toast('没有麦克风/摄像头权限，无法加入通话')
    }
    if (this.state?.callId !== res.call_id) return stream.getTracks().forEach((t) => t.stop())
    this.patch({ localStream: stream })

    // 发布本人轨道：track_name 由服务端分配为 {我的id}/audio|video
    try {
      const tracks: { track_name: string; kind: TrackKind; mid?: string }[] = []
      for (const t of stream.getTracks()) {
        pc.addTrack(t, stream)
        const k = t.kind === 'video' ? 'video' : 'audio'
        tracks.push({ track_name: `${me}/${k}`, kind: k as TrackKind })
      }
      const offer = await pc.createOffer()
      await pc.setLocalDescription(offer)
      attachMids(pc, tracks)
      const resp = norm(await api.publishTracks(res.call_id, { session_description: { type: 'offer', sdp: offer.sdp ?? '' }, tracks }))
      await this.applyDesc(resp)
    } catch (e) {
      console.warn('group publish failed', e)
      this.deps.toast('发布音视频失败，请检查网络后重试')
    }
    // 拉房间现状：补齐成员与已发布的轨道并订阅
    await this.syncRoom()
  }

  /** GET 房间状态 → 更新成员名单，订阅所有未订阅的他人轨道 */
  private async syncRoom() {
    const s = this.state
    if (!s) return
    try {
      const room = await api.groupCallRoom(s.callId)
      if (this.state?.callId !== s.callId) return
      const me = this.deps.meId()
      for (const u of room.participants ?? []) if (u.id !== me) this.ensureParticipant(u)
      const byOwner = new Map<number, TrackKind[]>()
      for (const t of room.tracks ?? []) {
        if (t.user_id === me) continue
        const list = byOwner.get(t.user_id) ?? []
        list.push(t.kind)
        byOwner.set(t.user_id, list)
      }
      for (const [ownerId, kinds] of byOwner) {
        const todo = kinds.filter((k) => !this.subscribed.has(`${ownerId}/${k}`))
        if (todo.length) await this.subscribe(ownerId, todo)
      }
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        this.deps.toast('通话已结束')
        this.teardown()
      }
    }
  }

  /** WS 重连后恢复：房间还在就刷新成员与订阅，已结束则退出 */
  async recover() {
    if (this.state) await this.syncRoom()
  }

  async leave() {
    const s = this.state
    if (!s) return
    await api.leaveGroupCall(s.callId).catch(() => {})
    this.teardown()
  }

  /** 登出时静默收尾（不再调 leave 接口） */
  dispose() {
    this.teardown()
  }

  // ---------- 订阅 / 发布控制 ----------

  private subscribe(ownerId: number, kinds: TrackKind[]): Promise<void> {
    return this.enqueue(async () => {
      const s = this.state
      if (!s || !this.pc) return
      const todo = kinds.filter((k) => !this.subscribed.has(`${ownerId}/${k}`))
      if (!todo.length) return
      try {
        const resp = norm(
          await api.subscribeTracks(s.callId, { tracks: todo.map((kind) => ({ owner_id: ownerId, kind })) }),
        )
        for (const k of todo) this.subscribed.add(`${ownerId}/${k}`)
        // 登记 mid -> 归属：优先按 track_name 解析，解析不出按请求顺序兜底
        resp.tracks.forEach((t, i) => {
          if (!t.mid) return
          const parsed = parseTrackName(t.trackName)
          this.midMap.set(t.mid, parsed ?? { ownerId, kind: t.kind ?? todo[i] ?? todo[0] })
        })
        await this.applyDesc(resp)
      } catch (e) {
        console.warn('group subscribe failed', e)
      }
    })
  }

  /** 处理 SFU 返回的 sessionDescription：answer 直接设置；offer 则回 answer 走 renegotiate */
  private async applyDesc(resp: NormResp) {
    const pc = this.pc
    const s = this.state
    if (!pc || !s || !resp.desc) return
    if (resp.desc.type === 'answer') {
      await pc.setRemoteDescription({ type: 'answer', sdp: resp.desc.sdp })
      return
    }
    await pc.setRemoteDescription({ type: 'offer', sdp: resp.desc.sdp })
    const answer = await pc.createAnswer()
    await pc.setLocalDescription(answer)
    const back = norm(await api.renegotiateGroupCall(s.callId, { type: 'answer', sdp: answer.sdp ?? '' }))
    if (back.desc && back.desc.type === 'answer') await pc.setRemoteDescription({ type: 'answer', sdp: back.desc.sdp })
  }

  private onTrack(e: RTCTrackEvent) {
    const s = this.state
    if (!s) return
    const mid = e.transceiver?.mid ?? undefined
    const info = mid ? this.midMap.get(mid) : undefined
    if (!info) return
    const p = s.participants.find((x) => x.user.id === info.ownerId)
    if (!p) return
    const stream = p.stream ?? new MediaStream()
    if (!stream.getTracks().some((t) => t.id === e.track.id)) stream.addTrack(e.track)
    e.track.onended = () => this.refreshParticipant(info.ownerId)
    this.upsertParticipant({ ...p, stream, hasVideo: info.kind === 'video' ? true : p.hasVideo })
  }

  private refreshParticipant(ownerId: number) {
    const s = this.state
    const p = s?.participants.find((x) => x.user.id === ownerId)
    if (!p?.stream) return
    const live = p.stream.getTracks().filter((t) => t.readyState === 'live')
    const stream = live.length ? new MediaStream(live) : null
    this.upsertParticipant({ ...p, stream, hasVideo: !!stream?.getVideoTracks().length })
  }

  private ensureParticipant(u: PublicUser) {
    const s = this.state
    if (!s || s.participants.some((x) => x.user.id === u.id)) return
    this.patch({ participants: [...s.participants, { user: u, stream: null, hasVideo: false }] })
  }

  private upsertParticipant(p: GParticipant) {
    const s = this.state
    if (!s) return
    this.patch({ participants: s.participants.map((x) => (x.user.id === p.user.id ? p : x)) })
  }

  private removeParticipant(userId: number) {
    const s = this.state
    if (!s) return
    for (const k of [...this.subscribed]) if (k.startsWith(`${userId}/`)) this.subscribed.delete(k)
    this.patch({ participants: s.participants.filter((x) => x.user.id !== userId) })
  }

  // ---------- 控件 ----------

  toggleMute() {
    const s = this.state
    if (!s?.localStream) return
    const next = !s.muted
    s.localStream.getAudioTracks().forEach((t) => (t.enabled = !next))
    this.patch({ muted: next })
  }

  /** 关摄像头 = 调 tracks/close 真正停发（SFU 不再转发）；重开 = 重新发布视频轨 */
  toggleCamera() {
    const s = this.state
    if (!s || s.kind !== 'video') return
    void this.enqueue(async () => {
      const cur = this.state
      if (!cur || !this.pc) return
      const me = this.deps.meId()
      try {
        if (!cur.cameraOff) {
          const resp = norm(await api.closeTracks(cur.callId, ['video']))
          await this.applyDesc(resp)
          const sender = this.pc.getSenders().find((x) => x.track?.kind === 'video')
          sender?.track?.stop()
          cur.localStream?.getVideoTracks().forEach((t) => t.stop())
          const rest = cur.localStream?.getAudioTracks() ?? []
          this.patch({ cameraOff: true, localStream: rest.length ? new MediaStream(rest) : cur.localStream })
        } else {
          const vs = await navigator.mediaDevices.getUserMedia({ video: { facingMode: 'user' } })
          const track = vs.getVideoTracks()[0]
          const merged = new MediaStream([...(cur.localStream?.getTracks() ?? []), track])
          this.pc.addTrack(track, merged)
          const offer = await this.pc.createOffer()
          await this.pc.setLocalDescription(offer)
          const vtracks: { track_name: string; kind: TrackKind; mid?: string }[] = [{ track_name: `${me}/video`, kind: 'video' }]
          attachMids(this.pc, vtracks)
          const resp = norm(
            await api.publishTracks(cur.callId, {
              session_description: { type: 'offer', sdp: offer.sdp ?? '' },
              tracks: vtracks,
            }),
          )
          await this.applyDesc(resp)
          this.patch({ cameraOff: false, localStream: merged })
        }
      } catch (e) {
        console.warn('group toggle camera failed', e)
        this.deps.toast('摄像头切换失败')
      }
    })
  }

  // ---------- 服务端事件（store 转发） ----------

  handle(type: string, d: any) {
    const s = this.state
    const me = this.deps.meId()
    switch (type) {
      case 'group_call_joined':
        if (s && d?.call_id === s.callId && d.user && d.user.id !== me) this.ensureParticipant(d.user)
        return
      case 'group_call_tracks':
        if (s && d?.call_id === s.callId && d.user_id !== me && Array.isArray(d.tracks)) {
          void this.subscribe(d.user_id, d.tracks.map((t: any) => t.kind as TrackKind))
        }
        return
      case 'group_call_tracks_closed':
        if (s && d?.call_id === s.callId && d.user_id !== me) {
          const kinds: TrackKind[] = d.kinds ?? []
          for (const k of kinds) this.subscribed.delete(`${d.user_id}/${k}`)
          const p = s.participants.find((x) => x.user.id === d.user_id)
          if (p?.stream) {
            const keep = p.stream.getTracks().filter((t) => !kinds.includes(t.kind as TrackKind))
            this.upsertParticipant({ ...p, stream: keep.length ? new MediaStream(keep) : null, hasVideo: kinds.includes('video') ? false : p.hasVideo })
          }
        }
        return
      case 'group_call_left':
        if (s && d?.call_id === s.callId && d.user_id !== me) this.removeParticipant(d.user_id)
        return
      case 'group_call_ended':
        if (s && d?.call_id === s.callId) {
          this.deps.toast('通话已结束')
          this.teardown()
        }
        return
    }
  }

  private teardown() {
    if (this.pc) {
      this.pc.ontrack = null
      try {
        this.pc.close()
      } catch {}
      this.pc = null
    }
    this.state?.localStream?.getTracks().forEach((t) => t.stop())
    this.midMap.clear()
    this.subscribed.clear()
    this.queue = Promise.resolve()
    if (this.state) this.setState(null)
  }
}
