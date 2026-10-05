// SPDX-License-Identifier: AGPL-3.0-only
import { useEffect, useRef, useState } from 'react'
import { Alert, Button, Checkbox, Form, Input, InputNumber, Modal, Select, Space } from 'antd'
import { useTranslation } from 'react-i18next'
import { previewURLImport, submitURLImport } from '../apis/urlImport.js'

export function URLImportModal({ open, onCancel, onTasks }) {
  const { i18n } = useTranslation()
  const text = (cn, en) => i18n.language.startsWith('zh') ? cn : en
  const [url, setURL] = useState('')
  const [preview, setPreview] = useState(null)
  const [wholeCollection, setWholeCollection] = useState(false)
  const [title, setTitle] = useState('')
  const [type, setType] = useState('tv_series')
  const [season, setSeason] = useState(1)
  const [checked, setChecked] = useState(false)
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [uncertain, setUncertain] = useState(false)
  const [task, setTask] = useState('')
  const read = useRef(null)
  const mutation = useRef(false)
  const draft = useRef({ url: '', title: false, type: false, season: false })
  const mounted = useRef(false)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false; read.current?.abort() }
  }, [])
  const invalidate = value => {
    if (value.trim() !== draft.current.url) draft.current = { url: value.trim(), title: false, type: false, season: false }
    read.current?.abort(); setLoading(false); setURL(value); setPreview(null); setWholeCollection(false); setChecked(false); setError('')
  }
  const inspect = async () => {
    if (mutation.current || uncertain || task || !url.trim()) return
    read.current?.abort()
    const controller = new AbortController(); read.current = controller
    setLoading(true); setPreview(null); setWholeCollection(false); setChecked(false); setError('')
    const requested = url.trim()
    try {
      const data = await previewURLImport(requested, { signal: controller.signal, timeout: 60000 })
      if (mounted.current && !controller.signal.aborted) {
        setPreview({ ...data, url: requested })
        if (!draft.current.title) setTitle(data.title)
        if (!draft.current.type) setType(data.mediaType)
        if (!draft.current.season) setSeason(1)
      }
    } catch (err) {
      if (mounted.current && !controller.signal.aborted) setError(err.message)
    } finally {
      if (mounted.current && !controller.signal.aborted) setLoading(false)
    }
  }
  const submit = async () => {
    if (mutation.current || loading || uncertain || task || !checked || !preview || (wholeCollection && !preview.collection) || preview.url !== url.trim() || !title.trim() || !Number.isSafeInteger(season) || season < 0) return
    mutation.current = true; setSaving(true); setError('')
    try {
      const taskId = await submitURLImport({ url: preview.url, provider: preview.provider, title: title.trim(), media_type: wholeCollection ? 'tv_series' : type, season, ...(wholeCollection ? { import_mode: 'collection', collection_season_id: preview.collection.seasonId, collection_mid: preview.collection.mid } : {}) }, { timeout: 120000 })
      if (mounted.current) { setTask(taskId); setChecked(false) }
    } catch (err) {
      if (mounted.current) { setError(err.message); setUncertain(true); setChecked(false) }
    } finally {
      mutation.current = false
      if (mounted.current) setSaving(false)
    }
  }
  const close = () => {
    if (mutation.current) return
    read.current?.abort(); setLoading(false); onCancel()
  }
  return <Modal title={text('链接导入弹幕', 'Import danmaku from URL')} open={open} onCancel={close}
    closable={!saving} maskClosable={!saving} keyboard={!saving} footer={null}>
    <Space direction="vertical" style={{ width: '100%' }}>
      <Form layout="vertical">
        <Form.Item label={text('视频或作品链接', 'Video or series URL')}>
          <Input value={url} maxLength={4096} disabled={saving || uncertain || !!task} onChange={event => invalidate(event.target.value)} onPressEnter={inspect} />
        </Form.Item>
        <Button onClick={inspect} loading={loading} disabled={saving || uncertain || !!task || !url.trim()}>{text('解析链接', 'Preview URL')}</Button>
        {preview && <>
          {preview.collection && <Form.Item label={text('导入范围', 'Import scope')}>
            <Select value={wholeCollection ? 'collection' : 'current'} disabled={saving || uncertain || !!task} onChange={value => {
              const whole = value === 'collection'; setWholeCollection(whole); setChecked(false)
              if (!draft.current.title) setTitle(whole ? preview.collection.title : preview.title)
            }} options={[
              { value: 'current', label: text('当前链接', 'Current URL') },
              { value: 'collection', label: text('整个合集：', 'Whole collection: ') + preview.collection.title + ' (' + preview.collection.total + ')' },
            ]} />
          </Form.Item>}
          {preview.collectionError && <Alert type="warning" showIcon message={text('合集信息未确认，仍可按当前链接范围导入', 'Collection discovery is unconfirmed; current URL import remains available')} description={preview.collectionError} />}
          {wholeCollection && <Alert type="warning" showIcon message={text('将导入整个合集，每个视频作为一集，使用源站主 CID，不展开所有分 P', 'Import the entire collection: one episode per video using its primary CID, not every multipart page')} description={text('合集导入按剧集类型保存；这可能产生大量下载请求', 'Collections are stored as series and may make many download requests')} />}
          {!wholeCollection && <Alert style={{ margin: '12px 0' }} type={preview.importScope === 'media' ? 'warning' : 'info'} showIcon
            message={preview.importScope === 'episode' ? text('导入此链接对应的单集', 'Import the exact linked episode') : text('导入整部作品的分集，可能产生大量下载请求', 'Import the media episode list; this may make many download requests')}
            description={text('来源：', 'Source: ') + preview.provider} />}
          <Form.Item label={text('作品标题', 'Media title')}><Input value={title} maxLength={4096} disabled={saving || uncertain || !!task} onChange={event => { draft.current.title = true; setTitle(event.target.value); setChecked(false) }} /></Form.Item>
          <Form.Item label={text('类型', 'Type')}><Select value={wholeCollection ? 'tv_series' : type} disabled={saving || uncertain || !!task || wholeCollection} onChange={value => { draft.current.type = true; setType(value); setChecked(false) }} options={[
            { value: 'tv_series', label: text('剧集 / 动漫 / 综艺', 'Series / animation / variety') },
            { value: 'movie', label: text('电影', 'Movie') }, { value: 'other', label: text('其他', 'Other') },
          ]} /></Form.Item>
          <Form.Item label={text('季数（默认 1；0 表示特别篇）', 'Season (default 1; 0 means specials)')}><InputNumber value={season} min={0} precision={0} disabled={saving || uncertain || !!task} onChange={value => { draft.current.season = true; setSeason(value); setChecked(false) }} /></Form.Item>
          <Checkbox checked={checked} disabled={saving || uncertain || !!task} onChange={event => setChecked(event.target.checked)}>{text('已确认上述导入范围及信息', 'I have reviewed the import scope and metadata')}</Checkbox>
        </>}
      </Form>
      {error && <Alert type="error" showIcon message={error} />}
      {uncertain && <Alert type="warning" showIcon message={text('提交结果未确认，请先查看任务列表，避免重复导入。不会自动重试；关闭窗口不会取消已提交任务。', 'Submission is unconfirmed. Check the task list before another import. No automatic retry; closing does not cancel a submitted task.')} />}
      {task && <Alert type="success" showIcon message={text('任务已提交，尚不代表导入完成', 'Task accepted; import is not yet complete')} description={<span style={{ overflowWrap: 'anywhere' }}>{task}</span>} />}
      <Space wrap>
        {!task && !uncertain && <Button type="primary" loading={saving} onClick={submit} disabled={loading || !checked || !preview || !title.trim() || !Number.isSafeInteger(season) || season < 0}>{text('提交导入任务', 'Submit import task')}</Button>}
        {(task || uncertain) && <Button onClick={onTasks}>{text('查看任务', 'View tasks')}</Button>}
        <Button onClick={close} disabled={saving}>{text('关闭', 'Close')}</Button>
      </Space>
    </Space>
  </Modal>
}
