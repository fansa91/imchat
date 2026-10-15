export interface User {
  id: number
  email: string
  nickname: string
  avatar_url: string
  created_at: string
}

export type MsgStatus = 'sending' | 'failed' | 'sent'

export const MSG_TEXT = 1
export const MSG_IMAGE = 2
export const MSG_VOICE = 3
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
  peer?: User
  last_message?: Message
  last_sender_name?: string
  name?: string
  owner_id?: number
  my_role?: number // 0 成员 1 管理员 2 群主
  member_count?: number
  members_preview?: User[]
  announcement?: string
  announcement_by?: number
  announcement_at?: string
}

export interface Member {
  user: User
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
}

export interface AuthResult {
  token: string
  expires_at: string
  user: User
}
