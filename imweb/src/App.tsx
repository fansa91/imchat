import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import { store } from './store'
import { applyTheme, loadPrefs, onSystemThemeChange, savePrefs, type Mode } from './theme'
import { Auth } from './components/Auth'
import { CallView } from './components/CallView'
import { ResetPasswordPage } from './components/ResetPassword'
import { ConversationList } from './components/ConversationList'
import { ChatView } from './components/ChatView'
import { NewChatDialog, SettingsDialog } from './components/Dialogs'
import { GroupInfoDialog } from './components/GroupInfo'
import { Icon } from './icons'

// 用 hash 路由：#/c/123，手机上浏览器返回键可以回到列表
const parseHash = () => {
  const m = location.hash.match(/^#\/c\/(\d+)/)
  return m ? Number(m[1]) : null
}

export default function App() {
  const state = useSyncExternalStore(store.subscribe, store.get)
  const [activeId, setActiveId] = useState<number | null>(parseHash)
  const [prefs, setPrefs] = useState(loadPrefs)
  const [newOpen, setNewOpen] = useState(false)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [infoOpen, setInfoOpen] = useState(false)

  useEffect(() => {
    applyTheme(prefs.seed, prefs.mode)
    savePrefs(prefs.seed, prefs.mode)
    return onSystemThemeChange(() => applyTheme(prefs.seed, prefs.mode))
  }, [prefs])

  useEffect(() => {
    store.boot()
    const h = () => setActiveId(parseHash())
    window.addEventListener('hashchange', h)
    return () => window.removeEventListener('hashchange', h)
  }, [])

  // 总未读数显示在标签页标题
  const unread = Object.values(state.convs).reduce((n, c) => n + c.unread, 0)
  useEffect(() => {
    document.title = unread ? `(${unread}) 消息` : '消息'
  }, [unread])

  const open = useCallback((id: number) => {
    if (parseHash() === id) return
    if (parseHash() !== null) location.replace(`#/c/${id}`)
    else location.hash = `#/c/${id}`
  }, [])
  const back = useCallback(() => {
    if (parseHash() !== null) history.length > 1 ? history.back() : location.replace('#/')
  }, [])
  const closeNew = useCallback(() => setNewOpen(false), [])
  const closeSettings = useCallback(() => setSettingsOpen(false), [])
  const closeInfo = useCallback(() => setInfoOpen(false), [])

  // v0.4.0：公开重置密码页（邮件链接 /reset-password?token=...），无路由库，按 path 判断
  if (location.pathname === '/reset-password') return <ResetPasswordPage />

  if (state.booting) {
    return (
      <div className="center full">
        <md-circular-progress indeterminate />
      </div>
    )
  }
  if (!state.me) return <Auth />

  const validActive = activeId !== null && state.convs[activeId] ? activeId : null

  return (
    <div className={'layout' + (validActive ? ' has-active' : '')}>
      <ConversationList
        state={state}
        activeId={validActive}
        onOpen={open}
        onNew={() => setNewOpen(true)}
        onSettings={() => setSettingsOpen(true)}
      />
      {validActive ? (
        <ChatView key={validActive} state={state} id={validActive} onBack={back} onInfo={() => setInfoOpen(true)} />
      ) : (
        <section className="pane chat-pane placeholder">
          <div className="center grow muted">
            <Icon name="chat" size={64} />
            <p className="title-medium">选择一个会话开始聊天</p>
          </div>
        </section>
      )}
      <CallView state={state} />
      <GroupInfoDialog open={infoOpen && !!validActive} onClose={closeInfo} state={state} id={validActive} />
      {state.toast && (
        <div className="toast" role="status">
          {state.toast}
        </div>
      )}
      <NewChatDialog open={newOpen} onClose={closeNew} onOpened={open} />
      <SettingsDialog
        open={settingsOpen}
        onClose={closeSettings}
        seed={prefs.seed}
        mode={prefs.mode}
        onTheme={(seed, mode) => setPrefs({ seed, mode })}
      />
    </div>
  )
}
