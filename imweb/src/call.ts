import type { CallKind, IceServers } from './types'

/**
 * v0.4.0 通话状态机（SPEC §1.3/§1.5）：只管 WebRTC 与信令，
 * UI 状态通过 onChange 同步给 store（state.call），渲染在 CallView。
 *
 * 角色固定：主叫是 offer 方。主叫发 invite 后立即发 offer；
 * 被叫点接听后才建 PeerConnection、先发 accept 再回 answer。
 * 信令的 offer 先于接听到达被叫时先缓存，接听时再 setRemoteDescription。
 * 终结以服务端事件为准：本地响铃计时只做 UI 兜底（超时后发 cancel 并收尾）。
 */

export type CallPhase = 'outgoing' | 'incoming' | 'connected'

export interface CallUiState {
  phase: CallPhase
  callId: string
  conversationId: number
  peerId: number
  kind: CallKind
  startedAt: number
  connectedAt?: number
  muted: boolean
  cameraOff: boolean
  localStream: MediaStream | null
  remoteStream: MediaStream | null
}

interface Signal {
  kind: 'offer' | 'answer' | 'candidate'
  sdp?: string
  candidate?: RTCIceCandidateInit
}

interface Deps {
  send: (type: string, data: unknown) => boolean
  getIce: () => Promise<IceServers>
  onChange: (call: CallUiState | null) => void
  toast: (msg: string) => void
  meId: () => number | undefined
  ringSeconds: () => number
}

const uuid = () =>
  (crypto as any).randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`

export class CallManager {
  private pc: RTCPeerConnection | null = null
  private state: CallUiState | null = null
  private pendingOffer: Signal | null = null
  private pendingCandidates: RTCIceCandidateInit[] = []
  private ringTimer = 0
  private inviteSent = false

  constructor(private deps: Deps) {}

  private setState(s: CallUiState | null) {
    this.state = s
    this.deps.onChange(s)
  }
  private patch(p: Partial<CallUiState>) {
    if (this.state) this.setState({ ...this.state, ...p })
  }

  // ---------- 主叫 ----------

  async start(conversationId: number, peerId: number, kind: CallKind) {
    if (this.state) return this.deps.toast('已在通话中')
    const callId = uuid()
    this.inviteSent = false
    this.setState({
      phase: 'outgoing', callId, conversationId, peerId, kind,
      startedAt: Date.now(), muted: false, cameraOff: false, localStream: null, remoteStream: null,
    })
    let stream: MediaStream
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: true,
        video: kind === 'video' ? { facingMode: 'user' } : false,
      })
    } catch {
      this.setState(null)
      return this.deps.toast('没有麦克风/摄像头权限，无法发起通话')
    }
    if (this.gone(callId)) return stream.getTracks().forEach((t) => t.stop()) // 等权限时已被取消
    this.patch({ localStream: stream })
    let pc: RTCPeerConnection
    try {
      pc = await this.createPc(stream)
    } catch {
      this.teardown()
      return this.deps.toast('获取通话线路失败，请稍后再试')
    }
    if (this.gone(callId)) return
    this.deps.send('call_invite', { call_id: callId, conversation_id: conversationId, kind })
    this.inviteSent = true
    try {
      const offer = await pc.createOffer()
      await pc.setLocalDescription(offer)
      this.deps.send('call_signal', { call_id: callId, signal: { kind: 'offer', sdp: offer.sdp } })
    } catch {
      this.fail(callId)
      return
    }
    this.armRingTimer(callId, () => {
      // 服务端也会超时并下发 hangup；本地先收尾并补一发 cancel（服务端已终结时会忽略）
      this.deps.toast('对方未接听')
      this.deps.send('call_cancel', { call_id: callId })
      this.teardown()
    })
  }

  // ---------- 被叫 ----------

  async accept() {
    const s = this.state
    if (!s || s.phase !== 'incoming') return
    let stream: MediaStream
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: true,
        video: s.kind === 'video' ? { facingMode: 'user' } : false,
      })
    } catch {
      this.deps.toast('没有麦克风/摄像头权限，无法接听')
      this.deps.send('call_hangup', { call_id: s.callId, reason: 'failed' })
      return this.teardown()
    }
    if (this.state?.callId !== s.callId) return stream.getTracks().forEach((t) => t.stop())
    this.patch({ localStream: stream })
    let pc: RTCPeerConnection
    try {
      pc = await this.createPc(stream)
    } catch {
      this.deps.send('call_hangup', { call_id: s.callId, reason: 'failed' })
      this.teardown()
      return this.deps.toast('获取通话线路失败')
    }
    if (this.state?.callId !== s.callId) return
    clearTimeout(this.ringTimer)
    this.deps.send('call_accept', { call_id: s.callId })
    this.patch({ phase: 'connected', connectedAt: Date.now() })
    if (this.pendingOffer?.sdp) {
      const offer = this.pendingOffer
      this.pendingOffer = null
      try {
        await pc.setRemoteDescription({ type: 'offer', sdp: offer.sdp })
        await this.flushCandidates(pc)
        const answer = await pc.createAnswer()
        await pc.setLocalDescription(answer)
        this.deps.send('call_signal', { call_id: s.callId, signal: { kind: 'answer', sdp: answer.sdp } })
      } catch {
        this.fail(s.callId)
      }
    }
  }

  reject() {
    const s = this.state
    if (!s || s.phase !== 'incoming') return
    this.deps.send('call_reject', { call_id: s.callId })
    this.teardown()
  }

  /** 当前用户主动结束：响铃/呼出未接通走 cancel，已接通走 hangup */
  hangup() {
    const s = this.state
    if (!s) return
    if (s.phase === 'outgoing') this.deps.send('call_cancel', { call_id: s.callId })
    else if (s.phase === 'incoming') this.deps.send('call_reject', { call_id: s.callId })
    else this.deps.send('call_hangup', { call_id: s.callId })
    this.teardown()
  }

  toggleMute() {
    const s = this.state
    if (!s?.localStream) return
    const next = !s.muted
    s.localStream.getAudioTracks().forEach((t) => (t.enabled = !next))
    this.patch({ muted: next })
  }

  /** 关摄像头 = 禁用 video track（不重协商，降级为纯语音，SPEC §1.5） */
  toggleCamera() {
    const s = this.state
    if (!s?.localStream) return
    const next = !s.cameraOff
    s.localStream.getVideoTracks().forEach((t) => (t.enabled = !next))
    this.patch({ cameraOff: next })
  }

  /** 登出 / 被踢时静默收尾 */
  dispose() {
    this.teardown()
  }

  // ---------- 服务端事件 ----------

  handle(type: string, d: any) {
    const me = this.deps.meId()
    switch (type) {
      case 'call_invite': {
        if (!d || d.from_user_id === me || this.state) return
        this.pendingOffer = null
        this.pendingCandidates = []
        this.setState({
          phase: 'incoming', callId: d.call_id, conversationId: d.conversation_id, peerId: d.from_user_id,
          kind: d.kind === 'video' ? 'video' : 'audio',
          startedAt: Date.now(), muted: false, cameraOff: false, localStream: null, remoteStream: null,
        })
        // UI 兜底：服务端超时 hangup 未到时本地收起来电层
        this.armRingTimer(d.call_id, () => this.teardown())
        return
      }
      case 'call_signal':
        return this.onSignal(d)
      case 'call_accept': {
        if (!this.matches(d)) return
        if (this.state!.phase === 'incoming') return this.teardown() // 自己在其他设备接听了
        if (this.state!.phase === 'outgoing') {
          clearTimeout(this.ringTimer)
          this.patch({ phase: 'connected', connectedAt: Date.now() })
        }
        return
      }
      case 'call_reject':
        if (this.matches(d)) {
          this.deps.toast('对方已拒绝')
          this.teardown()
        }
        return
      case 'call_cancel':
        if (this.matches(d)) this.teardown()
        return
      case 'call_unavailable': {
        if (!this.matches(d)) return
        this.deps.toast(d.reason === 'offline' ? '对方不在线' : d.reason === 'busy' ? '对方忙线中' : '通话不可用')
        this.teardown()
        return
      }
      case 'call_hangup': {
        if (!this.matches(d)) return
        if (d.reason === 'timeout' && this.state!.phase === 'outgoing') this.deps.toast('对方未接听')
        else if (d.reason === 'failed') this.deps.toast('通话连接失败')
        // answered_elsewhere 等其他情形静默收尾
        this.teardown()
        return
      }
    }
  }

  private matches(d: any) {
    return !!d && !!this.state && d.call_id === this.state.callId
  }

  /** 这通通话是否已被取消/替换（await 期间可能被终结事件收尾） */
  private gone(callId: string) {
    return this.state?.callId !== callId
  }

  private async onSignal(d: any) {
    if (!this.matches(d) || !d.signal || d.from_user_id === this.deps.meId()) return
    const sig = d.signal as Signal
    const pc = this.pc
    if (sig.kind === 'offer') {
      // 被叫接听前先缓存；已接通后的重复 offer 忽略（本版不做重协商）
      if (this.state!.phase === 'incoming') this.pendingOffer = sig
      return
    }
    if (sig.kind === 'answer') {
      if (!pc || !sig.sdp) return
      try {
        await pc.setRemoteDescription({ type: 'answer', sdp: sig.sdp })
        await this.flushCandidates(pc)
      } catch (e) {
        console.warn('setRemoteDescription(answer) failed', e)
      }
      return
    }
    if (sig.kind === 'candidate' && sig.candidate) {
      if (pc && pc.remoteDescription) {
        pc.addIceCandidate(sig.candidate).catch(() => {})
      } else {
        this.pendingCandidates.push(sig.candidate)
      }
    }
  }

  // ---------- 内部 ----------

  private async createPc(stream: MediaStream): Promise<RTCPeerConnection> {
    const { iceServers } = await this.deps.getIce()
    const callId = this.state!.callId
    const pc = new RTCPeerConnection({ iceServers })
    this.pc = pc
    stream.getTracks().forEach((t) => pc.addTrack(t, stream))
    pc.ontrack = (e) => {
      if (this.state?.callId === callId) this.patch({ remoteStream: e.streams[0] ?? new MediaStream([e.track]) })
    }
    pc.onicecandidate = (e) => {
      if (e.candidate && this.state?.callId === callId) {
        this.deps.send('call_signal', { call_id: callId, signal: { kind: 'candidate', candidate: e.candidate.toJSON() } })
      }
    }
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' && this.state?.callId === callId) this.fail(callId)
    }
    return pc
  }

  private async flushCandidates(pc: RTCPeerConnection) {
    const list = this.pendingCandidates
    this.pendingCandidates = []
    for (const c of list) await pc.addIceCandidate(c).catch(() => {})
  }

  private armRingTimer(callId: string, fn: () => void) {
    clearTimeout(this.ringTimer)
    this.ringTimer = window.setTimeout(() => {
      if (this.state?.callId === callId) fn()
    }, Math.max(5, this.deps.ringSeconds()) * 1000)
  }

  private fail(callId: string) {
    if (this.state?.callId !== callId) return
    this.deps.send('call_hangup', { call_id: callId, reason: 'failed' })
    this.deps.toast('通话连接失败')
    this.teardown()
  }

  private teardown() {
    clearTimeout(this.ringTimer)
    this.ringTimer = 0
    if (this.pc) {
      this.pc.onicecandidate = null
      this.pc.ontrack = null
      this.pc.onconnectionstatechange = null
      try {
        this.pc.close()
      } catch {}
      this.pc = null
    }
    this.state?.localStream?.getTracks().forEach((t) => t.stop())
    this.pendingOffer = null
    this.pendingCandidates = []
    this.inviteSent = false
    if (this.state) this.setState(null)
  }
}
