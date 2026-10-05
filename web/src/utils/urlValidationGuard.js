// SPDX-License-Identifier: AGPL-3.0-only
// Request identity is private to this dialog. A URL returning to its former
// value does not revive a canceled preview, including after close/reopen.
export function createURLValidationGuard() {
  let active = null
  const invalidate = () => {
    active?.controller.abort()
    active = null
  }
  return {
    invalidate,
    begin(url, scope) {
      invalidate()
      const controller = new AbortController()
      active = { url: url.trim(), scope, controller, signal: controller.signal }
      return active
    },
    isCurrent(request, url, scope) {
      return request != null && active === request && !request.signal.aborted &&
        request.url === (url || '').trim() && request.scope === scope
    },
  }
}
