// SPDX-License-Identifier: AGPL-3.0-only
import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Card, Descriptions, Space, Tag } from 'antd'
import { useTranslation } from 'react-i18next'
import { getPredownloadStatus } from '../apis'

export default function PredownloadStatusPanel({ revision = 0 }) {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const text = (cn, en) => zh ? cn : en
  const [data, setData] = useState(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const current = useRef(null)
  const read = useCallback(async () => {
    current.current?.abort()
    const controller = new AbortController(); current.current = controller
    setLoading(true); setError(''); setData(null)
    try {
      const response = await getPredownloadStatus({ signal: controller.signal, timeout: 15000 })
      if (!controller.signal.aborted) setData(response.data)
    } catch (err) {
      if (!controller.signal.aborted) setError(err.message)
    } finally {
      if (!controller.signal.aborted) setLoading(false)
    }
  }, [])
  useEffect(() => {
    read()
    window.addEventListener('focus', read)
    return () => { current.current?.abort(); window.removeEventListener('focus', read) }
  }, [read, revision])
  const value = item => typeof item === 'number' && Number.isFinite(item) ? item : '—'
  const bytes = item => typeof item === 'number' && Number.isFinite(item) ? `${(item / 1048576).toFixed(2)} MiB` : '—'
  const limits = data?.limits || {}
  const budget = data?.budget || {}
  const counters = data?.counters || {}
  return <Card size="small" title={text('下一集弹幕预下载状态', 'Next-episode comment predownload status')} style={{ marginBottom: 16 }} extra={<Button size="small" onClick={read} loading={loading}>{text('刷新快照', 'Refresh snapshot')}</Button>}>
    <Space direction="vertical" style={{ width: '100%' }}>
      <Alert type="info" showIcon message={text('只预下载弹幕，不下载视频。需开启下方开关及至少一种后备功能；仅在当前集有弹幕、源未完结、下一集可精确匹配时尝试。达到预算会跳过，不自动清除已有弹幕。', 'Downloads comments only, never video. Requires opt-in and a fallback mode. Attempts only after a nonempty current episode, for an unfinished source and an exact next-episode match. Budget limits skip speculative work rather than delete existing comments.')} />
      {error && <Alert type="error" showIcon message={error} />}
      {data && <>
        <Space wrap><Tag color={data.effectiveEnabled ? 'green' : 'default'}>{data.effectiveEnabled ? text('当前有效', 'Currently effective') : text('当前未启用', 'Currently inactive')}</Tag>
          <span>{text('配置开关', 'Opt-in')}: {data.configuredEnabled ? text('开', 'On') : text('关', 'Off')} · {text('后备功能', 'Fallback')}: {data.fallbackEnabled ? text('开', 'On') : text('关', 'Off')}</span></Space>
        {(data.degraded || budget.degraded || budget.unknownEntries > 0) && <Alert type="warning" showIcon message={text('配置或存储预算存在异常；不确定的预留仍计入预算，请检查服务日志，不要直接删除预算记录', 'Configuration or storage accounting is degraded. Uncertain reservations remain charged; inspect server logs instead of deleting budget records.')} />}
        <Descriptions size="small" column={2} items={[
          { key: 'concurrency', label: text('预下载并发上限', 'Prefetch concurrency cap'), children: value(limits.concurrency) },
          { key: 'queue', label: text('意向队列上限', 'Intent queue cap'), children: value(limits.intentQueue) },
          { key: 'pools', label: text('未播放池 / 上限', 'Unplayed pools / cap'), children: `${value(budget.count)} / ${value(limits.maxUnplayedPools)}` },
          { key: 'total', label: text('已占用预算 / 上限', 'Charged bytes / cap'), children: `${bytes(budget.totalBytes)} / ${bytes(limits.maxUnplayedBytes)}` },
          { key: 'reserved', label: text('预留池 / 字节', 'Reserved pools / bytes'), children: `${value(budget.reservedCount)} / ${bytes(budget.reservedBytes)}` },
          { key: 'published', label: text('已写入池 / 字节', 'Published pools / bytes'), children: `${value(budget.publishedCount)} / ${bytes(budget.publishedBytes)}` },
          { key: 'episode', label: text('单集 XML 上限', 'Per-episode XML cap'), children: bytes(limits.maxEpisodeBytes) },
          { key: 'reservation', label: text('每次下载预留', 'Reservation per download'), children: bytes(limits.reservationBytes) },
          { key: 'multiplier', label: text('已写入池计入倍数', 'Published-pool charge multiplier'), children: value(limits.publishedChargeMultiplier) },
          { key: 'unknown', label: text('待检查预算记录', 'Uncertain budget entries'), children: value(budget.unknownEntries) },
          ...['observed', 'dropped', 'busy', 'admitted', 'errors'].map(key => ({ key, label: key, children: value(counters[key]) })),
        ]} />
        <span>{text('计数仅是触发与入队状态，不等于下载成功；预算统计不代表整个弹幕库占用', 'Counters describe triggers/admission, not successful downloads; accounting does not represent the entire library size')}</span>
      </>}
    </Space>
  </Card>
}
