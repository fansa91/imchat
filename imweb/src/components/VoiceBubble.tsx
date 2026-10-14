import { useCallback, useEffect, useRef, useState } from 'react'
import { parseVoice, store, type VoiceState } from '../store'
import type { Message } from '../types'
import { Icon } from '../icons'
import { fmtDuration } from '../voice'

let current: HTMLAudioElement | null = null // 同一时间只播放一条

/**
 * 语音气泡（Material Web 没有音频播放器组件，自己拼）：
 * md-icon-button 播放/暂停 + 用 M3 颜色变量画的波形进度条（跟随动态主题）+ 已播放/总时长。
 * 点击或拖动波形可以跳转。播放只用本地副本（IndexedDB / 刚录的内存 Blob）。
 */
export function VoiceBubble({ m, state }: { m: Message; state?: VoiceState }) {
  const vc = parseVoice(m)
  const audio = useRef<HTMLAudioElement | null>(null)
  const waveRef = useRef<HTMLDivElement>(null)
  const dragging = useRef(false)
  const [playing, setPlaying] = useState(false)
  const [progress, setProgress] = useState(0)
  const dur = vc?.duration_ms ?? 0
  const BARS = 28
  const src = vc?.waveform?.length ? vc.waveform : Array.from({ length: BARS }, (_, i) => 20 + ((i * 37) % 50))
  const bars = Array.from({ length: BARS }, (_, i) => {
    const a = Math.floor((i * src.length) / BARS)
    const b = Math.max(a + 1, Math.floor(((i + 1) * src.length) / BARS))
    return Math.max(...src.slice(a, b))
  })
  const effective: VoiceState | undefined = m.seq === 0 ? 'ready' : state

  useEffect(
    () => () => {
      audio.current?.pause()
      if (current === audio.current) current = null
    },
    [],
  )

  /** 准备 Audio 元素（本地 blob URL）；没有本地副本则重新下载 */
  const ensureAudio = useCallback(async () => {
    if (audio.current) return audio.current
    const url = await store.voiceUrl(m)
    if (!url) {
      store.retryVoice(m)
      return null
    }
    const a = new Audio(url)
    a.preload = 'auto'
    a.ontimeupdate = () => !dragging.current && setProgress(Math.min(1, (a.currentTime * 1000) / (dur || 1)))
    a.onplay = () => setPlaying(true)
    a.onpause = () => setPlaying(false)
    a.onended = () => {
      setPlaying(false)
      setProgress(0)
    }
    audio.current = a
    return a
  }, [m, dur])

  const toggle = async (e: Event | React.SyntheticEvent) => {
    e.stopPropagation()
    if (effective === 'error') return store.retryVoice(m)
    if (effective !== 'ready') return
    if (playing && audio.current) return audio.current.pause()
    const a = await ensureAudio()
    if (!a) return
    if (current && current !== a) current.pause()
    current = a
    try {
      await a.play()
    } catch (err) {
      console.warn('play failed', err)
      store.showToast('无法播放这条语音')
    }
  }

  // 点击 / 拖动波形跳转
  const ratioAt = (clientX: number) => {
    const r = waveRef.current!.getBoundingClientRect()
    return Math.min(1, Math.max(0, (clientX - r.left) / r.width))
  }
  const seek = async (ratio: number) => {
    setProgress(ratio)
    const a = await ensureAudio()
    if (a) a.currentTime = (ratio * dur) / 1000
  }
  const onWaveDown = (e: React.PointerEvent) => {
    if (effective !== 'ready') return
    e.stopPropagation()
    dragging.current = true
    ;(e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId)
    setProgress(ratioAt(e.clientX))
  }
  const onWaveMove = (e: React.PointerEvent) => {
    if (dragging.current) setProgress(ratioAt(e.clientX))
  }
  const onWaveUp = (e: React.PointerEvent) => {
    if (!dragging.current) return
    dragging.current = false
    seek(ratioAt(e.clientX))
  }
  const onWaveKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowRight') seek(Math.min(1, progress + 0.1))
    if (e.key === 'ArrowLeft') seek(Math.max(0, progress - 0.1))
  }

  if (effective === 'expired') {
    return (
      <div className="voice-bubble expired" data-state="expired">
        <Icon name="mic" size={18} />
        <span>语音已过期</span>
      </div>
    )
  }

  const width = Math.min(240, 150 + (dur / 1000) * 3)
  const shown = playing || progress > 0 ? progress * dur : 0
  return (
    <div className="voice-bubble" data-state={effective ?? 'loading'} style={{ minWidth: width }}>
      {effective === 'loading' ? (
        <span className="voice-play loading" aria-label="下载中">
          <md-circular-progress indeterminate style={{ '--md-circular-progress-size': '24px' } as any} />
        </span>
      ) : (
        <md-icon-button
          class="voice-play"
          aria-label={effective === 'error' ? '重新下载' : playing ? '暂停' : '播放语音'}
          onClick={toggle}
          onPointerDown={(e: React.PointerEvent) => e.stopPropagation()}
        >
          <Icon name={effective === 'error' ? 'error' : playing ? 'pause' : 'play'} size={24} />
        </md-icon-button>
      )}
      <div
        ref={waveRef}
        className="wave"
        role="slider"
        tabIndex={0}
        aria-label="播放进度"
        aria-valuemin={0}
        aria-valuemax={Math.round(dur / 1000)}
        aria-valuenow={Math.round(shown / 1000)}
        onPointerDown={onWaveDown}
        onPointerMove={onWaveMove}
        onPointerUp={onWaveUp}
        onPointerCancel={() => (dragging.current = false)}
        onKeyDown={onWaveKey}
        onClick={(e) => e.stopPropagation()}
      >
        {bars.map((h, i) => (
          <span key={i} className={(i + 0.5) / bars.length <= progress ? 'on' : ''} style={{ height: `${Math.max(22, h)}%` }} />
        ))}
      </div>
      <span className="voice-dur">
        {fmtDuration(shown)} / {fmtDuration(dur)}
      </span>
    </div>
  )
}
