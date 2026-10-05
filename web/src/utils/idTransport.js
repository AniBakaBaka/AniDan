// SPDX-License-Identifier: AGPL-3.0-only
import { parse } from 'lossless-json'

export const EXACT_IDS_HEADER = 'X-AniDan-Exact-IDs'
export const EXACT_IDS_VERSION = 'decimal-string-v1'

const unsafeKeys = new Set(['__proto__', 'constructor', 'prototype'])
const identifierAliases = new Set(['id', 'ids', 'cid', 'aid', 'bvid', 'vid', 'pid', 'mid', 'gid', 'uid', 'egid'])
const decimalInteger = /^-?(?:0|[1-9]\d*)$/
const hasControlCharacter = text => [...text].some(character => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127)
const looksStructured = text => ['{', '['].includes(text.trimStart()[0])

// Match names, not arbitrary suffixes such as "valid" or "grid".
export const isIdentifierKey = key => identifierAliases.has(key) || /(?:^|_)[iI][dD][sS]?$/.test(key) || /(?:Id|ID)s?$/.test(key)

export function asIdentifier(value) {
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) throw new TypeError('Unsafe numeric identifier; use the original decimal string')
    return String(value)
  }
  if (typeof value === 'string' && value.length > 0 && value.trim() === value && !hasControlCharacter(value)) return value
  throw new TypeError('Identifier must be a nonempty string or safe integer')
}

export function sameIdentifier(left, right) {
  if (left == null || right == null || left === '' || right === '') return false
  try { return asIdentifier(left) === asIdentifier(right) }
  catch { return false }
}

// Keep token provenance private: a response object cannot impersonate a number.
class NumberToken {
  constructor(source) { this.source = source }
}

function checkKey(key) {
  if (unsafeKeys.has(key)) throw new TypeError('Unsafe JSON object key')
}

function decode(value, identifier) {
  if (value instanceof NumberToken) {
    if (!identifier) return Number(value.source)
    // Go emits IDs as decimal integer tokens. Reject fractional/exponent ID
    // tokens instead of silently rounding an unexpected representation.
    if (!decimalInteger.test(value.source)) throw new TypeError('Numeric identifier must be a decimal integer')
    return value.source === '-0' ? '0' : value.source
  }
  if (Array.isArray(value)) return value.map(item => decode(item, identifier))
  if (value && typeof value === 'object') {
    const result = {}
    for (const [key, child] of Object.entries(value)) {
      checkKey(key)
      result[key] = decode(child, isIdentifierKey(key))
    }
    return result
  }
  // Empty/null optional external IDs are normal API values. Path helpers still
  // require a nonempty identifier before an actual resource can be targeted.
  return identifier && value != null && value !== '' ? asIdentifier(value) : value
}

export function parseAPIJSON(text, { identifier = false } = {}) {
  if (typeof text !== 'string') throw new TypeError('API JSON must be text')
  // The dependency uses ordinary object assignment. Validate keys using native
  // JSON grammar first, before it sees any input. All numeric results from this
  // validation pass are discarded; only the lossless pass supplies values.
  JSON.parse(text, (key, value) => { checkKey(key); return value })
  return decode(parse(text, undefined, { parseNumber: source => new NumberToken(source) }), identifier)
}

export async function readAPIJSON(response, options) {
  return parseAPIJSON(await response.text(), options)
}

// Use a replacer so toJSON results are validated too, and inherited array ID
// context reaches every element without changing non-ID numeric metrics.
export function stringifyAPIJSON(value, { identifier = false } = {}) {
  let exactIdentifiers = false
  const idArrays = new WeakSet()
  let root = true
  const body = JSON.stringify(value, function (key, child) {
    checkKey(key)
    const isID = root ? identifier : isIdentifierKey(key) || idArrays.has(this)
    root = false
    if (Array.isArray(child)) {
      if (isID) idArrays.add(child)
      return child
    }
    if (isID && child != null && child !== '' && typeof child !== 'object') {
      exactIdentifiers = true
      return asIdentifier(child)
    }
    return child
  })
  return { body, exactIdentifiers }
}

export function normalizeAPIParams(params) {
  if (params == null || params instanceof URLSearchParams) return params
  // Serialization also validates the final values produced by any toJSON.
  return JSON.parse(stringifyAPIJSON(params).body)
}

export function isBinaryOrForm(value) {
  return (typeof FormData !== 'undefined' && value instanceof FormData) ||
    (typeof URLSearchParams !== 'undefined' && value instanceof URLSearchParams) ||
    (typeof Blob !== 'undefined' && value instanceof Blob) ||
    (typeof ArrayBuffer !== 'undefined' && (value instanceof ArrayBuffer || ArrayBuffer.isView(value))) ||
    (typeof ReadableStream !== 'undefined' && value instanceof ReadableStream) ||
    (value && typeof value.pipe === 'function')
}

export function transformAPIRequest(data, headers) {
  if (isBinaryOrForm(data)) {
    // The JSON default must not serialize multipart fields or override a
    // URLSearchParams body's form content type. Axios supplies the boundary.
    if (typeof FormData !== 'undefined' && data instanceof FormData) headers.setContentType(undefined)
    if (data instanceof URLSearchParams) headers.setContentType('application/x-www-form-urlencoded;charset=utf-8')
    return data
  }
  const contentType = String(headers.getContentType() || '').toLowerCase()
  if (data === undefined) return data
  if (contentType && !contentType.includes('json')) {
    // Let Axios retain its native encoder for non-JSON bodies, but validate
    // object fields before a form encoder could hide an unsafe numeric ID.
    if (data !== null && typeof data === 'object') return normalizeAPIParams(data)
    return this.requestIdentifier === true ? asIdentifier(data) : data
  }
  if (typeof data === 'string') {
    try { data = parseAPIJSON(data, { identifier: this.requestIdentifier === true }) }
    catch (error) {
      // Keep Axios's ordinary raw-text request behavior, but never hide invalid
      // JSON objects, arrays or unsafe-key/identifier failures as string data.
      if (!(error instanceof SyntaxError) || looksStructured(data)) throw error
    }
  }
  const result = stringifyAPIJSON(data, { identifier: this.requestIdentifier === true })
  headers.setContentType('application/json')
  if (result.exactIdentifiers && ['post', 'put', 'patch', 'delete'].includes(this.method?.toLowerCase())) {
    headers.set(EXACT_IDS_HEADER, EXACT_IDS_VERSION)
  }
  return result.body
}

export function transformAPIResponse(data, headers) {
  if (typeof data !== 'string' || data === '' || (this.responseType && this.responseType !== 'json')) return data
  try { return parseAPIJSON(data, { identifier: this.responseIdentifier === true }) }
  catch (error) {
    const jsonResponse = this.responseType === 'json' || this.responseIdentifier === true ||
      String(headers?.getContentType?.() || headers?.['content-type'] || '').toLowerCase().includes('json') || looksStructured(data)
    if (jsonResponse || !(error instanceof SyntaxError)) throw error
    return data
  }
}
