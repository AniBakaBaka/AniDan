import { parseAPIJSON } from '../utils/idTransport'
import { useState, useEffect, useRef, useCallback } from 'react'
import { useTranslation } from 'react-i18next'
import { Modal, Button, Tag, Spin, Badge, Typography, Divider, Alert, Card, Progress, Row, Col, Statistic } from 'antd'
import { SyncOutlined, RocketOutlined, CheckCircleOutlined, CloseCircleOutlined, HistoryOutlined, CloudServerOutlined } from '@ant-design/icons'
import { checkAppUpdate } from '../apis'
import { fetchEventSource } from '@microsoft/fetch-event-source'
import Cookies from 'js-cookie'
import ReleaseHistoryModal from './ReleaseHistoryModal'
import ContainerActionModal from './ContainerActionModal'
import { createContainerAPI } from '../apis/container.js'

import ReactMarkdown from 'react-markdown'
import { useAtomValue } from 'jotai'
import { isMobileAtom } from '../../store'

const containerAPI = createContainerAPI({ getToken: () => Cookies.get('danmu_token') })
const { Text, Title } = Typography

/**
 * 预处理 GitHub Release 的 changelog 文本，使 ReactMarkdown 能正确渲染。
 * 仅统一换行符为 \n，不做额外的换行替换，以保留 Markdown 列表等结构的正确解析。
 */
const preprocessChangelog = (text) => {
  if (!text) return text
  return text.replace(/\r\n/g, '\n')
}

// Markdown 渲染样式
const markdownComponents = {
  a: ({ href, children }) => (
    <a href={href} target="_blank" rel="noopener noreferrer" style={{ color: 'var(--color-primary)' }} className="hover:underline">
      {children}
    </a>
  ),
  p: ({ children }) => <p className="my-1">{children}</p>,
  ul: ({ children }) => <ul className="list-disc list-inside my-1 space-y-0.5">{children}</ul>,
  ol: ({ children }) => <ol className="list-decimal list-inside my-1 space-y-0.5">{children}</ol>,
  li: ({ children }) => <li className="ml-2">{children}</li>,
  code: ({ children }) => (
    <code style={{ backgroundColor: 'var(--color-hover)' }} className="px-1 py-0.5 rounded text-sm font-mono">{children}</code>
  ),
  blockquote: ({ children }) => (
    <blockquote style={{ borderColor: 'var(--color-primary)', backgroundColor: 'var(--color-hover)' }} className="border-l-4 pl-3 py-1 my-2 rounded-r text-sm">
      {children}
    </blockquote>
  ),
  strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
}

export const VersionModal = ({ open, onClose, currentVersion }) => {
  const { t } = useTranslation()
  const isMobile = useAtomValue(isMobileAtom)
  const [loading, setLoading] = useState(false)
  const [updateInfo, setUpdateInfo] = useState(null)
  const [dockerStatus, setDockerStatus] = useState(null)
  const [dockerStats, setDockerStats] = useState(null)
  const [releaseHistoryOpen, setReleaseHistoryOpen] = useState(false)
  const [containerAction, setContainerAction] = useState(null)
  const [loadErrors, setLoadErrors] = useState([])
  const { t: nativeT } = useTranslation('nativeContainer')
  const loadGeneration = useRef(0)
  const loadController = useRef(null)
  const statsAbortController = useRef(null)

  const stopStatsSSE = useCallback(() => {
    statsAbortController.current?.abort()
    statsAbortController.current = null
  }, [])

  const startStatsSSE = useCallback((generation) => {
    const token = Cookies.get('danmu_token')
    if (!token) return
    stopStatsSSE()
    const controller = new AbortController()
    statsAbortController.current = controller
    const current = () => !controller.signal.aborted && generation === loadGeneration.current
    const unavailable = () => { if (current()) setDockerStats({ available: false }) }
    fetchEventSource('/api/ui/docker/stats', {
      signal: controller.signal,
      openWhenHidden: true,
      headers: { Authorization: `Bearer ${token}` },
      onopen: async response => {
        if (!response.ok || !response.headers.get('content-type')?.includes('text/event-stream')) throw new Error('Statistics unavailable')
      },
      onmessage: event => {
        if (!current()) return
        const data = parseAPIJSON(event.data)
        if (typeof data.available !== 'boolean') throw new Error('Invalid statistics')
        setDockerStats(data)
      },
      onclose: unavailable,
      onerror: error => { unavailable(); throw error },
    }).catch(unavailable)
  }, [stopStatsSSE])

  const loadData = useCallback(async () => {
    const generation = ++loadGeneration.current
    loadController.current?.abort()
    const controller = new AbortController()
    loadController.current = controller
    stopStatsSSE()
    setDockerStats(null)
    setLoading(true)
    setLoadErrors([])
    const [release, docker] = await Promise.allSettled([checkAppUpdate(), containerAPI.status(controller.signal)])
    if (generation !== loadGeneration.current || controller.signal.aborted) return
    setUpdateInfo(release.status === 'fulfilled' ? release.value.data : null)
    setDockerStatus(docker.status === 'fulfilled' ? docker.value : null)
    setLoadErrors([...(release.status === 'rejected' ? ['releaseError'] : []), ...(docker.status === 'rejected' ? ['statusError'] : [])])
    if (docker.status === 'fulfilled' && docker.value.enabled && docker.value.socketAvailable) startStatsSSE(generation)
    setLoading(false)
  }, [startStatsSSE, stopStatsSSE])

  useEffect(() => {
    if (open) loadData()
    else { setContainerAction(null); setReleaseHistoryOpen(false) }
    return () => {
      loadGeneration.current += 1
      loadController.current?.abort()
      stopStatsSSE()
    }
  }, [open, loadData, stopStatsSSE])

  // 渲染更新日志
  const renderChangelog = () => {
    if (!updateInfo?.changelog) return null

    return (
      <div className={isMobile ? 'flex-1 min-h-0 overflow-y-auto rounded-lg p-4 mt-2' : 'max-h-[300px] overflow-y-auto rounded-lg p-4 mt-4'} style={{ backgroundColor: 'var(--color-hover)' }}>
        <Title level={5}>{t('versionModal.changelog')}</Title>
        <div className="text-sm">
          <ReactMarkdown components={markdownComponents}>
            {preprocessChangelog(updateInfo.changelog)}
          </ReactMarkdown>
        </div>
      </div>
    )
  }

  return (
    <Modal
      title={t('versionModal.title')}
      open={open}
      onCancel={onClose}
      footer={null}
      width={isMobile ? '95%' : 600}
      styles={{ body: { maxHeight: isMobile ? 'calc(100vh - 120px)' : 'none', overflow: isMobile ? 'auto' : 'visible', display: 'flex', flexDirection: 'column' } }}
    >
      <Spin spinning={loading}>
        <div className={isMobile ? 'flex flex-col' : 'space-y-4'} style={isMobile ? { maxHeight: 'calc(100vh - 160px)' } : {}}>
          {/* 当前版本 */}
          <div className="flex items-center justify-between">
            <Text>{t('versionModal.currentVersion')}</Text>
            <Tag color="blue">{currentVersion}</Tag>
          </div>

          {/* 最新版本 */}
          {updateInfo && (
            <div className="flex items-center justify-between">
              <Text>{t('versionModal.latestVersion')}</Text>
              <div className="flex items-center gap-2">
                {updateInfo.hasUpdate ? (
                  <Tag color="green">{updateInfo.latestVersion}</Tag>
                ) : (
                  <Tag>{updateInfo.latestVersion || '—'}</Tag>
                )}
                {updateInfo.hasUpdate && <Badge status="processing" text={t('versionModal.hasNewVersion')} />}
              </div>
            </div>
          )}

          {/* Docker 状态 */}
          <Divider />
          <div className="flex items-center justify-between">
            <Text>{t('versionModal.dockerStatus')}</Text>
            {dockerStatus?.socketAvailable ? (
              <Tag icon={<CheckCircleOutlined />} color="success">{t('versionModal.connected')}</Tag>
            ) : (
              <Tag icon={<CloseCircleOutlined />} color="default">{t('versionModal.disconnected')}</Tag>
            )}
          </div>

          {loadErrors.map(error => <Alert key={error} type="error" showIcon message={nativeT(error)} />)}
          {dockerStatus && <Alert type={dockerStatus.enabled ? 'info' : 'warning'} showIcon
            message={dockerStatus.message} description={nativeT('policy')} />}
          {updateInfo?.configured === false && <Alert type="info" showIcon message={nativeT('noRelease')} />}
          {dockerStats?.available === false && <Alert type="info" showIcon message={nativeT('statsEnded')} />}

          {/* 容器资源统计卡片 */}
          {dockerStats?.available && (
            <Card
              size="small"
              className="!mt-4"
              title={
                <div className="flex items-center gap-2">
                  <CloudServerOutlined />
                  <span>{dockerStats.containerName || t('versionModal.containerStatus')}</span>
                  <Tag color={dockerStats.status === 'running' ? 'success' : 'warning'} className="!ml-2">
                    {{ running: t('versionModal.statusRunning'), exited: t('versionModal.statusExited'), paused: t('versionModal.statusPaused'), restarting: t('versionModal.statusRestarting'), created: t('versionModal.statusCreated'), dead: t('versionModal.statusDead') }[dockerStats.status] || dockerStats.status}
                  </Tag>
                </div>
              }
            >
              <Row gutter={[16, 12]}>
                <Col span={12}>
                  <div className="text-xs mb-1" style={{ color: 'var(--color-text-secondary)' }}>{t('versionModal.cpuUsage')}</div>
                  <Progress
                    percent={dockerStats.cpu?.percent || 0}
                    size="small"
                    status={dockerStats.cpu?.percent > 80 ? 'exception' : 'normal'}
                    format={(percent) => `${percent}%`}
                  />
                </Col>
                <Col span={12}>
                  <div className="text-xs mb-1" style={{ color: 'var(--color-text-secondary)' }}>{t('versionModal.memoryUsage')} ({dockerStats.memory?.limitFormatted || '-'})</div>
                  <Progress
                    percent={dockerStats.memory?.percent || 0}
                    size="small"
                    status={dockerStats.memory?.percent > 80 ? 'exception' : 'normal'}
                    format={() => `${dockerStats.memory?.usageFormatted || '0 B'}`}
                  />
                </Col>
                <Col span={12}>
                  <Statistic
                    title={<span>{t('versionModal.networkRx')} <span className="text-green-500">↓{dockerStats.network?.rxRateFormatted || '0 B/s'}</span></span>}
                    value={dockerStats.network?.rxFormatted || '0 B'}
                    valueStyle={{ fontSize: '14px' }}
                  />
                </Col>
                <Col span={12}>
                  <Statistic
                    title={<span>{t('versionModal.networkTx')} <span className="text-blue-500">↑{dockerStats.network?.txRateFormatted || '0 B/s'}</span></span>}
                    value={dockerStats.network?.txFormatted || '0 B'}
                    valueStyle={{ fontSize: '14px' }}
                  />
                </Col>
              </Row>
            </Card>
          )}

          {/* 更新日志 */}
          {renderChangelog()}

          <Divider className="!my-2" />
          <div className="flex flex-wrap gap-2">
            <Button onClick={() => setReleaseHistoryOpen(true)} icon={<HistoryOutlined />}>{t('versionModal.changelog')}</Button>
            <Button onClick={loadData} icon={<SyncOutlined />} loading={loading}>{t('common.refresh')}</Button>
            {dockerStatus?.enabled && dockerStatus?.canRestart && <Button onClick={() => setContainerAction('restart')}>
              {nativeT('prepareRestart')}
            </Button>}
            {dockerStatus?.enabled && dockerStatus?.canUpdate && <Button type="primary" icon={<RocketOutlined />} onClick={() => setContainerAction('update')}>
              {nativeT('prepareUpdate')}
            </Button>}
            {updateInfo?.releaseUrl && <Button href={updateInfo.releaseUrl} target="_blank" rel="noopener noreferrer">Release</Button>}
          </div>
        </div>
      </Spin>

      <ContainerActionModal open={open && !!containerAction} action={containerAction || 'update'} onClose={() => setContainerAction(null)} />

      {/* 更新日志弹窗 */}
      <ReleaseHistoryModal
        open={releaseHistoryOpen}
        onClose={() => setReleaseHistoryOpen(false)}
      />
    </Modal>
  )
}

export default VersionModal

