// SPDX-License-Identifier: AGPL-3.0-only
export function aggregationSettings(value) {
  if (typeof value?.enabled !== 'boolean' || !Number.isFinite(value.windowSeconds) ||
      value.windowSeconds < 1 || value.windowSeconds > 3600 ||
      !Number.isInteger(value.threshold) || value.threshold < 1 || value.threshold > 1000) {
    throw new TypeError('Invalid aggregation settings')
  }
  return { enabled: value.enabled, windowSeconds: value.windowSeconds, threshold: value.threshold }
}
// Diagnostics contain counters, not notification content. Even if the server
// later adds text fields, this panel never displays their values.
export function notificationCounters(value, prefix = '', depth = 0) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || depth > 3) return []
  const result = []
  for (const [key, item] of Object.entries(value).slice(0, 64)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (typeof item === 'number' && Number.isFinite(item)) result.push([path, item])
    else if (item && typeof item === 'object') result.push(...notificationCounters(item, path, depth + 1))
    if (result.length >= 64) break
  }
  return result.slice(0, 64)
}
