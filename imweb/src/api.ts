import type {
  AuthResult,
  ClientConfig,
  Conversation,
  FriendEntry,
  FriendRequests,
  GroupCallJoinResult,
  GroupRoomState,
  IceServers,
  Member,
  Message,
  Presence,
  SessionDescription,
  TrackKind,
  TracksResponse,
  User,
  UserSearchResult,
} from './types'

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(message)
  }
}

let token = localStorage.getItem('im.token') || ''
let onUnauthorized: () => void = () => {}

export const getToken = () => token
export function setToken(t: string) {
  token = t
  if (t) localStorage.setItem('im.token', t)
  else localStorage.removeItem('im.token')
}
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  let res: Response
  try {
    res = await fetch(path, {
      method,
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch {
    throw new ApiError(0, 'network', '网络连接失败')
  }
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    if (res.status === 401 && token) onUnauthorized()
    throw new ApiError(res.status, data.code || 'error', data.message || `请求失败 (${res.status})`)
  }
  return data as T
}

export const api = {
  register: (email: string, password: string, nickname: string) =>
    req<AuthResult>('POST', '/api/register', { email, password, nickname }),
  login: (email: string, password: string) => req<AuthResult>('POST', '/api/login', { email, password }),
  me: () => req<User>('GET', '/api/me'),
  // v0.5.0：搜索返回 PublicUser + relation，且不再含 email（SPEC §1.2/§1.3）
  searchUser: (email: string) => req<UserSearchResult>('GET', `/api/users/search?email=${encodeURIComponent(email)}`),
  searchUserById: (id: number) => req<UserSearchResult>('GET', `/api/users/search?id=${id}`),
  openDirect: (peerId: number) => req<{ conversation_id: number }>('POST', '/api/conversations/direct', { peer_id: peerId }),
  conversations: () => req<Conversation[]>('GET', '/api/conversations'),
  messages: (cid: number, opts: { after?: number; before?: number; changedSince?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (opts.after) q.set('after_seq', String(opts.after))
    if (opts.before) q.set('before_seq', String(opts.before))
    if (opts.changedSince) q.set('changed_since', String(opts.changedSince))
    q.set('limit', String(opts.limit ?? 50))
    return req<Message[]>('GET', `/api/conversations/${cid}/messages?${q}`)
  },
  reads: (cid: number) => req<Record<string, number>>('GET', `/api/conversations/${cid}/reads`),
  // v0.2.0
  config: () => req<ClientConfig>('GET', '/api/config'),
  conversation: (cid: number) => req<Conversation>('GET', `/api/conversations/${cid}`),
  createGroup: (name: string, memberIds: number[]) =>
    req<{ conversation_id: number; conversation?: Conversation }>('POST', '/api/conversations/group', { name, member_ids: memberIds }),
  updateGroup: (cid: number, patch: { name?: string; announcement?: string }) => req<Conversation>('PATCH', `/api/conversations/${cid}`, patch),
  members: (cid: number) => req<Member[]>('GET', `/api/conversations/${cid}/members`),
  addMembers: (cid: number, userIds: number[]) => req<{ added: number[] }>('POST', `/api/conversations/${cid}/members`, { user_ids: userIds }),
  removeMember: (cid: number, uid: number) => req<{ ok: boolean }>('DELETE', `/api/conversations/${cid}/members/${uid}`),
  setRole: (cid: number, uid: number, role: 'admin' | 'member') => req<{ ok: boolean }>('PUT', `/api/conversations/${cid}/members/${uid}/role`, { role }),
  leave: (cid: number) => req<{ ok: boolean }>('POST', `/api/conversations/${cid}/leave`),
  hidden: (cid: number, minSeq: number) => req<number[]>('GET', `/api/conversations/${cid}/hidden?min_seq=${minSeq}`),
  presign: (
    body:
      | { kind: 'voice'; mime: string; size: number; duration_ms: number }
      | { kind: 'image'; mime: string; size: number; width: number; height: number }
      // v0.4.0
      | { kind: 'video'; mime: string; size: number; duration_ms: number; width: number; height: number }
      | { kind: 'file'; mime: string; size: number; name: string }
      | { kind: 'avatar'; mime: string; size: number; width: number; height: number },
  ) =>
    req<{ key: string; url: string; method: string; headers: Record<string, string>; expires_at: string }>('POST', '/api/uploads/presign', body),
  media: (cid: number, seq: number) => req<{ url: string; mime: string; size: number }>('GET', `/api/conversations/${cid}/messages/${seq}/media`),
  // v0.3.0
  presence: (ids: number[]) => req<Presence[]>('GET', `/api/presence?ids=${ids.join(',')}`),
  transcribe: (cid: number, seq: number) =>
    req<{ text: string; cached: boolean }>('POST', `/api/conversations/${cid}/messages/${seq}/transcribe`),
  // v0.4.0（allow_find 为 v0.5.0 新增，SPEC §2.1）
  patchMe: (patch: { nickname?: string; avatar_key?: string; allow_find?: boolean }) => req<User>('PATCH', '/api/me', patch),
  changePassword: (oldPassword: string, newPassword: string) =>
    req<AuthResult>('POST', '/api/me/password', { old_password: oldPassword, new_password: newPassword }),
  forgotPassword: (email: string) => req<{ ok: boolean }>('POST', '/api/password/forgot', { email }),
  resetPassword: (token: string, newPassword: string) => req<{ ok: boolean }>('POST', '/api/password/reset', { token, new_password: newPassword }),
  getIceServers: () => req<IceServers>('GET', '/api/calls/ice-servers'),
  // v0.5.0：退出其他设备——服务端 token_version+1 并给当前设备换发新 token（SPEC §2.1）
  revokeOtherSessions: () => req<{ token: string }>('POST', '/api/me/revoke-other-sessions'),
  // v0.5.0 好友（SPEC §1.3）
  friends: () => req<FriendEntry[]>('GET', '/api/friends'),
  friendRequests: () => req<FriendRequests>('GET', '/api/friends/requests'),
  sendFriendRequest: (userId: number, message?: string) =>
    req<{ id?: number; status: string }>('POST', '/api/friends/requests', { user_id: userId, ...(message ? { message } : {}) }),
  acceptFriendRequest: (id: number) => req<{ ok: boolean }>('POST', `/api/friends/requests/${id}/accept`),
  rejectFriendRequest: (id: number) => req<{ ok: boolean }>('POST', `/api/friends/requests/${id}/reject`),
  cancelFriendRequest: (id: number) => req<{ ok: boolean }>('DELETE', `/api/friends/requests/${id}`),
  patchFriend: (userId: number, remark: string) => req<{ ok: boolean }>('PATCH', `/api/friends/${userId}`, { remark }),
  removeFriend: (userId: number) => req<{ ok: boolean }>('DELETE', `/api/friends/${userId}`),
  // v0.5.0 群通话：服务端代理 Cloudflare SFU（SPEC §3.3）
  createGroupCall: (conversationId: number, kind: 'audio' | 'video') =>
    req<GroupCallJoinResult>('POST', '/api/calls/group', { conversation_id: conversationId, kind }),
  joinGroupCall: (callId: string) => req<GroupCallJoinResult>('POST', `/api/calls/group/${callId}/join`),
  publishTracks: (callId: string, body: { session_description: SessionDescription; tracks: { track_name: string; kind: TrackKind }[] }) =>
    req<TracksResponse>('POST', `/api/calls/group/${callId}/tracks/publish`, body),
  subscribeTracks: (callId: string, body: { tracks: { owner_id: number; kind: TrackKind }[]; session_description?: SessionDescription }) =>
    req<TracksResponse>('POST', `/api/calls/group/${callId}/tracks/subscribe`, body),
  renegotiateGroupCall: (callId: string, sessionDescription: SessionDescription) =>
    req<TracksResponse>('POST', `/api/calls/group/${callId}/renegotiate`, { session_description: sessionDescription }),
  closeTracks: (callId: string, kinds: TrackKind[]) => req<TracksResponse>('POST', `/api/calls/group/${callId}/tracks/close`, { kinds }),
  leaveGroupCall: (callId: string) => req<{ ok: boolean }>('POST', `/api/calls/group/${callId}/leave`),
  groupCallRoom: (callId: string) => req<GroupRoomState>('GET', `/api/calls/group/${callId}`),
}

/**
 * 带进度与取消的 PUT（预签名直传用，fetch 没有上传进度）。
 * 返回的 promise 在 abort 后以 AbortError 拒绝，HTTP 非 2xx 时以普通 Error 拒绝。
 */
export function putWithProgress(
  url: string,
  headers: Record<string, string>,
  body: Blob,
  onProgress: (fraction: number) => void,
): { promise: Promise<void>; abort: () => void } {
  const xhr = new XMLHttpRequest()
  const promise = new Promise<void>((resolve, reject) => {
    xhr.open('PUT', url)
    for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v)
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded / e.total)
    }
    xhr.onload = () => (xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new Error(`上传失败 (${xhr.status})`)))
    xhr.onerror = () => reject(new Error('网络连接失败'))
    xhr.onabort = () => reject(new DOMException('aborted', 'AbortError'))
    xhr.send(body)
  })
  return { promise, abort: () => xhr.abort() }
}
