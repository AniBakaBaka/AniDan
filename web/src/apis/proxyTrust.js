// SPDX-License-Identifier: AGPL-3.0-only
import api from './fetch.js'
export const ACCELERATE_CONFIRMATION = 'TRUST_GATEWAY_WITH_ORIGIN_CREDENTIALS'
export const getProxyConfig = (options = {}) => api.get('/api/ui/config/proxy', null, options)
export const setProxyConfig = (values, options = {}) => api.put('/api/ui/config/proxy', values, options)
export const testProxy = (values, options = {}) => api.post('/api/ui/proxy/test', values, options)
export const testSingleTarget = (values, options = {}) => api.post('/api/ui/proxy/test-single', values, options)

// The server owns URL canonicalization. An edited raw URL must be saved again.
export function savedAccelerateMatches(values, saved) {
  return values.proxyMode === 'accelerate' && saved?.proxyMode === 'accelerate' &&
    saved.accelerateTrusted === true && saved.accelerateTrustVersion === 1 && saved.effectiveMode === 'accelerate' &&
    typeof saved.accelerateTrustedGateway === 'string' && saved.accelerateTrustedGateway !== '' &&
    values.accelerateProxyUrl === saved.accelerateTrustedGateway
}
export function proxySavePayload(values, saved, acknowledged) {
  const payload = { ...values, proxySslVerify: true }
  delete payload.accelerateTrust
  if (values.proxyMode === 'accelerate' && !savedAccelerateMatches(values, saved)) {
    if (!acknowledged) throw new Error('Explicit gateway trust acknowledgement required')
    payload.accelerateTrust = { gateway: values.accelerateProxyUrl, confirmation: ACCELERATE_CONFIRMATION }
  }
  return payload
}
