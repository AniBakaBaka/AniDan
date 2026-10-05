// SPDX-License-Identifier: AGPL-3.0-only
import api from './fetch.js'
import { importLocalItems } from './index.js'
import { asIdentifier } from '../utils/idTransport.js'

export const LOCAL_BINDING_CONFIRM = 'BIND_CURRENT_LOCAL_ASSOCIATION'
export function localReviewID(value) {
  const id = asIdentifier(value)
  if (!/^[1-9]\d{0,18}$/.test(id) || BigInt(id) > 9223372036854775807n) throw new Error('Invalid local item ID')
  return id
}
const text = (value, max = 4096) => {
  if (typeof value !== 'string' || value.length > max * 2 || [...value].length > max) throw new Error('Invalid review text')
  return value
}
const optionalText = value => value == null ? null : text(value)
const integer = (value, max = Number.MAX_SAFE_INTEGER) => {
  if (!Number.isSafeInteger(value) || value < 0 || value > max) throw new Error('Invalid review quantity')
  return value
}
const year = value => value == null ? null : integer(value)
function repeatOptions(value, expected) {
  const itemId = localReviewID(value?.itemId)
  if (itemId !== expected || !value?.provider || !value?.mediaId) throw new Error('Repeat identity unavailable')
  return { itemId, provider: text(value.provider, 500), mediaId: text(value.mediaId, 255) }
}
export function validateLocalBindingResponse(value, expectedID, now = Date.now()) {
  const itemId = localReviewID(expectedID)
  if (localReviewID(value?.itemId) !== itemId) throw new Error('Review item changed')
  const options = repeatOptions(value.repeatOptions, itemId)
  if (value.status === 'bound') {
    if (value.identityValidated !== true) throw new Error('Binding identity was not validated')
    return { status: 'bound', itemId, identityValidated: true, repeatOptions: options }
  }
  if (value.status !== 'reviewable') throw new Error('Review unavailable')
  const expiresAt = text(value.expiresAt, 64)
  if (!Number.isFinite(Date.parse(expiresAt)) || Date.parse(expiresAt) <= now) throw new Error('Review expired')
  const reviewToken = text(value.reviewToken, 2048)
  if (!reviewToken || !/^[\x21-\x7e]+$/.test(reviewToken)) throw new Error('Review token missing')
  const i = value.item, a = value.target, s = value.source, e = value.episode, f = value.files
  if (localReviewID(i?.id) !== itemId || i.isImported !== true || !i.sourcePath || !e?.poolPath || f?.byteEqual !== true) throw new Error('Review evidence incomplete')
  const sourceBytes = integer(f.sourceBytes, 32 * 1024 * 1024), poolBytes = integer(f.poolBytes, 32 * 1024 * 1024)
  if (!sourceBytes || sourceBytes !== poolBytes || !/^[a-f0-9]{64}$/.test(f.sourceSha256) || f.sourceSha256 !== f.poolSha256 || !integer(f.commentCount)) throw new Error('File equality not established')
  const source = { sourceId: localReviewID(s?.sourceId), provider: text(s.provider, 500), mediaId: text(s.mediaId, 255), sourceOrder: integer(s.sourceOrder), createdAt: text(s.createdAt, 64) }
  if (source.provider !== options.provider || source.mediaId !== options.mediaId) throw new Error('Source options changed')
  return {
    status: 'reviewable', itemId, reviewToken, expiresAt, repeatOptions: options,
    item: { id: itemId, title: text(i.title), mediaType: text(i.mediaType, 64), year: year(i.year), season: integer(i.season), episode: integer(i.episode), createdAt: text(i.createdAt, 64), sourcePath: text(i.sourcePath), isImported: true, tmdbId: optionalText(i.tmdbId), tvdbId: optionalText(i.tvdbId), imdbId: optionalText(i.imdbId) },
    target: { animeId: localReviewID(a?.animeId), metadataId: localReviewID(a.metadataId), title: text(a.title), type: text(a.type, 64), year: year(a.year), season: integer(a.season), tmdbId: optionalText(a.tmdbId), tvdbId: optionalText(a.tvdbId), imdbId: optionalText(a.imdbId) },
    source,
    episode: { episodeId: localReviewID(e.episodeId), index: integer(e.index), providerEpisodeId: text(e.providerEpisodeId), poolPath: text(e.poolPath) },
    files: { sourceBytes, poolBytes, sourceSha256: f.sourceSha256, poolSha256: f.poolSha256, commentCount: f.commentCount, byteEqual: true },
  }
}
export async function previewLocalBinding(itemId, options = {}) {
  const response = await api.post(`/api/ui/local-items/${encodeURIComponent(localReviewID(itemId))}/binding-preview`, {}, options)
  if (response.status !== 200) throw new Error('Review response unconfirmed')
  return validateLocalBindingResponse(response.data, itemId)
}
export async function confirmLocalBinding(review, options = {}) {
  const value = validateLocalBindingResponse(review, review.itemId)
  if (value.status !== 'reviewable') throw new Error('No pending review')
  const response = await api.post(`/api/ui/local-items/${encodeURIComponent(value.itemId)}/binding-confirm`, { reviewToken: value.reviewToken, confirm: LOCAL_BINDING_CONFIRM }, options)
  if (response.status !== 200) throw new Error('Binding commit unconfirmed')
  const result = validateLocalBindingResponse(response.data, value.itemId)
  if (result.status !== 'bound') throw new Error('Binding commit unconfirmed')
  return result
}
export async function repeatReviewedLocalImport(bound, options = {}) {
  const value = validateLocalBindingResponse(bound, bound.itemId)
  if (value.status !== 'bound') throw new Error('Validated binding required')
  const response = await importLocalItems({ items: [value.repeatOptions] }, options)
  if (response.status !== 202 || typeof response.data?.taskId !== 'string' || !response.data.taskId) throw new Error('Import acceptance unconfirmed')
  return response.data.taskId
}

// One request generation per selected item. Closing/changing/refreshing cannot
// resurrect an earlier token, even if the same item is selected again.
export function createLocalReviewGuard() {
  let current = null
  return {
    invalidate() { current?.controller.abort(); current = null },
    begin(itemId) { current?.controller.abort(); const controller = new AbortController(); current = { itemId: localReviewID(itemId), controller, signal: controller.signal }; return current },
    isCurrent(request, itemId) { return current === request && !request.signal.aborted && request.itemId === String(itemId) },
  }
}
