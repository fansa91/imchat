import type { CallContent, Conversation, Message, SystemContent, User } from './types'

const pad = (n: number) => String(n).padStart(2, '0')
const sameDay = (a: Date, b: Date) => a.toDateString() === b.toDateString()

export function hm(iso: string) {
  const d = new Date(iso)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** 会话列表时间：今天 HH:mm / 昨天 / 周X / M月D日 / yyyy/M/D */
export function listTime(iso: string) {
  const d = new Date(iso)
  const now = new Date()
  if (sameDay(d, now)) return hm(iso)
  const y = new Date(now)
  y.setDate(now.getDate() - 1)
  if (sameDay(d, y)) return '昨天'
  const diff = (now.getTime() - d.getTime()) / 86400000
  if (diff < 7) return '周' + '日一二三四五六'[d.getDay()]
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日`
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`
}

/** 聊天中的日期分隔 */
export function dayLabel(iso: string) {
  const d = new Date(iso)
  const t = listTime(iso)
  return /^\d{2}:\d{2}$/.test(t) ? `今天 ${hm(iso)}` : `${t} ${hm(iso)}`
}

export const displayName = (u?: User) => u?.nickname || u?.email?.split('@')[0] || '未知用户'

export function initial(name: string) {
  const ch = [...name.trim()][0] || '?'
  return ch.toUpperCase()
}

/** 根据用户 ID 选一组 M3 容器色作为头像底色 */
export function avatarTone(id = 0) {
  return ['primary', 'secondary', 'tertiary'][id % 3]
}


export const convTitle = (c?: Conversation) => (c?.type === 2 ? c.name || '群聊' : displayName(c?.peer))

/** 系统消息文本：把自己替换成“你” */
export function systemText(m: Message, meId?: number) {
  let p: SystemContent
  try {
    p = JSON.parse(m.content)
  } catch {
    return m.content
  }
  const who = (u?: { id: number; nickname: string }) => (u ? (u.id === meId ? '你' : u.nickname) : '')
  const names = (p.targets ?? []).map(who).join('、')
  const a = who(p.actor)
  switch (p.op) {
    case 'create':
      return `${a} 创建了群聊“${p.name}”${names ? `，并邀请了 ${names}` : ''}`
    case 'join':
      return `${a} 邀请 ${names} 加入了群聊`
    case 'remove':
      return `${a} 将 ${names} 移出了群聊`
    case 'leave':
      return `${a} 退出了群聊${names ? `，${names} 成为新群主` : ''}`
    case 'rename':
      return `${a} 将群名修改为“${p.name}”`
    case 'announce':
      return p.value ? `${a} 更新了群公告` : `${a} 清空了群公告`
    case 'role':
      return p.value === 'admin' ? `${a} 将 ${names} 设为管理员` : `${a} 取消了 ${names} 的管理员`
  }
  return p.text || '[系统消息]'
}

/** 会话列表 / 引用里的单行预览 */
export function previewOf(m: Message, meId: number | undefined, senderName: (id: number) => string) {
  if (m.recalled) return `${m.sender_id === meId ? '你' : senderName(m.sender_id)}撤回了一条消息`
  if (m.msg_type === 10) return systemText(m, meId)
  return shortPreview(m.msg_type, m.content)
}

export const UNSUPPORTED = '[不支持的消息，请升级]'

/** 引用条 / 回复栏 / 会话列表里按类型显示的简短内容 */
export function shortPreview(msgType: number, content: string) {
  if (msgType === 1) return content
  if (msgType === 2) return '[图片]'
  if (msgType === 3) return '[语音]'
  if (msgType === 4) return '[视频]'
  if (msgType === 5) {
    // v0.4.0：文件预览带文件名（服务端 PreviewText 只有“[文件]”，客户端自行拼）
    try {
      const name = (JSON.parse(content) as { name?: string }).name
      if (name) return `[文件] ${name}`
    } catch {}
    return '[文件]'
  }
  if (msgType === 6) {
    try {
      const c = JSON.parse(content) as CallContent
      return c.kind === 'video' ? '[视频通话]' : '[语音通话]'
    } catch {
      return '[通话]'
    }
  }
  return UNSUPPORTED
}

/** 文件大小：B / KB / MB，保留一位小数（去掉末尾 .0） */
export function fmtSize(bytes: number) {
  if (!bytes || bytes < 0) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB']
  let v = bytes / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${Math.round(v * 10) / 10} ${units[i]}`
}

/** mm:ss（超过 1 小时显示 h:mm:ss），通话时长用 */
export function fmtCallDuration(ms: number) {
  const s = Math.max(0, Math.round(ms / 1000))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const mm = h ? String(m).padStart(2, '0') : String(m)
  return `${h ? h + ':' : ''}${mm}:${String(sec).padStart(2, '0')}`
}

/** 通话记录消息的展示文案（SPEC §1.4），meId 用于区分主叫/被叫视角 */
export function callText(content: string, meId?: number): string {
  let c: CallContent
  try {
    c = JSON.parse(content)
  } catch {
    return '[通话]'
  }
  const label = c.kind === 'video' ? '视频通话' : '语音通话'
  const iAmCaller = c.caller_id === meId
  switch (c.outcome) {
    case 'completed':
      return `${label} ${fmtCallDuration(c.duration_ms)}`
    case 'missed':
      return iAmCaller ? '对方未接听' : '未接听'
    case 'rejected':
      return iAmCaller ? '对方已拒绝' : '已拒绝'
    case 'cancelled':
      return iAmCaller ? '已取消' : '对方已取消'
    case 'busy':
      return '对方忙线'
    case 'failed':
      return '通话失败'
    default:
      return label
  }
}

/** 在线状态文字：“在线” / “最后在线 今天 14:03” */
export function presenceText(p?: { online: boolean; last_seen: string | null }) {
  if (!p) return ''
  if (p.online) return '在线'
  return p.last_seen ? `最后在线 ${dayLabel(p.last_seen)}` : '离线'
}
