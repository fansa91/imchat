import { useEffect, useRef, useState } from 'react'
import { store, type State } from '../store'
import { api, ApiError } from '../api'
import { Avatar, OnlineDot } from './Avatar'
import { Icon } from '../icons'
import { useDialog } from './Dialogs'
import { displayName, presenceText } from '../util'
import type { FriendEntry, PublicUser, UserSearchResult } from '../types'

/**
 * v0.5.0 好友页（SPEC §1.4）：好友列表（备注优先显示、在线状态）、
 * 新的朋友（申请列表 + 红点）、搜索添加（邮箱 / 用户 ID）、好友详情。
 */
export function FriendsPage({ state, onOpenChat }: { state: State; onOpenChat: (cid: number) => void }) {
  const [adding, setAdding] = useState(false)
  const [showRequests, setShowRequests] = useState(false)
  const [detail, setDetail] = useState<FriendEntry | null>(null)
  const incoming = state.friendRequests.incoming
  const friends = Object.values(state.friends).sort((a, b) =>
    displayName(a.user, a.remark).localeCompare(displayName(b.user, b.remark), 'zh-Hans-CN'),
  )
  // 详情弹窗里跟随最新好友数据（改备注/被删后同步）
  const detailLive = detail ? (state.friends[detail.user.id] ?? null) : null

  return (
    <section className="pane list-pane side-page">
      <header className="top-bar">
        <h1 className="title-large">好友</h1>
        <div className="spacer" />
        <md-icon-button aria-label="添加好友" onClick={() => setAdding((v) => !v)}>
          <Icon name="personAdd" />
        </md-icon-button>
      </header>
      {state.conn !== 'online' && (
        <div className="conn-banner">
          <Icon name="cloudOff" size={18} />
          {state.conn === 'connecting' ? '正在连接…' : '连接已断开，正在重试'}
        </div>
      )}

      <div className="conv-scroll page-scroll">
        {adding && <AddFriendPanel onOpenChat={onOpenChat} />}

        <button className="entry-row" onClick={() => setShowRequests((v) => !v)}>
          <span className="entry-icon">
            <Icon name="personAdd" size={22} />
          </span>
          <span className="entry-main">新的朋友</span>
          {incoming.length > 0 && <span className="badge">{incoming.length > 99 ? '99+' : incoming.length}</span>}
          {showRequests && <Icon name="close" size={20} />}
        </button>
        {showRequests && <RequestsPanel state={state} />}

        <div className="section-label">我的好友（{friends.length}）</div>
        {friends.length === 0 ? (
          <div className="empty-list">
            <p>还没有好友</p>
            <md-filled-tonal-button onClick={() => setAdding(true)}>添加好友</md-filled-tonal-button>
          </div>
        ) : (
          <ul className="conv-list" role="list">
            {friends.map((f) => {
              const online = state.presence[f.user.id]?.online ?? f.online
              const status = state.config.presence_enabled ? presenceText(state.presence[f.user.id]) : online ? '在线' : '离线'
              return (
                <li key={f.user.id}>
                  <button className="conv-item" onClick={() => setDetail(f)}>
                    <md-ripple />
                    <OnlineDot online={online} size={48}>
                      <Avatar user={f.user} size={48} />
                    </OnlineDot>
                    <div className="conv-main">
                      <div className="conv-row">
                        <span className="conv-name">{displayName(f.user, f.remark)}</span>
                      </div>
                      <div className="conv-row">
                        <span className="conv-preview">
                          {f.remark && f.user.nickname ? `${f.user.nickname} · ` : ''}
                          {status}
                        </span>
                      </div>
                    </div>
                  </button>
                </li>
              )
            })}
          </ul>
        )}
      </div>

      <FriendDetailDialog friend={detailLive} onClose={() => setDetail(null)} onOpenChat={onOpenChat} />
    </section>
  )
}

/** 搜索添加：输入邮箱或用户 ID，结果卡按 relation 渲染操作按钮 */
function AddFriendPanel({ onOpenChat }: { onOpenChat: (cid: number) => void }) {
  const [query, setQuery] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<UserSearchResult | null>(null)
  const [attach, setAttach] = useState<string | null>(null) // null 未展开附言；字符串为附言草稿

  const search = async (e?: React.FormEvent) => {
    e?.preventDefault()
    const q = query.trim()
    if (!q || busy) return
    setBusy(true)
    setError('')
    setResult(null)
    setAttach(null)
    try {
      const u = /^\d+$/.test(q) ? await api.searchUserById(Number(q)) : await api.searchUser(q)
      setResult(u)
    } catch (err) {
      setError(err instanceof ApiError && err.status === 404 ? '没有找到这个用户' : err instanceof ApiError ? err.message : '搜索失败')
    } finally {
      setBusy(false)
    }
  }

  const act = async (fn: () => Promise<unknown>, after?: () => void) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await fn()
      after?.()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '操作失败，请稍后再试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="add-panel">
      <form className="email-row" onSubmit={search}>
        <md-outlined-text-field
          label="邮箱或用户 ID"
          value={query}
          error={!!error || undefined}
          error-text={error}
          onInput={(e: any) => {
            setQuery(e.target.value)
            setError('')
          }}
          style={{ flex: 1 }}
        />
        <md-filled-tonal-button type="submit" disabled={busy || !query.trim() || undefined}>
          {busy ? '查找中…' : '搜索'}
        </md-filled-tonal-button>
      </form>
      {result && (
        <div className="result-card">
          <Avatar user={result} size={44} />
          <div className="conv-main">
            <div className="title-medium">{displayName(result, store.get().friends[result.id]?.remark)}</div>
            <div className="body-small muted">ID：{result.id}</div>
          </div>
          {result.relation === 'none' &&
            (attach === null ? (
              <md-filled-tonal-button disabled={busy || undefined} onClick={() => setAttach('')}>
                添加
              </md-filled-tonal-button>
            ) : null)}
          {result.relation === 'pending_out' && <md-filled-tonal-button disabled>等待验证</md-filled-tonal-button>}
          {result.relation === 'pending_in' && (
            <md-filled-tonal-button
              disabled={busy || undefined}
              onClick={() => act(() => store.acceptFriendByUser(result.id), () => setResult({ ...result, relation: 'friend' }))}
            >
              接受
            </md-filled-tonal-button>
          )}
          {result.relation === 'friend' && (
            <md-filled-tonal-button
              disabled={busy || undefined}
              onClick={() => act(async () => onOpenChat(await store.openDirectWith(result)))}
            >
              发消息
            </md-filled-tonal-button>
          )}
        </div>
      )}
      {result && result.relation === 'none' && attach !== null && (
        <form
          className="attach-form"
          onSubmit={(e) => {
            e.preventDefault()
            void act(() => store.sendFriendRequest(result.id, attach.trim() || undefined), () => {
              setResult({ ...result, relation: 'pending_out' })
              setAttach(null)
            })
          }}
        >
          <md-outlined-text-field
            label="申请附言（可选）"
            maxLength={128}
            value={attach}
            onInput={(e: any) => setAttach(e.target.value)}
            style={{ flex: 1 }}
          />
          <md-filled-tonal-button type="submit" disabled={busy || undefined}>
            发送申请
          </md-filled-tonal-button>
        </form>
      )}
    </div>
  )
}

/** 申请列表：收到的（接受/拒绝）与我发出的（等待验证/撤回） */
function RequestsPanel({ state }: { state: State }) {
  const { incoming, outgoing } = state.friendRequests
  const [busyId, setBusyId] = useState(0)
  const run = async (id: number, fn: () => Promise<unknown>) => {
    if (busyId) return
    setBusyId(id)
    try {
      await fn()
    } catch (e) {
      store.showToast(e instanceof ApiError ? e.message : '操作失败')
    } finally {
      setBusyId(0)
    }
  }
  if (!incoming.length && !outgoing.length) return <div className="empty-list small">暂无好友申请</div>
  return (
    <div className="requests-panel">
      {incoming.map((r) => (
        <div key={r.id} className="request-row">
          <Avatar user={r.user} size={40} />
          <div className="conv-main">
            <div className="conv-name">{displayName(r.user)}</div>
            <div className="conv-preview">{r.message || '请求添加你为好友'}</div>
          </div>
          <md-text-button disabled={busyId === r.id || undefined} onClick={() => run(r.id, () => store.rejectFriendRequest(r.id))}>
            拒绝
          </md-text-button>
          <md-filled-tonal-button disabled={busyId === r.id || undefined} onClick={() => run(r.id, () => store.acceptFriendRequest(r.id))}>
            接受
          </md-filled-tonal-button>
        </div>
      ))}
      {outgoing.map((r) => (
        <div key={r.id} className="request-row">
          <Avatar user={r.user} size={40} />
          <div className="conv-main">
            <div className="conv-name">{displayName(r.user)}</div>
            <div className="conv-preview">{r.message || '等待对方验证'}</div>
          </div>
          <md-text-button disabled={busyId === r.id || undefined} onClick={() => run(r.id, () => store.cancelFriendRequest(r.id))}>
            撤回
          </md-text-button>
        </div>
      ))}
    </div>
  )
}

/** 好友详情：备注、发消息、1 对 1 通话（复用 v0.4）、删除好友 */
function FriendDetailDialog({
  friend,
  onClose,
  onOpenChat,
}: {
  friend: FriendEntry | null
  onClose: () => void
  onOpenChat: (cid: number) => void
}) {
  const pending = useRef<(() => void) | null>(null)
  const handleClosed = () => {
    onClose()
    const fn = pending.current
    pending.current = null
    fn?.()
  }
  const ref = useDialog(!!friend, handleClosed)
  const [remark, setRemark] = useState<string | null>(null)
  const [confirmDel, setConfirmDel] = useState(false)
  const [busy, setBusy] = useState(false)
  const callOn = !!store.get().config.call_enabled

  useEffect(() => {
    if (friend) {
      setRemark(null)
      setConfirmDel(false)
      setBusy(false)
    }
  }, [friend?.user.id])

  if (!friend) return <md-dialog ref={ref} />
  const u: PublicUser = friend.user

  const run = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    try {
      await fn()
    } catch (e) {
      store.showToast(e instanceof ApiError ? e.message : '操作失败')
    } finally {
      setBusy(false)
    }
  }
  const openChatThen = (after?: (cid: number) => void) =>
    run(async () => {
      const cid = await store.openDirectWith(u)
      pending.current = () => {
        onOpenChat(cid)
        after?.(cid)
      }
      ref.current?.close()
    })

  return (
    <md-dialog ref={ref}>
      <div slot="headline" className="group-head">
        <Avatar user={u} size={44} />
        <span>{displayName(u, friend.remark)}</span>
      </div>
      <div slot="content" className="settings">
        <div className="body-small muted">
          昵称：{u.nickname || '（未设置）'} · ID：{u.id}
          {friend.online ? ' · 在线' : ''}
        </div>

        <div className="label-large">备注名</div>
        {remark === null ? (
          <div className="field-row">
            <span className="group-name">{friend.remark || '未设置备注'}</span>
            <md-text-button onClick={() => setRemark(friend.remark)}>设置备注</md-text-button>
          </div>
        ) : (
          <form
            className="field-row"
            onSubmit={(e) => {
              e.preventDefault()
              void run(async () => {
                await store.setFriendRemark(u.id, remark.trim())
                setRemark(null)
              })
            }}
          >
            <md-outlined-text-field label="备注名（仅自己可见）" maxLength={64} value={remark} onInput={(e: any) => setRemark(e.target.value)} style={{ flex: 1 }} />
            <md-filled-tonal-button type="submit" disabled={busy || undefined}>
              保存
            </md-filled-tonal-button>
          </form>
        )}

        <div className="detail-actions">
          <md-filled-tonal-button disabled={busy || undefined} onClick={() => openChatThen()}>
            <Icon slot="icon" name="chat" size={18} />
            发消息
          </md-filled-tonal-button>
          {callOn && (
            <>
              <md-filled-tonal-button disabled={busy || undefined} onClick={() => openChatThen((cid) => store.startCall(cid, 'audio'))}>
                <Icon slot="icon" name="call" size={18} />
                语音通话
              </md-filled-tonal-button>
              <md-filled-tonal-button disabled={busy || undefined} onClick={() => openChatThen((cid) => store.startCall(cid, 'video'))}>
                <Icon slot="icon" name="videocam" size={18} />
                视频通话
              </md-filled-tonal-button>
            </>
          )}
        </div>

        {confirmDel ? (
          <div className="form-error">
            确定删除好友？聊天记录不会删除。
            <div className="field-row end" style={{ marginTop: 8 }}>
              <md-text-button onClick={() => setConfirmDel(false)}>取消</md-text-button>
              <md-text-button
                class="danger"
                disabled={busy || undefined}
                onClick={() =>
                  run(async () => {
                    await store.removeFriend(u.id)
                    ref.current?.close()
                  })
                }
              >
                确定删除
              </md-text-button>
            </div>
          </div>
        ) : (
          <div>
            <md-text-button class="danger" onClick={() => setConfirmDel(true)}>
              删除好友
            </md-text-button>
          </div>
        )}
      </div>
      <div slot="actions">
        <md-text-button onClick={() => ref.current?.close()}>完成</md-text-button>
      </div>
    </md-dialog>
  )
}
