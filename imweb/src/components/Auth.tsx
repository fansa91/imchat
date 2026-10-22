import { useState, useSyncExternalStore } from 'react'
import { store } from '../store'
import { api, ApiError } from '../api'
import { Icon } from '../icons'

export function Auth() {
  const state = useSyncExternalStore(store.subscribe, store.get)
  const [mode, setMode] = useState<'login' | 'register'>('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [nickname, setNickname] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // v0.4.0 忘记密码：填邮箱的表单与“已提交”的提示态
  const [forgot, setForgot] = useState(false)
  const [forgotEmail, setForgotEmail] = useState('')
  const [forgotSent, setForgotSent] = useState(false)
  const [forgotBusy, setForgotBusy] = useState(false)
  const [forgotError, setForgotError] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    setError('')
    if (mode === 'register' && password.length < 8) return setError('密码至少 8 位')
    setBusy(true)
    try {
      await store.login(email, password, mode === 'register', nickname)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '出错了，请稍后再试')
    } finally {
      setBusy(false)
    }
  }

  /*
   * 「忘记密码」入口的显示取舍（SPEC §4.4 要求仅 password_reset_enabled 时显示）：
   * /api/config 需要登录态，登录前拿不到这个开关。本版取舍是登录表单一直显示入口，
   * 提交后服务端未开启（501 reset_disabled）时明确提示“服务器未开启重置密码”；
   * 其余情况不论邮箱是否存在都显示统一文案（防枚举，与服务端行为一致）。
   */
  const submitForgot = async (e: React.FormEvent) => {
    e.preventDefault()
    if (forgotBusy) return
    setForgotError('')
    setForgotBusy(true)
    try {
      await api.forgotPassword(forgotEmail.trim())
      setForgotSent(true)
    } catch (err) {
      if (err instanceof ApiError && (err.status === 501 || err.code === 'reset_disabled')) {
        setForgotError('服务器未开启重置密码')
      } else if (err instanceof ApiError && err.status === 429) {
        setForgotError('请求太频繁，请稍后再试')
      } else if (err instanceof ApiError && err.status === 400) {
        setForgotError('邮箱格式不正确')
      } else {
        setForgotError(err instanceof ApiError ? err.message : '出错了，请稍后再试')
      }
    } finally {
      setForgotBusy(false)
    }
  }

  if (forgot) {
    return (
      <div className="auth">
        <form className="auth-card" onSubmit={submitForgot}>
          <div className="auth-logo">
            <Icon name="chat" size={32} />
          </div>
          <h1 className="display">忘记密码</h1>
          {forgotSent ? (
            <>
              <p className="muted">如果该邮箱已注册，重置邮件已发送。请打开邮件里的链接设置新密码（30 分钟内有效）。</p>
              <md-filled-button
                class="auth-submit"
                onClick={() => {
                  setForgot(false)
                  setForgotSent(false)
                }}
              >
                返回登录
              </md-filled-button>
            </>
          ) : (
            <>
              <p className="muted">填写注册邮箱，我们会发送一封重置密码的邮件</p>
              <md-outlined-text-field
                label="邮箱"
                type="email"
                autocomplete="email"
                required
                value={forgotEmail}
                onInput={(e: any) => setForgotEmail(e.target.value)}
              />
              {forgotError && <div className="form-error">{forgotError}</div>}
              <md-filled-button type="submit" disabled={forgotBusy || undefined} class="auth-submit">
                {forgotBusy ? '请稍候…' : '发送重置邮件'}
              </md-filled-button>
              <md-text-button
                onClick={() => {
                  setForgot(false)
                  setForgotError('')
                }}
              >
                返回登录
              </md-text-button>
            </>
          )}
        </form>
      </div>
    )
  }

  return (
    <div className="auth">
      <form className="auth-card" onSubmit={submit}>
        <div className="auth-logo">
          <Icon name="chat" size={32} />
        </div>
        <h1 className="display">{mode === 'login' ? '欢迎回来' : '创建账号'}</h1>
        <p className="muted">{mode === 'login' ? '登录以继续聊天' : '用邮箱注册一个新账号'}</p>

        {state.notice && <div className="form-error">{state.notice}</div>}

        <md-tabs
          activeTabIndex={mode === 'login' ? 0 : 1}
          onchange={(e: any) => {
            setMode(e.target.activeTabIndex === 0 ? 'login' : 'register')
            setError('')
          }}
        >
          <md-primary-tab>登录</md-primary-tab>
          <md-primary-tab>注册</md-primary-tab>
        </md-tabs>

        <md-outlined-text-field
          label="邮箱"
          type="email"
          autocomplete="email"
          required
          value={email}
          onInput={(e: any) => setEmail(e.target.value)}
        />
        {mode === 'register' && (
          <md-outlined-text-field
            label="昵称（可选）"
            maxLength={32}
            value={nickname}
            onInput={(e: any) => setNickname(e.target.value)}
          />
        )}
        <md-outlined-text-field
          label="密码"
          type="password"
          autocomplete={mode === 'login' ? 'current-password' : 'new-password'}
          required
          supporting-text={mode === 'register' ? '至少 8 位' : undefined}
          value={password}
          onInput={(e: any) => setPassword(e.target.value)}
        />

        {error && <div className="form-error">{error}</div>}

        <md-filled-button type="submit" disabled={busy || undefined} class="auth-submit">
          {busy ? '请稍候…' : mode === 'login' ? '登录' : '注册'}
        </md-filled-button>
        {mode === 'login' && (
          <md-text-button
            onClick={() => {
              setForgotEmail(email)
              setForgot(true)
            }}
          >
            忘记密码？
          </md-text-button>
        )}
      </form>
    </div>
  )
}
