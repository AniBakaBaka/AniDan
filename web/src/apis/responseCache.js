import { readAPIJSON } from '../utils/idTransport.js'
// SPDX-License-Identifier: AGPL-3.0-only
// Single-shot requests: a failed mutation is never retried automatically.
export function createResponseCacheAPI({ getToken, fetchImpl = globalThis.fetch }) {
  const request = async (action, params = {}, method = 'GET', signal) => {
    const token = getToken()
    if (!token) throw new Error('Authentication required')
    const query = new URLSearchParams(Object.entries(params).filter(([, value]) => value !== ''))
    const controller = new AbortController()
    const abort = () => controller.abort()
    if (signal?.aborted) abort()
    signal?.addEventListener('abort', abort, { once: true })
    const timer = setTimeout(abort, 35000)
    try {
      const response = await fetchImpl(`/api/ui/cache/backend/${action}${query.size ? `?${query}` : ''}`, {
        method, signal: controller.signal, cache: 'no-store', redirect: 'error',
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      if (action === 'detail') return { rawJSON: await response.text() }
      const data = await readAPIJSON(response)
      if (method === 'DELETE' && data.success !== true) throw new Error('Operation not confirmed')
      return data
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', abort)
    }
  }
  return {
    stats: signal => request('stats', {}, 'GET', signal),
    list: (region, after, signal) => request('list', { region, after, limit: 20 }, 'GET', signal),
    detail: (key, signal) => request('detail', { key }, 'GET', signal),
    delete: (key, signal) => request('key', { key }, 'DELETE', signal),
    clear: (region, signal) => request('clear', { region }, 'DELETE', signal),
  }
}
