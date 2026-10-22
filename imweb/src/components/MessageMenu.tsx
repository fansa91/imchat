import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { store } from '../store'
import type { Message } from '../types'
import { MSG_CALL, MSG_TEXT, MSG_VOICE } from '../types'
import { Icon } from '../icons'

/** 长按（手机，底部弹出）/ 悬停按钮、右键（桌面，气泡旁浮层）的消息菜单 */
export function MessageMenu({
  m,
  rect,
  reactions,
  readInfo,
  onReply,
  onClose,
}: {
  m: Message
  rect: DOMRect
  reactions: string[]
  readInfo?: string
  onReply: (m: Message) => void
  onClose: () => void
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)
  const [custom, setCustom] = useState(false)
  const [emoji, setEmoji] = useState('')
  const me = store.get().me!
  const mine = m.sender_id === me.id
  const mobile = window.matchMedia('(max-width: 839px)').matches

  useLayoutEffect(() => {
    if (mobile || !ref.current) return
    const w = ref.current.offsetWidth
    const h = ref.current.offsetHeight
    let top = rect.bottom + 6
    if (top + h > window.innerHeight - 8) top = Math.max(8, rect.top - h - 6)
    let left = mine ? rect.right - w : rect.left
    left = Math.max(8, Math.min(left, window.innerWidth - w - 8))
    setPos({ top, left })
  }, [rect, mine, mobile, custom])

  useEffect(() => {
    const k = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', k)
    return () => window.removeEventListener('keydown', k)
  }, [onClose])

  const act = (fn: () => void) => () => {
    fn()
    onClose()
  }
  const mineReacted = new Set((m.reactions ?? []).filter((r) => r.user_ids.includes(me.id)).map((r) => r.emoji))

  return (
    <div className="menu-scrim" onClick={onClose} onContextMenu={(e) => (e.preventDefault(), onClose())}>
      <div
        ref={ref}
        className={'msg-menu' + (mobile ? ' sheet' : '')}
        style={mobile ? undefined : pos ? { top: pos.top, left: pos.left } : { visibility: 'hidden' }}
        onClick={(e) => e.stopPropagation()}
        role="menu"
      >
        <div className="react-bar">
          {reactions.map((e) => (
            <button key={e} className={'react-btn' + (mineReacted.has(e) ? ' on' : '')} aria-label={`回应 ${e}`} onClick={act(() => store.react(m, e))}>
              {e}
            </button>
          ))}
          <button className="react-btn plus" aria-label="其他表情" onClick={() => setCustom((v) => !v)}>
            ＋
          </button>
        </div>
        {custom && (
          <form
            className="custom-emoji"
            onSubmit={(e) => {
              e.preventDefault()
              const v = emoji.trim()
              if (v) {
                store.react(m, [...v].slice(0, 8).join(''))
                onClose()
              }
            }}
          >
            <input autoFocus placeholder="输入或粘贴一个表情" value={emoji} onChange={(e) => setEmoji(e.target.value)} maxLength={16} />
            <button type="submit">确定</button>
          </form>
        )}
        <div className="menu-actions">
          {/* 通话记录不可引用回复、不可撤回（SPEC §1.4）：不显示回复，撤回由 canRecall 排除 */}
          {m.msg_type !== MSG_CALL && (
            <button role="menuitem" onClick={act(() => onReply(m))}>
              <Icon name="reply" size={20} /> 回复
            </button>
          )}
          {m.msg_type === MSG_TEXT && (
            <button
              role="menuitem"
              onClick={act(() => {
                navigator.clipboard?.writeText(m.content).then(
                  () => store.showToast('已复制'),
                  () => store.showToast('复制失败'),
                )
              })}
            >
              <Icon name="copy" size={20} /> 复制
            </button>
          )}
          {m.msg_type === MSG_VOICE && m.seq > 0 && store.get().config.stt_enabled && !store.get().transcripts[`${m.conversation_id}:${m.seq}`] && (
            <button role="menuitem" onClick={act(() => store.transcribe(m))}>
              <Icon name="subtitles" size={20} /> 转文字
            </button>
          )}
          {store.canRecall(m) && (
            <button role="menuitem" onClick={act(() => store.recall(m))}>
              <Icon name="undo" size={20} /> 撤回
            </button>
          )}
          <button role="menuitem" className="danger" onClick={act(() => store.deleteForMe(m))}>
            <Icon name="delete" size={20} /> 删除（仅自己）
          </button>
        </div>
        {readInfo && <div className="menu-foot">{readInfo}</div>}
      </div>
    </div>
  )
}
