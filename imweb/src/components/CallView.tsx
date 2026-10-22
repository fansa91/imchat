import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { store, type State } from '../store'
import { Avatar } from './Avatar'
import { Icon } from '../icons'
import { convTitle, displayName, fmtCallDuration } from '../util'

/**
 * 通话层（v0.4.0，SPEC §1.5）：来电全屏层 / 通话中层。
 * 状态全部来自 store.state.call（由 call.ts 状态机维护）；
 * 无离线推送：来电只在有在线 WS 连接时出现，本组件不假装支持后台来电。
 */
export function CallView({ state }: { state: State }) {
  const call = state.call
  const localRef = useRef<HTMLVideoElement>(null)
  const remoteRef = useRef<HTMLVideoElement>(null)
  const [, tick] = useState(0)

  // 计时刷新（已接通显示通话时长，响铃/呼出显示等待秒数靠文案即可，这里只驱动时长）
  useEffect(() => {
    if (!call) return
    const t = window.setInterval(() => tick((n) => n + 1), 500)
    return () => clearInterval(t)
  }, [call?.callId])

  useEffect(() => {
    if (localRef.current && call?.localStream && localRef.current.srcObject !== call.localStream) {
      localRef.current.srcObject = call.localStream
    }
  }, [call?.localStream, call?.kind, call?.phase])
  useEffect(() => {
    if (remoteRef.current && call?.remoteStream && remoteRef.current.srcObject !== call.remoteStream) {
      remoteRef.current.srcObject = call.remoteStream
    }
  }, [call?.remoteStream, call?.kind, call?.phase])

  if (!call) return null
  const conv = state.convs[call.conversationId]
  const peer = conv?.peer ?? state.users[call.peerId]
  const peerName = conv ? convTitle(conv, conv.peer ? state.friends[conv.peer.id]?.remark : undefined) : displayName(peer, state.friends[call.peerId]?.remark)
  const isVideo = call.kind === 'video'
  const hasRemoteVideo = isVideo && !!call.remoteStream && call.remoteStream.getVideoTracks().length > 0
  const showStage = isVideo && (call.phase === 'connected' || call.phase === 'outgoing')
  const status =
    call.phase === 'incoming'
      ? `邀请你进行${isVideo ? '视频' : '语音'}通话`
      : call.phase === 'outgoing'
        ? '正在等待对方接听…'
        : fmtCallDuration((call.connectedAt ?? call.startedAt) ? Date.now() - (call.connectedAt ?? call.startedAt) : 0)

  return createPortal(
    <div className="call-overlay" role="dialog" aria-label={isVideo ? '视频通话' : '语音通话'}>
      {showStage && (
        <>
          <video ref={remoteRef} className="call-remote" autoPlay playsInline />
          <video ref={localRef} className="call-local" autoPlay playsInline muted />
          {!hasRemoteVideo && (
            <div className="call-waiting">
              <Avatar user={peer} size={88} />
              <div className="title-large">{peerName}</div>
              <div className="call-status">{status}</div>
            </div>
          )}
        </>
      )}
      {!showStage && (
        <div className="call-center">
          <Avatar user={peer} size={96} />
          <div className="title-large">{peerName}</div>
          <div className="call-status">{status}</div>
          {/* 来电阶段本地还没有媒体；被叫接听后才申请权限（call.ts 处理）。
              语音通话的远端流也挂在 video 元素上（隐藏，仅出声） */}
          {call.phase !== 'incoming' && <video ref={remoteRef} hidden autoPlay playsInline />}
          {call.phase !== 'incoming' && isVideo && <video ref={localRef} className="call-local center" autoPlay playsInline muted />}
        </div>
      )}
      {showStage && <div className="call-status floating">{call.phase === 'connected' ? status : '正在等待对方接听…'}</div>}

      <div className="call-actions">
        {call.phase === 'incoming' ? (
          <>
            <div className="call-action">
              <button className="call-btn reject" aria-label="拒绝" onClick={() => store.rejectCall()}>
                <Icon name="callEnd" size={28} />
              </button>
              <span>拒绝</span>
            </div>
            <div className="call-action">
              <button className="call-btn accept" aria-label="接听" onClick={() => store.acceptCall()}>
                <Icon name={isVideo ? 'videocam' : 'call'} size={28} />
              </button>
              <span>接听</span>
            </div>
          </>
        ) : (
          <>
            <div className="call-action">
              <button
                className={'call-btn toggle' + (call.muted ? ' off' : '')}
                aria-label={call.muted ? '取消静音' : '静音'}
                onClick={() => store.toggleCallMute()}
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
                  onClick={() => store.toggleCallCamera()}
                >
                  <Icon name={call.cameraOff ? 'videocamOff' : 'videocam'} size={24} />
                </button>
                <span>{call.cameraOff ? '摄像头已关' : '摄像头'}</span>
              </div>
            )}
            <div className="call-action">
              <button className="call-btn reject" aria-label={call.phase === 'outgoing' ? '取消' : '挂断'} onClick={() => store.hangupCall()}>
                <Icon name="callEnd" size={28} />
              </button>
              <span>{call.phase === 'outgoing' ? '取消' : '挂断'}</span>
            </div>
          </>
        )}
      </div>
    </div>,
    document.body,
  )
}
