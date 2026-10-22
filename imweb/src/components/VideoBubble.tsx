import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { api, ApiError } from '../api'
import { parseVideo, store } from '../store'
import type { Message } from '../types'
import { Icon } from '../icons'
import { fitBox } from '../image'
import { fmtDuration } from '../voice'

type Load = { state: 'loading' } | { state: 'ready'; url: string } | { state: 'expired' } | { state: 'error' }

/**
 * 视频气泡（v0.4.0）：按宽高预留占位，用 <video preload="metadata"> 显示首帧
 * （poster_key 指向的对象没有单独的取流接口，网页端靠视频首帧展示封面效果，
 * poster 本身仍随消息上传，供其他客户端使用）；点击全屏播放。
 * 待发送时用本地 Blob 预览并显示上传进度，可取消。
 */
export function VideoBubble({ m }: { m: Message }) {
  const vc = parseVideo(m)
  const [load, setLoad] = useState<Load>({ state: 'loading' })
  const [attempt, setAttempt] = useState(0)
  const [viewer, setViewer] = useState(false)
  const box = fitBox(vc?.width ?? 0, vc?.height ?? 0)
  const pending = m.seq === 0

  useEffect(() => {
    let alive = true
    if (pending) {
      const u = store.localMediaUrl(m)
      setLoad(u ? { state: 'ready', url: u } : { state: 'loading' })
      return
    }
    setLoad((l) => (l.state === 'ready' ? l : { state: 'loading' }))
    api
      .media(m.conversation_id, m.seq)
      .then((r) => alive && setLoad({ state: 'ready', url: r.url }))
      .catch((e) => alive && setLoad({ state: e instanceof ApiError && (e.status === 410 || e.status === 404) ? 'expired' : 'error' }))
    return () => {
      alive = false
    }
  }, [m.conversation_id, m.seq, m.client_msg_id, attempt, pending])

  if (!vc) return <span>[视频]</span>

  return (
    <>
      <div className="video-bubble" style={{ width: box.w, height: box.h }}>
        {load.state === 'ready' ? (
          <button
            type="button"
            className="video-frame"
            aria-label={pending ? '视频待发送' : '播放视频'}
            onClick={(e) => {
              e.stopPropagation()
              if (!pending) setViewer(true)
            }}
          >
            <video src={load.url} preload="metadata" muted playsInline />
            {!pending && (
              <span className="video-play">
                <Icon name="play" size={30} />
              </span>
            )}
          </button>
        ) : (
          <span className="image-msg">
            {load.state === 'loading' ? (
              <md-circular-progress indeterminate style={{ '--md-circular-progress-size': '32px' } as any} />
            ) : load.state === 'expired' ? (
              <>
                <Icon name="movie" size={22} />
                视频已失效
              </>
            ) : (
              <button
                type="button"
                className="link-btn"
                onClick={(e) => {
                  e.stopPropagation()
                  setAttempt((a) => a + 1)
                }}
              >
                <Icon name="error" size={22} />
                加载失败，点击重试
              </button>
            )}
          </span>
        )}
        <span className="video-dur">{fmtDuration(vc.duration_ms)}</span>
        {pending && m.status === 'sending' && (
          <div className="upload-bar" onClick={(e) => e.stopPropagation()}>
            <md-linear-progress value={m.progress ?? 0} aria-label="上传进度" />
            <button type="button" className="upload-cancel" aria-label="取消发送" onClick={() => store.discardPending(m)}>
              <Icon name="close" size={16} />
            </button>
          </div>
        )}
      </div>
      {viewer && load.state === 'ready' &&
        createPortal(
          <div className="viewer" role="dialog" aria-label="播放视频">
            <div className="viewer-bar">
              <a className="viewer-btn" href={load.url} download aria-label="保存视频" title="保存视频" onClick={(e) => e.stopPropagation()}>
                <Icon name="download" />
              </a>
              <button className="viewer-btn" aria-label="关闭" title="关闭" onClick={() => setViewer(false)}>
                <Icon name="close" />
              </button>
            </div>
            <div className="viewer-stage video-stage">
              <video src={load.url} controls autoPlay playsInline />
            </div>
          </div>,
          document.body,
        )}
    </>
  )
}
