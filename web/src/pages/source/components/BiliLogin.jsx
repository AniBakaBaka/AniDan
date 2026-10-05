// SPDX-License-Identifier: AGPL-3.0-only
import { Alert, Button, Modal, QRCode, Space, Typography } from 'antd'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { biliLogout, executeScraperAction, getbiliLoginQrcode, getbiliUserinfo, pollBiliLogin } from '../../../apis'

export const BiliLogin = ({ onChanged }) => {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [info, setInfo] = useState(null)
  const [qr, setQR] = useState(null)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')
  const sequence = useRef(0)
  const timer = useRef(null)
  const stop = () => { sequence.current++; clearTimeout(timer.current) }
  useEffect(() => () => { sequence.current++; clearTimeout(timer.current) }, [])

  const refresh = async id => {
    try {
      const response = await getbiliUserinfo()
      if (sequence.current === id) setInfo(response.data)
    } catch (error) {
      if (sequence.current === id) setError(error.message || t('nativeBili.failed'))
    }
  }
  const show = () => {
    stop(); setError(''); setStatus(''); setInfo(null); setQR(null); setBusy(false); setOpen(true)
    refresh(sequence.current)
  }
  const close = () => {
    const key = qr?.qrcodeKey
    stop(); setOpen(false); setQR(null); setBusy(false)
    executeScraperAction('bilibili', 'cancel_login', { qrcodeKey: key || '' }).catch(() => {})
  }
  const poll = async (id, key) => {
    if (sequence.current !== id) return
    try {
      const response = await pollBiliLogin({ qrcodeKey: key })
      if (sequence.current !== id) return
      const code = response.data.code
      if (code === 0) {
        setQR(null); setStatus(t('nativeBili.saved')); refresh(id); onChanged?.(); return
      }
      if (code === 86038) { setQR(null); setStatus(t('nativeBili.expired')); return }
      setStatus(t(code === 86090 ? 'nativeBili.confirmMobile' : 'nativeBili.waiting'))
      timer.current = setTimeout(() => poll(id, key), 1500)
    } catch (error) {
      if (sequence.current === id) { setError(error.message || t('nativeBili.failed')); setQR(null) }
    }
  }
  const generate = async () => {
    stop(); const id = sequence.current
    setBusy(true); setQR(null); setStatus(''); setError('')
    try {
      const response = await getbiliLoginQrcode()
      if (sequence.current !== id) return
      setQR(response.data); setStatus(t('nativeBili.waiting'))
      timer.current = setTimeout(() => poll(id, response.data.qrcodeKey), 1500)
    } catch (error) {
      if (sequence.current === id) setError(error.message || t('nativeBili.failed'))
    } finally { if (sequence.current === id) setBusy(false) }
  }
  const logout = () => Modal.confirm({
    title: t('nativeBili.logout'), content: t('nativeBili.logoutConfirm'), okButtonProps: { danger: true },
    onOk: async () => { stop(); await biliLogout(); setInfo({ isLogin: false }); setQR(null); setStatus(''); onChanged?.() },
  })
  return <>
    <Button size="small" onClick={show}>{t('nativeBili.title')}</Button>
    <Modal title={t('nativeBili.title')} open={open} onCancel={close} footer={<Button onClick={close}>{t('common.close')}</Button>} destroyOnHidden>
      <Space direction="vertical" className="w-full" size="middle">
        <Alert type="info" showIcon message={t('nativeBili.notice')} />
        {error && <Alert type="error" showIcon message={error} />}
        {info && <Typography.Text>{info.isLogin ? `${t('nativeBili.loggedIn')}: ${info.uname || ''}` : t('nativeBili.loggedOut')}</Typography.Text>}
        <Space><Button type="primary" loading={busy} onClick={generate}>{t('nativeBili.generate')}</Button><Button onClick={() => refresh(sequence.current)} disabled={busy}>{t('common.refresh')}</Button>{info?.isLogin && <Button danger onClick={logout}>{t('nativeBili.logout')}</Button>}</Space>
        {qr?.url && <QRCode value={qr.url} size={220} />}
        {status && <Typography.Text role="status">{status}</Typography.Text>}
      </Space>
    </Modal>
  </>
}
