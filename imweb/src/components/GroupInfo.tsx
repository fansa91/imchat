import { useEffect, useState } from 'react'
import { store, type State } from '../store'
import { ApiError } from '../api'
import { Icon } from '../icons'
import { Avatar, ConvAvatar, OnlineDot } from './Avatar'
import { useDialog } from './Dialogs'
import { displayName, listTime } from '../util'
import type { Member } from '../types'

const ROLE = ['', '管理员', '群主']

/** 群聊信息：群名、公告、成员列表（添加 / 移除 / 设管理员）、退出群聊 */
export function GroupInfoDialog({ open, onClose, state, id }: { open: boolean; onClose: () => void; state: State; id: number | null }) {
  const ref = useDialog(open, onClose)
  const conv = id ? state.convs[id] : undefined
  const members: Member[] = (id && state.members[id]) || []
  const meId = state.me?.id
  const myRole = members.find((m) => m.user.id === meId)?.role ?? conv?.my_role ?? 0
  const [editName, setEditName] = useState<string | null>(null)
  const [editAnn, setEditAnn] = useState<string | null>(null)
  const [email, setEmail] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmLeave, setConfirmLeave] = useState(false)
  const [openRow, setOpenRow] = useState<number | null>(null)

  useEffect(() => {
    if (open && id) {
      store.loadMembers(id)
      setEditName(null)
      setEditAnn(null)
      setEmail('')
      setError('')
      setConfirmLeave(false)
      setOpenRow(null)
    }
  }, [open, id])

  const run = async (fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await fn()
      return true
    } catch (e) {
      setError(e instanceof ApiError ? (e.status === 404 ? '没有找到这个用户' : e.message) : '出错了')
      return false
    } finally {
      setBusy(false)
    }
  }

  if (!conv || !id) return <md-dialog ref={ref} />

  return (
    <md-dialog ref={ref} class="group-dialog">
      <div slot="headline" className="group-head">
        <ConvAvatar conv={conv} size={40} />
        <span>群聊信息</span>
      </div>
      <div slot="content" className="group-info">
        <section>
          <div className="label-large">群名称</div>
          {editName === null ? (
            <div className="field-row">
              <span className="group-name">{conv.name}</span>
              <md-text-button onClick={() => setEditName(conv.name ?? '')}>修改</md-text-button>
            </div>
          ) : (
            <form
              className="field-row"
              onSubmit={async (e) => {
                e.preventDefault()
                if (await run(() => store.renameGroup(id, editName))) setEditName(null)
              }}
            >
              <md-outlined-text-field label="群名称" maxLength={32} value={editName} onInput={(e: any) => setEditName(e.target.value)} style={{ flex: 1 }} />
              <md-filled-tonal-button type="submit" disabled={busy || !editName.trim() || undefined}>
                保存
              </md-filled-tonal-button>
            </form>
          )}
        </section>

        <section>
          <div className="label-large">群公告</div>
          {editAnn === null ? (
            <div className="field-row">
              <span className={'ann-body' + (conv.announcement ? '' : ' muted')}>{conv.announcement || '暂无公告'}</span>
              {myRole >= 1 && <md-text-button onClick={() => setEditAnn(conv.announcement ?? '')}>编辑</md-text-button>}
            </div>
          ) : (
            <form
              className="ann-form"
              onSubmit={async (e) => {
                e.preventDefault()
                if (await run(() => store.setAnnouncement(id, editAnn))) setEditAnn(null)
              }}
            >
              <textarea className="ann-input" maxLength={1000} rows={3} value={editAnn} onChange={(e) => setEditAnn(e.target.value)} placeholder="群公告（只有群主和管理员可以修改）" />
              <div className="field-row end">
                <md-text-button type="button" onClick={() => setEditAnn(null)}>
                  取消
                </md-text-button>
                <md-filled-tonal-button type="submit" disabled={busy || undefined}>
                  发布
                </md-filled-tonal-button>
              </div>
            </form>
          )}
          {conv.announcement_at && conv.announcement && <div className="body-small muted">更新于 {listTime(conv.announcement_at)}</div>}
        </section>

        <section>
          <div className="label-large">
            成员（{members.length || conv.member_count}/{state.config.group_max_members}）
          </div>
          <form
            className="field-row"
            onSubmit={async (e) => {
              e.preventDefault()
              if (!email.trim()) return
              const ok = await run(async () => {
                const u = await store.findUser(email)
                await store.addMembers(id, [u.id])
              })
              if (ok) setEmail('')
            }}
          >
            <md-outlined-text-field
              label="邀请成员（邮箱）"
              type="email"
              value={email}
              onInput={(e: any) => {
                setEmail(e.target.value)
                setError('')
              }}
              style={{ flex: 1 }}
            />
            <md-filled-tonal-button type="submit" disabled={busy || !email.trim() || undefined} aria-label="邀请">
              <Icon slot="icon" name="personAdd" size={18} />
              邀请
            </md-filled-tonal-button>
          </form>
          {error && <div className="form-error">{error}</div>}
          <ul className="member-list">
            {members.map((m) => {
              const isMe = m.user.id === meId
              const canRemove = !isMe && myRole >= 1 && m.role < myRole
              const canRole = !isMe && myRole === 2
              return (
                <li key={m.user.id} className="member-row">
                  <OnlineDot online={!!state.config.presence_enabled && !!state.presence[m.user.id]?.online} size={36}><Avatar user={m.user} size={36} /></OnlineDot>
                  <div className="member-main">
                    <div>
                      {displayName(m.user, state.friends[m.user.id]?.remark)}
                      {isMe && <span className="muted">（你）</span>}
                      {m.role > 0 && <span className={'role-badge r' + m.role}>{ROLE[m.role]}</span>}
                    </div>
                    {/* v0.5.0：群成员响应已收紧为 PublicUser，不再下发 email（SPEC §1.2） */}
                    <div className="body-small muted">ID：{m.user.id}</div>
                  </div>
                  {(canRemove || canRole) && (
                    <md-icon-button aria-label={`管理 ${displayName(m.user)}`} onClick={() => setOpenRow(openRow === m.user.id ? null : m.user.id)}>
                      <Icon name="more" size={20} />
                    </md-icon-button>
                  )}
                  {openRow === m.user.id && (
                    <div className="member-actions">
                      {canRole && (
                        <md-text-button onClick={() => run(() => store.setRole(id, m.user.id, m.role === 1 ? 'member' : 'admin')).then(() => setOpenRow(null))}>
                          {m.role === 1 ? '取消管理员' : '设为管理员'}
                        </md-text-button>
                      )}
                      {canRemove && (
                        <md-text-button class="danger" onClick={() => run(() => store.removeMember(id, m.user.id)).then(() => setOpenRow(null))}>
                          移出群聊
                        </md-text-button>
                      )}
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        </section>
      </div>
      <div slot="actions">
        {confirmLeave ? (
          <>
            <span className="body-small muted leave-q">{myRole === 2 ? '退出后群主将转给其他成员，确定？' : '确定退出群聊？'}</span>
            <md-text-button onClick={() => setConfirmLeave(false)}>取消</md-text-button>
            <md-text-button
              class="danger"
              onClick={async () => {
                if (await run(() => store.leaveGroup(id))) {
                  ref.current?.close()
                  location.replace('#/')
                }
              }}
            >
              确定退出
            </md-text-button>
          </>
        ) : (
          <>
            <md-text-button class="danger" onClick={() => setConfirmLeave(true)}>
              退出群聊
            </md-text-button>
            <md-text-button onClick={() => ref.current?.close()}>完成</md-text-button>
          </>
        )}
      </div>
    </md-dialog>
  )
}
