// SPDX-License-Identifier: AGPL-3.0-only
// AniDan's native Go capability panel. Other source views retain Misaka's AGPL UI.
import { Alert, Button, Card, Empty, Form, Input, InputNumber, Modal, Space, Switch, Tag, Typography } from 'antd'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { getScrapers, getScraperLoadCheck, getSingleScraper, setScrapers, setSingleScraper } from '../../../apis'
import { useMessage } from '../../../MessageContext'
import { BiliLogin } from './BiliLogin'

const camel = value => value.replace(/_([a-z])/g, (_, letter) => letter.toUpperCase())

export const NativeScrapers = () => {
  const { t } = useTranslation()
  const message = useMessage()
  const [rows, setRows] = useState([])
  const [capabilities, setCapabilities] = useState([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [selected, setSelected] = useState(null)
  const [configLoading, setConfigLoading] = useState(false)
  const [configError, setConfigError] = useState('')
  const [configReady, setConfigReady] = useState(false)
  const configRequest = useRef(0)
  const [form] = Form.useForm()

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [sources, check] = await Promise.all([getScrapers(), getScraperLoadCheck()])
      if (!Array.isArray(sources.data) || !Array.isArray(check.data?.capabilities)) {
        throw new Error(t('nativeSources.invalidResponse'))
      }
      setRows([...sources.data].sort((a, b) => (a.displayOrder || 0) - (b.displayOrder || 0)))
      setCapabilities(check.data.capabilities)
    } catch (error) {
      setError(error.message || t('common.fetch_failed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { load() }, [load])
  useEffect(() => () => { configRequest.current++ }, [])

  const saveRows = async next => {
    setBusy(true)
    try {
      await setScrapers(next.map(({ providerName, isEnabled, displayOrder, useProxy }) => ({ providerName, isEnabled, displayOrder, useProxy })))
      setRows(next)
      message.success(t('common.save_success'))
    } catch (error) {
      message.error(error.message || t('common.save_failed'))
    } finally {
      setBusy(false)
    }
  }

  const move = (index, offset) => {
    const next = [...rows]
    ;[next[index], next[index + offset]] = [next[index + offset], next[index]]
    saveRows(next.map((row, index) => ({ ...row, displayOrder: index + 1 })))
  }

  const close = () => {
    configRequest.current++
    setSelected(null)
    setConfigError('')
    form.resetFields()
  }

  const configure = async row => {
    const request = ++configRequest.current
    setSelected(row)
    setConfigLoading(true)
    setConfigReady(false)
    setConfigError('')
    form.resetFields()
    try {
      const response = await getSingleScraper({ name: row.providerName })
      if (request !== configRequest.current) return
      const values = { ...response.data }
      Object.keys(row.configurableFields || {}).forEach(key => {
        values[key] = response.data[key] ?? response.data[camel(key)]
      })
      form.setFieldsValue(values)
      setConfigReady(true)
    } catch (error) {
      if (request === configRequest.current) setConfigError(error.message || t('common.fetch_failed'))
    } finally {
      if (request === configRequest.current) setConfigLoading(false)
    }
  }

  const saveConfig = async () => {
    try {
      const values = await form.validateFields()
      setBusy(true)
      // Fields come only from the native provider schema; do not send legacy plugin options.
      await setSingleScraper({ name: selected.providerName, ...values })
      message.success(t('common.save_success'))
      close()
      await load()
    } catch (error) {
      if (!error.errorFields) setConfigError(error.message || t('common.save_failed'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Space direction="vertical" size="middle" className="w-full">
      <Alert type="info" showIcon message={t('nativeSources.title')} description={t('nativeSources.description')} />
      <Card title={t('scrapers.danmakuSearchSource')} loading={loading} extra={<Button onClick={load} disabled={busy}>{t('common.refresh')}</Button>}>
        {error && <Alert className="mb-4" type="error" showIcon message={error} action={<Button onClick={load}>{t('common.retry')}</Button>} />}
        {!loading && !error && rows.length === 0 && <Empty />}
        <Space direction="vertical" size="middle" className="w-full">
          {rows.map((row, index) => {
            const capability = capabilities.find(item => item.name === row.providerName)
            const available = row.available && capability && (capability.search || capability.episodes || capability.comments)
            return (
              <Card key={row.providerName} size="small" data-testid={`source-${row.providerName}`}>
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <Typography.Text strong>{row.displayName || row.providerName}</Typography.Text>
                  <Space wrap>
                    <Button size="small" disabled={busy || index === 0} aria-label={t('nativeSources.moveUp', { name: row.providerName })} onClick={() => move(index, -1)}>↑</Button>
                    <Button size="small" disabled={busy || index === rows.length - 1} aria-label={t('nativeSources.moveDown', { name: row.providerName })} onClick={() => move(index, 1)}>↓</Button>
                    {row.providerName === 'bilibili' && <BiliLogin onChanged={load} />}
                    <Button size="small" disabled={!available || busy} onClick={() => configure(row)}>{t('common.edit')}</Button>
                    <Switch aria-label={t('nativeSources.enable', { name: row.providerName })} checked={row.isEnabled} disabled={busy || (!available && !row.isEnabled)} onChange={isEnabled => saveRows(rows.map(item => item.providerName === row.providerName ? { ...item, isEnabled } : item))} />
                  </Space>
                </div>
                <div className="mt-3">
                  {row.logRawResponses && <Tag color="orange">{t('scrapers.rawResponseEnabled')}</Tag>}
                  {['search', 'episodes', 'comments'].map(key => <Tag key={key} color={capability?.[key] ? 'blue' : 'default'}>{t(`nativeSources.${key}`)}: {t(capability?.[key] ? 'nativeSources.implemented' : 'nativeSources.unavailable')}</Tag>)}
                </div>
                <Typography.Paragraph className="!mb-0 !mt-2" type="secondary">
                  {capability?.status || t('nativeSources.unavailable')}
                  {capability?.blocker && ` · ${capability.blocker}`}
                </Typography.Paragraph>
              </Card>
            )
          })}
        </Space>
      </Card>
      <Modal title={selected ? `${t('common.edit')} · ${selected.providerName}` : ''} open={!!selected} onCancel={close} onOk={saveConfig} confirmLoading={busy} okButtonProps={{ disabled: configLoading || !configReady }} cancelButtonProps={{ disabled: busy }} closable={!busy} maskClosable={!busy} keyboard={!busy} destroyOnHidden>
        {configError && <Alert type="error" showIcon message={configError} action={<Button onClick={() => configure(selected)}>{t('common.retry')}</Button>} />}
        <Form form={form} layout="vertical" disabled={configLoading || busy} preserve={false} className="mt-4">
          {Object.entries(selected?.configurableFields || {}).map(([key, field]) => (
            <Form.Item key={key} name={key} label={field[0] || key} extra={field[2]} rules={field[1] === 'integer' ? [{ required: true }] : []}>
              {field[1] === 'integer' ? <InputNumber precision={0} className="!w-full" /> : field[3]?.secret || /cookie|secret/i.test(key) ? <Input.Password autoComplete="off" /> : <Input autoComplete="off" />}
            </Form.Item>
          ))}
          <Form.Item name="useProxy" valuePropName="checked" label={t('nativeSources.useProxy')}><Switch /></Form.Item>
          {selected?.isLoggable && <Form.Item name="logRawResponses" valuePropName="checked" label={t('scrapers.recordRawResponse')} extra={t('scrapers.rawResponseDesc')}><Switch /></Form.Item>}
          {selected && <Form.Item name={`${selected.providerName}EpisodeBlacklistRegex`} label={t('nativeSources.blacklist')}><Input /></Form.Item>}
        </Form>
      </Modal>
    </Space>
  )
}
