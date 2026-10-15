/**
 * 图片：发送前在浏览器里压缩（canvas）。
 * - 长边最大 2048px，JPEG 质量 0.85；
 * - PNG / WebP 有透明像素时保留透明（优先 WebP，浏览器不支持编码 WebP 时用 PNG）；
 * - GIF 原样上传（保留动画），只读出宽高；
 * - 压缩后反而更大且无需缩放时用原图。
 */

export const MAX_EDGE = 2048
export const JPEG_QUALITY = 0.85
export const ALLOWED_MIME = ['image/jpeg', 'image/png', 'image/webp', 'image/gif']

export interface PreparedImage {
  blob: Blob
  mime: string
  width: number
  height: number
}

type Source = { el: CanvasImageSource; width: number; height: number; close: () => void }

async function decode(file: Blob): Promise<Source> {
  if (typeof createImageBitmap === 'function') {
    try {
      const b = await createImageBitmap(file, { imageOrientation: 'from-image' } as ImageBitmapOptions)
      return { el: b, width: b.width, height: b.height, close: () => b.close() }
    } catch {
      // 部分浏览器不认识选项或格式，回退 <img>
    }
  }
  const url = URL.createObjectURL(file)
  try {
    const img = new Image()
    img.decoding = 'async'
    img.src = url
    await img.decode()
    return { el: img, width: img.naturalWidth, height: img.naturalHeight, close: () => URL.revokeObjectURL(url) }
  } catch (e) {
    URL.revokeObjectURL(url)
    throw e
  }
}

function toBlob(c: HTMLCanvasElement, mime: string, q?: number) {
  return new Promise<Blob | null>((resolve) => c.toBlob(resolve, mime, q))
}

/** 抽样检查是否有透明像素（缩小到最多 256px 再看 alpha） */
function hasAlpha(src: Source) {
  const s = Math.min(1, 256 / Math.max(src.width, src.height))
  const w = Math.max(1, Math.round(src.width * s))
  const h = Math.max(1, Math.round(src.height * s))
  const c = document.createElement('canvas')
  c.width = w
  c.height = h
  const ctx = c.getContext('2d', { willReadFrequently: true })
  if (!ctx) return false
  ctx.drawImage(src.el, 0, 0, w, h)
  const d = ctx.getImageData(0, 0, w, h).data
  for (let i = 3; i < d.length; i += 4) if (d[i] < 250) return true
  return false
}

export async function prepareImage(file: File | Blob): Promise<PreparedImage> {
  const type = (file.type || '').toLowerCase()
  const src = await decode(file)
  try {
    const { width, height } = src
    if (!width || !height) throw new Error('empty image')
    if (type === 'image/gif') return { blob: file, mime: 'image/gif', width, height }

    const scale = Math.min(1, MAX_EDGE / Math.max(width, height))
    const w = Math.max(1, Math.round(width * scale))
    const h = Math.max(1, Math.round(height * scale))
    const transparent = (type === 'image/png' || type === 'image/webp') && hasAlpha(src)

    const c = document.createElement('canvas')
    c.width = w
    c.height = h
    const ctx = c.getContext('2d')!
    if (!transparent) {
      ctx.fillStyle = '#fff' // JPEG 没有透明通道
      ctx.fillRect(0, 0, w, h)
    }
    ctx.imageSmoothingQuality = 'high'
    ctx.drawImage(src.el, 0, 0, w, h)

    let out: Blob | null = null
    if (transparent) {
      out = await toBlob(c, 'image/webp', JPEG_QUALITY)
      if (!out || out.type !== 'image/webp') out = await toBlob(c, 'image/png')
    } else {
      out = await toBlob(c, 'image/jpeg', JPEG_QUALITY)
    }
    if (!out) throw new Error('encode failed')
    // 不需要缩放且原图更小、格式允许：直接用原图（例如已经压过的 JPEG）
    if (scale === 1 && ALLOWED_MIME.includes(type) && file.size <= out.size) {
      return { blob: file, mime: type, width, height }
    }
    return { blob: out, mime: out.type || 'image/jpeg', width: w, height: h }
  } finally {
    src.close()
  }
}

/** 气泡占位尺寸：按宽高比缩放到最大 260×320 */
export function fitBox(width: number, height: number, maxW = 260, maxH = 320) {
  if (!width || !height) return { w: 200, h: 150 }
  const s = Math.min(1, maxW / width, maxH / height)
  // 太细长的图给个最小边，避免成一条线
  return { w: Math.max(60, Math.round(width * s)), h: Math.max(60, Math.round(height * s)) }
}

export const extOf = (mime: string) =>
  ({ 'image/jpeg': 'jpg', 'image/png': 'png', 'image/webp': 'webp', 'image/gif': 'gif' })[mime] ?? 'img'
