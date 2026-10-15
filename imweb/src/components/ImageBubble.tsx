import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { MediaError, parseImage, store } from '../store'
import type { Message } from '../types'
import { Icon } from '../icons'
import { extOf, fitBox } from '../image'

type Load = { state: 'loading' } | { state: 'ready'; url: string } | { state: 'expired' } | { state: 'error' }

/** 图片气泡：按宽高预留占位，加载本地缓存或下载后显示；点击全屏查看 */
export function ImageBubble({ m }: { m: Message }) {
  const ic = parseImage(m)
  const [load, setLoad] = useState<Load>({ state: 'loading' })
  const [attempt, setAttempt] = useState(0)
  const [viewer, setViewer] = useState(false)
  const box = fitBox(ic?.width ?? 0, ic?.height ?? 0)

  useEffect(() => {
    let alive = true
    setLoad((l) => (l.state === 'ready' ? l : { state: 'loading' }))
    store.imageUrl(m).then(
      (url) => alive && setLoad({ state: 'ready', url }),
      (e) => alive && setLoad({ state: e instanceof MediaError && e.kind === 'expired' ? 'expired' : 'error' }),
    )
    return () => {
      alive = false
    }
    // seq 变化（发送确认）后换成持久键重新取
  }, [m.conversation_id, m.seq, m.client_msg_id, attempt])

  if (!ic) return <span>[图片]</span>

  return (
    <>
      <button
        type="button"
        className={'image-bubble ' + load.state}
        style={{ width: box.w, height: box.h }}
        aria-label={load.state === 'error' ? '图片加载失败，点击重试' : '查看图片'}
        onClick={(e) => {
          e.stopPropagation()
          if (load.state === 'ready') setViewer(true)
          else if (load.state === 'error') setAttempt((a) => a + 1)
        }}
      >
        {load.state === 'ready' && <img src={load.url} alt="图片" draggable={false} />}
        {load.state === 'loading' && <md-circular-progress indeterminate style={{ '--md-circular-progress-size': '32px' } as any} />}
        {load.state === 'error' && (
          <span className="image-msg">
            <Icon name="error" size={22} />
            点击重试
          </span>
        )}
        {load.state === 'expired' && (
          <span className="image-msg">
            <Icon name="image" size={22} />
            图片已失效
          </span>
        )}
      </button>
      {viewer && load.state === 'ready' && (
        <ImageViewer url={load.url} name={`IMChat_${m.conversation_id}_${m.seq || 'local'}.${extOf(ic.mime)}`} onClose={() => setViewer(false)} />
      )}
    </>
  )
}

/** 全屏查看：滚轮 / 双指缩放，拖动平移，双击切换放大，Esc 关闭，可下载 */
export function ImageViewer({ url, name, onClose }: { url: string; name: string; onClose: () => void }) {
  const [t, setT] = useState({ s: 1, x: 0, y: 0 })
  const pointers = useRef(new Map<number, { x: number; y: number }>())
  const closeTimer = useRef(0)
  useEffect(() => () => clearTimeout(closeTimer.current), [])
  const gesture = useRef<{ dist: number; s: number; x: number; y: number; px: number; py: number; moved: boolean } | null>(null)

  useEffect(() => {
    const k = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
      if (e.key === '+' || e.key === '=') setT((v) => ({ ...v, s: Math.min(8, v.s * 1.25) }))
      if (e.key === '-') setT((v) => clamp({ ...v, s: Math.max(1, v.s / 1.25) }))
      if (e.key === '0') setT({ s: 1, x: 0, y: 0 })
    }
    window.addEventListener('keydown', k)
    return () => window.removeEventListener('keydown', k)
  }, [onClose])

  const clamp = (v: { s: number; x: number; y: number }) => (v.s <= 1 ? { s: 1, x: 0, y: 0 } : v)

  const onWheel = (e: React.WheelEvent) => {
    const f = e.deltaY < 0 ? 1.15 : 1 / 1.15
    setT((v) => clamp({ ...v, s: Math.min(8, Math.max(1, v.s * f)) }))
  }

  const onDown = (e: React.PointerEvent) => {
    ;(e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId)
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY })
    const ps = [...pointers.current.values()]
    const dist = ps.length >= 2 ? Math.hypot(ps[0].x - ps[1].x, ps[0].y - ps[1].y) : 0
    gesture.current = { dist, s: t.s, x: t.x, y: t.y, px: e.clientX, py: e.clientY, moved: gesture.current?.moved ?? false }
    if (pointers.current.size === 1) gesture.current.moved = false
  }
  const onMove = (e: React.PointerEvent) => {
    if (!pointers.current.has(e.pointerId) || !gesture.current) return
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY })
    const g = gesture.current
    const ps = [...pointers.current.values()]
    if (ps.length >= 2 && g.dist) {
      const d = Math.hypot(ps[0].x - ps[1].x, ps[0].y - ps[1].y)
      g.moved = true
      setT((v) => clamp({ ...v, s: Math.min(8, Math.max(1, (g.s * d) / g.dist)) }))
    } else if (ps.length === 1) {
      const dx = e.clientX - g.px
      const dy = e.clientY - g.py
      if (Math.hypot(dx, dy) > 4) g.moved = true
      if (t.s > 1) setT((v) => ({ ...v, x: g.x + dx, y: g.y + dy }))
    }
  }
  const onUp = (e: React.PointerEvent) => {
    pointers.current.delete(e.pointerId)
    if (pointers.current.size === 0) {
      const moved = gesture.current?.moved
      gesture.current = null
      // 未缩放时点空白处（非拖动）关闭
      // 稍等一下再关，给双击放大留出时间
      if (!moved && t.s === 1) {
        clearTimeout(closeTimer.current)
        closeTimer.current = window.setTimeout(onClose, 280)
      }
    } else {
      const [p] = [...pointers.current.values()]
      gesture.current = { dist: 0, s: t.s, x: t.x, y: t.y, px: p.x, py: p.y, moved: true }
    }
  }

  return createPortal(
    <div className="viewer" role="dialog" aria-label="查看图片" onWheel={onWheel}>
      <div className="viewer-bar">
        <a className="viewer-btn" href={url} download={name} aria-label="保存图片" title="保存图片" onClick={(e) => e.stopPropagation()}>
          <Icon name="download" />
        </a>
        <button className="viewer-btn" aria-label="关闭" title="关闭 (Esc)" onClick={onClose}>
          <Icon name="close" />
        </button>
      </div>
      <div
        className="viewer-stage"
        onPointerDown={onDown}
        onPointerMove={onMove}
        onPointerUp={onUp}
        onPointerCancel={onUp}
        onDoubleClick={() => {
          clearTimeout(closeTimer.current)
          setT((v) => (v.s > 1 ? { s: 1, x: 0, y: 0 } : { s: 2.5, x: 0, y: 0 }))
        }}
      >
        <img
          src={url}
          alt="图片"
          draggable={false}
          style={{ transform: `translate(${t.x}px, ${t.y}px) scale(${t.s})`, cursor: t.s > 1 ? 'grab' : 'zoom-in' }}
        />
      </div>
    </div>,
    document.body,
  )
}
