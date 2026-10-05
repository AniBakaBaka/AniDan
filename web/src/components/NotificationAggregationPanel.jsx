// SPDX-License-Identifier: AGPL-3.0-only
import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Card, Descriptions, Form, InputNumber, Space, Switch, Typography } from 'antd'
import { useTranslation } from 'react-i18next'
import { getNotificationAggregation, updateNotificationAggregation, getNotificationLifecycleStatus } from '../apis'
import { aggregationSettings, notificationCounters } from '../utils/notificationAggregation'
import { createNotificationDraftGuard } from '../utils/notificationDraft'

export default function NotificationAggregationPanel() {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const text = (cn, en) => zh ? cn : en
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [data, setData] = useState(null)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  const alive = useRef(false)
  const reads = useRef(null)
  const writes = useRef(null)
  const guard = useRef(createNotificationDraftGuard())
  const load = useCallback(async () => {
    reads.current?.abort()
    const controller = new AbortController(); reads.current = controller
    guard.current.invalidate()
    setLoading(true); setError(''); setSaved(false); setData(null)
    try {
      const [settings, status] = await Promise.all([
        getNotificationAggregation({ signal: controller.signal, timeout: 15000 }),
        getNotificationLifecycleStatus({ signal: controller.signal, timeout: 15000 }),
      ])
      const value = aggregationSettings(settings.data)
      if (!controller.signal.aborted && alive.current) { form.setFieldsValue(value); setData({ settings: settings.data, status: status.data }); return true }
    } catch (err) {
      if (!controller.signal.aborted && alive.current) setError(err.message)
    } finally {
      if (!controller.signal.aborted && alive.current) setLoading(false)
    }
  }, [form])
  useEffect(() => {
    alive.current = true
    const draft = guard.current
    load()
    return () => { alive.current = false; draft.invalidate(); reads.current?.abort(); writes.current?.abort() }
  }, [load])
  const save = async () => {
    const operation = guard.current.begin()
    if (!operation) return
    setSaving(true); setSaved(false); setError('')
    const controller = new AbortController(); writes.current = controller
    let sent = false
    try {
      const value = aggregationSettings(await form.validateFields())
      if (!alive.current || !guard.current.current(operation.ticket)) return
      sent = true
      await updateNotificationAggregation(value, { signal: controller.signal, timeout: 15000 })
      if (alive.current && guard.current.current(operation.ticket) && await load()) setSaved(true)
    } catch (err) {
      if (alive.current) setError(sent ? text('保存结果未确认，请重新读取设置后再决定是否重试', 'Save outcome unconfirmed. Reload settings before deciding whether to retry.') : err.message || text('请检查输入', 'Check the inputs'))
    } finally {
      operation.finish(); writes.current = null
      if (alive.current) setSaving(false)
    }
  }
  const counters = notificationCounters(data?.status)
  const degraded = data?.settings?.degraded === true || data?.status?.degraded === true || data?.status?.observer?.degraded === true || data?.status?.aggregation?.degraded === true
  return <Card size="small" title={text('自动通知合并与运行状态', 'Automatic notification aggregation and status')} style={{ marginBottom: 16 }}>
    <Space direction="vertical" style={{ width: '100%' }}>
      <Alert type="info" showIcon message={text('仅合并已订阅的自动通知，不新增订阅或发送测试消息。运行计数仅代表观察、缓冲或任务入队，不证明接收方已收到；关闭页面不会撤销已经提交的保存。', 'Aggregates subscribed automatic notifications only; it does not add subscriptions or send test messages. Counters indicate observation, buffering or durable admission, not recipient delivery. Closing does not undo an already submitted save.')} />
      {error && <Alert type="error" showIcon message={error} />}
      {saved && <Alert type="success" message={text('设置已保存，请以重新读取的值为准', 'Settings saved; the reloaded values are authoritative')} />}
      {degraded && <Alert type="warning" showIcon message={text('通知观察或合并服务已降级，请检查下方计数和服务日志', 'Notification observation or aggregation is degraded. Inspect the counters and server logs.')} />}
      <Form form={form} layout="inline" disabled={loading || saving || !data} onValuesChange={() => { guard.current.invalidate(); setSaved(false) }}>
        <Form.Item name="enabled" label={text('启用合并', 'Enable aggregation')} valuePropName="checked"><Switch /></Form.Item>
        <Form.Item name="windowSeconds" label={text('窗口（秒）', 'Window (seconds)')} rules={[{ required: true }]}><InputNumber min={1} max={3600} /></Form.Item>
        <Form.Item name="threshold" label={text('触发阈值', 'Trigger threshold')} rules={[{ required: true }]}><InputNumber min={1} max={1000} precision={0} /></Form.Item>
      </Form>
      {data?.settings.limits && <Typography.Text type="secondary">
        {text('服务端上限', 'Server limits')}: {notificationCounters(data.settings.limits).map(([key, value]) => `${key}: ${value}`).join(' · ')}
      </Typography.Text>}
      <Space>
        <Button onClick={load} disabled={saving} loading={loading}>{text('重新读取设置和状态', 'Reload settings and status')}</Button>
        <Button type="primary" onClick={save} loading={saving} disabled={loading || !data}>{text('保存', 'Save')}</Button>
      </Space>
      <Descriptions size="small" column={2} items={counters.map(([key, value]) => ({ key, label: key, children: value }))} />
    </Space>
  </Card>
}
