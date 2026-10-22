export interface User {
  id: number
  email: string
  nickname: string
  avatar_url: string
  created_at: string
  /** v0.5.0：仅 GET /api/me 返回——隐私开关「允许通过邮箱 / 用户 ID 找到我」 */
  allow_find?: boolean
}

/** v0.5.0 他人视角的用户结构（SPEC §1.2）：不含 email */
export interface PublicUser {
  id: number
  nickname: string
  avatar_url: string
}

/** v0.5.0 好友关系状态（对搜索者而言，SPEC §1.3） */
export type FriendRelation = 'none' | 'friend' | 'pending_out' | 'pending_in'

export interface UserSearchResult extends PublicUser {
  relation: FriendRelation
}

/** v0.5.0 好友列表项（SPEC §1.3）：remark 是我给对方的备注（单向） */
export interface FriendEntry {
  user: PublicUser
  remark: string
  online: boolean
  created_at: string
}

export interface FriendRequestItem {
  id: number
  user: PublicUser
  message: string
  created_at: string
}

export interface FriendRequests {
  incoming: FriendRequestItem[]
  outgoing: FriendRequestItem[]
}

export type MsgStatus = 'sending' | 'failed' | 'sent'

export const MSG_TEXT = 1
export const MSG_IMAGE = 2
export const MSG_VOICE = 3
export const MSG_VIDEO = 4
export const MSG_FILE = 5
export const MSG_CALL = 6
export const MSG_SYSTEM = 10

export interface Reaction {
  emoji: string
  count: number
  user_ids: number[]
}

export interface ReplyPreview {
  seq: number
  sender_id: number
  msg_type: number
  preview: string
  recalled: boolean
  missing?: boolean
}

export interface Message {
  conversation_id: number
  seq: number // 未确认的本地消息为 0
  sender_id: number
  client_msg_id: string
  msg_type: number
  content: string
  created_at: string
  reply_to_seq?: number
  reply_to?: ReplyPreview
  recalled?: boolean
  recalled_at?: string
  mod_seq?: number
  reactions?: Reaction[]
  status?: MsgStatus
  /** v0.4.0：本地上传进度 0~1（仅 pending 的媒体消息用，不上送服务端） */
  progress?: number
}

/** 语音消息 content（JSON） */
export interface VoiceContent {
  key: string
  mime: string
  duration_ms: number
  size: number
  waveform?: number[]
}

/** 图片消息 content（JSON），v0.3.0 */
export interface ImageContent {
  key: string
  mime: string
  size: number
  width: number
  height: number
}

/** 视频消息 content（JSON），v0.4.0（SPEC §2.2） */
export interface VideoContent {
  key: string
  mime: string
  size: number
  duration_ms: number
  width: number
  height: number
  poster_key?: string
}

/** 文件消息 content（JSON），v0.4.0（SPEC §2.2） */
export interface FileContent {
  key: string
  mime: string
  size: number
  name: string
}

export type CallKind = 'audio' | 'video'
export type CallOutcome = 'completed' | 'missed' | 'rejected' | 'cancelled' | 'busy' | 'failed'

/** 通话记录消息 content（JSON），v0.4.0（SPEC §1.4），只能由服务端生成；v0.5.0 扩展群通话字段 */
export interface CallContent {
  call_id: string
  kind: CallKind
  outcome: CallOutcome
  duration_ms: number
  caller_id: number
  /** v0.5.0：single | group，缺失按 single */
  mode?: 'single' | 'group'
  participant_count?: number
  joined_count?: number
}

/** v0.5.0 群通话（SPEC §3.3）：服务端代理 Cloudflare SFU 的 SDP 载荷 */
export interface SessionDescription {
  type: string
  sdp: string
}

export type TrackKind = 'audio' | 'video'

export interface PublishedTrack {
  track_name: string
  kind: TrackKind
  mid?: string
}

/** GET /api/calls/group/{call_id} 的 tracks 项（带发布者） */
export interface GroupRoomTrack extends PublishedTrack {
  user_id: number
}

export interface GroupCallJoinResult {
  call_id: string
  session_id: string
  ice_servers: IceServers
  participants: PublicUser[]
}

export interface GroupRoomState {
  state: string
  participants: PublicUser[]
  tracks: GroupRoomTrack[]
}

/** publish / subscribe / close 的响应：透传 Cloudflare 的 sessionDescription 与重协商标记 */
export interface TracksResponse {
  session_description?: SessionDescription
  requires_immediate_renegotiation?: boolean
  tracks?: PublishedTrack[]
}

/** GET /api/calls/ice-servers 响应（SPEC §1.2） */
export interface IceServers {
  iceServers: RTCIceServer[]
  expires_at: string
}

/** 在线状态（GET /api/presence 与 WS presence 事件） */
export interface Presence {
  user_id: number
  online: boolean
  last_seen: string | null
}

/** 系统消息 content（JSON） */
export interface SystemContent {
  op: 'create' | 'join' | 'leave' | 'remove' | 'rename' | 'announce' | 'role' | string
  actor: { id: number; nickname: string }
  targets?: { id: number; nickname: string }[]
  name?: string
  value?: string
  text: string
}

export interface Conversation {
  id: number
  type: number // 1 单聊 2 群聊
  last_seq: number
  mod_seq?: number
  read_seq: number
  join_seq?: number
  unread: number
  updated_at: string
  peer?: PublicUser
  last_message?: Message
  last_sender_name?: string
  name?: string
  owner_id?: number
  my_role?: number // 0 成员 1 管理员 2 群主
  member_count?: number
  members_preview?: PublicUser[]
  announcement?: string
  announcement_by?: number
  announcement_at?: string
}

export interface Member {
  user: PublicUser
  role: number
  read_seq: number
  joined_at: string
}

export interface ClientConfig {
  recall_seconds: number
  group_max_members: number
  reactions: string[]
  voice_enabled: boolean
  voice_max_seconds?: number
  voice_max_bytes?: number
  voice_ttl_days?: number
  // v0.3.0：旧服务端没有这些字段时隐藏对应功能
  image_enabled?: boolean
  image_max_bytes?: number
  presence_enabled?: boolean
  stt_enabled?: boolean
  // v0.4.0（SPEC §5）：同样缺失时按关闭处理
  call_enabled?: boolean
  call_ring_timeout_seconds?: number
  file_enabled?: boolean
  file_max_bytes?: number
  video_enabled?: boolean
  video_max_bytes?: number
  password_reset_enabled?: boolean
  // v0.5.0（SPEC §3.1）：群通话（SFU）
  group_call_enabled?: boolean
  group_call_max_participants?: number
}

export interface AuthResult {
  token: string
  expires_at: string
  user: User
}
