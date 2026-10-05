// SPDX-License-Identifier: AGPL-3.0-only
import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Card, Descriptions, Space, Tag } from 'antd'
import { useTranslation } from 'react-i18next'
import { getNotificationProgressStatus } from '../apis'
import { notificationCounters } from '../utils/notificationAggregation'

export default function NotificationProgressPanel() {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const text = (cn, en) => zh ? cn : en
  const [data, setData] = useState(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const request = useRef(null)
  const load = useCallback(async () => {
    request.current?.abort()
    const controller = new AbortController(); request.current = controller
    setLoading(true); setError(''); setData(null)
    try {
      const response = await getNotificationProgressStatus({ signal: controller.signal, timeout: 15000 })
      if (!controller.signal.aborted) setData(response.data)
    } catch (err) {
      if (!controller.signal.aborted) setError(err.message)
    } finally {
      if (!controller.signal.aborted) setLoading(false)
    }
  }, [])
  useEffect(() => { load(); return () => request.current?.abort() }, [load])
  const stats = notificationCounters({ tasks: data?.tasks, destinations: data?.destinations, counters: data?.counters })
  const limits = notificationCounters(data?.limits)
  return <Card size="small" title={text('任务进度通知状态', 'Task progress notification status')} style={{ marginBottom: 16 }} extra={<Button size="small" loading={loading} onClick={load}>{text('刷新快照', 'Refresh snapshot')}</Button>}>
    <Space direction="vertical" style={{ width: '100%' }}>
      <Alert type="info" showIcon message={text('仅 Telegram 支持此任务进度文本消息，标题、标签和任务编号带有生成的格式；需在频道中单独勾选“任务进度通知”，通配订阅不会自动启用。通过编辑同一条消息更新，收件配置变化或结果不确定后不会另发替代消息。互动搜索结果、按钮和图片仍走各自流程。', 'Telegram supports these text task-progress messages with generated heading, label and task-ID formatting. Explicitly select task_progress on a channel; wildcard subscriptions do not enable it. Updates edit the same message; recipient changes or uncertain outcomes never trigger replacement sends. Interactive search results, buttons and images keep their separate workflows.')} />
      <Alert type="info" showIcon message={text('进度更新由独立工作线程处理：暂停通知任务队列不会暂停进度。关闭频道或对应订阅可停止后续更新，已经发出的请求可能仍会完成。', 'Progress uses a separate worker: pausing notification jobs does not pause progress. Disable the channel or corresponding subscription to stop subsequent updates; an in-flight request may still finish.')} />
      <Alert type="info" showIcon message={text('Telegram 且图片模式为 text 时，自动后备搜索/匹配进度使用已有的 fallback_search_complete / match_fallback_complete 单独订阅。通配订阅或仅开启 task_progress 都不会启用后备进度；其他图片模式保留原来的最终通知。', 'For Telegram in text image mode, automatic fallback search/match progress uses the existing explicit fallback_search_complete / match_fallback_complete subscriptions. Wildcard subscriptions or task_progress alone do not enable fallback progress; other image modes retain their existing final notifications.')} />
      {error && <Alert type="error" showIcon message={error} />}
      {data && <>
        <Space wrap>
          <Tag color={data.closed ? 'default' : 'blue'}>{data.closed ? text('已关闭', 'Closed') : data.active ? text('正在处理', 'Processing') : text('等待任务', 'Idle')}</Tag>
          {(data.supportedChannels || []).filter(value => value === 'telegram').map(value => <Tag key={value}>Telegram</Tag>)}
        </Space>
        {data.degraded && <Alert type="warning" showIcon message={text('进度通知存在丢弃、过期或未确认记录，请检查计数和日志；不要通过重发来猜测送达结果', 'Progress has dropped, expired or unconfirmed records. Inspect counters and logs; do not infer delivery by resending.')} />}
        <Descriptions size="small" column={2} items={stats.map(([key, value]) => ({ key, label: key, children: value }))} />
        <span>{text('运行上限', 'Runtime limits')}: {limits.map(([key, value]) => `${key}: ${value}`).join(' · ')}</span>
        <span>{text('sendAccepted / editAccepted 只表示接口确认，不代表接收方已收到或阅读', 'sendAccepted / editAccepted indicate API acknowledgement only, not recipient delivery or reading')}</span>
      </>}
    </Space>
  </Card>
}
