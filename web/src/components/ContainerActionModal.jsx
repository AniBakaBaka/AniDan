import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { useLocation } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Alert, Button, Checkbox, Descriptions, Modal, Progress, Select, Space, Spin, Typography } from 'antd'
import Cookies from 'js-cookie'
import { confirmationExpired, createContainerAPI } from '../apis/container.js'
import { createContainerSession } from '../utils/containerSession.js'

const api = createContainerAPI({ getToken: () => Cookies.get('danmu_token') })
const knownErrors = new Set(['authenticationRequired', 'invalidConfirmation', 'requestRejected', 'invalidStream', 'interrupted', 'invalidResult', 'operationFailed', 'expired'])

export default function ContainerActionModal({ open, action = 'restart', onClose }) {
  const { t } = useTranslation('nativeContainer')
  const location = useLocation()
  const imageLabelId = useId()
  const [state, setState] = useState({ phase: 'idle', logs: [] })
  const [image, setImage] = useState(undefined)
  const [acknowledged, setAcknowledged] = useState(false)
  const [now, setNow] = useState(Date.now())
  const session = useMemo(() => createContainerSession(api, setState), [])

  const previousLocation = useRef(location.key)
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  useEffect(() => {
    if (previousLocation.current !== location.key) {
      previousLocation.current = location.key
      session.cancel()
      closeRef.current()
    }
  }, [location.key, session])

  useEffect(() => {
    if (open) {
      setImage(undefined)
      setAcknowledged(false)
      session.load(action)
    }
    return () => session.dispose()
  }, [open, action, session])
  // Reset acknowledgement on every newly prepared or canceled review.
  useEffect(() => { setAcknowledged(false) }, [state.review])
  useEffect(() => {
    if (state.phase !== 'review') return
    setNow(Date.now())
    const timer = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(timer)
  }, [state.phase])
  useEffect(() => {
    if (state.phase !== 'running') return
    const warn = event => { event.preventDefault(); event.returnValue = '' }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [state.phase])

  const busy = ['loading', 'preparing', 'running'].includes(state.phase)
  const running = state.phase === 'running'
  const expired = state.phase === 'expired' || state.phase === 'review' && confirmationExpired(state.review, now)
  const allowed = state.status?.enabled === true && state.status?.[action === 'update' ? 'canUpdate' : 'canRestart'] === true
  const images = Array.isArray(state.status?.allowedImages) ? state.status.allowedImages.filter(value => typeof value === 'string') : []
  const close = () => { session.cancel(); onClose() }
  const details = state.review

  return (
    <Modal title={`${t('title')}: ${t(action)}`} open={open} onCancel={close} footer={null}
      closable={!running} maskClosable={!busy} keyboard={!busy} width={680}>
      <Space direction="vertical" size="middle" style={{ width: '100%' }}>
        <Alert type="info" showIcon message={t('policy')} />
        {state.phase === 'loading' && <Spin />}
        {state.status && <>
          <Typography.Text>{state.status.message}</Typography.Text>
          <Typography.Paragraph style={{ overflowWrap: 'anywhere' }}>{t('target')}: {state.status.containerId || '—'}</Typography.Paragraph>
          {!allowed && <Alert type="warning" showIcon message={t('disabled')} />}
        </>}
        {state.phase === 'selecting' && allowed && <>
          {action === 'update' && <>
            <Typography.Text id={imageLabelId}>{t('selectImage')}</Typography.Text>
            <Select aria-labelledby={imageLabelId} value={image} onChange={setImage}
              placeholder={t('selectImage')} style={{ width: '100%' }} options={images.map(value => ({ label: value, value }))}
              popupMatchSelectWidth disabled={!images.length} />
            {!images.length && <Alert type="warning" message={t('noImages')} />}
          </>}
          <Button type="primary" onClick={() => session.prepare(action === 'update' ? image : '')}
            disabled={action === 'update' && !images.includes(image)}>{t('prepare')}</Button>
        </>}
        {state.phase === 'preparing' && <Spin tip={t('preparing')}><div style={{ minHeight: 50 }} /></Spin>}
        {details && <>
          <Typography.Title level={5}>{t('review')}</Typography.Title>
          <Descriptions bordered size="small" column={1} styles={{ content: { overflowWrap: 'anywhere' } }} items={[
            { key: 'action', label: t('action'), children: details.action },
            { key: 'target', label: t('target'), children: details.containerId },
            { key: 'image', label: t('image'), children: details.image || t('none') },
            { key: 'expires', label: t('expires'), children: details.expiresAt },
          ]} />
          <Alert type="warning" showIcon message={details.warning} />
        </>}
        {state.phase === 'review' && !expired && <>
          <Checkbox checked={acknowledged} onChange={event => setAcknowledged(event.target.checked)}>{t('acknowledge')}</Checkbox>
          <Button danger type="primary" disabled={!acknowledged} onClick={() => session.execute(acknowledged)}>{t('execute')}</Button>
          <Button onClick={() => session.back()}>{t('back')}</Button>
        </>}
        {expired && <Alert type="warning" showIcon message={t('expired')} />}
        {running && <>
          <Alert type="warning" showIcon message={t('running')} description={t('watching')} />
          {action === 'update' && <Progress percent={state.progress} status="active" />}
          <Button onClick={() => session.cancel()}>{t('stop')}</Button>
        </>}
        {!!state.logs.length && <div role="log" aria-live="polite" style={{ maxHeight: 200, overflowY: 'auto', overflowWrap: 'anywhere' }}>
          {state.logs.map((line, index) => <div key={index}>{line}</div>)}
        </div>}
        {['failed', 'uncertain'].includes(state.phase) && <Alert type={state.phase === 'uncertain' ? 'warning' : 'error'} showIcon
          message={t(state.phase)} description={<>
            <p>{t(knownErrors.has(state.error?.code) ? state.error.code : 'unknownError', { status: state.error?.status })}</p>
            {state.error?.detail && <p>{state.error.detail}</p>}
            <p>{t('inspect')}</p>
          </>} />}
        {state.phase === 'done' && <>
          <Alert type="success" showIcon message={t(action === 'restart' ? 'restartAccepted' : 'updateDone')}
            description={t(action === 'restart' ? 'restartHealth' : 'updateNext')} />
          {action === 'update' && <Descriptions bordered column={1} styles={{ content: { overflowWrap: 'anywhere' } }} items={[
            { key: 'new', label: t('newTarget'), children: state.result.containerId },
            { key: 'old', label: t('rollback'), children: state.result.rollbackContainerId },
          ]} />}
        </>}
        {!running && <Button onClick={close}>{t(busy || state.phase === 'review' ? 'cancel' : 'close')}</Button>}
      </Space>
    </Modal>
  )
}
