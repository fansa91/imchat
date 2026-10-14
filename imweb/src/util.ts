import type { Conversation, Message, SystemContent, User } from './types'

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
  if (m.msg_type === 1) return m.content
  if (m.msg_type === 3) return '[语音]'
  if (m.msg_type === 2) return '[图片]'
  return '[消息]'
}
