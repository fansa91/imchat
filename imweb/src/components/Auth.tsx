import { useState } from 'react'
import { store } from '../store'
import { ApiError } from '../api'
import { Icon } from '../icons'

export function Auth() {
  const [mode, setMode] = useState<'login' | 'register'>('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [nickname, setNickname] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

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

  return (
    <div className="auth">
      <form className="auth-card" onSubmit={submit}>
        <div className="auth-logo">
          <Icon name="chat" size={32} />
        </div>
        <h1 className="display">{mode === 'login' ? '欢迎回来' : '创建账号'}</h1>
        <p className="muted">{mode === 'login' ? '登录以继续聊天' : '用邮箱注册一个新账号'}</p>

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
      </form>
    </div>
  )
}
