// SPDX-License-Identifier: AGPL-3.0-only
import api from './fetch.js'

export const PREPARE_MIGRATION = 'PREPARE_MIGRATION'
export const APPLY_MIGRATED_LIBRARY = 'APPLY_MIGRATED_LIBRARY'
export const READ_DETECTED_LEGACY = 'READ_DETECTED_LEGACY'
export const MIGRATION_STATES = ['uploaded', 'inspecting', 'review', 'preparing', 'prepared', 'failed', 'canceled', 'uncertain', 'activating', 'active']
export const isMigrationRunning = state => ['inspecting', 'preparing', 'activating'].includes(state)
const endpoint = '/api/ui/migration'
const boundedText = (value, limit = 4096) => {
  if (typeof value !== 'string' || value.length > limit) throw new Error('Invalid migration text')
  return value
}
const optionalText = value => value == null ? '' : boundedText(value)
const quantity = value => {
  if (!Number.isSafeInteger(value) || value < 0) throw new Error('Invalid migration quantity')
  return value
}
const hash = value => {
  if (typeof value !== 'string' || !/^[a-f0-9]{64}$/.test(value)) throw new Error('Invalid migration digest')
  return value
}
export function migrationID(value) {
  if (typeof value !== 'string' || !/^[a-f0-9]{32}$/.test(value)) throw new Error('Invalid migration operation ID')
  return value
}
const operationPath = id => `${endpoint}/${migrationID(id)}`
const token = value => {
  if (typeof value !== 'string' || !/^[\x21-\x7e]{1,2048}$/.test(value)) throw new Error('Missing migration approval token')
  return value
}
const boundedList = (value, max, validate) => {
  if (!Array.isArray(value) || value.length > max) throw new Error('Migration response exceeds display limits')
  return value.map(validate)
}
const detectionTime = value => {
  const text = boundedText(value, 64)
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(text) || !Number.isFinite(Date.parse(text))) throw new Error('Invalid detected source timestamp')
  return text
}
function detectedSource(value, required = true) {
  const settingsSource = boundedText(value?.settingsSource ?? '', 16)
  const sourceDriver = boundedText(value?.sourceDriver ?? '', 16)
  const configDirectory = boundedText(value?.configDirectory ?? '')
  const host = boundedText(value?.host ?? '', 256)
  const database = boundedText(value?.database ?? '', 256)
  const port = quantity(value?.port ?? 0)
  if ((!['config', 'compose'].includes(settingsSource) && (required || settingsSource !== '')) ||
      (!['mysql', 'postgres'].includes(sourceDriver) && (required || sourceDriver !== '')) || port > 65535 ||
      (required && (!configDirectory || !host || !database || port === 0)) || /[\x00-\x1f\x7f]/.test(configDirectory + host + database)) throw new Error('Invalid detected source display')
  return { settingsSource, configDirectory, sourceDriver, host, port, database }
}
export const migrationExpired = (review, now = Date.now()) => !review || !Number.isFinite(Date.parse(review.expiresAt)) || Date.parse(review.expiresAt) <= now

export function validateMigrationDetection(value) {
  const seen = new Set()
  return {
    candidates: boundedList(value?.candidates, 16, candidate => {
      const id = hash(candidate?.id)
      if (seen.has(id) || typeof candidate.available !== 'boolean') throw new Error('Invalid detected installation')
      seen.add(id)
      const rootId = boundedText(candidate.rootId, 128)
      if (!rootId) throw new Error('Missing detected installation root')
      const result = {
        id, rootId, label: boundedText(candidate.label, 256), ...detectedSource(candidate, candidate.available),
        available: candidate.available, reason: optionalText(candidate.reason),
        warnings: boundedList(candidate.warnings ?? [], 32, warning => boundedText(warning)),
      }
      if (candidate.available) {
        result.selectionToken = token(candidate.selectionToken)
        result.expiresAt = detectionTime(candidate.expiresAt)
      }
      return result
    }),
    diagnostics: boundedList(value?.diagnostics ?? [], 32, diagnostic => boundedText(diagnostic)),
  }
}

export function defaultDetectedCandidate(detection) {
  const available = detection?.candidates.filter(candidate => candidate.available) || []
  return available.length === 1 ? available[0].id : ''
}

// Invalidate the previous selection on a switch, or every selection on a new
// read, dismissal, or an uncertain write. Descriptive metadata remains visible.
export function discardMigrationSelection(detection, id) {
  if (!detection) return null
  return { ...detection, candidates: detection.candidates.map(candidate => {
    if (id && candidate.id !== id) return candidate
    const { selectionToken: _token, expiresAt: _expires, ...display } = candidate
    return display
  }) }
}

export function validateMigrationMappings(mappings, roots, maxMappings = 128) {
  if (!Array.isArray(mappings) || mappings.length > Math.min(quantity(maxMappings), 128)) throw new Error('Invalid file mappings')
  const seen = new Set()
  return mappings.map(value => {
    const from = boundedText(value?.from).trim()
    const rootId = boundedText(value?.rootId, 128)
    const path = boundedText(value?.path ?? '').trim()
    if (!from || /[\x00-\x1f]/.test(from) || seen.has(from) || !roots.some(root => root.id === rootId) ||
      /[\x00-\x1f]/.test(path) || /^[\/\\]/.test(path) || /^[a-z]:/i.test(path) || path.split(/[\/\\]/).includes('..')) throw new Error('Invalid file mapping')
    seen.add(from)
    return { from, rootId, path }
  })
}

export function validateMigrationFile(file, capabilities, config = false) {
  if (capabilities.uploadAvailable === false || capabilities.inspectionAvailable === false) throw new Error('Migration uploads are unavailable')
  const name = file?.name || ''
  if (!(config ? /\.ya?ml$/i : /\.json(?:\.gz)?$/i).test(name)) throw new Error(config ? 'Select a YAML configuration' : 'Select a legacy JSON export')
  const limit = config ? Math.min(capabilities.uploadMaxBytes, capabilities.legacyConfigMaxBytes ?? 1024 * 1024) : capabilities.uploadMaxBytes
  if (!Number.isSafeInteger(file?.size) || file.size <= 0 || file.size > limit) throw new Error('File exceeds the upload limit')
  return file
}

export function validateMigrationCapabilities(value) {
  if (!Array.isArray(value?.targetDrivers) || !value.targetDrivers.includes('sqlite') || !Array.isArray(value.roots) || typeof value.activationAvailable !== 'boolean') throw new Error('Migration capabilities unavailable')
  return {
    uploadMaxBytes: quantity(value.uploadMaxBytes),
    uploadAvailable: value.uploadAvailable !== false && value.inspectionAvailable !== false && value.uploadMaxBytes > 0,
    uploadReason: optionalText(value.uploadReason),
    inspectionAvailable: value.inspectionAvailable !== false,
    inspectionReason: optionalText(value.inspectionReason),
    detectionAvailable: value.detectionAvailable === true && value.inspectionAvailable !== false,
    detectionReason: optionalText(value.detectionReason),
    detectedExportMaxBytes: quantity(value.detectedExportMaxBytes ?? 0),
    detectedExportTimeoutSeconds: quantity(value.detectedExportTimeoutSeconds ?? 0),
    decodedMaxBytes: quantity(value.decodedMaxBytes),
    rowMaxBytes: quantity(value.rowMaxBytes),
    fileMaxCount: quantity(value.fileMaxCount),
    receiptEntryMaxCount: value.receiptEntryMaxCount == null ? null : quantity(value.receiptEntryMaxCount),
    operationLimit: quantity(value.operationLimit),
    maxMappings: Math.min(quantity(value.maxMappings ?? 128), 128),
    legacyConfigMaxBytes: quantity(value.legacyConfigMaxBytes ?? Math.min(value.uploadMaxBytes, 1024 * 1024)),
    sourceFormats: Array.isArray(value.sourceFormats) ? value.sourceFormats.map(item => boundedText(item, 128)) : [],
    targetDrivers: ['sqlite'],
    roots: value.roots.map(root => ({ id: boundedText(root.id, 128), label: boundedText(root.label, 256), path: boundedText(root.path) })),
    activationAvailable: value.activationAvailable,
    activationReason: optionalText(value.activationReason),
    activation: value.activation && ['idle', 'unavailable', 'pending', 'activating', 'active', 'failed', 'uncertain'].includes(value.activation.state)
      ? { state: value.activation.state, operationId: value.activation.operationId ? migrationID(value.activation.operationId) : '', message: optionalText(value.activation.message) } : null,
  }
}

export function validateMigrationReview(value) {
  if (!value || !Array.isArray(value.tables) || !Array.isArray(value.warnings)) throw new Error('Migration review incomplete')
  const expiresAt = boundedText(value.expiresAt, 64)
  if (!Number.isFinite(Date.parse(expiresAt))) throw new Error('Invalid migration review expiry')
  const reviewToken = value.reviewToken ? token(value.reviewToken) : ''
  if (!reviewToken && !migrationExpired({ expiresAt })) throw new Error('Current migration approval token unavailable')
  return {
    sourceDBType: boundedText(value.sourceDBType, 64), sourceCreatedAt: optionalText(value.sourceCreatedAt),
    targetAdminUsername: value.targetAdminUsername == null ? '' : boundedText(value.targetAdminUsername, 256),
    targetTimezone: value.targetTimezone == null ? '' : boundedText(value.targetTimezone, 128),
    snapshotSHA256: hash(value.snapshotSHA256), proofSHA256: hash(value.proofSHA256),
    tables: value.tables.map(table => ({ name: boundedText(table.name, 256), rows: quantity(table.rows) })),
    fileCount: quantity(value.fileCount), totalFileBytes: quantity(value.totalFileBytes),
    warnings: value.warnings.map(warning => boundedText(warning)), targetDir: boundedText(value.targetDir),
    expiresAt, reviewToken,
  }
}

// Deliberately project server responses. Source configuration, credentials and
// historical job parameters must never be retained in the view model.
export function validateMigrationOperation(value, expectedID, summary = false) {
  const id = migrationID(value?.id)
  if ((expectedID && id !== migrationID(expectedID)) || !MIGRATION_STATES.includes(value.state)) throw new Error('Migration status unavailable')
  const result = {
    id, state: value.state, filename: optionalText(value.filename), size: value.size == null ? null : quantity(value.size),
    sha256: value.sha256 ? hash(value.sha256) : '', createdAt: optionalText(value.createdAt), updatedAt: optionalText(value.updatedAt),
    jobId: optionalText(value.jobId), configUploaded: value.configUploaded === true,
    error: optionalText(value.error), errorCode: optionalText(value.errorCode), uploadInterrupted: value.uploadInterrupted === true,
  }
  if (value.sourceMode != null && !['upload', 'detected'].includes(value.sourceMode)) throw new Error('Invalid migration source mode')
  result.sourceMode = value.sourceMode === 'detected' ? 'detected' : 'upload'
  if (result.sourceMode === 'detected') {
    result.sourceDisplay = detectedSource(value.sourceDisplay)
    if (value.sourceDisplay.capturedAt && value.sourceDisplay.capturedAt !== '0001-01-01T00:00:00Z') result.sourceDisplay.capturedAt = detectionTime(value.sourceDisplay.capturedAt)
  }
  if (summary) return result
  if (value.roots != null) result.roots = boundedList(value.roots, 128, root => ({ from: boundedText(root.from), rootId: boundedText(root.rootId, 128), path: boundedText(root.path) }))
  if (value.progress && Number.isFinite(value.progress.percent) && value.progress.percent >= 0 && value.progress.percent <= 100) result.progress = { percent: value.progress.percent, message: optionalText(value.progress.message) }
  if (value.review && value.state === 'review') result.review = validateMigrationReview(value.review)
  if (value.state === 'review' && !result.review) throw new Error('Migration review unavailable')
  if (value.prepared && ['prepared', 'activating', 'active'].includes(value.state)) {
    const receipt = value.prepared
    result.result = {
      targetDir: boundedText(receipt.targetDir), configFile: boundedText(receipt.configFile),
      receiptSHA256: hash(receipt.receiptSHA256), configSHA256: hash(receipt.configSHA256),
      recoveryReviewRequired: receipt.recoveryReviewRequired === true,
    }
    result.activationAvailable = receipt.activationAvailable === true
    result.activationReason = optionalText(receipt.activationReason)
    if (value.state === 'prepared' && receipt.applyToken) result.applyToken = token(receipt.applyToken)
    if (receipt.expiresAt) result.applyExpiresAt = boundedText(receipt.expiresAt, 64)
  }
  if (value.state === 'prepared' && !result.result) throw new Error('Prepared migration receipt unavailable')
  return result
}

export function discardMigrationTokens(value) {
  if (!value) return null
  const { review, applyToken: _token, applyExpiresAt: _expires, ...rest } = value
  return { ...rest, ...(review ? { review: { ...review, reviewToken: '' } } : {}) }
}

export async function getMigrationCapabilities(options = {}) {
  return validateMigrationCapabilities((await api.get(`${endpoint}/capabilities`, null, options)).data)
}
export async function getMigrationDetection(options = {}) {
  return validateMigrationDetection((await api.get(`${endpoint}/detect`, null, options)).data)
}

export function importDetectedMigration(candidate, acknowledgements, capabilities, options = {}) {
  if (capabilities?.detectionAvailable !== true || capabilities?.inspectionAvailable === false || candidate?.available !== true ||
      migrationExpired(candidate) || acknowledgements?.sourceStopped !== true || acknowledgements?.backupConfirmed !== true ||
      acknowledgements?.effectiveSettingsConfirmed !== true) throw new Error('Detected import needs a current selection and acknowledgements')
  const body = {
    candidateId: hash(candidate.id), selectionToken: token(candidate.selectionToken), confirmation: READ_DETECTED_LEGACY,
    sourceStopped: true, backupConfirmed: true, effectiveSettingsConfirmed: true,
  }
  return api.post(`${endpoint}/detect/import`, body, options).then(response => {
    if (response.status !== 202) throw new Error('Detected import acceptance unconfirmed')
    const operation = validateMigrationOperation(response.data)
    if (operation.state !== 'inspecting' || operation.sourceMode !== 'detected' || !operation.jobId) throw new Error('Detected import acceptance unconfirmed')
    return operation
  })
}
export async function getMigrationOperations(options = {}) {
  const response = await api.get(`${endpoint}/operations`, null, options)
  return validateMigrationHistory(response.data)
}
export function validateMigrationHistory(value) {
  if (!Array.isArray(value?.items) || !Array.isArray(value.diagnostics || [])) throw new Error('Migration history unavailable')
  if (value.items.length > 128 || (value.diagnostics?.length || 0) > 32) throw new Error('Migration history exceeds display limits')
  return {
    items: value.items.map(item => validateMigrationOperation(item, undefined, true)),
    diagnostics: (value.diagnostics || []).map(item => boundedText(item)),
    retainedIncompleteUploads: quantity(value.retainedIncompleteUploads ?? 0),
  }
}
export async function getMigrationOperation(id, options = {}) {
  return validateMigrationOperation((await api.get(operationPath(id), null, options)).data, id)
}
async function uploadFile(path, file, options) {
  const form = new FormData()
  form.append('file', file)
  const response = await api.post(path, form, { ...options, headers: { ...options.headers, 'Content-Type': 'multipart/form-data' } })
  return validateMigrationOperation(response.data)
}
export function uploadMigration(file, capabilities, options = {}) {
  validateMigrationFile(file, capabilities)
  return uploadFile(`${endpoint}/upload`, file, options)
}
export function uploadMigrationConfig(id, file, capabilities, options = {}) {
  validateMigrationFile(file, capabilities, true)
  return uploadFile(`${operationPath(id)}/config`, file, options)
}
async function accepted(path, body, options) {
  const response = await api.post(path, body, options)
  if (response.status !== 202 || !response.data?.jobId) throw new Error('Migration acceptance unconfirmed')
  return { jobId: boundedText(response.data.jobId, 128) }
}
// All mutations are single requests. A network error is an unknown outcome,
// never permission to repeat a write; the operator must read server state first.
export function inspectMigration(id, mappings, capabilities, options = {}) {
  if (capabilities.inspectionAvailable === false) throw new Error('Migration inspection is unavailable')
  return accepted(`${operationPath(id)}/inspect`, { roots: validateMigrationMappings(mappings, capabilities.roots, capabilities.maxMappings) }, options)
}
export function prepareMigration(operation, acknowledgements, options = {}) {
  if (operation.state !== 'review' || migrationExpired(operation.review) || !acknowledgements?.sourceStopped || !acknowledgements?.backupConfirmed) throw new Error('Migration preparation needs a current review and acknowledgements')
  return accepted(`${operationPath(operation.id)}/prepare`, {
    reviewToken: token(operation.review.reviewToken), confirmation: PREPARE_MIGRATION, sourceStopped: true, backupConfirmed: true,
  }, options)
}
export async function cancelMigration(id, options = {}) {
  const response = await api.post(`${operationPath(id)}/cancel`, {}, options)
  return response.data
}
export function applyMigration(operation, acknowledgements, options = {}) {
  if (operation.state !== 'prepared' || operation.activationAvailable !== true || !acknowledgements?.backupConfirmed || !acknowledgements?.reloginConfirmed || migrationExpired({ expiresAt: operation.applyExpiresAt })) throw new Error('Migration activation needs fresh explicit approval')
  return accepted(`${operationPath(operation.id)}/apply`, {
    applyToken: token(operation.applyToken), confirmation: APPLY_MIGRATED_LIBRARY, backupConfirmed: true, reloginConfirmed: true,
  }, options)
}

// Read generations prevent responses from an old selection, close or navigation
// from reviving approval tokens. The synchronous write lock stops double clicks.
export function createMigrationGuard() {
  let read = null
  let write = null
  return {
    beginRead(id) { read?.controller.abort(); read = { id, controller: new AbortController() }; return read },
    isCurrent(request) { return read === request && !request.controller.signal.aborted },
    invalidate() { read?.controller.abort(); read = null },
    beginWrite(id) { if (write) return null; write = { id }; return write },
    endWrite(request) { if (write === request) write = null },
    isWriting() { return write !== null },
  }
}
