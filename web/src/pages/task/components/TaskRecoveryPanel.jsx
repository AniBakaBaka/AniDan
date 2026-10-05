// SPDX-License-Identifier: AGPL-3.0-only
import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Card, Checkbox, Descriptions, Space } from 'antd'
import { useTranslation } from 'react-i18next'
import { approveTaskRecovery, getTaskRecoveryStatus } from '../../../apis/taskRecovery.js'

export function TaskRecoveryPanel() {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const text = (cn, en) => zh ? cn : en
  const [data, setData] = useState(null)
  const [error, setError] = useState('')
  const [checked, setChecked] = useState(false)
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [uncertain, setUncertain] = useState(false)
  const request = useRef(null)
  const mutation = useRef(null)
  const mounted = useRef(false)
  const read = useCallback(async () => {
    if (mutation.current) return
    request.current?.abort()
    const controller = new AbortController(); request.current = controller
    setData(null); setChecked(false); setError(''); setLoading(true)
    try {
      const response = await getTaskRecoveryStatus({ signal: controller.signal, timeout: 15000 })
      if (!controller.signal.aborted) { setData(response.data); setUncertain(false) }
    } catch (err) {
      if (!controller.signal.aborted) setError(err.message)
    } finally {
      if (!controller.signal.aborted) setLoading(false)
    }
  }, [])
  useEffect(() => {
    mounted.current = true
    read()
    return () => { mounted.current = false; request.current?.abort(); mutation.current?.abort() }
  }, [read])
  const approve = async () => {
    if (!checked || data?.recoveryReviewRequired !== true || mutation.current || loading) return
    const controller = new AbortController(); mutation.current = controller
    request.current?.abort(); setSaving(true); setChecked(false); setError('')
    try {
      const response = await approveTaskRecovery({ signal: controller.signal, timeout: 15000 })
      if (response.data?.recoveryReviewRequired !== false) throw new Error(text('审批结果未确认', 'Approval result was not confirmed'))
      if (mounted.current) { setData(previous => ({ ...previous, recoveryReviewRequired: false })); setUncertain(false) }
    } catch (err) {
      if (mounted.current) { setData(null); setError(err.message); setUncertain(true) }
    } finally {
      mutation.current = null
      if (mounted.current) setSaving(false)
    }
  }
  const migration = data?.migration
  const display = value => typeof value === 'string' && value ? value : '—'
  const fields = [
    ['sourceDBType', text('原数据库类型', 'Source database type')],
    ['sourceCreatedAt', text('源快照创建时间', 'Source snapshot created at')],
    ['migratedAt', text('迁移时间', 'Migrated at')],
    ['initializedAt', text('首次初始化时间', 'First initialized at')],
    ['receiptSHA256', text('迁移收据 SHA-256', 'Migration receipt SHA-256')],
    ['snapshotSHA256', text('源快照 SHA-256', 'Source snapshot SHA-256')],
  ]
  return <Card size="small" style={{ marginBottom: 16 }} title={text('历史任务恢复审核', 'Historical task recovery review')}
    extra={<Button size="small" onClick={read} loading={loading} disabled={saving}>{text('刷新状态', 'Refresh status')}</Button>}>
    <Space direction="vertical" style={{ width: '100%' }}>
      {error && <Alert type="error" showIcon message={error} />}
      {uncertain && <Alert type="warning" showIcon message={text('审批结果不确定，不会自动重试。请先刷新状态，再决定是否继续；请求可能已生效。', 'Approval outcome is uncertain; no automatic retry. Refresh status before deciding whether to continue, because the request may already have taken effect.')} />}
      {!data && !loading && <span>{text('未确认恢复状态，审批操作不可用', 'Recovery status is unconfirmed; approval is unavailable')}</span>}
      {migration && <Descriptions size="small" column={1} items={fields.map(([key, label]) => ({ key, label, children: <span style={{ overflowWrap: 'anywhere' }}>{display(migration[key])}</span> }))} />}
      {data?.recoveryReviewRequired === false && <Alert type="info" showIcon message={text('当前没有等待审批的恢复限制。这不代表历史任务已执行或通知已送达。', 'No recovery review is currently pending. This does not mean historical jobs have run or notifications were delivered.')} />}
      {data?.recoveryReviewRequired === true && <>
        <Alert type="warning" showIcon message={text('待处理任务与已启用的定时任务等待审核。快照创建后，这些任务可能已在原服务执行；请先检查重复下载、通知及其他外部操作的影响。', 'Pending jobs and enabled schedules await review. They may already have run on the original server after the snapshot; first review duplicate downloads, notifications and other external effects.')} />
        <Alert type="info" showIcon message={text('审核限制仅暂停历史待处理任务恢复与定时任务，不是离线或只读模式；新提交的任务、独立通知轮询及其他启动服务不受此限制。', 'This gate only pauses historical pending-job recovery and schedules. It is not offline or read-only mode; newly submitted work, separate notification polling and other startup services are not suspended.')} />
        {migration && <Alert type="info" showIcon message={text('以上为迁移来源信息。若收据丢失或发生变化，服务会拒绝审批；请恢复经过验证的证据，不要删除审核标记强行启动。', 'Migration provenance is shown above. Missing or changed receipts prevent approval; restore verified evidence rather than delete the review marker to force startup.')} />}
        <Checkbox checked={checked} disabled={saving} onChange={event => setChecked(event.target.checked)}>{text('我已审核历史任务及其影响，同意恢复待处理任务和已启用的定时任务', 'I have reviewed the historical jobs and their effects and approve recovery of pending jobs and enabled schedules')}</Checkbox>
        <Button type="primary" disabled={!checked || loading} loading={saving} onClick={approve}>{text('确认恢复任务与定时任务', 'Approve pending jobs and schedules')}</Button>
      </>}
    </Space>
  </Card>
}
