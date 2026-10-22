import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react'
import { store } from '../store'
import type { Message } from '../types'
import { shortPreview } from '../util'
import { Icon } from '../icons'
import { canRecord, fmtDuration, Recorder } from '../voice'

const drafts = new Map<number, string>()

export interface ComposerHandle {
  focus: () => void
  setText: (t: string) => void
}

/**
 * 输入栏：文字 / 语音。
 * 语音两种方式：点一下麦克风开始录（再点发送或取消）；按住麦克风说话，松开发送，按住时左滑取消。
 */
export const Composer = forwardRef<ComposerHandle, {
  id: number
  replyTo: Message | null
  replyName: string
  onCancelReply: () => void
  onSent: () => void
  /** kind 省略时由上层按文件类型判断（粘贴/拖入）；菜单里选的按 kind 发送 */
  onFiles: (files: File[], kind?: 'image' | 'video' | 'file') => void
}>(function Composer({ id, replyTo, replyName, onCancelReply, onSent, onFiles }, ref) {
  const [text, setText] = useState(() => drafts.get(id) ?? '')
  const input = useRef<HTMLTextAreaElement>(null)
  const cfg = store.get().config
  const maxSec = cfg.voice_max_seconds ?? 60
  const [rec, setRec] = useState<null | { elapsed: number; levels: number[]; holding: boolean; cancelHint: boolean }>(null)
  const recorder = useRef<Recorder | null>(null)
  const hold = useRef<{ start: number; x: number } | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const videoInput = useRef<HTMLInputElement>(null)
  const anyInput = useRef<HTMLInputElement>(null)
  const [attachOpen, setAttachOpen] = useState(false)
  const tapModeAt = useRef(0) // 进入点按录音模式的时间，用于忽略同一次点按产生的“幽灵点击”

  useImperativeHandle(ref, () => ({
    focus: () => input.current?.focus(),
    setText: (t: string) => {
      setText(t)
      drafts.set(id, t)
      setTimeout(() => input.current?.focus())
    },
  }))

  useEffect(() => () => recorder.current?.cancel(), [])

  const send = () => {
    if (!text.trim()) return
    store.send(id, text, replyTo?.seq)
    setText('')
    drafts.delete(id)
    onSent()
    input.current?.focus()
  }

  // 粘贴图片（截图 / 复制的图片）直接发送
  const onPaste = (e: React.ClipboardEvent) => {
    if (!cfg.image_enabled) return
    const files = [...e.clipboardData.files].filter((f) => f.type.startsWith('image/'))
    if (!files.length) return
    e.preventDefault()
    onFiles(files)
  }

  const onKey = (e: React.KeyboardEvent) => {
    // 桌面 Enter 发送、Shift+Enter 换行；触屏设备 Enter 换行
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing && window.matchMedia('(pointer: fine)').matches) {
      e.preventDefault()
      send()
    }
  }

  const startRecording = async () => {
    if (recorder.current) return
    if (!canRecord()) return store.showToast('当前浏览器不支持录音')
    const r = new Recorder(maxSec * 1000)
    recorder.current = r
    setRec({ elapsed: 0, levels: [], holding: !!hold.current, cancelHint: false })
    r.onLevel = (level, elapsed) =>
      setRec((s) => (s ? { ...s, elapsed, levels: [...s.levels.slice(-39), level] } : s))
    r.onAutoStop = () => finishRecording(true)
    try {
      await r.start()
    } catch (e) {
      recorder.current = null
      setRec(null)
      hold.current = null
      store.showToast((e as Error)?.name === 'NotAllowedError' ? '没有麦克风权限' : '无法开始录音')
    }
  }

  const finishRecording = async (sendIt: boolean) => {
    const r = recorder.current
    if (!r) return
    recorder.current = null
    hold.current = null
    setRec(null)
    if (!sendIt) {
      r.cancel()
      return
    }
    const out = await r.stop()
    if (!out) return
    if (out.durationMs < 700) return store.showToast('说话时间太短')
    store.sendVoice(id, out, replyTo?.seq)
    onSent()
  }

  // 按住说话
  const onMicDown = (e: React.PointerEvent) => {
    if (rec) return
    ;(e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId)
    hold.current = { start: Date.now(), x: e.clientX }
    startRecording()
  }
  const onMicMove = (e: React.PointerEvent) => {
    if (!hold.current) return
    const cancelHint = e.clientX - hold.current.x < -80
    setRec((s) => (s && s.cancelHint !== cancelHint ? { ...s, cancelHint } : s))
  }
  const onMicUp = (e: React.PointerEvent) => {
    const h = hold.current
    if (!h) return
    if (Date.now() - h.start < 350) {
      // 只是点了一下：进入“点按录音”模式，等用户点发送/取消
      hold.current = null
      tapModeAt.current = Date.now()
      setRec((s) => (s ? { ...s, holding: false } : s))
      return
    }
    finishRecording(e.clientX - h.x >= -80)
  }

  return (
    <footer className="composer-wrap">
      {replyTo && (
        <div className="reply-bar">
          <Icon name="reply" size={18} />
          <div className="reply-bar-text">
            <span className="reply-bar-name">回复 {replyName}</span>
            <span className="reply-bar-preview">{shortPreview(replyTo.msg_type, replyTo.content)}</span>
          </div>
          <md-icon-button aria-label="取消回复" onClick={onCancelReply}>
            <Icon name="close" size={20} />
          </md-icon-button>
        </div>
      )}
      <div className="composer">
        {(cfg.image_enabled || cfg.video_enabled || cfg.file_enabled) && !rec && (
          <div className="attach-wrap">
            <md-icon-button class="image-btn" aria-label="发送附件" title="发送图片 / 视频 / 文件（图片也可粘贴或拖入）" onClick={() => setAttachOpen((o) => !o)}>
              <Icon name="add" />
            </md-icon-button>
            {attachOpen && (
              <>
                <div className="attach-scrim" onClick={() => setAttachOpen(false)} />
                <div className="attach-menu" role="menu">
                  {cfg.image_enabled && (
                    <button
                      type="button"
                      role="menuitem"
                      onClick={() => {
                        setAttachOpen(false)
                        fileInput.current?.click()
                      }}
                    >
                      <Icon name="image" size={20} /> 图片
                    </button>
                  )}
                  {cfg.video_enabled && (
                    <button
                      type="button"
                      role="menuitem"
                      onClick={() => {
                        setAttachOpen(false)
                        videoInput.current?.click()
                      }}
                    >
                      <Icon name="movie" size={20} /> 视频
                    </button>
                  )}
                  {cfg.file_enabled && (
                    <button
                      type="button"
                      role="menuitem"
                      onClick={() => {
                        setAttachOpen(false)
                        anyInput.current?.click()
                      }}
                    >
                      <Icon name="folder" size={20} /> 文件
                    </button>
                  )}
                </div>
              </>
            )}
            <input
              ref={fileInput}
              type="file"
              accept="image/jpeg,image/png,image/webp,image/gif,image/*"
              multiple
              hidden
              onChange={(e) => {
                const files = [...(e.target.files ?? [])]
                e.target.value = ''
                if (files.length) onFiles(files, 'image')
              }}
            />
            <input
              ref={videoInput}
              type="file"
              accept="video/mp4,video/webm,video/quicktime,video/*"
              hidden
              onChange={(e) => {
                const files = [...(e.target.files ?? [])]
                e.target.value = ''
                if (files.length) onFiles(files, 'video')
              }}
            />
            <input
              ref={anyInput}
              type="file"
              hidden
              onChange={(e) => {
                const files = [...(e.target.files ?? [])]
                e.target.value = ''
                if (files.length) onFiles(files, 'file')
              }}
            />
          </div>
        )}
        {rec ? (
          <div className={'recording' + (rec.cancelHint ? ' cancel' : '')} aria-live="polite">
            <div className="rec-row">
              {!rec.holding && (
                <md-icon-button aria-label="取消录音" onClick={() => Date.now() - tapModeAt.current > 500 && finishRecording(false)}>
                  <Icon name="delete" size={22} />
                </md-icon-button>
              )}
              <span className="rec-dot" />
              <span className="rec-time">
                {fmtDuration(rec.elapsed)} / {fmtDuration(maxSec * 1000)}
              </span>
              <div className="rec-wave" aria-hidden="true">
                {rec.levels.map((l, i) => (
                  <span key={i} style={{ height: `${Math.max(10, l * 100)}%` }} />
                ))}
              </div>
              <span className="rec-hint">{rec.holding ? (rec.cancelHint ? '松开取消' : '松开发送，左滑取消') : '点 ➤ 发送'}</span>
            </div>
            <md-linear-progress class="rec-progress" value={Math.min(1, rec.elapsed / (maxSec * 1000))} aria-label="录音时长" />
          </div>
        ) : (
          <div className="composer-field">
            <textarea
              ref={input}
              rows={1}
              placeholder="发消息"
              value={text}
              maxLength={5000}
              onChange={(e) => {
                setText(e.target.value)
                drafts.set(id, e.target.value)
                if (e.target.value) store.sendTyping(id)
              }}
              onKeyDown={onKey}
              onPaste={onPaste}
              enterKeyHint="send"
            />
          </div>
        )}
        {rec && !rec.holding ? (
          <md-filled-icon-button aria-label="发送语音" onClick={() => Date.now() - tapModeAt.current > 500 && finishRecording(true)}>
            <Icon name="send" />
          </md-filled-icon-button>
        ) : text.trim() || !cfg.voice_enabled ? (
          <md-filled-icon-button aria-label="发送" disabled={!text.trim() || undefined} onClick={send}>
            <Icon name="send" />
          </md-filled-icon-button>
        ) : (
          <button
            className={'mic-btn' + (rec ? ' active' : '')}
            aria-label="录音（点按或按住说话）"
            title="点按录音，或按住说话"
            onPointerDown={onMicDown}
            onPointerMove={onMicMove}
            onPointerUp={onMicUp}
            onPointerCancel={() => finishRecording(false)}
            onContextMenu={(e) => e.preventDefault()}
          >
            <Icon name="mic" />
          </button>
        )}
      </div>
    </footer>
  )
})
