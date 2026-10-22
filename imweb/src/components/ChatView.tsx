import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { store, type State } from '../store'
import type { Message } from '../types'
import { MSG_CALL, MSG_SYSTEM } from '../types'
import { ConvAvatar } from './Avatar'
import { Composer, type ComposerHandle } from './Composer'
import { MessageItem } from './MessageItem'
import { MessageMenu } from './MessageMenu'
import { Icon } from '../icons'
import { convTitle, dayLabel, displayName, presenceText } from '../util'
import { voiceKey } from '../voice'

const GAP = 5 * 60 * 1000 // 超过 5 分钟显示时间分隔

export function ChatView({ state, id, onBack, onInfo }: { state: State; id: number; onBack: () => void; onInfo: () => void }) {
  const conv = state.convs[id]
  const thread = state.threads[id]
  const me = state.me!
  const isGroup = conv?.type === 2
  const typing = state.typing[id]
  const typingOn = !!typing?.ts
  const reads = state.reads[id] ?? {}
  const scroller = useRef<HTMLDivElement>(null)
  const stickBottom = useRef(true)
  const prevHeight = useRef(0)
  const prevFirst = useRef<number | undefined>(undefined)
  const composer = useRef<ComposerHandle>(null)
  const [replyTo, setReplyTo] = useState<Message | null>(null)
  const [menu, setMenu] = useState<{ m: Message; rect: DOMRect } | null>(null)
  const [flash, setFlash] = useState(0)
  const [dragging, setDragging] = useState(false)
  const dragDepth = useRef(0)
  const [annHidden, setAnnHidden] = useState(() => localStorage.getItem(`im.ann.${id}`) === (conv?.announcement_at ?? ''))

  useEffect(() => {
    stickBottom.current = true
    store.openConversation(id).catch(() => {})
    if (window.matchMedia('(pointer: fine)').matches) composer.current?.focus()
    return () => store.closeConversation()
  }, [id])

  useEffect(() => {
    setAnnHidden(localStorage.getItem(`im.ann.${id}`) === (conv?.announcement_at ?? ''))
  }, [id, conv?.announcement_at])

  const items = thread?.items ?? []
  const all: Message[] = [...items, ...(thread?.pending ?? [])]

  // 保持滚动位置：新消息时贴底，加载历史时保持视口不跳
  useLayoutEffect(() => {
    const el = scroller.current
    if (!el) return
    const first = items[0]?.seq
    if (prevFirst.current !== undefined && first !== undefined && first < prevFirst.current) {
      el.scrollTop += el.scrollHeight - prevHeight.current
    } else if (stickBottom.current) {
      el.scrollTop = el.scrollHeight
    }
    prevFirst.current = first
    prevHeight.current = el.scrollHeight
  })

  useEffect(() => {
    if (document.visibilityState === 'visible') store.markRead(id)
  }, [id, conv?.last_seq])

  const onScroll = () => {
    const el = scroller.current!
    stickBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    if (el.scrollTop < 200) store.loadMore(id)
  }

  const nameOf = useCallback(
    (uid: number) => (uid === me.id ? '你' : displayName(state.users[uid], state.friends[uid]?.remark) || '群成员'),
    [state.users, state.friends, me.id],
  )

  const jumpTo = useCallback(
    async (seq: number) => {
      stickBottom.current = false
      const ok = await store.ensureLoaded(id, seq)
      if (!ok) return store.showToast('原消息已不可见')
      requestAnimationFrame(() => {
        document.getElementById(`msg-${id}-${seq}`)?.scrollIntoView({ block: 'center', behavior: 'smooth' })
        setFlash(seq)
        setTimeout(() => setFlash((f) => (f === seq ? 0 : f)), 1600)
      })
    },
    [id],
  )

  const onMenu = useCallback((m: Message, rect: DOMRect) => setMenu({ m, rect }), [])
  const onReedit = useCallback((t: string) => composer.current?.setText(t), [])
  const closeMenu = useCallback(() => setMenu(null), [])

  // 已读：单聊看对方，群聊统计“N人已读”
  const others = Object.entries(reads).filter(([uid]) => Number(uid) !== me.id)
  const readCount = (seq: number) => others.filter(([, s]) => s >= seq).length
  const memberCount = conv?.member_count ?? others.length + 1
  const readLabel = (seq: number) => {
    const n = readCount(seq)
    if (!isGroup) return n > 0 ? '已读' : '已送达'
    if (n === 0) return '已送达'
    return n >= memberCount - 1 ? '全部已读' : `${n}人已读`
  }

  let lastMine = -1
  all.forEach((m, i) => {
    if (m.sender_id === me.id && m.msg_type !== MSG_SYSTEM && m.msg_type !== MSG_CALL && !m.recalled) lastMine = i
  })

  // 在线状态（服务端 presence_enabled 时）
  const presenceOn = !!state.config.presence_enabled
  const peerPresence = !isGroup && conv?.peer ? state.presence[conv.peer.id] : undefined
  let groupOnline = 0
  if (presenceOn && isGroup) {
    const ids = new Set<number>((state.members[id]?.map((x) => x.user.id) ?? conv?.members_preview?.map((u) => u.id)) ?? [])
    ids.delete(me.id)
    for (const u of ids) if (state.presence[u]?.online) groupOnline++
    if (state.conn === 'online') groupOnline++ // 自己
  }
  const subtitle = typingOn
    ? isGroup
      ? `${displayName(state.users[typing!.uid], state.friends[typing!.uid]?.remark)} 正在输入…`
      : '正在输入…'
    : isGroup
      ? `${conv?.member_count ?? ''} 位成员` + (presenceOn && groupOnline ? ` · ${groupOnline} 人在线` : '')
      : (presenceOn && presenceText(peerPresence)) || ''
  const title = convTitle(conv, conv?.peer ? state.friends[conv.peer.id]?.remark : undefined)
  // v0.5.0 群通话：本群进行中的房间（我不在里面时显示加入条）
  const groupRoom = isGroup ? state.groupRooms[id] : undefined
  const inThisCall = state.groupCall?.conversationId === id

  const attachOn = !!state.config.image_enabled || !!state.config.video_enabled || !!state.config.file_enabled
  /** kind 由 Composer 菜单指定；粘贴/拖入时省略，按 MIME 判断（图→图片、视频→视频、其余→文件） */
  const sendFiles = useCallback(
    (files: File[], kind?: 'image' | 'video' | 'file') => {
      if (!files.length) return
      const reply = replyTo?.seq
      for (const f of files.slice(0, 9)) {
        const k = kind ?? (f.type.startsWith('image/') ? 'image' : f.type.startsWith('video/') ? 'video' : 'file')
        if (k === 'image') store.sendImage(id, f, reply)
        else if (k === 'video') store.sendVideo(id, f, reply)
        else store.sendFile(id, f, reply)
      }
      if (files.length > 9) store.showToast('一次最多发送 9 个')
      setReplyTo(null)
      stickBottom.current = true
    },
    [id, replyTo],
  )
  const hasFiles = (e: React.DragEvent) => attachOn && [...e.dataTransfer.types].includes('Files')

  const showAnn = isGroup && !!conv?.announcement && !annHidden

  return (
    <section
      className="pane chat-pane"
      onDragEnter={(e) => {
        if (!hasFiles(e)) return
        e.preventDefault()
        dragDepth.current++
        setDragging(true)
      }}
      onDragOver={(e) => {
        if (!hasFiles(e)) return
        e.preventDefault()
        e.dataTransfer.dropEffect = 'copy'
      }}
      onDragLeave={(e) => {
        if (!hasFiles(e)) return
        dragDepth.current = Math.max(0, dragDepth.current - 1)
        if (!dragDepth.current) setDragging(false)
      }}
      onDrop={(e) => {
        if (!hasFiles(e)) return
        e.preventDefault()
        dragDepth.current = 0
        setDragging(false)
        sendFiles([...e.dataTransfer.files])
      }}
    >
      {dragging && (
        <div className="drop-overlay" aria-hidden="true">
          <Icon name="image" size={48} />
          <span className="title-medium">松开发送文件</span>
        </div>
      )}
      <header className="top-bar chat-bar">
        <md-icon-button class="back-btn" aria-label="返回" onClick={onBack}>
          <Icon name="back" />
        </md-icon-button>
        <ConvAvatar conv={conv} size={40} online={presenceOn && !!peerPresence?.online} />
        <div className="chat-title">
          <div className="title-medium">{title}</div>
          <div className={'body-small ' + (typingOn || (presenceOn && peerPresence?.online) ? 'accent' : 'muted')}>{subtitle}</div>
        </div>
        <div className="spacer" />
        {/* v0.4.0 通话：仅单聊且服务端开启时显示；无离线推送，对方不在线打不通（服务端直接判 offline） */}
        {!isGroup && state.config.call_enabled && (
          <>
            <md-icon-button aria-label="语音通话" title="语音通话（对方需在线）" onClick={() => store.startCall(id, 'audio')}>
              <Icon name="call" />
            </md-icon-button>
            <md-icon-button aria-label="视频通话" title="视频通话（对方需在线）" onClick={() => store.startCall(id, 'video')}>
              <Icon name="videocam" />
            </md-icon-button>
          </>
        )}
        {/* v0.5.0 群通话：群聊且服务端开启时显示；已有房间时改走下方加入条 */}
        {isGroup && state.config.group_call_enabled && !groupRoom && (
          <>
            <md-icon-button aria-label="群语音通话" title="群语音通话" onClick={() => store.startGroupCall(id, 'audio')}>
              <Icon name="call" />
            </md-icon-button>
            <md-icon-button aria-label="群视频通话" title="群视频通话" onClick={() => store.startGroupCall(id, 'video')}>
              <Icon name="videocam" />
            </md-icon-button>
          </>
        )}
        {isGroup && (
          <md-icon-button aria-label="群聊信息" onClick={onInfo}>
            <Icon name="info" />
          </md-icon-button>
        )}
      </header>

      {isGroup && groupRoom && !inThisCall && (
        <div className="gcall-bar" role="note">
          <Icon name="call" size={20} />
          <span className="ann-text">通话中 · {groupRoom.count} 人</span>
          <md-filled-tonal-button onClick={() => store.joinGroupCall(id)}>加入</md-filled-tonal-button>
        </div>
      )}

      {showAnn && (
        <div className="announcement" role="note">
          <Icon name="campaign" size={20} />
          <button className="ann-text" onClick={onInfo} title="查看群公告">
            {conv!.announcement}
          </button>
          <md-icon-button
            aria-label="关闭公告"
            onClick={() => {
              localStorage.setItem(`im.ann.${id}`, conv?.announcement_at ?? '')
              setAnnHidden(true)
            }}
          >
            <Icon name="close" size={18} />
          </md-icon-button>
        </div>
      )}

      <div className="messages" ref={scroller} onScroll={onScroll}>
        {thread?.loadingMore && (
          <div className="center pad">
            <md-circular-progress indeterminate style={{ '--md-circular-progress-size': '28px' } as any} />
          </div>
        )}
        {thread && !thread.hasMore && items.length >= 50 && <div className="history-end">以上是全部消息</div>}
        {!thread?.loaded && (
          <div className="center grow">
            <md-circular-progress indeterminate />
          </div>
        )}
        {thread?.loaded && all.length === 0 && (
          <div className="center grow muted">
            <ConvAvatar conv={conv} size={72} />
            <p>和 {title} 打个招呼吧</p>
          </div>
        )}

        {all.map((m, i) => {
          const prev = all[i - 1]
          const next = all[i + 1]
          const t = new Date(m.created_at).getTime()
          const showTime = !prev || t - new Date(prev.created_at).getTime() > GAP
          const nextBreak = !next || new Date(next.created_at).getTime() - t > GAP
          const plain = (x?: Message) => !!x && x.msg_type !== MSG_SYSTEM && !x.recalled
          const first = showTime || !plain(prev) || prev.sender_id !== m.sender_id
          const last = nextBreak || !plain(next) || next.sender_id !== m.sender_id
          const replyOrig = m.reply_to ? items.find((x) => x.seq === m.reply_to!.seq) : undefined
          return (
            <div key={m.client_msg_id + ':' + m.sender_id}>
              {showTime && <div className="time-sep">{dayLabel(m.created_at)}</div>}
              <MessageItem
                m={m}
                meId={me.id}
                isGroup={isGroup}
                first={first}
                last={last}
                senderName={nameOf(m.sender_id)}
                senderUser={state.users[m.sender_id]}
                replyName={m.reply_to ? nameOf(m.reply_to.sender_id) : ''}
                replyOrig={replyOrig}
                voiceState={m.seq ? state.voice[voiceKey(id, m.seq)] : undefined}
                transcript={m.seq ? state.transcripts[voiceKey(id, m.seq)] : undefined}
                highlighted={flash === m.seq && m.seq > 0}
                onMenu={onMenu}
                onJump={jumpTo}
                onReedit={onReedit}
              />
              {i === lastMine && (
                <div className="receipt">
                  {m.status === 'sending' ? '发送中…' : m.status === 'failed' ? '发送失败' : readLabel(m.seq)}
                </div>
              )}
            </div>
          )
        })}
        {typingOn && (
          <div className="row theirs first last">
            <div className="bubble typing-bubble" aria-label="对方正在输入">
              <span />
              <span />
              <span />
            </div>
          </div>
        )}
      </div>

      <Composer
        key={id}
        ref={composer}
        id={id}
        replyTo={replyTo}
        replyName={replyTo ? nameOf(replyTo.sender_id) : ''}
        onCancelReply={() => setReplyTo(null)}
        onSent={() => {
          setReplyTo(null)
          stickBottom.current = true
        }}
        onFiles={sendFiles}
      />

      {menu && (
        <MessageMenu
          m={menu.m}
          rect={menu.rect}
          reactions={state.config.reactions}
          readInfo={menu.m.sender_id === me.id && menu.m.seq ? readLabel(menu.m.seq) : undefined}
          onReply={(m) => {
            setReplyTo(m)
            composer.current?.focus()
          }}
          onClose={closeMenu}
        />
      )}
    </section>
  )
}
