// SPDX-License-Identifier: AGPL-3.0-only
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Alert, Button, Card, Modal, Popconfirm, Select, Space, Table, Typography } from 'antd'
import { useTranslation } from 'react-i18next'
import Cookies from 'js-cookie'
import { createResponseCacheAPI } from '../apis/responseCache'

const regions = ['provider_search', 'provider_episodes', 'metadata_search', 'metadata_details', 'calendar']
export default function ResponseCachePanel() {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const text = (cn, en) => zh ? cn : en
  const api = useMemo(() => createResponseCacheAPI({ getToken: () => Cookies.get('danmu_token') }), [])
  const [region, setRegion] = useState('')
  const [cursors, setCursors] = useState([''])
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [detail, setDetail] = useState(null)
  const [detailError, setDetailError] = useState('')
  const [detailLoading, setDetailLoading] = useState(false)
  const reads = useRef(null)
  const details = useRef(null)
  const mutation = useRef(null)
  const alive = useRef(false)
  const cursor = cursors[cursors.length - 1]
  const load = useCallback(async () => {
    reads.current?.abort()
    const controller = new AbortController()
    reads.current = controller
    setLoading(true); setError(''); setData(null)
    try {
      const [stats, page] = await Promise.all([api.stats(controller.signal), api.list(region, cursor, controller.signal)])
      if (!controller.signal.aborted && alive.current) setData({ stats, page })
    } catch (err) {
      if (!controller.signal.aborted && alive.current) setError(err.message)
    } finally {
      if (!controller.signal.aborted && alive.current) setLoading(false)
    }
  }, [api, region, cursor])
  useEffect(() => {
    alive.current = true
    return () => { alive.current = false; reads.current?.abort(); details.current?.abort(); mutation.current?.abort() }
  }, [])
  useEffect(() => { load(); return () => reads.current?.abort() }, [load])
  const closeDetail = () => { details.current?.abort(); setDetail(null); setDetailError(''); setDetailLoading(false) }
  const view = async key => {
    details.current?.abort()
    const controller = new AbortController(); details.current = controller
    setDetail({ key }); setDetailError(''); setDetailLoading(true)
    try {
      const value = await api.detail(key, controller.signal)
      if (!controller.signal.aborted && alive.current) setDetail(value)
    } catch (err) {
      if (!controller.signal.aborted && alive.current) setDetailError(err.message)
    } finally {
      if (!controller.signal.aborted && alive.current) setDetailLoading(false)
    }
  }
  const mutate = async key => {
    if (mutation.current) return
    const controller = new AbortController(); mutation.current = controller
    reads.current?.abort(); closeDetail(); setBusy(true); setError('')
    try {
      if (key) await api.delete(key, controller.signal)
      else await api.clear(region, controller.signal)
      if (alive.current && !controller.signal.aborted) {
        if (cursors.length > 1) setCursors([''])
        else await load()
      }
    } catch (err) {
      if (alive.current) setError(`${text('操作结果未确认，请刷新检查后再决定是否重试', 'Outcome unconfirmed. Refresh and inspect before retrying')}: ${err.message}`)
    } finally {
      mutation.current = null
      if (alive.current) { setBusy(false); setLoading(false) }
    }
  }
  const health = data?.stats.health
  const columns = [
    { title: text('区域', 'Region'), dataIndex: 'region', width: 150 },
    { title: text('键（不含原始请求）', 'Opaque key'), dataIndex: 'key', ellipsis: true },
    { title: 'Bytes', dataIndex: 'bytes', width: 85 },
    { title: text('到期时间', 'Expires'), dataIndex: 'expiresAt', width: 180 },
    { title: text('操作', 'Actions'), key: 'actions', width: 160, render: (_, row) => <Space>
      <Button size="small" disabled={busy} onClick={() => view(row.key)}>{text('详情', 'Detail')}</Button>
      <Popconfirm title={text('删除这条响应缓存？', 'Delete this response cache entry?')} onConfirm={() => mutate(row.key)}>
        <Button size="small" danger disabled={busy}>{text('删除', 'Delete')}</Button>
      </Popconfirm>
    </Space> },
  ]
  return <Card size="small" title={text('原生响应缓存后端', 'Native response cache backend')} style={{ marginBottom: 16 }}>
    <Space direction="vertical" style={{ width: '100%' }}>
      <Alert type="info" showIcon message={text('仅管理配置的响应缓存命名空间；下方旧 SQL 诊断不代表 Redis 或进程内缓存总量。关闭窗口不会撤销已发送的删除操作。', 'Manages only the configured response-cache namespace. Legacy SQL diagnostics below do not represent Redis or in-process totals. Closing does not undo a submitted deletion.')} />
      {error && <Alert type="error" showIcon message={error} />}
      {health && <>
        <Typography.Text>{text('配置 / 实际后端', 'Configured / effective')}: {health.configured} / {health.effective} · {text('命名空间', 'Namespace')}: {data.stats.namespace}</Typography.Text>
        {(health.degraded || health.closed) && <Alert type="warning" showIcon message={health.reason || text('缓存不可用或已降级', 'Cache is unavailable or degraded')} />}
        <Typography.Text>{text('整个命名空间条目', 'Whole-namespace entries')}: {data.stats.stats.entries} · {text('整个命名空间字节', 'Whole-namespace bytes')}: {data.stats.stats.bytes}</Typography.Text>
      </>}
      <Space wrap>
        <Select aria-label={text('响应缓存区域', 'Response cache region')} value={region} disabled={busy} style={{ width: 200 }} onChange={value => { closeDetail(); setRegion(value); setCursors(['']) }} options={[{ value: '', label: text('全部区域', 'All regions') }, ...regions.map(value => ({ value, label: value }))]} />
        <Button onClick={load} disabled={busy} loading={loading}>{text('刷新', 'Refresh')}</Button>
        <Popconfirm title={text('清除此命名空间内所选区域的响应缓存？', 'Clear response cache for the selected region in this namespace?')} onConfirm={() => mutate()}>
          <Button danger loading={busy} disabled={loading || !data}>{text('清除所选区域', 'Clear selected region')}</Button>
        </Popconfirm>
      </Space>
      <Table columns={columns} dataSource={data?.page.items || []} rowKey="key" pagination={false} loading={loading} size="small" scroll={{ x: 760 }} />
      <Space>
        <Button disabled={busy || loading || cursors.length < 2} onClick={() => setCursors(value => value.slice(0, -1))}>{text('上一页', 'Previous')}</Button>
        <span>{cursors.length}</span>
        <Button disabled={busy || loading || !data?.page.next} onClick={() => setCursors(value => [...value, data.page.next])}>{text('下一页', 'Next')}</Button>
      </Space>
    </Space>
    <Modal title={text('响应缓存详情', 'Response cache detail')} open={detail !== null} onCancel={closeDetail} footer={null} width={700}>
      {detailLoading ? text('读取中…', 'Loading…') : detailError ? <Alert type="error" message={detailError} /> : <pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', maxHeight: 400, overflow: 'auto' }}>{detail?.rawJSON || JSON.stringify(detail, null, 2)}</pre>}
    </Modal>
  </Card>
}
