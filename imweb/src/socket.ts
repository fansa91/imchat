type Handler = (data: any, id?: string) => void

/** WebSocket 封装：自动重连（指数退避）、心跳、按 type 分发 */
export class Socket {
  private ws?: WebSocket
  private handlers = new Map<string, Set<Handler>>()
  private retry = 0
  private timer?: number
  private ping?: number
  private stopped = true
  onStatus: (s: 'connecting' | 'online' | 'offline') => void = () => {}
  onOpen: () => void = () => {}

  constructor(private token: () => string) {}

  start() {
    this.stopped = false
    this.connect()
    window.addEventListener('online', this.kick)
    document.addEventListener('visibilitychange', this.kick)
  }

  stop() {
    this.stopped = true
    window.removeEventListener('online', this.kick)
    document.removeEventListener('visibilitychange', this.kick)
    clearTimeout(this.timer)
    clearInterval(this.ping)
    this.ws?.close()
    this.ws = undefined
  }

  get open() {
    return this.ws?.readyState === WebSocket.OPEN
  }

  on(type: string, h: Handler) {
    if (!this.handlers.has(type)) this.handlers.set(type, new Set())
    this.handlers.get(type)!.add(h)
    return () => this.handlers.get(type)!.delete(h)
  }

  send(type: string, data: unknown, id?: string) {
    if (!this.open) return false
    this.ws!.send(JSON.stringify({ type, id, data }))
    return true
  }

  // 回到前台或网络恢复时立即重连
  private kick = () => {
    if (this.stopped || document.visibilityState !== 'visible') return
    if (!this.ws || this.ws.readyState === WebSocket.CLOSED) {
      clearTimeout(this.timer)
      this.retry = 0
      this.connect()
    }
  }

  private connect() {
    if (this.stopped) return
    this.onStatus('connecting')
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const ws = new WebSocket(`${proto}://${location.host}/ws?token=${encodeURIComponent(this.token())}`)
    this.ws = ws
    ws.onopen = () => {
      this.retry = 0
      this.onStatus('online')
      clearInterval(this.ping)
      this.ping = window.setInterval(() => this.send('ping', {}), 20000)
      this.onOpen()
    }
    ws.onmessage = (ev) => {
      let env: { type: string; id?: string; data: any }
      try {
        env = JSON.parse(ev.data)
      } catch {
        return
      }
      this.handlers.get(env.type)?.forEach((h) => h(env.data, env.id))
    }
    ws.onclose = () => {
      clearInterval(this.ping)
      if (this.ws !== ws) return
      this.ws = undefined
      if (this.stopped) return
      this.onStatus('offline')
      const delay = Math.min(30000, 1000 * 2 ** this.retry) * (0.7 + Math.random() * 0.6)
      this.retry++
      this.timer = window.setTimeout(() => this.connect(), delay)
    }
  }
}
