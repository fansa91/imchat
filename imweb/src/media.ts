/**
 * v0.4.0 媒体辅助：视频元数据/截帧、头像裁剪、文件名清洗。
 * 都在浏览器本地完成，失败时尽量降级（无封面 / 时长未知），不阻断发送。
 */

export interface VideoMeta {
  durationMs: number
  width: number
  height: number
  /** 首帧截图（JPEG），截不到为 null */
  poster: { blob: Blob; width: number; height: number } | null
}

const POSTER_MAX_EDGE = 640
export const AVATAR_SIZE = 512

function toBlob(c: HTMLCanvasElement, mime: string, q?: number) {
  return new Promise<Blob | null>((resolve) => c.toBlob(resolve, mime, q))
}

/** 读视频时长/宽高并截首帧作封面；整体超时 8 秒，截帧失败不影响元数据 */
export function probeVideo(file: Blob): Promise<VideoMeta> {
  return new Promise((resolve) => {
    const url = URL.createObjectURL(file)
    const video = document.createElement('video')
    video.muted = true
    video.playsInline = true
    video.preload = 'metadata'
    let done = false
    const finish = (meta: VideoMeta) => {
      if (done) return
      done = true
      clearTimeout(timer)
      URL.revokeObjectURL(url)
      resolve(meta)
    }
    const timer = window.setTimeout(
      () => finish({ durationMs: 0, width: video.videoWidth || 0, height: video.videoHeight || 0, poster: null }),
      8000,
    )
    video.onerror = () => finish({ durationMs: 0, width: 0, height: 0, poster: null })
    video.onloadedmetadata = () => {
      // 某些 WebM 的 duration 是 Infinity：跳到大时间点逼它算出真实时长
      if (!isFinite(video.duration)) {
        const onDur = () => {
          video.removeEventListener('durationchange', onDur)
          capture()
        }
        video.addEventListener('durationchange', onDur)
        try {
          video.currentTime = 1e7
        } catch {
          capture()
        }
        window.setTimeout(capture, 1200)
        return
      }
      capture()
    }
    let captured = false
    const capture = () => {
      if (captured || done) return
      captured = true
      const width = video.videoWidth || 0
      const height = video.videoHeight || 0
      const durationMs = isFinite(video.duration) ? Math.round(video.duration * 1000) : 0
      if (!width || !height) return finish({ durationMs, width, height, poster: null })
      const grab = async () => {
        try {
          const s = Math.min(1, POSTER_MAX_EDGE / Math.max(width, height))
          const w = Math.max(1, Math.round(width * s))
          const h = Math.max(1, Math.round(height * s))
          const c = document.createElement('canvas')
          c.width = w
          c.height = h
          const ctx = c.getContext('2d')
          if (!ctx) return finish({ durationMs, width, height, poster: null })
          ctx.drawImage(video, 0, 0, w, h)
          const blob = await toBlob(c, 'image/jpeg', 0.8)
          finish({ durationMs, width, height, poster: blob ? { blob, width: w, height: h } : null })
        } catch {
          finish({ durationMs, width, height, poster: null })
        }
      }
      const target = Math.min(0.5, (video.duration || 0) / 2)
      if (target > 0 && Math.abs(video.currentTime - target) > 0.05) {
        video.onseeked = () => void grab()
        try {
          video.currentTime = target
        } catch {
          void grab()
        }
        window.setTimeout(() => void grab(), 1500)
      } else {
        void grab()
      }
    }
    video.src = url
  })
}

/** 头像：居中裁正方形，缩到 512px，JPEG 0.85（SPEC §3.2） */
export async function prepareAvatar(file: File | Blob): Promise<{ blob: Blob; mime: string; width: number; height: number }> {
  const url = URL.createObjectURL(file)
  try {
    const img = new Image()
    img.decoding = 'async'
    img.src = url
    await img.decode()
    const side = Math.min(img.naturalWidth, img.naturalHeight)
    if (!side) throw new Error('empty image')
    const out = Math.min(AVATAR_SIZE, side)
    const c = document.createElement('canvas')
    c.width = out
    c.height = out
    const ctx = c.getContext('2d')!
    ctx.fillStyle = '#fff'
    ctx.fillRect(0, 0, out, out)
    ctx.imageSmoothingQuality = 'high'
    ctx.drawImage(img, (img.naturalWidth - side) / 2, (img.naturalHeight - side) / 2, side, side, 0, 0, out, out)
    const blob = await toBlob(c, 'image/jpeg', 0.85)
    if (!blob) throw new Error('encode failed')
    return { blob, mime: 'image/jpeg', width: out, height: out }
  } finally {
    URL.revokeObjectURL(url)
  }
}

/** 文件名清洗：与服务端规则对齐（去路径分隔符/控制字符，1–128 字符） */
export function sanitizeFileName(name: string): string {
  // eslint-disable-next-line no-control-regex
  const cleaned = name.replace(/[\\/:*?"<>|\u0000-\u001f\u007f]/g, '_').trim()
  return [...cleaned].slice(0, 128).join('') || 'file'
}

/** 按文件名/MIME 猜视频 mime（部分系统给 .mov 文件的 type 为空） */
export function videoMimeOf(file: File): string {
  const t = (file.type || '').toLowerCase()
  if (['video/mp4', 'video/webm', 'video/quicktime'].includes(t)) return t
  const ext = file.name.toLowerCase().split('.').pop()
  if (ext === 'mp4' || ext === 'm4v') return 'video/mp4'
  if (ext === 'webm') return 'video/webm'
  if (ext === 'mov') return 'video/quicktime'
  return t
}
