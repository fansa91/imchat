import { useCallback, useEffect, useRef, useState } from 'react'
import { store } from '../store'
import { ApiError } from '../api'
import { SEEDS, type Mode } from '../theme'
import { Icon } from '../icons'
import { Avatar } from './Avatar'
import { displayName } from '../util'
import type { User } from '../types'

export function useDialog(open: boolean, onClose: () => void) {
  const ref = useRef<any>(null)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const h = () => onClose()
    el.addEventListener('closed', h)
    return () => el.removeEventListener('closed', h)
  }, [onClose])
  useEffect(() => {
    if (ref.current) ref.current.open = open
  }, [open])
  return ref
}

export function NewChatDialog({ open, onClose, onOpened }: { open: boolean; onClose: () => void; onOpened: (id: number) => void }) {
  const opened = useRef<number | null>(null)
  // 等对话框关闭动画结束（焦点已归还）再跳转，聊天输入框才能拿到焦点
  const handleClosed = useCallback(() => {
    onClose()
    if (opened.current !== null) {
      onOpened(opened.current)
      opened.current = null
    }
  }, [onClose, onOpened])
  const ref = useDialog(open, handleClosed)
  const [mode, setMode] = useState<'direct' | 'group'>('direct')
  const [email, setEmail] = useState('')
  const [groupName, setGroupName] = useState('')
  const [picked, setPicked] = useState<User[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setEmail('')
      setError('')
      setGroupName('')
      setPicked([])
    }
  }, [open])

  const errText = (err: unknown) =>
    err instanceof ApiError && err.status === 404 ? '没有找到这个用户' : err instanceof ApiError ? err.message : '出错了'

  const addPerson = async () => {
    if (!email.trim() || busy) return
    setBusy(true)
    setError('')
    try {
      const u = await store.findUser(email)
      if (u.id === store.get().me?.id) setError('不用添加自己')
      else if (!picked.some((p) => p.id === u.id)) setPicked((l) => [...l, u])
      setEmail('')
    } catch (err) {
      setError(errText(err))
    } finally {
      setBusy(false)
    }
  }

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    if (mode === 'group') {
      if (email.trim()) return addPerson()
      if (!picked.length) return setError('至少添加 1 位成员')
      setBusy(true)
      setError('')
      try {
        opened.current = await store.createGroup(groupName.trim(), picked.map((u) => u.id))
        ref.current?.close()
      } catch (err) {
        setError(errText(err))
      } finally {
        setBusy(false)
      }
      return
    }
    if (!email.trim()) return
    setBusy(true)
    setError('')
    try {
      opened.current = await store.startChatByEmail(email)
      ref.current?.close()
    } catch (err) {
      setError(errText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <md-dialog ref={ref}>
      <div slot="headline">{mode === 'direct' ? '发起聊天' : '发起群聊'}</div>
      <form slot="content" id="new-chat-form" onSubmit={submit} className="dialog-form">
        <div className="segmented small" role="radiogroup">
          {(['direct', 'group'] as const).map((m) => (
            <button
              key={m}
              type="button"
              role="radio"
              aria-checked={mode === m}
              className={mode === m ? 'on' : ''}
              onClick={() => {
                setMode(m)
                setError('')
              }}
            >
              {m === 'direct' ? '单聊' : '群聊'}
            </button>
          ))}
        </div>
        {mode === 'group' && (
          <md-outlined-text-field
            label="群名称（可选）"
            maxLength={32}
            value={groupName}
            onInput={(e: any) => setGroupName(e.target.value)}
            style={{ width: '100%' }}
          />
        )}
        <div className="email-row">
          <md-outlined-text-field
            label={mode === 'direct' ? '对方邮箱' : '添加成员（邮箱）'}
            type="email"
            value={email}
            error={!!error || undefined}
            error-text={error}
            onInput={(e: any) => {
              setEmail(e.target.value)
              setError('')
            }}
            onKeyDown={(e: any) => e.key === 'Enter' && (e.preventDefault(), mode === 'group' ? addPerson() : submit())}
            style={{ flex: 1 }}
          />
          {mode === 'group' && (
            <md-filled-tonal-button type="button" disabled={busy || !email.trim() || undefined} onClick={addPerson}>
              添加
            </md-filled-tonal-button>
          )}
        </div>
        {mode === 'group' && picked.length > 0 && (
          <div className="picked">
            {picked.map((u) => (
              <span key={u.id} className="person-chip">
                <Avatar user={u} size={24} />
                {displayName(u)}
                <button type="button" aria-label={`移除 ${displayName(u)}`} onClick={() => setPicked((l) => l.filter((x) => x.id !== u.id))}>
                  <Icon name="close" size={16} />
                </button>
              </span>
            ))}
          </div>
        )}
      </form>
      <div slot="actions">
        <md-text-button onClick={() => ref.current?.close()}>取消</md-text-button>
        {mode === 'direct' ? (
          <md-filled-tonal-button disabled={busy || !email.trim() || undefined} onClick={() => submit()}>
            {busy ? '查找中…' : '开始聊天'}
          </md-filled-tonal-button>
        ) : (
          <md-filled-tonal-button disabled={busy || !picked.length || undefined} onClick={() => submit()}>
            {busy ? '请稍候…' : `创建群聊（${picked.length + 1}人）`}
          </md-filled-tonal-button>
        )}
      </div>
    </md-dialog>
  )
}

export function SettingsDialog({
  open,
  onClose,
  seed,
  mode,
  onTheme,
}: {
  open: boolean
  onClose: () => void
  seed: string
  mode: Mode
  onTheme: (seed: string, mode: Mode) => void
}) {
  const ref = useDialog(open, onClose)
  const me = store.get().me
  return (
    <md-dialog ref={ref}>
      <div slot="headline">设置</div>
      <div slot="content" className="settings">
        {me && (
          <div className="me-card">
            <Avatar user={me} size={48} />
            <div>
              <div className="title-medium">{displayName(me)}</div>
              <div className="body-small muted">{me.email}</div>
            </div>
          </div>
        )}
        <div className="label-large">主题色</div>
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
        <div className="label-large">外观</div>
        <div className="segmented" role="radiogroup">
          {(['light', 'dark', 'system'] as Mode[]).map((m) => (
            <button key={m} role="radio" aria-checked={mode === m} className={mode === m ? 'on' : ''} onClick={() => onTheme(seed, m)}>
              {mode === m && <Icon name="check" size={18} />}
              {{ light: '浅色', dark: '深色', system: '跟随系统' }[m]}
            </button>
          ))}
        </div>
      </div>
      <div slot="actions">
        <md-text-button
          onClick={() => {
            ref.current?.close()
            store.logout()
          }}
        >
          <Icon slot="icon" name="logout" size={18} />
          退出登录
        </md-text-button>
        <md-text-button onClick={() => ref.current?.close()}>完成</md-text-button>
      </div>
    </md-dialog>
  )
}
