import { parseAPIJSON, readAPIJSON } from '../utils/idTransport.js'
// Native container transport. Mutations deliberately use fetch once, never an
// EventSource/reconnecting client. Confirmation tokens live only in request headers.
export class ContainerRequestError extends Error {
  constructor(code, status = 0, detail = '') {
    super(code)
    this.name = 'ContainerRequestError'
    this.code = code
    this.status = status
    this.detail = detail
  }
}

const validID = value => typeof value === 'string' && /^[a-f0-9]{64}$/.test(value)
export const confirmationExpired = (confirmation, now = Date.now()) =>
  !confirmation || !Number.isFinite(Date.parse(confirmation.expiresAt)) || Date.parse(confirmation.expiresAt) <= now

export function validateConfirmation(value, { action, image = '', containerId }, now = Date.now()) {
  if (!value || value.action !== action || value.image !== image || value.containerId !== containerId ||
      !validID(value.containerId) || !validID(value.confirmationToken) ||
      typeof value.warning !== 'string' || !value.warning.trim() || confirmationExpired(value, now)) {
    throw new ContainerRequestError('invalidConfirmation')
  }
  return value
}

async function checkResponse(response) {
  if (response.ok) return
  let detail = ''
  try {
    const data = await readAPIJSON(response)
    if (typeof data.detail === 'string') detail = data.detail
  } catch { /* Error documents are never rendered as HTML. */ }
  throw new ContainerRequestError('requestRejected', response.status, detail)
}

// Returns only a positively observed native DONE event, or throws. EOF, malformed
// records, cancellation and transport errors cannot be interpreted as success.
export async function readContainerProgress(response, onProgress = () => {}, signal) {
  await checkResponse(response)
  if (!response.headers.get('content-type')?.toLowerCase().startsWith('text/event-stream') || !response.body) {
    throw new ContainerRequestError('invalidStream')
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let data = []
  const consumeLine = line => {
    if (line !== '') {
      if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''))
      return null
    }
    if (!data.length) return null
    let record
    try { record = parseAPIJSON(data.join('\n')) } catch { throw new ContainerRequestError('invalidStream') }
    data = []
    if (!record || typeof record.status !== 'string' ||
        (record.progress != null && (!Number.isFinite(record.progress) || record.progress < 0 || record.progress > 100))) {
      throw new ContainerRequestError('invalidStream')
    }
    if (record.event && !['DONE', 'ERROR'].includes(record.event)) throw new ContainerRequestError('invalidStream')
    if (record.event === 'DONE' && (!validID(record.containerId) || !validID(record.rollbackContainerId) || record.containerId === record.rollbackContainerId)) {
      throw new ContainerRequestError('invalidStream')
    }
    onProgress(record)
    if (record.event === 'ERROR') throw new ContainerRequestError('operationFailed', 0, record.status)
    return record.event === 'DONE' ? record : null
  }
  try {
    for (;;) {
      if (signal?.aborted) throw new ContainerRequestError('interrupted')
      const { value, done } = await reader.read()
      if (signal?.aborted) throw new ContainerRequestError('interrupted')
      buffer += decoder.decode(value, { stream: !done })
      if (buffer.length > 64 * 1024) throw new ContainerRequestError('invalidStream')
      let index
      while ((index = buffer.indexOf('\n')) !== -1) {
        const line = buffer.slice(0, index).replace(/\r$/, '')
        buffer = buffer.slice(index + 1)
        const terminal = consumeLine(line)
        if (terminal) return terminal
      }
      if (data.join('\n').length > 64 * 1024) throw new ContainerRequestError('invalidStream')
      if (done) throw new ContainerRequestError('interrupted')
    }
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}

export function createContainerAPI({ getToken, fetchImpl = globalThis.fetch }) {
  const bounded = async (signal, timeout, operation) => {
    const controller = new AbortController()
    const abort = () => controller.abort()
    if (signal?.aborted) controller.abort()
    else signal?.addEventListener('abort', abort, { once: true })
    const timer = setTimeout(abort, timeout)
    try { return await operation(controller.signal) }
    finally { clearTimeout(timer); signal?.removeEventListener('abort', abort) }
  }
  const request = async (url, { signal, headers, ...options } = {}) => {
    const token = getToken()
    if (!token) throw new ContainerRequestError('authenticationRequired', 401)
    return fetchImpl(url, {
      ...options,
      signal,
      cache: 'no-store',
      redirect: 'error',
      headers: { Authorization: `Bearer ${token}`, ...headers },
    })
  }
  const json = async (url, options) => {
    const response = await request(url, options)
    await checkResponse(response)
    return readAPIJSON(response)
  }
  return {
    status: signal => bounded(signal, 15_000, requestSignal => json('/api/ui/docker/status', { signal: requestSignal })),
    confirm: (action, image, signal) => bounded(signal, 15_000, requestSignal => json('/api/ui/docker/confirm', {
      method: 'POST', signal: requestSignal,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(action === 'update' ? { action, image } : { action }),
    })),
    restart: (confirmation, signal) => bounded(signal, 60_000, requestSignal => json('/api/ui/restart', {
      method: 'POST', signal: requestSignal,
      headers: { 'X-AniDan-Confirmation': confirmation.confirmationToken },
    })),
    update: (confirmation, signal, onProgress) => bounded(signal, 660_000, async requestSignal => readContainerProgress(await request(
      `/api/ui/update/stream?${new URLSearchParams({ image: confirmation.image })}`, {
        method: 'POST', signal: requestSignal,
        headers: { 'X-AniDan-Confirmation': confirmation.confirmationToken },
      }), onProgress, requestSignal)),
  }
}
