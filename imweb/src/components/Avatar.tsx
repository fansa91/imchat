import { avatarTone, displayName, initial } from '../util'
import type { Conversation, User } from '../types'

export function Avatar({ user, size = 40 }: { user?: User; size?: number }) {
  const tone = avatarTone(user?.id)
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
      {initial(displayName(user))}
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

export function ConvAvatar({ conv, size = 40 }: { conv?: Conversation; size?: number }) {
  if (conv?.type === 2) return <GroupAvatar users={conv.members_preview ?? []} size={size} />
  return <Avatar user={conv?.peer} size={size} />
}
