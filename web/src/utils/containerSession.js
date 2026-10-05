import { confirmationExpired, ContainerRequestError, validateConfirmation } from '../apis/container.js'

// One dialog lifetime. All callbacks are generation-scoped. Aborts discard
// review tokens and never retry a request; a sent mutation has an unknown result
// until the server positively reports its outcome.
export function createContainerSession(api, onChange, now = Date.now) {
  let state = { phase: 'idle', logs: [] }
  let generation = 0
  let controller
  let confirmation = null
  const publish = patch => { state = { ...state, ...patch }; onChange(state) }
  const begin = () => {
    generation += 1
    controller?.abort()
    controller = new AbortController()
    confirmation = null
    return { id: generation, signal: controller.signal }
  }
  const live = id => generation === id
  const available = action => state.status?.enabled === true && state.status?.[action === 'update' ? 'canUpdate' : 'canRestart'] === true
  const readFailure = error => publish({ phase: 'failed', error, review: null })
  return {
    getState: () => state,
    async load(action) {
      const { id, signal } = begin()
      publish({ phase: 'loading', action, status: null, review: null, error: null, result: null, logs: [], progress: 0 })
      try {
        const status = await api.status(signal)
        if (live(id)) publish({ phase: 'selecting', status })
      } catch (error) { if (live(id)) readFailure(error) }
    },
    async prepare(image = '') {
      if (state.phase !== 'selecting' || !available(state.action)) return false
      // An explicit selection is mandatory even if only one image is allowed.
      if (state.action === 'update' && (!image || !state.status.allowedImages?.includes(image))) return false
      const { id, signal } = begin()
      const { action, status } = state
      publish({ phase: 'preparing', review: null, error: null })
      try {
        const value = validateConfirmation(await api.confirm(action, image, signal), { action, image, containerId: status.containerId }, now())
        if (!live(id)) return false
        confirmation = value
        // Never expose the one-use token to React state, logs, storage or markup.
        const { action: confirmedAction, image: confirmedImage, containerId, expiresAt, warning } = value
        const review = { action: confirmedAction, image: confirmedImage, containerId, expiresAt, warning }
        publish({ phase: 'review', review })
        return true
      } catch (error) { if (live(id)) readFailure(error) }
      return false
    },
    back() {
      if (!['review', 'preparing'].includes(state.phase)) return
      begin()
      publish({ phase: 'selecting', review: null, error: null })
    },
    async execute(acknowledged) {
      if (state.phase !== 'review' || acknowledged !== true) return false
      if (confirmationExpired(confirmation, now())) {
        confirmation = null
        publish({ phase: 'expired', error: new ContainerRequestError('expired') })
        return false
      }
      const value = confirmation
      const { id, signal } = begin() // Consumes the local token synchronously before awaiting.
      publish({ phase: 'running', error: null, logs: [], progress: 0 })
      try {
        const result = state.action === 'restart'
          ? await api.restart(value, signal)
          : await api.update(value, signal, record => {
            if (live(id)) publish({ logs: [...state.logs.slice(-199), record.status], progress: record.progress ?? state.progress })
          })
        if (!live(id)) return false
        if (state.action === 'restart' && result?.success !== true) throw new ContainerRequestError('invalidResult')
        if (state.action === 'update' && (result?.event !== 'DONE' || result.rollbackContainerId !== value.containerId)) throw new ContainerRequestError('invalidResult')
        publish({ phase: 'done', result })
        return true
      } catch (error) {
        if (live(id)) publish({
          phase: error.code === 'operationFailed' || [400, 401, 403, 409, 428, 503].includes(error.status) ? 'failed' : 'uncertain',
          error,
        })
      }
      return false
    },
    cancel() {
      const running = state.phase === 'running'
      begin()
      publish(running
        ? { phase: 'uncertain', error: new ContainerRequestError('interrupted') }
        : { phase: 'idle', review: null })
    },
    dispose() { begin() },
  }
}
