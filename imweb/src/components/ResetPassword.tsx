import { useState } from 'react'
import { api, ApiError } from '../api'
import { Icon } from '../icons'

/**
 * 公开重置密码页（v0.4.0，SPEC §4.4）：邮件链接落在 /reset-password?token=...。
 * 本项目没有 react-router，App 入口按 location.pathname 判断渲染本组件。
 * 不需要登录态；成功后回登录页用新密码登录。
 */
export function ResetPasswordPage() {
  const token = new URLSearchParams(location.search).get('token') ?? ''
  const [pw, setPw] = useState('')
  const [pw2, setPw2] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [done, setDone] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setError('')
    if (!token) return setError('链接无效或已过期')
    if (pw.length < 8 || pw.length > 72) return setError('密码长度需为 8 到 72 位')
    if (pw !== pw2) return setError('两次输入的密码不一致')
    setBusy(true)
    try {
      await api.resetPassword(token, pw)
      setDone(true)
    } catch (err) {
      // token 无效/过期/已使用服务端统一 bad_token，这里也只给统一文案
      setError(err instanceof ApiError && err.code === 'bad_password' ? '密码长度需为 8 到 72 位' : '链接无效或已过期')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="auth">
      <form className="auth-card" onSubmit={submit}>
        <div className="auth-logo">
          <Icon name="chat" size={32} />
        </div>
        <h1 className="display">重置密码</h1>
        {done ? (
          <>
            <p className="muted">密码已重置，所有设备上的旧登录都已退出。请用新密码重新登录。</p>
            <md-filled-button
              class="auth-submit"
              onClick={() => {
                location.href = '/'
              }}
            >
              去登录
            </md-filled-button>
          </>
        ) : (
          <>
            <p className="muted">为你的账号设置一个新密码</p>
            <md-outlined-text-field
              label="新密码"
              type="password"
              autocomplete="new-password"
              required
              supporting-text="8 到 72 位"
              value={pw}
              onInput={(e: any) => setPw(e.target.value)}
            />
            <md-outlined-text-field
              label="再输入一次"
              type="password"
              autocomplete="new-password"
              required
              value={pw2}
              onInput={(e: any) => setPw2(e.target.value)}
            />
            {error && <div className="form-error">{error}</div>}
            <md-filled-button type="submit" disabled={busy || undefined} class="auth-submit">
              {busy ? '请稍候…' : '重置密码'}
            </md-filled-button>
          </>
        )}
      </form>
    </div>
  )
}
