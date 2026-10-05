// SPDX-License-Identifier: AGPL-3.0-only
import api from './fetch.js'

export async function previewURLImport(url, options = {}) {
  const response = await api.post('/api/ui/validate-url', { url }, options)
  const value = response.data
  if (value?.isValid !== true) throw new Error(value?.errorMessage || 'URL metadata unavailable')
  if (!['episode', 'media'].includes(value.importScope) || !['tv_series', 'movie', 'other'].includes(value.mediaType) || typeof value.title !== 'string' || !value.title.trim() || typeof value.provider !== 'string' || !value.provider) {
    throw new Error('URL import metadata or scope is not confirmed')
  }
  const collection = value.collection
  if (collection != null && (!/^[0-9]{1,32}$/.test(collection.seasonId) || typeof collection.seasonId !== 'string' || !/^[0-9]{1,32}$/.test(collection.mid) || typeof collection.mid !== 'string' || typeof collection.title !== 'string' || !collection.title.trim() || collection.title.length > 4096 || !Number.isSafeInteger(collection.total) || collection.total < 1)) {
    return { ...value, collection: null, collectionError: 'Collection metadata is not confirmed' }
  }
  return value
}

// Explicit submit only. No automatic retry after an uncertain transport result.
export async function submitURLImport(data, options = {}) {
  const response = await api.post('/api/ui/import-from-url', data, options)
  if (response.status !== 202 || typeof response.data?.taskId !== 'string' || !response.data.taskId) throw new Error('Import task acceptance was not confirmed')
  return response.data.taskId
}
