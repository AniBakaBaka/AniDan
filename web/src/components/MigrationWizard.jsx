// SPDX-License-Identifier: AGPL-3.0-only
import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Button, Card, Checkbox, Descriptions, Input, Modal, Progress, Select, Space, Spin, Steps, Table, Tag } from 'antd'
import { useTranslation } from 'react-i18next'
import {
  applyMigration, cancelMigration, createMigrationGuard, defaultDetectedCandidate, discardMigrationSelection, discardMigrationTokens,
  getMigrationCapabilities, getMigrationDetection, getMigrationOperation, getMigrationOperations, importDetectedMigration, inspectMigration,
  isMigrationRunning, migrationExpired, migrationID, prepareMigration, uploadMigration,
  uploadMigrationConfig, validateMigrationFile, validateMigrationMappings,
} from '../apis/migration.js'

const savedOperationKey = 'anidan.migration.operation'
function rememberedOperation() {
  try { return migrationID(sessionStorage.getItem(savedOperationKey)) } catch { return '' }
}
function rememberOperation(id) {
  try { if (id) sessionStorage.setItem(savedOperationKey, migrationID(id)); else sessionStorage.removeItem(savedOperationKey) } catch { /* History remains available from the server. */ }
}
const formatBytes = bytes => bytes == null ? '—' : bytes < 1024 ? `${bytes} B` : bytes < 1024 * 1024 ? `${(bytes / 1024).toFixed(1)} KiB` : bytes < 1024 * 1024 * 1024 ? `${(bytes / (1024 * 1024)).toFixed(1)} MiB` : `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GiB`

export default function MigrationWizard() {
  const { i18n } = useTranslation()
  const zh = i18n.language.startsWith('zh')
  const tr = (cn, en) => zh ? cn : en
  const guard = useRef(createMigrationGuard())
  const mounted = useRef(false)
  const openRef = useRef(false)
  const selectedRef = useRef('')
  const [open, setOpen] = useState(false)
  const [capabilities, setCapabilities] = useState(null)
  const [operations, setOperations] = useState([])
  const [historyDiagnostics, setHistoryDiagnostics] = useState([])
  const [operation, setOperation] = useState(null)
  const [selected, setSelected] = useState('')
  const [selectionRevision, setSelectionRevision] = useState(0)
  const [detection, setDetection] = useState(null)
  const [detectionError, setDetectionError] = useState(false)
  const [candidateID, setCandidateID] = useState('')
  const [manual, setManual] = useState(false)
  const [file, setFile] = useState(null)
  const [configFile, setConfigFile] = useState(null)
  const [mappings, setMappings] = useState([])
  const [reviewStale, setReviewStale] = useState(false)
  const [reading, setReading] = useState(false)
  const [writing, setWriting] = useState('')
  const [uncertain, setUncertain] = useState(false)
  const [notice, setNotice] = useState('')
  const [acks, setAcks] = useState({})
  const [applyOpen, setApplyOpen] = useState(false)
  const [now, setNow] = useState(Date.now())
  const busy = reading || !!writing
  const running = isMigrationRunning(operation?.state)
  const inspectable = operation && ['uploaded', 'review'].includes(operation.state) && !uncertain && !busy
  const detectedOperation = operation?.sourceMode === 'detected'
  const editable = inspectable && !detectedOperation
  const candidate = detection?.candidates.find(item => item.id === candidateID)
  const candidateExpired = migrationExpired(candidate, now)
  const review = operation?.review
  const expired = migrationExpired(review, now)
  const applyExpired = operation?.applyExpiresAt && migrationExpired({ expiresAt: operation.applyExpiresAt }, now)
  const canApply = operation?.state === 'prepared' && operation.applyToken && !applyExpired && capabilities?.activationAvailable && operation.activationAvailable !== false && !uncertain

  const selectOperation = useCallback(id => {
    guard.current.invalidate()
    selectedRef.current = id
    setSelected(id); rememberOperation(id)
    setSelectionRevision(current => current + 1)
    setOperation(null); setAcks({}); setApplyOpen(false); setConfigFile(null)
    setReviewStale(false); setNotice(''); setUncertain(false); setMappings([])
    setDetection(current => discardMigrationSelection(current)); setCandidateID(''); setManual(false); setDetectionError(false)
  }, [])

  const readCurrent = useCallback(async (id = selectedRef.current, preserveMappings = false, includeDetection = !id) => {
    if (guard.current.isWriting() || !openRef.current) return
    const request = guard.current.beginRead(id)
    setReading(true); setAcks({}); setApplyOpen(false)
    setOperation(current => discardMigrationTokens(current))
    setDetection(current => discardMigrationSelection(current)); setCandidateID(''); setDetectionError(false)
    try {
      const [caps, history, statusRead, detectionRead] = await Promise.all([
        getMigrationCapabilities({ signal: request.controller.signal }),
        getMigrationOperations({ signal: request.controller.signal }),
        id ? getMigrationOperation(id, { signal: request.controller.signal }).then(status => ({ status }), error => ({ error })) : Promise.resolve({ status: null }),
        includeDetection ? getMigrationDetection({ signal: request.controller.signal }).then(value => ({ value }), error => ({ error })) : Promise.resolve(null),
      ])
      if (!guard.current.isCurrent(request) || !mounted.current || !openRef.current) return
      setCapabilities(caps); setOperations(history.items); setHistoryDiagnostics(history.diagnostics)
      if (detectionRead) {
        setDetection(detectionRead.value || null); setDetectionError(!!detectionRead.error)
        setCandidateID(defaultDetectedCandidate(detectionRead.value))
      }
      if (statusRead.error?.response?.status === 404) {
        setOperation(null); setUncertain(false); setNotice('operationUnavailable'); setMappings([])
        return
      }
      if (statusRead.error) throw statusRead.error
      const status = statusRead.status
      setOperation(status); setNow(Date.now())
      setUncertain(false); setNotice(''); setReviewStale(false)
      if (!preserveMappings) setMappings(status?.roots || [])
    } catch (error) {
      if (!guard.current.isCurrent(request) || !mounted.current || !openRef.current) return
      setOperation(current => discardMigrationTokens(current)); setUncertain(true); setNotice(error.response?.status === 403 ? 'adminRequired' : 'readFailed')
    } finally {
      if (guard.current.isCurrent(request) && mounted.current && openRef.current) setReading(false)
    }
  }, [])

  useEffect(() => {
    const requestGuard = guard.current
    mounted.current = true
    return () => { mounted.current = false; openRef.current = false; requestGuard.invalidate() }
  }, [])

  useEffect(() => {
    if (!open) return
    const requestGuard = guard.current
    void readCurrent(selected, false, true)
    return () => requestGuard.invalidate()
  }, [open, selected, selectionRevision, readCurrent])

  useEffect(() => {
    if (!open || !running || uncertain || writing || reading) return
    const timer = setTimeout(() => void readCurrent(), 2000)
    return () => clearTimeout(timer)
  }, [open, running, uncertain, writing, reading, operation, readCurrent])

  useEffect(() => {
    if (!open || (!review && !operation?.applyExpiresAt && !candidate?.expiresAt)) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [open, review, operation?.applyExpiresAt, candidate?.expiresAt])

  const show = () => {
    openRef.current = true
    selectOperation(rememberedOperation())
    setOpen(true)
  }
  const close = () => {
    if (guard.current.isWriting()) return
    openRef.current = false; guard.current.invalidate()
    setOpen(false); setApplyOpen(false); setOperation(null); setAcks({}); setFile(null); setConfigFile(null); setReading(false)
    setDetection(current => discardMigrationSelection(current)); setCandidateID('')
  }
  const errorText = {
    readFailed: tr('暂时无法核实服务器状态。请重新读取；不要重复提交。', 'Server status could not be verified. Read it again before submitting anything.'),
    outcomeUnknown: tr('请求结果尚未核实，确认凭据已清除。请先重新读取状态并查找迁移记录；原版读取或上传可能已经开始，勿盲目重复提交。', 'The request outcome is unverified and approval tokens were cleared. Read status and check migration history first; the source read or upload may already have started. Do not blindly repeat it.'),
    invalidFile: tr('请选择限制大小内的旧版 v2 .json 或 .json.gz 导出。原生 .tar.gz 请使用下面的备份还原入口。', 'Select a legacy v2 .json or .json.gz export within the size limit. Use backup restore below for native .tar.gz bundles.'),
    invalidConfig: tr('请选择不超过 1 MiB（或服务器较小上传上限）的 .yml 或 .yaml 旧版配置文件。', 'Select a .yml or .yaml legacy configuration no larger than 1 MiB or the server’s smaller upload limit.'),
    invalidMapping: tr('请填写不重复的旧路径前缀、选择只读根目录，并使用不包含 .. 的相对目录。', 'Provide distinct old path prefixes, choose read-only roots, and use relative folders without .. segments.'),
    operationUnavailable: tr('此账号无法读取上次选择的迁移记录。启用后旧账号的记录会保持私密；请选择当前账号可访问的记录，或新建迁移。', 'This account cannot read the previously selected migration. After activation, records owned by the old account remain private. Choose an accessible record or start a new migration.'),
    adminRequired: tr('请使用服务器配置的管理员账号登录后操作迁移。', 'Sign in with the server’s configured administrator account to use migration.'),
  }

  async function perform(kind) {
    if (guard.current.isWriting() || busy || uncertain || !capabilities) return
    if (kind === 'upload' && (!file || !capabilities.uploadAvailable)) return
    if (kind === 'detected' && (selectedRef.current || manual || !capabilities.detectionAvailable || !candidate?.available || !candidate.selectionToken || candidateExpired || !acks.detectStopped || !acks.detectBackup || !acks.effective)) return
    if (!['upload', 'detected'].includes(kind) && (!operation || operation.id !== selectedRef.current)) return
    if (kind === 'config' && (!editable || !configFile || operation.configUploaded || !capabilities.uploadAvailable)) return
    if (kind === 'inspect' && (!inspectable || configFile || !capabilities.inspectionAvailable)) return
    if (kind === 'prepare' && (operation.state !== 'review' || configFile || reviewStale || migrationExpired(review) || !acks.backup || !acks.stopped || !acks.reviewed)) return
    if (kind === 'apply' && (!canApply || !applyOpen || !acks.applyBackup || !acks.relogin)) return
    try {
      if (kind === 'upload') validateMigrationFile(file, capabilities)
      if (kind === 'config') validateMigrationFile(configFile, capabilities, true)
      if (kind === 'inspect') validateMigrationMappings(detectedOperation ? [] : mappings, capabilities.roots, capabilities.maxMappings)
    } catch { setNotice(kind === 'inspect' ? 'invalidMapping' : kind === 'config' ? 'invalidConfig' : 'invalidFile'); return }
    const request = guard.current.beginWrite(selectedRef.current)
    if (!request) return
    guard.current.invalidate(); setWriting(kind); setReading(false); setNotice(''); setAcks({})
    const chosen = operation
    const chosenCandidate = candidate
    setDetection(current => discardMigrationSelection(current))
    setOperation(current => discardMigrationTokens(current))
    let nextID = selectedRef.current
    try {
      if (kind === 'upload') nextID = (await uploadMigration(file, capabilities)).id
      if (kind === 'detected') nextID = (await importDetectedMigration(chosenCandidate, { sourceStopped: true, backupConfirmed: true, effectiveSettingsConfirmed: true }, capabilities)).id
      if (kind === 'config') await uploadMigrationConfig(chosen.id, configFile, capabilities)
      if (kind === 'inspect') await inspectMigration(chosen.id, detectedOperation ? [] : mappings, capabilities)
      if (kind === 'prepare') await prepareMigration(chosen, { sourceStopped: true, backupConfirmed: true })
      if (kind === 'cancel') await cancelMigration(chosen.id)
      if (kind === 'apply') await applyMigration(chosen, { backupConfirmed: true, reloginConfirmed: true })
      if (!mounted.current || !openRef.current) return
      setFile(null); setConfigFile(null); setApplyOpen(false)
      if (nextID !== selectedRef.current) selectOperation(nextID)
    } catch {
      if (!mounted.current || !openRef.current) return
      setOperation(current => discardMigrationTokens(current)); setUncertain(true); setNotice('outcomeUnknown'); setApplyOpen(false)
      setDetection(current => discardMigrationSelection(current)); setCandidateID('')
      return
    } finally {
      guard.current.endWrite(request)
      if (mounted.current) setWriting('')
    }
    if (mounted.current && openRef.current && nextID === request.id) void readCurrent(nextID, kind === 'config')
  }

  function updateMappings(next) {
    setMappings(next); setReviewStale(true); setAcks({}); setOperation(current => discardMigrationTokens(current))
  }
  function selectCandidate(id) {
    if (busy || uncertain || guard.current.isWriting() || id === candidateID) return
    guard.current.invalidate()
    setDetection(current => candidateID ? discardMigrationSelection(current, candidateID) : current); setCandidateID(id); setAcks({})
  }
  function changeSourceMode(nextManual) {
    if (busy || uncertain || guard.current.isWriting()) return
    guard.current.invalidate(); setManual(nextManual); setAcks({}); setFile(null); setNotice('')
    setDetection(current => discardMigrationSelection(current)); setCandidateID('')
    if (!nextManual) void readCurrent('', false, true)
  }
  const updateMapping = (index, key, value) => updateMappings(mappings.map((row, i) => i === index ? { ...row, [key]: value } : row))
  const phaseLabel = state => ({
    uploaded: tr('已上传，尚未检查', 'Uploaded; not inspected'), inspecting: tr('正在检查', 'Inspecting'),
    review: tr('等待核对与确认', 'Awaiting review'), preparing: tr('正在准备新库', 'Preparing new library'),
    prepared: tr('新库已准备，尚未启用', 'Prepared; not activated'), failed: tr('失败', 'Failed'),
    canceled: tr('已取消', 'Canceled'), uncertain: tr('结果待核实', 'Outcome unverified'),
    activating: tr('正在启用，服务可能暂时断开', 'Activating; service may disconnect'), active: tr('已启用', 'Activated'),
  }[state] || state)
  const detailItems = rows => rows.filter(([, value]) => value !== '' && value != null).map(([label, value]) => ({ key: label, label, children: <span style={{ overflowWrap: 'anywhere' }}>{String(value)}</span> }))
  const rootLabel = id => {
    const root = capabilities?.roots.find(item => item.id === id)
    return root ? `${root.label} (${root.path})` : id
  }
  const sourceItems = source => detailItems([
    [tr('原版只读挂载', 'Original read-only mount'), source.rootId ? rootLabel(source.rootId) : ''],
    [tr('原版配置目录', 'Original configuration directory'), source.configDirectory],
    [tr('配置来源', 'Settings source'), source.settingsSource === 'compose' ? 'Docker Compose' : source.settingsSource === 'config' ? 'config.yml' : ''],
    [tr('源数据库', 'Source database'), source.sourceDriver === 'mysql' ? 'MySQL' : source.sourceDriver === 'postgres' ? 'PostgreSQL' : ''],
    [tr('源主机', 'Source host'), source.host], [tr('源端口', 'Source port'), source.port || ''], [tr('源数据库名称', 'Source database name'), source.database],
    [tr('迁移目标', 'Migration target'), tr('新建独立 SQLite 库', 'New independent SQLite library')],
    [tr('快照捕获时间', 'Snapshot captured at'), source.capturedAt],
  ])
  const acknowledgement = (key, cn, en) => <Checkbox checked={!!acks[key]} disabled={busy || uncertain} onChange={event => setAcks(current => ({ ...current, [key]: event.target.checked }))}>{tr(cn, en)}</Checkbox>
  const step = !operation ? 0 : ['uploaded', 'inspecting'].includes(operation.state) ? 1 : operation.state === 'review' ? 2 : ['prepared', 'activating', 'active'].includes(operation.state) ? 4 : 3

  return <Card size="small" className="mb-5" title={tr('迁移旧版数据', 'Migrate legacy data')}>
    <Space direction="vertical" style={{ width: '100%' }}>
      <p>{tr('自动检测服务器可读取的原版安装，确认后读取并检查迁移，再核对、准备和单独确认启用。可返回这里查看已保存的进度。', 'Detect original installations available to the server, approve a source read and inspection, then review, prepare and separately activate the new library. Return here to resume saved progress.')}</p>
      <Button onClick={show}>{tr('打开迁移向导', 'Open migration wizard')}</Button>
    </Space>
    <Modal open={open} title={tr('旧版数据迁移向导', 'Legacy migration wizard')} width={940} footer={null} onCancel={close} closable={!writing} maskClosable={!writing} keyboard={!writing} destroyOnClose>
      <Space direction="vertical" size="middle" style={{ width: '100%' }}>
        <Alert type="info" showIcon message={tr('支持范围：检测原版 MySQL / PostgreSQL → 新建 SQLite 库', 'Supported: detected original MySQL / PostgreSQL → new SQLite library')} description={<>
          <p>{tr('打开向导只检查服务器允许读取的原版目录与配置，不连接 SQL。只有确认“读取原版并检查迁移”才会连接所选源库，捕获一次快照并检查。该快照不会持续同步原库。', 'Opening the wizard only checks permitted original directories and settings; it does not connect to SQL. “Read original and inspect migration” explicitly connects to the selected source, captures one snapshot and inspects it. The snapshot does not continuously synchronize with the source.')}</p>
          <p>{tr('原版配置与 XML、NFO、海报等引用文件须可从服务器的只读挂载目录读取，迁移服务器还必须能访问所选 SQL 主机。挂载文件不等于已连通数据库；Compose 服务名需要可达的容器网络，127.0.0.1 指迁移服务器所在环境。请先停止原实例写入并保留一致副本。', 'Original settings and referenced XML, NFO and poster files must be available through server read-only mounts, and the migration server must reach the selected SQL host. A file mount does not provide database connectivity; Compose service names need a reachable container network, and 127.0.0.1 refers to the migration server environment. Stop source writers and retain a consistent copy first.')}</p>
          <p>{tr('高级手动入口仍支持来自 SQLite、PostgreSQL 或 MySQL 的旧版 v2 .json / .json.gz。原生 .tar.gz 完整备份请使用本页原有备份还原入口。目标仅支持新建 SQLite 库。', 'The advanced manual path also accepts legacy v2 .json / .json.gz exports from SQLite, PostgreSQL or MySQL. Use this page’s existing backup restore controls for native .tar.gz full backups. The target is always a new SQLite library.')}</p>
          <p>{tr('检查与准备不会改动原库或当前运行库。启用会短暂中断服务、使当前登录失效；之后使用迁移后的账号重新登录。旧数据保留。', 'Inspection and preparation leave the source and current library intact. Activation briefly interrupts service and invalidates the current login; sign in with a migrated account afterward. Old data is retained.')}</p>
        </>} />
        <Steps size="small" current={step} responsive items={[
          { title: tr('检测与选择', 'Detect and choose') }, { title: tr('读取与检查', 'Read and inspect') },
          { title: tr('核对', 'Review') }, { title: tr('准备', 'Prepare') }, { title: tr('结果与启用', 'Result and activation') },
        ]} />
        <Space wrap style={{ width: '100%' }}>
          <Select aria-label={tr('迁移记录', 'Migration history')} style={{ width: 'min(500px, 75vw)' }} value={selected || undefined} placeholder={tr('选择服务器上的迁移记录', 'Choose a saved migration')} disabled={!!writing} options={operations.map(item => ({ value: item.id, label: `${item.filename || item.id} · ${phaseLabel(item.state)} · ${item.id.slice(0, 8)}` }))} onChange={selectOperation} />
          <Button disabled={!!writing} loading={reading} onClick={() => readCurrent()}>{tr('重新读取状态', 'Read status again')}</Button>
          <Button disabled={!!writing || uncertain} onClick={() => { selectOperation(''); setFile(null) }}>{tr('新建迁移', 'New migration')}</Button>
        </Space>
        {notice && <Alert type="warning" showIcon message={errorText[notice]} />}
        {historyDiagnostics.map((diagnostic, index) => <Alert key={index} type="warning" showIcon message={diagnostic} />)}
        {!capabilities && !notice && <Spin />}
        {capabilities && <Card size="small" title={tr('服务器迁移限制', 'Server migration limits')}>
          <Descriptions size="small" column={{ xs: 1, sm: 2 }} items={detailItems([
            [tr('上传文件', 'Upload file'), formatBytes(capabilities.uploadMaxBytes)],
            [tr('解压后的导出', 'Decoded export'), formatBytes(capabilities.decodedMaxBytes)],
            [tr('单条记录', 'Single record'), formatBytes(capabilities.rowMaxBytes)],
            [tr('引用文件数量', 'Referenced files'), capabilities.fileMaxCount],
            [tr('校验条目总数（含生成配置）', 'Verified entries including generated configuration'), capabilities.receiptEntryMaxCount],
            [tr('旧版配置文件', 'Legacy configuration'), formatBytes(capabilities.legacyConfigMaxBytes)],
            [tr('保留迁移记录数量', 'Retained operations'), capabilities.operationLimit],
            [tr('路径映射数量', 'Path mappings'), capabilities.maxMappings],
            [tr('自动读取导出上限', 'Detected export limit'), formatBytes(capabilities.detectedExportMaxBytes)],
            [tr('自动读取超时（秒）', 'Detected export timeout (seconds)'), capabilities.detectedExportTimeoutSeconds],
          ])} />
          <div>{tr('上传和迁移记录保留供恢复，达到记录上限后需由管理员归档已完成记录。', 'Uploads and operation records are retained for recovery. An administrator must archive completed records when the limit is reached.')}</div>
        </Card>}
        {capabilities && (!capabilities.uploadAvailable || !capabilities.inspectionAvailable) && <Alert type="warning" showIcon message={!capabilities.inspectionAvailable ? tr('当前配置不允许迁移上传或检查', 'Migration upload and inspection are unavailable with the current configuration') : tr('当前配置不允许迁移上传', 'Migration uploads are unavailable with the current configuration')} description={capabilities.inspectionReason || capabilities.uploadReason || tr('请让管理员检查服务器目录和上传请求大小限制。', 'Ask an administrator to check the server directory and request-size limit.')} />}
        {capabilities && !capabilities.activationAvailable && <Alert type="warning" showIcon message={tr('此部署可检查和准备迁移，尚不能在前端启用', 'This deployment can inspect and prepare migrations, but cannot activate them from the UI')} description={capabilities.activationReason || tr('上传前请让管理员检查服务的启用能力配置。', 'Ask an administrator to check service activation configuration before uploading.')} />}
        {capabilities?.activation?.state === 'active' && <Alert type="success" showIcon message={tr('当前服务已启用迁移后的数据库', 'The service is running the activated migrated library')} description={tr('迁移前账号的操作记录保持私密。历史任务和定时任务仍需到任务页单独核对恢复。', 'The previous account’s operation records remain private. Review historical jobs and schedules separately on the task page.')} />}
        {['pending', 'activating', 'failed', 'uncertain'].includes(capabilities?.activation?.state) && <Alert type="warning" showIcon message={tr('服务启用状态：', 'Service activation status: ') + capabilities.activation.state} description={capabilities.activation.message || tr('请重新读取状态；尚未确认启用完成。', 'Read status again; activation completion has not been confirmed.')} />}
        {capabilities && !capabilities.roots.length && <Alert type="warning" showIcon message={tr('尚未配置可读取的源文件目录', 'No source file roots are configured')} description={tr('如果导出引用 XML、NFO 或海报，请先让管理员把停止写入的一致副本放入服务器只读挂载目录。缺少引用文件时检查不会通过。', 'If the export references XML, NFO or posters, an administrator must first make the stopped-source copy available under a server read-only mount. Missing referenced files will block inspection.')} />}
        {!selected && capabilities && !manual && <Card size="small" title={tr('自动检测原版安装', 'Detect original installations')}>
          <Space direction="vertical" style={{ width: '100%' }}>
            <div>{tr('检测结果来自服务器已配置的只读目录。检测本身不会读取远程 SQL 数据，也不会开始迁移。', 'Detection uses the server’s configured read-only directories. Detection itself does not read remote SQL data or start a migration.')}</div>
            <Button disabled={!!writing} loading={reading} onClick={() => readCurrent('', false, true)}>{tr('刷新检测与迁移记录', 'Refresh detection and migration history')}</Button>
            {!capabilities.detectionAvailable && <Alert type="warning" showIcon message={tr('当前部署无法自动检测原版', 'Original-installation detection is unavailable')} description={capabilities.detectionReason || tr('请让管理员检查原版只读挂载目录和迁移检查能力。', 'Ask an administrator to check original read-only mounts and migration inspection availability.')} />}
            {detectionError && <Alert type="warning" showIcon message={tr('无法核实原版检测结果，请刷新。也可以使用高级手动导入。', 'Detected installations could not be verified. Refresh detection or use advanced manual import.')} />}
            {detection?.diagnostics.map((diagnostic, index) => <Alert key={index} type="warning" showIcon message={diagnostic} />)}
            {detection && !detection.candidates.length && <Alert type="info" showIcon message={tr('未发现可识别的原版安装', 'No recognizable original installation was found')} description={tr('请确认原版 config.yml 或受支持的 Compose 配置及引用文件已只读挂载到服务器配置的源目录。随后刷新检测，或使用高级手动导入。', 'Make the original config.yml or supported Compose settings and referenced files available under configured server read-only roots, then refresh detection or use advanced manual import.')} />}
            {!!detection?.candidates.length && <>
              <Select aria-label={tr('选择原版安装', 'Choose original installation')} style={{ width: '100%' }} placeholder={tr('发现多个原版安装，请明确选择一个', 'Multiple original installations found; choose one explicitly')} value={candidateID || undefined} disabled={busy || uncertain || !capabilities.detectionAvailable} options={detection.candidates.map(item => ({ value: item.id, label: `${item.label} · ${item.configDirectory}${item.available ? '' : tr('（不可读取）', ' (unavailable)')}`, disabled: !item.available }))} onChange={selectCandidate} />
              {detection.candidates.filter(item => !item.available).map(item => <Alert key={item.id} type="warning" showIcon message={item.label || item.configDirectory} description={item.reason || tr('此安装暂不满足自动读取条件，请检查服务器配置。', 'This installation does not meet automatic-read requirements. Check server configuration.')} />)}
            </>}
            {candidate && <>
              <Descriptions bordered column={{ xs: 1, sm: 2 }} size="small" items={sourceItems(candidate)} />
              {candidate.warnings.map((warning, index) => <Alert key={index} type="warning" showIcon message={warning} />)}
              {(!candidate.selectionToken || candidateExpired) && <Alert type="warning" showIcon message={tr('此次选择的确认凭据已失效。请刷新检测后重新核对并确认。', 'Approval for this selection is no longer current. Refresh detection, review it and acknowledge again.')} />}
              {acknowledgement('effective', '我已核对所示 config.yml / Compose 与原版实际生效的环境设置一致，且没有遗漏的环境变量或运行时覆盖', 'I verified that the displayed config.yml / Compose reflects the original effective environment, with no omitted environment or runtime overrides')}
              {acknowledgement('detectStopped', '原版实例和其他写入者已停止写入，源数据库与引用文件来自一致副本', 'The original instance and other writers have stopped; the source database and referenced files form a consistent copy')}
              {acknowledgement('detectBackup', '我已保留原始数据及可恢复的完整备份，确认读取上述源数据库', 'I retained the original data and a restorable full backup and approve reading the source database shown above')}
              <Button type="primary" disabled={busy || uncertain || !capabilities.detectionAvailable || !candidate.available || !candidate.selectionToken || candidateExpired || !acks.effective || !acks.detectStopped || !acks.detectBackup} loading={writing === 'detected'} onClick={() => perform('detected')}>{tr('读取原版并检查迁移', 'Read original and inspect migration')}</Button>
            </>}
            <Button disabled={busy || uncertain} onClick={() => changeSourceMode(true)}>{tr('高级：手动上传旧版导出', 'Advanced: upload a legacy export manually')}</Button>
          </Space>
        </Card>}
        {!selected && capabilities && manual && <Card size="small" title={tr('高级：选择旧版数据库导出', 'Advanced: choose a legacy database export')}>
          <Space direction="vertical" style={{ width: '100%' }}>
            <label htmlFor="migration-export">{tr('支持 .json / .json.gz；文件上限：', 'Accepts .json / .json.gz; maximum: ')}{formatBytes(capabilities.uploadMaxBytes)}</label>
            <input id="migration-export" type="file" accept=".json,.json.gz" disabled={busy || uncertain || !capabilities.uploadAvailable} onChange={event => { setFile(event.target.files?.[0] || null); setNotice('') }} />
            {file && <div style={{ overflowWrap: 'anywhere' }}>{file.name} · {formatBytes(file.size)}</div>}
            <Button type="primary" disabled={!file || busy || uncertain || !capabilities.uploadAvailable} loading={writing === 'upload'} onClick={() => perform('upload')}>{tr('上传导出', 'Upload export')}</Button>
            <Button disabled={busy || uncertain} onClick={() => changeSourceMode(false)}>{tr('返回自动检测', 'Return to automatic detection')}</Button>
          </Space>
        </Card>}
        {operation && <>
          <Descriptions bordered column={1} size="small" items={detailItems([
            [tr('迁移编号', 'Migration ID'), operation.id], [tr('状态', 'Status'), phaseLabel(operation.state)],
            [tr('导出文件', 'Export file'), operation.filename], [tr('上传大小', 'Upload size'), formatBytes(operation.size)],
            ['SHA-256', operation.sha256], [tr('最近更新', 'Last updated'), operation.updatedAt],
          ])} />
          {detectedOperation && <Card size="small" title={tr('已记录的原版来源与自动文件映射', 'Recorded original source and automatic file mappings')}>
            <Space direction="vertical" style={{ width: '100%' }}>
              <Descriptions bordered column={{ xs: 1, sm: 2 }} size="small" items={sourceItems(operation.sourceDisplay)} />
              <Alert type="info" showIcon message={tr('本记录保存一次捕获的快照，不持续同步原库', 'This operation records a captured snapshot; it does not continuously synchronize with the source')} description={tr('配置和文件映射由服务器从所选安装推导，此处不可修改。重新检查仅检查已捕获的快照与引用文件，不会重新读取 SQL；需要新快照时请新建迁移。', 'The server derives settings and file mappings from the selected installation; they cannot be edited here. Re-inspection only checks the captured snapshot and referenced files, without reading SQL again. Start a new migration for a new snapshot.')} />
              {mappings.length ? <Table size="small" rowKey={(row, index) => `${row.rootId}-${index}`} pagination={false} dataSource={mappings} columns={[
                { title: tr('原版文件路径', 'Original file prefix'), dataIndex: 'from', render: value => <span style={{ overflowWrap: 'anywhere' }}>{value}</span> },
                { title: tr('服务器只读根目录', 'Server read-only root'), dataIndex: 'rootId', render: value => <span style={{ overflowWrap: 'anywhere' }}>{rootLabel(value)}</span> },
                { title: tr('相对目录', 'Relative folder'), dataIndex: 'path', render: value => <span style={{ overflowWrap: 'anywhere' }}>{value || tr('根目录', 'Root directory')}</span> },
              ]} /> : <div>{tr('文件映射将在服务器检查后显示。', 'File mappings appear after the server inspects the source.')}</div>}
              {['uploaded', 'review'].includes(operation.state) && <Button disabled={!inspectable || !capabilities?.inspectionAvailable} loading={writing === 'inspect'} onClick={() => perform('inspect')}>{tr('重新检查已捕获的快照与引用文件', 'Re-inspect the captured snapshot and referenced files')}</Button>}
            </Space>
          </Card>}
          {running && <Alert type="info" showIcon icon={<Spin size="small" />} message={phaseLabel(operation.state)} description={tr('显示的是服务器已确认阶段，不代表全部完成。关闭页面不会取消服务器任务；重新打开后可继续查看。', 'This is the last confirmed server phase, not completion. Closing this page does not cancel the server task; reopen it to resume viewing.')} />}
          {operation.progress && <div><Progress percent={operation.progress.percent} status={operation.state === 'failed' ? 'exception' : running ? 'active' : 'normal'} /><div>{operation.progress.message}</div></div>}
          {operation.state === 'failed' && <Alert type="error" showIcon message={tr('迁移未完成。请检查一致副本、文件映射和导出格式，再新建迁移。此记录保留供核查。', 'Migration did not complete. Check the consistent copy, mappings and export format, then start a new migration. This record is retained for review.')} description={operation.error} />}
          {operation.state === 'uploaded' && operation.error && <Alert type="warning" showIcon message={tr('检查尚未通过，可修正后再次检查同一份上传', 'Inspection needs correction; the same upload can be inspected again')} description={operation.error} />}
          {operation.state === 'canceled' && <Alert type="info" showIcon message={tr('迁移已取消，未自动启用任何新库。', 'Migration was canceled. No new library was activated automatically.')} />}
          {operation.state === 'uncertain' && <Alert type="warning" showIcon message={tr('服务器无法确认此任务的最终结果。请保留旧数据并核查服务器状态，勿重复准备或启用。', 'The server cannot confirm this operation’s outcome. Preserve old data and verify server status before another preparation or activation.')} />}
          {!detectedOperation && ['uploaded', 'review'].includes(operation.state) && <Card size="small" title={tr('源文件与旧版配置', 'Source files and legacy configuration')}>
            <Space direction="vertical" style={{ width: '100%' }}>
              <p>{tr('把旧库中的路径前缀映射到已配置的只读根目录及其相对文件夹。不要填写数据库密码或连接地址。根目录下全部内容请选择空的相对目录。', 'Map each old stored path prefix to a configured read-only root and a relative folder. Leave the relative folder empty to use the root itself. Do not enter database passwords or connection strings.')}</p>
              {mappings.map((mapping, index) => <Space direction="vertical" key={index} style={{ width: '100%', border: '1px solid #8884', padding: 12 }}>
                <Input aria-label={tr(`旧路径前缀 ${index + 1}`, `Old path prefix ${index + 1}`)} placeholder={tr('旧路径前缀，例如 /old/data', 'Old path prefix, e.g. /old/data')} value={mapping.from} disabled={!editable} onChange={event => updateMapping(index, 'from', event.target.value)} />
                <Select aria-label={tr(`只读根目录 ${index + 1}`, `Read-only root ${index + 1}`)} style={{ width: '100%' }} placeholder={tr('选择只读根目录', 'Choose a read-only root')} value={mapping.rootId || undefined} disabled={!editable} options={capabilities?.roots.map(root => ({ value: root.id, label: `${root.label} (${root.path})` }))} onChange={value => updateMapping(index, 'rootId', value)} />
                <Input aria-label={tr(`相对目录 ${index + 1}`, `Relative folder ${index + 1}`)} placeholder={tr('根目录内的相对文件夹（可留空）', 'Relative folder inside the root (optional)')} value={mapping.path} disabled={!editable} onChange={event => updateMapping(index, 'path', event.target.value)} />
                <Button disabled={!editable} onClick={() => updateMappings(mappings.filter((_, i) => i !== index))}>{tr('移除此映射', 'Remove mapping')}</Button>
              </Space>)}
              <Button disabled={!editable || !capabilities?.roots.length || mappings.length >= capabilities.maxMappings} onClick={() => updateMappings([...mappings, { from: '', rootId: '', path: '' }])}>{tr('添加路径映射', 'Add path mapping')} ({mappings.length}/{capabilities?.maxMappings})</Button>
              <Alert type="info" message={tr('可选：上传旧版 config.yml，以保留原账号认证与 OTP 密钥', 'Optional: upload legacy config.yml to preserve authentication and OTP keys')} description={tr('配置由服务器处理，前端不会显示原始内容。每次迁移只接受一次配置上传，上限 1 MiB；如需更换，请新建迁移。上传后必须重新检查。没有原密钥可能无法使用旧账号的登录或多因素验证。', 'The server handles this configuration; its raw contents are never displayed. Each migration accepts one configuration upload, up to 1 MiB; start a new migration to replace it. Inspect again after uploading. Without original keys, legacy login or multi-factor authentication may not work.')} />
              <Tag>{operation.configUploaded ? tr('已提供旧版配置', 'Legacy configuration supplied') : tr('未提供旧版配置', 'No legacy configuration supplied')}</Tag>
              <input aria-label={tr('旧版 YAML 配置', 'Legacy YAML configuration')} type="file" accept=".yml,.yaml" disabled={!editable || operation.configUploaded || !capabilities?.uploadAvailable} onChange={event => { setConfigFile(event.target.files?.[0] || null); setReviewStale(true); setAcks({}); setOperation(current => discardMigrationTokens(current)) }} />
              <Button disabled={!editable || !configFile || operation.configUploaded || !capabilities?.uploadAvailable} loading={writing === 'config'} onClick={() => perform('config')}>{tr('上传配置并使旧检查失效', 'Upload configuration and invalidate old review')}</Button>
              {configFile && <div>{tr('请先上传已选择的配置文件，再检查迁移。', 'Upload the selected configuration before inspecting the migration.')}</div>}
              <Button type="primary" disabled={!editable || !!configFile || !capabilities?.inspectionAvailable} loading={writing === 'inspect'} onClick={() => perform('inspect')}>{tr('检查导出和引用文件', 'Inspect export and referenced files')}</Button>
            </Space>
          </Card>}
          {review && <Card size="small" title={tr('核对本次迁移', 'Review this migration')}>
            <Space direction="vertical" style={{ width: '100%' }}>
              <Descriptions bordered column={1} size="small" items={detailItems([
                [tr('源数据库类型', 'Source database'), review.sourceDBType], [tr('源导出时间', 'Source export time'), review.sourceCreatedAt],
                [tr('目标类型', 'Target database'), 'SQLite'], [tr('独立目标目录', 'New target directory'), review.targetDir],
                [tr('迁移后管理员账号', 'Migrated administrator account'), review.targetAdminUsername], [tr('迁移后时区', 'Migrated timezone'), review.targetTimezone],
                [tr('文件数量', 'File count'), review.fileCount], [tr('文件总大小', 'Total file size'), formatBytes(review.totalFileBytes)],
                [tr('快照 SHA-256', 'Snapshot SHA-256'), review.snapshotSHA256], [tr('检查证据 SHA-256', 'Inspection proof SHA-256'), review.proofSHA256],
                [tr('核对有效至', 'Review expires'), review.expiresAt],
              ])} />
              <Table size="small" rowKey="name" pagination={false} dataSource={review.tables} columns={[{ title: tr('数据表', 'Table'), dataIndex: 'name' }, { title: tr('记录数', 'Rows'), dataIndex: 'rows' }]} />
              {review.warnings.map((warning, index) => <Alert key={index} type="warning" showIcon message={warning} />)}
              {(expired || reviewStale) && <Alert type="warning" message={reviewStale ? tr('映射已改变，必须重新检查后才能准备新库。', 'Mappings changed. Inspect again before preparing.') : tr('核对已过期，请重新检查。', 'Review expired. Inspect again.')} />}
              {acknowledgement('stopped', '旧实例已停止写入，导出与引用文件来自同一份一致副本', 'The old instance stopped writing, and the export and referenced files form one consistent copy')}
              {acknowledgement('backup', '我已保留原始数据及可恢复的完整备份', 'I retained the original data and a restorable full backup')}
              {acknowledgement('reviewed', '我已核对上述表数量、文件数量、目标目录和警告，确认准备新库', 'I reviewed table counts, file counts, target directory and warnings and approve preparing the new library')}
              <Button type="primary" disabled={busy || uncertain || expired || configFile || reviewStale || !review.reviewToken || !acks.stopped || !acks.backup || !acks.reviewed} loading={writing === 'prepare'} onClick={() => perform('prepare')}>{tr('确认并准备新库', 'Confirm and prepare new library')}</Button>
            </Space>
          </Card>}
          {operation.result && <Card size="small" title={tr('迁移结果', 'Migration result')}>
            <Space direction="vertical" style={{ width: '100%' }}>
              <Alert type={operation.state === 'active' ? 'success' : 'info'} showIcon message={phaseLabel(operation.state)} />
              <Descriptions bordered column={1} size="small" items={detailItems([
                [tr('目标目录', 'Target directory'), operation.result.targetDir], [tr('目标配置', 'Target configuration'), operation.result.configFile],
                [tr('收据 SHA-256', 'Receipt SHA-256'), operation.result.receiptSHA256], [tr('配置 SHA-256', 'Configuration SHA-256'), operation.result.configSHA256],
              ])} />
              <Alert type="info" message={tr('历史任务和定时任务需要另行审核恢复', 'Historical jobs and schedules need a separate recovery review')} description={tr('迁移和启用不会代替任务页已有的恢复确认。登录迁移后的账号后，请到任务页核对。', 'Migration and activation do not replace the existing task recovery approval. Review tasks after signing in with the migrated account.')} />
              {operation.state === 'prepared' && (!capabilities?.activationAvailable || operation.activationAvailable === false) && <Alert type="warning" message={tr('当前部署暂不支持从前端启用', 'This deployment cannot activate from the UI')} description={operation.activationReason || capabilities?.activationReason || tr('新库已保留。请让管理员检查服务的启用能力配置。', 'The prepared library is retained. Ask an administrator to check the service activation configuration.')} />}
              {operation.state === 'prepared' && applyExpired && <Alert type="warning" message={tr('启用凭据已过期，请重新读取状态。', 'Activation approval expired. Read status again.')} />}
              {operation.state === 'prepared' && <Button type="primary" disabled={!canApply || busy} onClick={() => { setApplyOpen(true); setAcks({}) }}>{tr('核对并启用新库', 'Review activation')}</Button>}
              {['activating', 'active'].includes(operation.state) && <Button href="/login">{tr('使用迁移后的账号登录', 'Sign in with a migrated account')}</Button>}
            </Space>
          </Card>}
          {['uploaded', 'inspecting', 'review', 'preparing'].includes(operation.state) && <Button disabled={busy || uncertain} loading={writing === 'cancel'} onClick={() => perform('cancel')}>{tr('请求取消此次迁移', 'Request migration cancellation')}</Button>}
        </>}
        <Button disabled={!!writing} onClick={close}>{tr('关闭并保留进度', 'Close and keep progress')}</Button>
      </Space>
    </Modal>
    <Modal open={applyOpen} title={tr('确认启用已准备的新库', 'Confirm activation of the prepared library')} onCancel={() => { if (!writing) { setApplyOpen(false); setAcks({}) } }} closable={!writing} maskClosable={!writing} keyboard={!writing} footer={null}>
      <Space direction="vertical" style={{ width: '100%' }}>
        <Alert type="warning" showIcon message={tr('启用将中断当前服务并退出当前账号', 'Activation interrupts the service and signs out the current account')} description={tr('系统将切换到上述已准备的新库。请使用迁移后的账号重新登录。旧库和原始副本保留，以便管理员恢复。', 'The service switches to the prepared library shown above. Sign in with a migrated account afterward. The old library and source copy are retained for administrator recovery.')} />
        <div style={{ overflowWrap: 'anywhere' }}>{operation?.result?.targetDir}</div>
        {acknowledgement('applyBackup', '我已保留当前运行库和原始数据的备份，并确认切换到这个目标目录', 'I retained backups of the current library and source data and approve switching to this target directory')}
        {acknowledgement('relogin', '我接受短暂服务中断和退出登录，并已准备好迁移后账号的登录信息', 'I accept the service interruption and sign-out and have the migrated account’s login information')}
        <Button type="primary" danger disabled={!canApply || busy || !acks.applyBackup || !acks.relogin} loading={writing === 'apply'} onClick={() => perform('apply')}>{tr('确认启用新库', 'Confirm activation')}</Button>
        <Button disabled={!!writing} onClick={() => { setApplyOpen(false); setAcks({}) }}>{tr('返回核对', 'Back to review')}</Button>
      </Space>
    </Modal>
  </Card>
}
