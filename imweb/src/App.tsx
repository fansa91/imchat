import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import { store, type State } from './store'
import { applyTheme, loadPrefs, onSystemThemeChange, savePrefs, type Mode } from './theme'
import { Auth } from './components/Auth'
import { CallView } from './components/CallView'
import { GroupCallView } from './components/GroupCallView'
import { ResetPasswordPage } from './components/ResetPassword'
import { ConversationList } from './components/ConversationList'
import { FriendsPage } from './components/FriendsPage'
import { MePage } from './components/MePage'
import { ChatView } from './components/ChatView'
import { NewChatDialog } from './components/Dialogs'
import { GroupInfoDialog } from './components/GroupInfo'
import { Avatar } from './components/Avatar'
import { Icon } from './icons'
import { convTitle, displayName } from './util'

// 用 hash 路由：#/c/123，手机上浏览器返回键可以回到列表
const parseHash = () => {
  const m = location.hash.match(/^#\/c\/(\d+)/)
  return m ? Number(m[1]) : null
}

type Tab = 'chats' | 'friends' | 'me'

export default function App() {
  const state = useSyncExternalStore(store.subscribe, store.get)
  const [activeId, setActiveId] = useState<number | null>(parseHash)
  const [tab, setTab] = useState<Tab>('chats')
  const [prefs, setPrefs] = useState(loadPrefs)
  const [newOpen, setNewOpen] = useState(false)
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
    setTab('chats')
    if (parseHash() === id) return
    if (parseHash() !== null) location.replace(`#/c/${id}`)
    else location.hash = `#/c/${id}`
  }, [])
  const back = useCallback(() => {
    if (parseHash() !== null) history.length > 1 ? history.back() : location.replace('#/')
  }, [])
  const closeNew = useCallback(() => setNewOpen(false), [])
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
  const pendingFriends = state.friendRequests.incoming.length

  return (
    <div className={'layout' + (validActive ? ' has-active' : '')}>
      {/* v0.5.0 左侧导航栏：消息 / 好友 / 我的（手机上沉到底部） */}
      <nav className="nav-rail" aria-label="主导航">
        <button type="button" className={'rail-btn' + (tab === 'chats' ? ' on' : '')} onClick={() => setTab('chats')}>
          <span className="rail-icon">
            <Icon name="chat" size={24} />
            {unread > 0 && <span className="rail-badge">{unread > 99 ? '99+' : unread}</span>}
          </span>
          <span className="rail-label">消息</span>
        </button>
        <button type="button" className={'rail-btn' + (tab === 'friends' ? ' on' : '')} onClick={() => setTab('friends')}>
          <span className="rail-icon">
            <Icon name="group" size={24} />
            {pendingFriends > 0 && <span className="rail-badge">{pendingFriends > 99 ? '99+' : pendingFriends}</span>}
          </span>
          <span className="rail-label">好友</span>
        </button>
        <button type="button" className={'rail-btn' + (tab === 'me' ? ' on' : '')} onClick={() => setTab('me')}>
          <span className="rail-icon">
            <Avatar user={state.me} size={26} />
          </span>
          <span className="rail-label">我的</span>
        </button>
      </nav>

      {tab === 'chats' && <ConversationList state={state} activeId={validActive} onOpen={open} onNew={() => setNewOpen(true)} />}
      {tab === 'friends' && <FriendsPage state={state} onOpenChat={open} />}
      {tab === 'me' && (
        <MePage state={state} seed={prefs.seed} mode={prefs.mode} onTheme={(seed, mode) => setPrefs({ seed, mode })} />
      )}
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
      <GroupCallView state={state} />
      <GroupInviteBanner state={state} />
      <GroupInfoDialog open={infoOpen && !!validActive} onClose={closeInfo} state={state} id={validActive} />
      {state.toast && (
        <div className="toast" role="status">
          {state.toast}
        </div>
      )}
      <NewChatDialog open={newOpen} onClose={closeNew} onOpened={open} />
    </div>
  )
}

/** v0.5.0 群通话邀请横幅（SPEC §3.4）：任意页面可见，按响铃时长自动收起 */
function GroupInviteBanner({ state }: { state: State }) {
  const invite = state.groupInvite
  useEffect(() => {
    if (!invite) return
    const seconds = state.config.call_ring_timeout_seconds ?? 60
    const t = window.setTimeout(() => store.clearGroupInvite(), Math.max(5, seconds) * 1000)
    return () => clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [invite?.callId])
  if (!invite || state.groupCall) return null
  const conv = state.convs[invite.conversationId]
  const creatorName = displayName(invite.creator, state.friends[invite.creator.id]?.remark)
  return (
    <div className="ginvite" role="alert">
      <Icon name={invite.kind === 'video' ? 'videocam' : 'call'} size={22} />
      <div className="ginvite-main">
        <div className="title-medium">
          {creatorName} 在「{conv ? convTitle(conv) : '群聊'}」发起了{invite.kind === 'video' ? '视频' : '语音'}通话
        </div>
        <div className="body-small muted">点击加入一起聊</div>
      </div>
      <md-filled-tonal-button
        onClick={() => {
          store.clearGroupInvite()
          store.joinGroupCall(invite.conversationId)
        }}
      >
        加入
      </md-filled-tonal-button>
      <md-text-button onClick={() => store.clearGroupInvite()}>忽略</md-text-button>
    </div>
  )
}
