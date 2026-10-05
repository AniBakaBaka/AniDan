// SPDX-License-Identifier: AGPL-3.0-only
import { asIdentifier } from './idTransport.js'

// Tree keys mix typed local media IDs with non-record group labels. Decode only
// explicit record key shapes; preserve all decimal digits and reject rounded JS numbers.
export function mediaItemIdFromKey(key) {
  if (typeof key === 'number') return asIdentifier(key)
  if (typeof key !== 'string') return null
  if (/^-?\d+$/.test(key)) return asIdentifier(key)
  const match = /^(?:movie|episode)-(-?\d+)$/.exec(key)
  return match ? asIdentifier(match[1]) : null
}
export function animeIdFromDragKey(key) {
  if (typeof key !== 'string') return null
  const match = /^anime-(-?\d+)$/.exec(key)
  return match ? asIdentifier(match[1]) : null
}
