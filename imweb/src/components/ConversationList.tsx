import type { Conversation } from '../types'
import type { State } from '../store'
import { ConvAvatar } from './Avatar'
import { convTitle, displayName, listTime, previewOf } from '../util'
import { Icon } from '../icons'

export function ConversationList({
  state,
  activeId,
  onOpen,
  onNew,
}: {
  state: State
  activeId: number | null
  onOpen: (id: number) => void
  onNew: () => void
}) {
  const list = Object.values(state.convs).sort(
    (a, b) => new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime(),
  )

  return (
    <section className="pane list-pane">
      <header className="top-bar">
        <h1 className="title-large">消息</h1>
      </header>
      {state.conn !== 'online' && (
        <div className="conn-banner">
          <Icon name="cloudOff" size={18} />
          {state.conn === 'connecting' ? '正在连接…' : '连接已断开，正在重试'}
        </div>
      )}

      <div className="conv-scroll">
        {list.length === 0 ? (
          <div className="empty-list">
            <p>还没有会话</p>
            <md-filled-tonal-button onClick={onNew}>发起聊天</md-filled-tonal-button>
          </div>
        ) : (
          <ul className="conv-list" role="list">
            {list.map((c) => (
              <ConvItem
                key={c.id}
                c={c}
                active={c.id === activeId}
                typing={!!state.typing[c.id]?.ts}
                online={c.type !== 2 && !!c.peer && !!state.presence[c.peer.id]?.online}
                meId={state.me?.id}
                users={state.users}
                friends={state.friends}
                groupRoom={state.groupRooms[c.id]}
                onOpen={onOpen}
              />
            ))}
          </ul>
        )}
      </div>

      <md-fab class="new-fab" variant="primary" label="新聊天" aria-label="新聊天" onClick={onNew}>
        <Icon slot="icon" name="edit" />
      </md-fab>
    </section>
  )
}

function ConvItem({
  c,
  active,
  typing,
  online,
  meId,
  users,
  friends,
  groupRoom,
  onOpen,
}: {
  c: Conversation
  active: boolean
  typing: boolean
  online: boolean
  meId?: number
  users: State['users']
  friends: State['friends']
  groupRoom?: State['groupRooms'][number]
  onOpen: (id: number) => void
}) {
  const name = convTitle(c, c.peer ? friends[c.peer.id]?.remark : undefined)
  const last = c.last_message
  const senderName = (id: number) =>
    users[id] ? displayName(users[id], friends[id]?.remark) : id === last?.sender_id && c.last_sender_name ? c.last_sender_name : '群成员'
  let preview = '开始聊天吧'
  if (typing) preview = '正在输入…'
  else if (groupRoom) preview = `通话中 · ${groupRoom.count} 人，点击加入`
  else if (last) {
    const body = previewOf(last, meId, senderName)
    const plain = !last.recalled && last.msg_type !== 10
    if (plain && last.sender_id === meId) preview = '我：' + body
    else if (plain && c.type === 2) preview = senderName(last.sender_id) + '：' + body
    else preview = body
  }
  return (
    <li>
      <button className={'conv-item' + (active ? ' active' : '')} onClick={() => onOpen(c.id)}>
        <md-ripple />
        <md-focus-ring style={{ '--md-focus-ring-shape': '28px' } as any} />
        <ConvAvatar conv={c} size={48} online={online} />
        <div className="conv-main">
          <div className="conv-row">
            <span className={'conv-name' + (c.unread ? ' bold' : '')}>{name}</span>
            <span className={'conv-time' + (c.unread ? ' accent' : '')}>{last ? listTime(last.created_at) : ''}</span>
          </div>
          <div className="conv-row">
            <span className={'conv-preview' + (typing || groupRoom ? ' accent' : '') + (c.unread ? ' strong' : '')}>{preview}</span>
            {c.unread > 0 && <span className="badge">{c.unread > 99 ? '99+' : c.unread}</span>}
          </div>
        </div>
      </button>
    </li>
  )
}
