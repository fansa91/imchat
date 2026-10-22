import { memo, useRef } from 'react'
import { store, type Transcript, type VoiceState } from '../store'
import type { Message, Reaction } from '../types'
import { MSG_CALL, MSG_FILE, MSG_IMAGE, MSG_SYSTEM, MSG_TEXT, MSG_VIDEO, MSG_VOICE } from '../types'
import { Avatar } from './Avatar'
import { VoiceBubble } from './VoiceBubble'
import { ImageBubble } from './ImageBubble'
import { VideoBubble } from './VideoBubble'
import { FileBubble } from './FileBubble'
import { Icon } from '../icons'
import { callText, hm, shortPreview, systemText, UNSUPPORTED } from '../util'

export interface ItemProps {
  m: Message
  meId: number
  isGroup: boolean
  first: boolean
  last: boolean
  senderName: string
  senderUser?: import('../types').PublicUser
  replyName: string
  replyOrig?: Message
  voiceState?: VoiceState
  transcript?: Transcript
  highlighted: boolean
  onMenu: (m: Message, rect: DOMRect) => void
  onJump: (seq: number) => void
  onReedit: (text: string) => void
}

export const MessageItem = memo(function MessageItem(p: ItemProps) {
  const { m, meId } = p
  const mine = m.sender_id === meId
  const bubbleRef = useRef<HTMLDivElement>(null)
  const press = useRef<{ t: number; x: number; y: number } | null>(null)

  const openMenu = () => {
    const r = bubbleRef.current?.getBoundingClientRect()
    if (r) p.onMenu(m, r)
  }
  // 触屏长按 450ms 打开菜单
  const onPointerDown = (e: React.PointerEvent) => {
    if (e.pointerType === 'mouse') return
    const t = window.setTimeout(() => {
      press.current = null
      navigator.vibrate?.(10)
      openMenu()
    }, 450)
    press.current = { t, x: e.clientX, y: e.clientY }
  }
  const cancelPress = (e?: React.PointerEvent) => {
    if (!press.current) return
    if (e && e.type === 'pointermove' && Math.hypot(e.clientX - press.current.x, e.clientY - press.current.y) < 10) return
    clearTimeout(press.current.t)
    press.current = null
  }
  const menuHandlers = {
    onContextMenu: (e: React.MouseEvent) => {
      e.preventDefault()
      openMenu()
    },
    onPointerDown,
    onPointerUp: () => cancelPress(),
    onPointerCancel: () => cancelPress(),
    onPointerMove: cancelPress,
  }

  if (m.msg_type === MSG_SYSTEM) {
    return (
      <div className="sys-line" id={`msg-${m.conversation_id}-${m.seq}`}>
        {systemText(m, meId)}
      </div>
    )
  }
  if (m.recalled) {
    const draft = mine ? store.recalledDrafts.get(`${m.conversation_id}:${m.seq}`) : undefined
    return (
      <div className="sys-line recalled-line" id={`msg-${m.conversation_id}-${m.seq}`} data-recalled="1">
        {mine ? '你' : p.senderName}撤回了一条消息
        {draft !== undefined && (
          <button className="link-btn" onClick={() => p.onReedit(draft)}>
            重新编辑
          </button>
        )}
      </div>
    )
  }
  // v0.4.0 通话记录：居中系统样式（与系统消息一致），可回应/删除，不可撤回/引用（菜单里处理）
  if (m.msg_type === MSG_CALL) {
    let kind: string | undefined
    try {
      kind = JSON.parse(m.content).kind
    } catch {}
    return (
      <div className="call-line-wrap" id={`msg-${m.conversation_id}-${m.seq}`}>
        <div ref={bubbleRef} className="sys-line call-line" title={hm(m.created_at)} {...menuHandlers}>
          <Icon name={kind === 'video' ? 'videocam' : 'call'} size={14} />
          {callText(m.content, meId)}
        </div>
        {!!m.reactions?.length && (
          <div className="call-reactions">
            <Reactions m={m} list={m.reactions} meId={meId} />
          </div>
        )}
      </div>
    )
  }

  const reply = m.reply_to
  const origRecalled = p.replyOrig ? !!p.replyOrig.recalled : !!reply?.recalled
  const origPreview = p.replyOrig ? shortPreview(p.replyOrig.msg_type, p.replyOrig.content) : reply?.preview
  const isImage = m.msg_type === MSG_IMAGE
  const isVideo = m.msg_type === MSG_VIDEO
  const isFile = m.msg_type === MSG_FILE

  return (
    <div
      className={`row ${mine ? 'mine' : 'theirs'} ${p.first ? 'first' : ''} ${p.last ? 'last' : ''} ${p.highlighted ? 'flash' : ''}`}
      id={`msg-${m.conversation_id}-${m.seq || m.client_msg_id}`}
      data-seq={m.seq}
    >
      {p.isGroup && !mine && <div className="row-avatar">{p.last ? <Avatar user={p.senderUser} size={28} /> : null}</div>}
      <div className="row-body">
        {p.isGroup && !mine && p.first && <div className="sender-name">{p.senderName}</div>}
        <div className="bubble-line">
          {mine && m.status === 'failed' && (
            <md-icon-button class="retry" aria-label="重新发送" title="发送失败，点击重发" onClick={() => store.retry(m)}>
              <Icon name="error" size={20} />
            </md-icon-button>
          )}
          <div
            ref={bubbleRef}
            className={
              'bubble' +
              (m.status === 'sending' ? ' sending' : '') +
              (m.msg_type === MSG_VOICE ? ' voice' : '') +
              (isImage ? ' image' : '') +
              (isVideo ? ' video' : '') +
              (isFile ? ' file' : '') +
              ((isImage || isVideo) && reply ? ' with-quote' : '')
            }
            title={hm(m.created_at)}
            {...menuHandlers}
          >
            {reply && (
              <button className="quote" onClick={(e) => (e.stopPropagation(), p.onJump(reply.seq))} title="跳到原消息">
                <span className="quote-name">{p.replyName}</span>
                <span className="quote-text">{reply.missing ? '原消息不可见' : origRecalled ? '原消息已撤回' : origPreview}</span>
              </button>
            )}
            {m.msg_type === MSG_VOICE ? (
              <VoiceBubble m={m} state={p.voiceState} />
            ) : isImage ? (
              <ImageBubble m={m} />
            ) : isVideo ? (
              <VideoBubble m={m} />
            ) : isFile ? (
              <FileBubble m={m} />
            ) : m.msg_type === MSG_TEXT ? (
              m.content
            ) : (
              <span className="unsupported">{UNSUPPORTED}</span>
            )}
          </div>
          {m.seq > 0 && (
            <div className="hover-tools">
              <button className="tool" aria-label="回应" title="回应 / 更多" onClick={openMenu}>
                <Icon name="smile" size={18} />
              </button>
              <button className="tool" aria-label="更多操作" onClick={openMenu}>
                <Icon name="more" size={18} />
              </button>
            </div>
          )}
        </div>
        {m.msg_type === MSG_VOICE && p.transcript && <TranscriptBlock t={p.transcript} />}
        {!!m.reactions?.length && <Reactions m={m} list={m.reactions} meId={meId} />}
      </div>
    </div>
  )
})

function Reactions({ m, list, meId }: { m: Message; list: Reaction[]; meId: number }) {
  return (
    <div className="reactions">
      {list.map((r) => (
        <button
          key={r.emoji}
          className={'chip' + (r.user_ids.includes(meId) ? ' on' : '')}
          onClick={() => store.react(m, r.emoji)}
          aria-label={`${r.emoji} ${r.count}`}
        >
          <span>{r.emoji}</span>
          {r.count > 1 && <span className="chip-n">{r.count}</span>}
        </button>
      ))}
    </div>
  )
}

/** 语音转文字结果：语音气泡下方的小文字块，可选中 / 一键复制 */
function TranscriptBlock({ t }: { t: Transcript }) {
  if (t.loading) {
    return (
      <div className="transcript loading" aria-live="polite">
        <md-circular-progress indeterminate style={{ '--md-circular-progress-size': '16px' } as any} />
        转写中…
      </div>
    )
  }
  if (!t.text) return null
  return (
    <div className="transcript">
      <span className="transcript-text">{t.text}</span>
      <button
        className="transcript-copy"
        aria-label="复制文字"
        title="复制"
        onClick={() =>
          navigator.clipboard?.writeText(t.text!).then(
            () => store.showToast('已复制'),
            () => store.showToast('复制失败'),
          )
        }
      >
        <Icon name="copy" size={16} />
      </button>
    </div>
  )
}
