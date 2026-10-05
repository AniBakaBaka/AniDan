// SPDX-License-Identifier: AGPL-3.0-only
// Tracks intent only; no credentials or form values are retained here.
export const createNotificationDraftGuard = () => {
  let revision = 0
  let mutationActive = false
  return {
    ticket: () => revision,
    invalidate: () => { revision += 1 },
    current: ticket => ticket === revision,
    begin: () => {
      if (mutationActive) return null
      mutationActive = true
      const ticket = revision
      let finished = false
      return { ticket, finish: () => {
        if (!finished) { mutationActive = false; finished = true }
      } }
    },
  }
}

// Unknown/imported event settings remain intact until their producers exist.
export const mergeNotificationEvents = (previous, selected, supported, known) => {
  const result = { ...(previous || {}) }
  const selectedSet = new Set(selected || [])
  const supportedSet = new Set(supported || [])
  for (const event of known) {
    if (supportedSet.has(event)) result[event] = selectedSet.has(event)
    else if (!Object.hasOwn(result, event)) result[event] = false
  }
  return result
}
export const notificationEventEnabled = value => value === true || value === 1 || value === 'true' || value === '1'
