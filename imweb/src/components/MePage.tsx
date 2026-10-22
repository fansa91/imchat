import { useEffect, useRef, useState } from 'react'
import { store, type State } from '../store'
import { api, ApiError, putWithProgress } from '../api'
import { SEEDS, type Mode } from '../theme'
import { Icon } from '../icons'
import { Avatar } from './Avatar'
import { displayName, fmtSize } from '../util'
import { prepareAvatar } from '../media'

/** 与 imweb/package.json 的 version 同步（发版时一起改） */
const APP_VERSION = '0.5.0'
const RELEASES_URL = 'https://github.com/fansa91/imchat/releases'

/**
 * v0.5.0「我的」页（SPEC §2.2）：原设置弹层内容整体迁入——
 * 资料卡/编辑资料、账号安全（改密码 + 退出其他设备）、隐私（allow_find）、
 * 外观、服务器、存储（本地缓存）、关于、退出登录。
 */
export function MePage({
  state,
  seed,
  mode,
  onTheme,
}: {
  state: State
  seed: string
  mode: Mode
  onTheme: (seed: string, mode: Mode) => void
}) {
  const me = state.me!
  const avatarInput = useRef<HTMLInputElement>(null)
  const [profileBusy, setProfileBusy] = useState(false)
  const [editNick, setEditNick] = useState<string | null>(null)
  const [profileError, setProfileError] = useState('')
  // 改密码
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')
  const [newPw2, setNewPw2] = useState('')
  const [pwBusy, setPwBusy] = useState(false)
  const [pwError, setPwError] = useState('')
  const [pwDone, setPwDone] = useState(false)
  // 退出其他设备 / 隐私 / 存储 / 退出登录
  const [revokeBusy, setRevokeBusy] = useState(false)
  const [findBusy, setFindBusy] = useState(false)
  const [usage, setUsage] = useState<number | null>(null)
  const [clearBusy, setClearBusy] = useState(false)
  const [confirmLogout, setConfirmLogout] = useState(false)
  // 头像上传依赖对象存储：image_enabled 即“存储已配置”
  const storageOn = !!state.config.image_enabled

  useEffect(() => {
    store.storageUsage().then(setUsage)
  }, [])

  const profileErrText = (e: unknown) =>
    e instanceof ApiError && e.status === 404 ? '服务端不支持编辑资料' : e instanceof ApiError ? e.message : '操作失败，请稍后再试'

  const onAvatarFile = async (f: File) => {
    if (profileBusy) return
    setProfileBusy(true)
    setProfileError('')
    try {
      const img = await prepareAvatar(f) // 裁正方形 512px（SPEC §3.2 of v0.4）
      const pre = await api.presign({ kind: 'avatar', mime: img.mime, size: img.blob.size, width: img.width, height: img.height })
      await putWithProgress(pre.url, pre.headers, img.blob, () => {}).promise
      await store.updateProfile({ avatar_key: pre.key })
      store.showToast('头像已更新')
    } catch (e) {
      setProfileError(profileErrText(e))
    } finally {
      setProfileBusy(false)
    }
  }

  const saveNick = async () => {
    const nickname = (editNick ?? '').trim()
    if (!nickname || profileBusy) return
    setProfileBusy(true)
    setProfileError('')
    try {
      await store.updateProfile({ nickname })
      setEditNick(null)
      store.showToast('昵称已更新')
    } catch (e) {
      setProfileError(profileErrText(e))
    } finally {
      setProfileBusy(false)
    }
  }

  const submitPw = async (e: React.FormEvent) => {
    e.preventDefault()
    if (pwBusy) return
    setPwError('')
    setPwDone(false)
    if (newPw.length < 8 || newPw.length > 72) return setPwError('新密码长度需为 8 到 72 位')
    if (newPw !== newPw2) return setPwError('两次输入的新密码不一致')
    setPwBusy(true)
    try {
      await store.changePassword(oldPw, newPw)
      setOldPw('')
      setNewPw('')
      setNewPw2('')
      setPwDone(true)
      store.showToast('密码已修改，其他设备已退出登录')
    } catch (err) {
      setPwError(err instanceof ApiError ? err.message : '修改失败，请稍后再试')
    } finally {
      setPwBusy(false)
    }
  }

  const revoke = async () => {
    if (revokeBusy) return
    setRevokeBusy(true)
    try {
      await store.revokeOtherSessions()
    } catch (e) {
      store.showToast(e instanceof ApiError ? e.message : '操作失败，请稍后再试')
    } finally {
      setRevokeBusy(false)
    }
  }

  const toggleFind = async () => {
    if (findBusy) return
    setFindBusy(true)
    try {
      await store.updateProfile({ allow_find: !me.allow_find })
    } catch (e) {
      store.showToast(e instanceof ApiError && e.status === 404 ? '服务端尚不支持隐私设置' : e instanceof ApiError ? e.message : '操作失败')
    } finally {
      setFindBusy(false)
    }
  }

  const copyId = async () => {
    try {
      await navigator.clipboard.writeText(String(me.id))
      store.showToast('用户 ID 已复制')
    } catch {
      store.showToast(`用户 ID：${me.id}`)
    }
  }

  const clear = async () => {
    if (clearBusy) return
    setClearBusy(true)
    try {
      await store.clearCaches()
      setUsage(await store.storageUsage())
    } finally {
      setClearBusy(false)
    }
  }

  return (
    <section className="pane list-pane side-page">
      <header className="top-bar">
        <h1 className="title-large">我的</h1>
      </header>
      <div className="conv-scroll page-scroll me-scroll">
        {/* 资料卡 */}
        <div className="me-card big">
          <button
            type="button"
            className="avatar-edit"
            disabled={!storageOn || profileBusy || undefined}
            title={storageOn ? '更换头像' : '服务器未配置存储，无法更换头像'}
            aria-label="更换头像"
            onClick={() => avatarInput.current?.click()}
          >
            <Avatar user={me} size={64} />
            <span className="avatar-edit-badge">
              <Icon name="edit" size={12} />
            </span>
          </button>
          <input
            ref={avatarInput}
            type="file"
            accept="image/jpeg,image/png,image/webp,image/gif,image/*"
            hidden
            onChange={(e) => {
              const f = e.target.files?.[0]
              e.target.value = ''
              if (f) void onAvatarFile(f)
            }}
          />
          <div className="me-main">
            <div className="title-large">{displayName(me)}</div>
            <div className="body-small muted">{me.email}</div>
            <button type="button" className="id-chip" onClick={copyId} title="复制用户 ID">
              ID：{me.id}
              <Icon name="copy" size={14} />
            </button>
          </div>
        </div>

        {/* 编辑资料 */}
        <div className="section-label">编辑资料</div>
        {editNick === null ? (
          <div className="field-row setting-row">
            <span className="setting-main">昵称：{me.nickname || '（未设置）'}</span>
            <md-text-button onClick={() => setEditNick(me.nickname ?? '')}>修改</md-text-button>
          </div>
        ) : (
          <form
            className="field-row"
            onSubmit={(e) => {
              e.preventDefault()
              void saveNick()
            }}
          >
            <md-outlined-text-field label="昵称" maxLength={64} value={editNick} onInput={(e: any) => setEditNick(e.target.value)} style={{ flex: 1 }} />
            <md-filled-tonal-button type="submit" disabled={profileBusy || !editNick.trim() || undefined}>
              保存
            </md-filled-tonal-button>
            <md-text-button type="button" onClick={() => setEditNick(null)}>
              取消
            </md-text-button>
          </form>
        )}
        {profileError && <div className="form-error">{profileError}</div>}
        <div className="body-small muted">点头像可更换头像（经预签名直传对象存储）。</div>

        {/* 账号安全 */}
        <div className="section-label">账号安全</div>
        <form className="pw-form" onSubmit={submitPw}>
          <md-outlined-text-field label="当前密码" type="password" autocomplete="current-password" required value={oldPw} onInput={(e: any) => setOldPw(e.target.value)} />
          <md-outlined-text-field label="新密码" type="password" autocomplete="new-password" required supporting-text="8 到 72 位" value={newPw} onInput={(e: any) => setNewPw(e.target.value)} />
          <md-outlined-text-field label="再输入新密码" type="password" autocomplete="new-password" required value={newPw2} onInput={(e: any) => setNewPw2(e.target.value)} />
          {pwError && <div className="form-error">{pwError}</div>}
          {pwDone && <div className="body-small accent">已修改，其他设备已退出登录</div>}
          <div className="field-row end">
            <md-filled-tonal-button type="submit" disabled={pwBusy || !oldPw || !newPw || !newPw2 || undefined}>
              {pwBusy ? '请稍候…' : '修改密码'}
            </md-filled-tonal-button>
          </div>
        </form>
        <div className="field-row setting-row">
          <div className="setting-main">
            <div>退出其他设备</div>
            <div className="body-small muted">让其他设备上的登录立即失效，本设备继续使用</div>
          </div>
          <md-filled-tonal-button disabled={revokeBusy || undefined} onClick={revoke}>
            {revokeBusy ? '请稍候…' : '退出其他设备'}
          </md-filled-tonal-button>
        </div>

        {/* 隐私 */}
        <div className="section-label">隐私</div>
        <div className="field-row setting-row">
          <div className="setting-main">
            <div>允许通过邮箱 / 用户 ID 找到我</div>
            <div className="body-small muted">关闭后，别人搜索你的邮箱或 ID 时会显示“用户不存在”；已是好友、已加入的群不受影响</div>
          </div>
          <md-switch
            selected={me.allow_find !== false || undefined}
            disabled={findBusy || undefined}
            aria-label="允许通过邮箱或用户 ID 找到我"
            onClick={toggleFind}
          />
        </div>

        {/* 外观 */}
        <div className="section-label">主题色</div>
        <div className="swatches">
          {SEEDS.map((s) => (
            <button
              key={s}
              className={'swatch' + (s.toLowerCase() === seed.toLowerCase() ? ' on' : '')}
              style={{ background: s }}
              aria-label={`主题色 ${s}`}
              onClick={() => onTheme(s, mode)}
            >
              {s.toLowerCase() === seed.toLowerCase() && <Icon name="check" size={20} />}
            </button>
          ))}
          <label className="swatch custom" title="自定义颜色">
            <input type="color" value={seed} onChange={(e) => onTheme(e.target.value, mode)} />
          </label>
        </div>
        <div className="section-label">外观</div>
        <div className="segmented" role="radiogroup">
          {(['light', 'dark', 'system'] as Mode[]).map((m) => (
            <button key={m} role="radio" aria-checked={mode === m} className={mode === m ? 'on' : ''} onClick={() => onTheme(seed, m)}>
              {mode === m && <Icon name="check" size={18} />}
              {{ light: '浅色', dark: '深色', system: '跟随系统' }[m]}
            </button>
          ))}
        </div>

        {/* 服务器 */}
        <div className="section-label">服务器</div>
        <div className="field-row setting-row">
          <div className="setting-main">
            <div>接口地址</div>
            <div className="body-small muted">{location.origin}（网页版与服务器同源，无需设置）</div>
          </div>
        </div>

        {/* 存储 */}
        <div className="section-label">存储</div>
        <div className="field-row setting-row">
          <div className="setting-main">
            <div>本地缓存{usage !== null ? ` · 约 ${fmtSize(usage)}` : ''}</div>
            <div className="body-small muted">语音 / 图片 / 转文字结果缓存在本机浏览器，清理后需要时会重新下载</div>
          </div>
          <md-filled-tonal-button disabled={clearBusy || undefined} onClick={clear}>
            {clearBusy ? '清理中…' : '清理缓存'}
          </md-filled-tonal-button>
        </div>

        {/* 关于 */}
        <div className="section-label">关于</div>
        <div className="field-row setting-row">
          <div className="setting-main">
            <div>IMChat 网页版 v{APP_VERSION}</div>
            <div className="body-small muted">更新内容见 GitHub Release 说明</div>
          </div>
          <md-text-button href={RELEASES_URL} target="_blank" rel="noreferrer">
            更新日志
          </md-text-button>
        </div>

        {/* 退出登录 */}
        {confirmLogout ? (
          <div className="form-error logout-box">
            确定退出登录？
            <div className="field-row end" style={{ marginTop: 8 }}>
              <md-text-button onClick={() => setConfirmLogout(false)}>取消</md-text-button>
              <md-text-button class="danger" onClick={() => store.logout()}>
                确定退出
              </md-text-button>
            </div>
          </div>
        ) : (
          <button type="button" className="logout-btn" onClick={() => setConfirmLogout(true)}>
            <Icon name="logout" size={20} />
            退出登录
          </button>
        )}
      </div>
    </section>
  )
}
