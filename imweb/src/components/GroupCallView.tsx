import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { store, type State } from '../store'
import { Avatar } from './Avatar'
import { Icon } from '../icons'
import { convTitle, displayName, fmtCallDuration } from '../util'
import type { GParticipant } from '../groupcall'
import type { PublicUser } from '../types'

/**
 * 群通话层（v0.5.0，SPEC §3.7）：宫格布局，点某人放大（聚焦）。
 * 状态来自 store.state.groupCall（groupcall.ts 状态机维护）；
 * 无离线推送，邀请只达在线成员，本组件不假装支持后台来电。
 */
export function GroupCallView({ state }: { state: State }) {
  const call = state.groupCall
  const [focused, setFocused] = useState<number | 'me' | null>(null)
  const [, tick] = useState(0)

  useEffect(() => {
    if (!call) return
    const t = window.setInterval(() => tick((n) => n + 1), 500)
    return () => clearInterval(t)
  }, [call?.callId])

  useEffect(() => {
    setFocused(null)
  }, [call?.callId])

  if (!call) return null
  const conv = state.convs[call.conversationId]
  const title = conv ? convTitle(conv) : '群通话'
  const isVideo = call.kind === 'video'
  const total = call.participants.length + 1
  const duration = fmtCallDuration(Date.now() - call.joinedAt)

  const remarkOf = (u: PublicUser) => state.friends[u.id]?.remark
  const tiles: { key: string; name: string; user?: PublicUser; stream: MediaStream | null; hasVideo: boolean; local?: boolean; extra?: string }[] = [
    {
      key: 'me',
      name: '我',
      user: state.me ?? undefined,
      stream: call.localStream,
      hasVideo: isVideo && !call.cameraOff && !!call.localStream?.getVideoTracks().length,
      local: true,
      extra: call.muted ? '已静音' : undefined,
    },
    ...call.participants.map((p: GParticipant) => ({
      key: `u${p.user.id}`,
      name: displayName(p.user, remarkOf(p.user)),
      user: p.user,
      stream: p.stream,
      hasVideo: p.hasVideo,
    })),
  ]
  const focusKey = focused === 'me' ? 'me' : typeof focused === 'number' ? `u${focused}` : null
  const focusTile = focusKey ? tiles.find((t) => t.key === focusKey) : undefined

  return createPortal(
    <div className="call-overlay" role="dialog" aria-label={isVideo ? '群视频通话' : '群语音通话'}>
      <div className="gcall-head">
        <div className="title-medium">{title}</div>
        <div className="call-status">
          {total} 人通话中 · {duration}
        </div>
      </div>

      {focusTile ? (
        <div className="gcall-focus">
          <Tile tile={focusTile} big onClick={() => setFocused(null)} />
          <div className="gcall-strip">
            {tiles
              .filter((t) => t.key !== focusTile.key)
              .map((t) => (
                <Tile key={t.key} tile={t} onClick={() => setFocused(t.key === 'me' ? 'me' : Number(t.key.slice(1)))} />
              ))}
          </div>
        </div>
      ) : (
        <div className={`gcall-grid n${Math.min(tiles.length, 9)}`}>
          {tiles.map((t) => (
            <Tile key={t.key} tile={t} onClick={() => setFocused(t.key === 'me' ? 'me' : Number(t.key.slice(1)))} />
          ))}
        </div>
      )}

      <div className="call-actions">
        <div className="call-action">
          <button
            className={'call-btn toggle' + (call.muted ? ' off' : '')}
            aria-label={call.muted ? '取消静音' : '静音'}
            onClick={() => store.toggleGroupMute()}
          >
            <Icon name={call.muted ? 'micOff' : 'mic'} size={24} />
          </button>
          <span>{call.muted ? '已静音' : '静音'}</span>
        </div>
        {isVideo && (
          <div className="call-action">
            <button
              className={'call-btn toggle' + (call.cameraOff ? ' off' : '')}
              aria-label={call.cameraOff ? '打开摄像头' : '关闭摄像头'}
              onClick={() => store.toggleGroupCamera()}
            >
              <Icon name={call.cameraOff ? 'videocamOff' : 'videocam'} size={24} />
            </button>
            <span>{call.cameraOff ? '摄像头已关' : '摄像头'}</span>
          </div>
        )}
        <div className="call-action">
          <button className="call-btn reject" aria-label="离开通话" onClick={() => void store.leaveGroupCall()}>
            <Icon name="callEnd" size={28} />
          </button>
          <span>离开</span>
        </div>
      </div>
    </div>,
    document.body,
  )
}

function Tile({
  tile,
  big,
  onClick,
}: {
  tile: { name: string; user?: PublicUser; stream: MediaStream | null; hasVideo: boolean; local?: boolean; extra?: string }
  big?: boolean
  onClick: () => void
}) {
  const videoRef = useRef<HTMLVideoElement>(null)
  useEffect(() => {
    const el = videoRef.current
    if (el && tile.stream && el.srcObject !== tile.stream) el.srcObject = tile.stream
  }, [tile.stream, tile.hasVideo])
  const showVideo = tile.hasVideo && !!tile.stream
  return (
    <button type="button" className={'gtile' + (big ? ' big' : '')} onClick={onClick} aria-label={tile.name}>
      {showVideo ? (
        <video ref={videoRef} autoPlay playsInline muted={!!tile.local} />
      ) : (
        <span className="gtile-fallback">
          <Avatar user={tile.user} size={big ? 88 : 56} />
          {/* 无视频时也要播放音频轨 */}
          {!!tile.stream && <video ref={videoRef} hidden autoPlay playsInline muted={!!tile.local} />}
        </span>
      )}
      <span className="gtile-name">
        {tile.name}
        {tile.extra ? ` · ${tile.extra}` : ''}
      </span>
    </button>
  )
}
