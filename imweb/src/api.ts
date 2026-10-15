import type { AuthResult, ClientConfig, Conversation, Member, Message, Presence, User } from './types'

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
  searchUser: (email: string) => req<User>('GET', `/api/users/search?email=${encodeURIComponent(email)}`),
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
      | { kind: 'image'; mime: string; size: number; width: number; height: number },
  ) =>
    req<{ key: string; url: string; method: string; headers: Record<string, string>; expires_at: string }>('POST', '/api/uploads/presign', body),
  media: (cid: number, seq: number) => req<{ url: string; mime: string; size: number }>('GET', `/api/conversations/${cid}/messages/${seq}/media`),
  // v0.3.0
  presence: (ids: number[]) => req<Presence[]>('GET', `/api/presence?ids=${ids.join(',')}`),
  transcribe: (cid: number, seq: number) =>
    req<{ text: string; cached: boolean }>('POST', `/api/conversations/${cid}/messages/${seq}/transcribe`),
}
