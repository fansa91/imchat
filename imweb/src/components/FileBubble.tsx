import { api, ApiError } from '../api'
import { parseFile, store } from '../store'
import type { Message } from '../types'
import { Icon } from '../icons'
import { fmtSize } from '../util'

/**
 * 文件气泡（v0.4.0）：扩展名图标 + 文件名 + 大小；点击先调 /media 拿短期 URL
 * 再用 a[download] 触发浏览器下载（与图片保存同一办法）。待发送时显示上传进度，可取消。
 */
export function FileBubble({ m }: { m: Message }) {
  const fc = parseFile(m)
  if (!fc) return <span>[文件]</span>
  const pending = m.seq === 0
  const ext = (fc.name.split('.').pop() || '').toUpperCase()
  const showExt = ext && ext !== fc.name.toUpperCase() && ext.length <= 5 ? ext : 'FILE'

  const download = async () => {
    if (pending) return
    try {
      const { url } = await api.media(m.conversation_id, m.seq)
      const a = document.createElement('a')
      a.href = url
      a.download = fc.name
      document.body.appendChild(a)
      a.click()
      a.remove()
    } catch (e) {
      store.showToast(e instanceof ApiError && (e.status === 410 || e.status === 404) ? '文件已失效' : '文件下载失败')
    }
  }

  return (
    <div
      className="file-bubble"
      role="button"
      tabIndex={0}
      aria-label={pending ? `文件 ${fc.name} 待发送` : `下载文件 ${fc.name}`}
      onClick={(e) => {
        e.stopPropagation()
        void download()
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter') void download()
      }}
    >
      <span className="file-icon">
        <Icon name="file" size={26} />
        <span className="file-ext">{showExt}</span>
      </span>
      <span className="file-main">
        <span className="file-name">{fc.name}</span>
        <span className="file-size">{fmtSize(fc.size)}</span>
      </span>
      {pending && m.status === 'sending' ? (
        <span className="file-progress" onClick={(e) => e.stopPropagation()}>
          <md-linear-progress value={m.progress ?? 0} aria-label="上传进度" />
          <button type="button" className="upload-cancel" aria-label="取消发送" onClick={() => store.discardPending(m)}>
            <Icon name="close" size={16} />
          </button>
        </span>
      ) : (
        !pending && <Icon name="download" size={20} />
      )}
    </div>
  )
}
