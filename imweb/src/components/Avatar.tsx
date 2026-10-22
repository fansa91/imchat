import { useEffect, useState, type ReactNode } from 'react'
import { getToken } from '../api'
import { avatarTone, displayName, initial } from '../util'
import type { Conversation, User } from '../types'

/**
 * 头像图加载（v0.4.0，SPEC §3.1）：user.avatar_url 是站内相对路径
 * （/api/users/{id}/avatar?v=...），<img> 带不了 Authorization 头，
 * 照图片消息的办法：fetch 带 token 取 Blob（自动跟随 302 到预签名地址）再用 Object URL。
 * 按完整 URL（含 v 参数）缓存，v 变即重新拉。历史手工填的绝对地址直接当图片用（不带 token）。
 */
const avatarUrls = new Map<string, Promise<string>>()

function avatarObjectUrl(url: string): Promise<string> {
  let p = avatarUrls.get(url)
  if (!p) {
    p = (async () => {
      const res = await fetch(url, { headers: { Authorization: `Bearer ${getToken()}` } })
      if (!res.ok) throw new Error(String(res.status))
      return URL.createObjectURL(await res.blob())
    })()
    avatarUrls.set(url, p)
    p.catch(() => avatarUrls.delete(url))
  }
  return p
}

export function Avatar({ user, size = 40 }: { user?: User; size?: number }) {
  const tone = avatarTone(user?.id)
  const raw = user?.avatar_url || ''
  const [img, setImg] = useState<string | null>(null)
  useEffect(() => {
    if (!raw) {
      setImg(null)
      return
    }
    if (/^https?:\/\//i.test(raw)) {
      setImg(raw)
      return
    }
    let alive = true
    avatarObjectUrl(raw).then(
      (u) => alive && setImg(u),
      () => alive && setImg(null),
    )
    return () => {
      alive = false
    }
  }, [raw])
  return (
    <div
      className="avatar"
      style={{
        width: size,
        height: size,
        fontSize: size * 0.42,
        background: `var(--md-sys-color-${tone}-container)`,
        color: `var(--md-sys-color-on-${tone}-container)`,
      }}
      aria-hidden="true"
    >
      {img ? <img src={img} alt="" draggable={false} /> : initial(displayName(user))}
    </div>
  )
}

/** 群头像：最多 4 个成员首字拼成九宫格（2x2），不足时 2 人左右排、3 人品字形 */
export function GroupAvatar({ users, size = 40 }: { users: User[]; size?: number }) {
  const list = users.slice(0, 4)
  const n = Math.max(1, list.length)
  const cell = n === 1 ? size : size / 2
  return (
    <div className={`avatar group-avatar n${n}`} style={{ width: size, height: size }} aria-hidden="true">
      {list.map((u) => {
        const tone = avatarTone(u.id)
        return (
          <span
            key={u.id}
            style={{
              fontSize: cell * 0.5,
              background: `var(--md-sys-color-${tone}-container)`,
              color: `var(--md-sys-color-on-${tone}-container)`,
            }}
          >
            {initial(displayName(u))}
          </span>
        )
      })}
    </div>
  )
}

/** online 为 true 时在头像右下角显示绿点（单聊对方在线） */
export function ConvAvatar({ conv, size = 40, online }: { conv?: Conversation; size?: number; online?: boolean }) {
  if (conv?.type === 2) return <GroupAvatar users={conv.members_preview ?? []} size={size} />
  return <OnlineDot online={online} size={size}><Avatar user={conv?.peer} size={size} /></OnlineDot>
}

export function OnlineDot({ online, size, children }: { online?: boolean; size: number; children: ReactNode }) {
  if (!online) return <>{children}</>
  const d = Math.max(8, Math.round(size * 0.26))
  return (
    <span className="avatar-wrap" style={{ width: size, height: size }}>
      {children}
      <span className="online-dot" style={{ width: d, height: d }} aria-label="在线" />
    </span>
  )
}
