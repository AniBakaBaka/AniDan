// SPDX-License-Identifier: AGPL-3.0-only
import React, { useEffect, useRef, useState } from 'react'
import { Alert, Button, Checkbox, Descriptions, Modal, Space } from 'antd'
import { useTranslation } from 'react-i18next'
import { confirmLocalBinding, createLocalReviewGuard, localReviewID, previewLocalBinding, repeatReviewedLocalImport } from '../../../apis/localBindingReview.js'

export default function LocalBindingReviewModal({ open, item, onClose, onQueued }) {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const tr = (cn, en) => zh ? cn : en
  let itemId = ''
  try { if (item?.id != null) itemId = localReviewID(item.id) } catch { /* Do not target a rounded or invalid identifier. */ }
  const guard = useRef(createLocalReviewGuard())
  const write = useRef(null)
  const [view, setView] = useState(null)
  const [phase, setPhase] = useState('idle')
  const [notice, setNotice] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const [now, setNow] = useState(Date.now())
  const [taskId, setTaskId] = useState('')
  const busy = phase === 'loading' || phase === 'confirming' || phase === 'submitting'
  const writing = phase === 'confirming' || phase === 'submitting'
  const expired = view?.status === 'reviewable' && Date.parse(view.expiresAt) <= now

  async function refresh() {
    if (!itemId || write.current) return
    const request = guard.current.begin(itemId)
    setView(null); setConfirmed(false); setNotice(''); setTaskId(''); setPhase('loading')
    try {
      const result = await previewLocalBinding(itemId, { signal: request.signal })
      if (!guard.current.isCurrent(request, itemId)) return
      setView(result); setNow(Date.now()); setPhase(result.status)
    } catch (error) {
      if (!guard.current.isCurrent(request, itemId)) return
      setPhase('error')
      setNotice(error.response?.status === 401
        ? tr('请先使用用户账号登录。', 'Sign in with a user account first.')
        : tr('无法确认当前关联，可能存在缺失、冲突或变化。请核查数据，不要清除导入标记或绑定。', 'The current association could not be verified. Evidence may be missing, conflicting or changed. Do not clear imported flags or bindings.'))
    }
  }

  useEffect(() => {
    guard.current.invalidate(); write.current = null
    setView(null); setConfirmed(false); setNotice(''); setTaskId(''); setPhase('idle')
    if (open && itemId) void refresh()
    return () => { guard.current.invalidate(); write.current = null }
    // The selected item, not its stale list metadata, controls this fresh read.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, itemId, item])

  useEffect(() => {
    if (!open || view?.status !== 'reviewable') return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [open, view])

  async function perform(kind) {
    if (write.current || !view || view.itemId !== itemId || busy) return
    if (kind === 'confirm' && (!confirmed || view.status !== 'reviewable' || Date.parse(view.expiresAt) <= Date.now())) return
    if (kind === 'repeat' && view.status !== 'bound') return
    const selected = view
    const request = guard.current.begin(itemId)
    write.current = request
    setNotice(''); setConfirmed(false); setPhase(kind === 'confirm' ? 'confirming' : 'submitting')
    try {
      if (kind === 'confirm') {
        const result = await confirmLocalBinding(selected, { signal: request.signal })
        if (!guard.current.isCurrent(request, itemId)) return
        setView(result); setPhase('bound')
        setNotice(tr('当前关联绑定已保存。尚未重新导入弹幕。', 'Current association binding saved. No comments have been reimported.'))
      } else {
        const id = await repeatReviewedLocalImport(selected, { signal: request.signal })
        if (!guard.current.isCurrent(request, itemId)) return
        setTaskId(id); setView(null); setPhase('accepted')
        setNotice(tr('重新导入任务已提交，不代表处理完成。请到任务页查看结果。', 'Reimport task accepted, not completed. Check the task page for its result.'))
        onQueued?.()
      }
    } catch (error) {
      if (!guard.current.isCurrent(request, itemId)) return
      setView(null); setPhase('uncertain')
      setNotice(kind === 'confirm'
        ? tr('确认结果未核实。请重新读取当前状态后再操作，不要重复发送确认。', 'Confirmation outcome is unverified. Read the current status before taking another action; do not resend confirmation.')
        : tr('任务提交结果未核实。请先检查任务页，避免重复导入。', 'Task submission outcome is unverified. Check the task page before another import.'))
    } finally {
      if (write.current === request) write.current = null
    }
  }

  const display = value => value == null || value === '' ? tr('未知', 'Unknown') : String(value)
  const externalIDs = value => `TMDB: ${display(value.tmdbId)} · TVDB: ${display(value.tvdbId)} · IMDb: ${display(value.imdbId)}`
  const details = view?.status === 'reviewable' ? [
    [tr('本地条目', 'Local item'), `${view.item.id} · ${view.item.title}`],
    [tr('条目类型／年份／季／集', 'Item type / year / season / episode'), [view.item.mediaType, view.item.year, view.item.season, view.item.episode].map(display).join(' / ')],
    [tr('条目外部 ID', 'Item external IDs'), externalIDs(view.item)],
    [tr('条目创建时间', 'Item created'), view.item.createdAt],
    [tr('库中作品', 'Library work'), `${view.target.animeId} · ${view.target.title}`],
    [tr('库中类型／年份／季', 'Library type / year / season'), [view.target.type, view.target.year, view.target.season].map(display).join(' / ')],
    [tr('库中外部 ID', 'Library external IDs'), externalIDs(view.target)],
    [tr('元数据记录 ID', 'Metadata record ID'), view.target.metadataId],
    [tr('来源', 'Source'), `${view.source.sourceId} · ${view.source.provider} · ${view.source.mediaId}`],
    [tr('来源顺序／创建时间', 'Source order / created'), `${view.source.sourceOrder} / ${view.source.createdAt}`],
    [tr('分集', 'Episode'), `${view.episode.episodeId} · ${view.episode.index} · ${view.episode.providerEpisodeId}`],
    [tr('原 XML 路径', 'Input XML path'), view.item.sourcePath],
    [tr('库中 XML 路径', 'Stored XML path'), view.episode.poolPath],
    [tr('文件字节数（原文件／库文件）', 'File bytes (input / stored)'), `${view.files.sourceBytes} / ${view.files.poolBytes}`],
    [tr('一致的 SHA-256', 'Matching SHA-256'), view.files.sourceSha256],
    [tr('弹幕数', 'Comment count'), view.files.commentCount],
    [tr('预览有效至', 'Preview expires'), view.expiresAt],
  ] : []

  return (
    <Modal open={open} title={tr('核对当前导入关联', 'Review current import association')} onCancel={() => { if (!writing) onClose?.() }} width={820} footer={null} destroyOnClose maskClosable={!writing} keyboard={!writing} closable={!writing}>
      <Space direction="vertical" style={{ width: '100%' }}>
        <Alert type="warning" showIcon message={tr('请先保存数据库和 XML 的一致备份，并在副本上测试。', 'Keep a consistent database/XML backup and test on a copy first.')} description={tr('核对的是当前关联，不证明历史归属，也不会修复已经混入的弹幕。不要清除导入标记或绑定来绕过检查。', 'This verifies the current association, not historical ownership, and does not repair mixed pools. Do not bypass checks by clearing imported flags or bindings.')} />
        {notice && <Alert type={phase === 'uncertain' || phase === 'error' ? 'warning' : 'info'} showIcon message={notice} />}
        {!itemId && <Alert type="warning" message={tr('条目 ID 无效，请刷新列表。', 'Invalid item ID. Refresh the list.')} />}
        {phase === 'loading' && <div>{tr('正在读取和核对…', 'Reading and verifying…')}</div>}
        {view?.status === 'reviewable' && <>
          <Descriptions column={1} bordered size="small" items={details.map(([label, value]) => ({ key: label, label, children: <span style={{ overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' }}>{display(value)}</span> }))} />
          <Alert type="info" message={tr('本次只新增 1 条当前关联绑定。不会改写弹幕、元数据或导入标记，也不会自动导入。', 'This adds one current-association binding only. It does not rewrite comments, metadata or imported flags, and does not start an import.')} />
          {expired && <Alert type="warning" message={tr('预览已过期，请重新核对。', 'Preview expired. Review again.')} />}
          <Checkbox checked={confirmed} disabled={busy || expired} onChange={event => setConfirmed(event.target.checked)}>{tr('我已核对上述当前关联，并确认新增这条绑定', 'I reviewed the association above and confirm adding this binding')}</Checkbox>
          <Button type="primary" disabled={!confirmed || busy || expired} loading={phase === 'confirming'} onClick={() => perform('confirm')}>{tr('确认当前关联', 'Confirm current association')}</Button>
        </>}
        {view?.status === 'bound' && <>
          <Alert type="success" message={tr('已有绑定的当前身份已通过校验', 'The existing binding identity is validated')} description={tr('这不表示当前源 XML 与旧弹幕池相同。重新导入会按已验证关联更新同一文件位置的内容。', 'This does not claim that input XML equals the previous pool. Reimport refreshes content at the same verified file assignment.')} />
          <div style={{ overflowWrap: 'anywhere' }}>{tr('条目／来源／媒体 ID：', 'Item / provider / media ID: ')}{view.repeatOptions.itemId} / {view.repeatOptions.provider} / {view.repeatOptions.mediaId}</div>
          <Button disabled={busy} loading={phase === 'submitting'} onClick={() => perform('repeat')}>{tr('按此关联重新导入', 'Reimport with this association')}</Button>
        </>}
        {taskId && <div style={{ overflowWrap: 'anywhere' }}>{tr('任务 ID：', 'Task ID: ')}{taskId}</div>}
        <Space wrap>
          <Button disabled={busy || phase === 'accepted'} onClick={refresh}>{tr('重新读取当前状态', 'Read current status again')}</Button>
          <Button href="/task" target="_blank" rel="noopener noreferrer">{tr('查看任务', 'View tasks')}</Button>
          <Button disabled={writing} onClick={onClose}>{tr('关闭', 'Close')}</Button>
        </Space>
      </Space>
    </Modal>
  )
}
