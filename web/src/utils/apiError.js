// Preserve both the normalized UI error and Axios-style access used by older views.
export function normalizeApiError(error) {
  const data = error.response?.data
  const payload = data && typeof data === 'object' && !Array.isArray(data) ? data : {}
  const detail = payload.detail ?? payload.message
  let message = typeof detail === 'string' ? detail : ''
  if (Array.isArray(detail)) {
    message = detail.map(item => typeof item === 'string' ? item : item?.msg || '').filter(Boolean).join('; ')
  }
  if (!message) message = error.message || 'Request failed'
  return {
    ...payload,
    message,
    ...(payload.detail !== undefined ? { detail: message } : {}),
    code: error.response?.status,
    status: error.response?.status,
    name: error.name,
    response: error.response,
  }
}
