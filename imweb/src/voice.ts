/**
 * 语音：本地永久缓存（IndexedDB）+ 录音。
 *
 * 存储策略：服务器只保留 7 天（MinIO 生命周期），客户端收到语音后立即下载，
 * 以消息 ID（`会话ID:seq`）为键把音频 Blob 永久存进 IndexedDB，播放优先用本地副本。
 */

const DB_NAME = 'imweb'
const STORE = 'voice'
let dbp: Promise<IDBDatabase> | null = null

function db(): Promise<IDBDatabase> {
  if (!dbp) {
    dbp = new Promise((resolve, reject) => {
      const r = indexedDB.open(DB_NAME, 1)
      r.onupgradeneeded = () => {
        if (!r.result.objectStoreNames.contains(STORE)) r.result.createObjectStore(STORE)
      }
      r.onsuccess = () => resolve(r.result)
      r.onerror = () => reject(r.error)
    })
  }
  return dbp
}

function tx<T>(mode: IDBTransactionMode, fn: (s: IDBObjectStore) => IDBRequest<T>): Promise<T> {
  return db().then(
    (d) =>
      new Promise<T>((resolve, reject) => {
        const t = d.transaction(STORE, mode)
        const req = fn(t.objectStore(STORE))
        req.onsuccess = () => resolve(req.result)
        req.onerror = () => reject(req.error)
      }),
  )
}

export const voiceKey = (cid: number, seq: number) => `${cid}:${seq}`

export const voiceDB = {
  get: (k: string) => tx<Blob | undefined>('readonly', (s) => s.get(k) as IDBRequest<Blob | undefined>),
  put: (k: string, b: Blob) => tx('readwrite', (s) => s.put(b, k)),
  del: (k: string) => tx('readwrite', (s) => s.delete(k)),
  has: (k: string) => tx<number>('readonly', (s) => s.count(k)).then((n) => n > 0),
}

/** 申请持久化存储，避免浏览器空间紧张时清掉本地语音 */
export async function requestPersist() {
  try {
    if (navigator.storage?.persist && !(await navigator.storage.persisted())) await navigator.storage.persist()
  } catch {}
}

// ---------- 录音 ----------

/** 选择浏览器支持的录音格式：优先 webm/opus（Chrome/Firefox），Safari 用 mp4 */
export function pickMime(): string {
  if (typeof MediaRecorder === 'undefined') return ''
  for (const m of ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4;codecs=mp4a.40.2', 'audio/mp4', 'audio/ogg;codecs=opus']) {
    if (MediaRecorder.isTypeSupported(m)) return m
  }
  return ''
}

export const canRecord = () => !!navigator.mediaDevices?.getUserMedia && typeof MediaRecorder !== 'undefined'

export interface Recording {
  blob: Blob
  mime: string // 基础 MIME，不含 codecs
  durationMs: number
  waveform: number[]
}

/** 录音器：开始后实时回调电平（0~1），stop() 返回结果，cancel() 丢弃 */
export class Recorder {
  private rec?: MediaRecorder
  private stream?: MediaStream
  private ctx?: AudioContext
  private raf = 0
  private chunks: Blob[] = []
  private levels: number[] = []
  private started = 0
  private timer = 0
  private done?: (r: Recording | null) => void
  private cancelled = false
  onLevel: (level: number, elapsedMs: number) => void = () => {}
  onAutoStop: () => void = () => {}

  constructor(private maxMs: number) {}

  async start() {
    this.stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } })
    const mime = pickMime()
    this.rec = new MediaRecorder(this.stream, mime ? { mimeType: mime, audioBitsPerSecond: 32000 } : undefined)
    this.rec.ondataavailable = (e) => e.data.size && this.chunks.push(e.data)
    this.rec.onstop = () => this.finish()
    try {
      this.ctx = new AudioContext()
      const src = this.ctx.createMediaStreamSource(this.stream)
      const an = this.ctx.createAnalyser()
      an.fftSize = 512
      src.connect(an)
      const buf = new Uint8Array(an.fftSize)
      const tick = () => {
        an.getByteTimeDomainData(buf)
        let sum = 0
        for (const v of buf) sum += ((v - 128) / 128) ** 2
        const level = Math.min(1, Math.sqrt(sum / buf.length) * 4)
        this.levels.push(level)
        this.onLevel(level, Date.now() - this.started)
        this.raf = requestAnimationFrame(tick)
      }
      this.raf = requestAnimationFrame(tick)
    } catch {}
    this.started = Date.now()
    this.rec.start(250)
    this.timer = window.setTimeout(() => this.onAutoStop(), this.maxMs)
  }

  get elapsed() {
    return this.started ? Date.now() - this.started : 0
  }

  stop(): Promise<Recording | null> {
    return new Promise((resolve) => {
      this.done = resolve
      if (this.rec && this.rec.state !== 'inactive') this.rec.stop()
      else this.finish()
    })
  }

  cancel() {
    this.cancelled = true
    this.stop()
  }

  private finish() {
    const durationMs = Math.min(this.maxMs, Date.now() - this.started)
    cancelAnimationFrame(this.raf)
    clearTimeout(this.timer)
    this.stream?.getTracks().forEach((t) => t.stop())
    this.ctx?.close().catch(() => {})
    const type = (this.rec?.mimeType || pickMime() || 'audio/webm').split(';')[0]
    const blob = new Blob(this.chunks, { type })
    const r = this.cancelled || !blob.size ? null : { blob, mime: type, durationMs, waveform: downsample(this.levels, 40) }
    this.done?.(r)
    this.done = undefined
  }
}

/** 把电平序列压缩成 n 个 0~100 的整数 */
export function downsample(levels: number[], n: number): number[] {
  if (!levels.length) return Array(n).fill(8)
  const out: number[] = []
  for (let i = 0; i < n; i++) {
    const a = Math.floor((i * levels.length) / n)
    const b = Math.max(a + 1, Math.floor(((i + 1) * levels.length) / n))
    let m = 0
    for (let j = a; j < b && j < levels.length; j++) m = Math.max(m, levels[j])
    out.push(Math.max(6, Math.round(m * 100)))
  }
  return out
}

export function fmtDuration(ms: number) {
  const s = Math.max(0, Math.round(ms / 1000))
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}
