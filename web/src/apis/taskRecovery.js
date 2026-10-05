// SPDX-License-Identifier: AGPL-3.0-only
import api from './fetch.js'

export const RECOVERY_CONFIRMATION = 'RECOVER_PENDING_AND_SCHEDULES'
export async function getTaskRecoveryStatus(options = {}) {
  const response = await api.get('/api/ui/tasks/recovery/status', null, options)
  if (typeof response.data?.recoveryReviewRequired !== 'boolean') {
    throw new Error('Recovery status was not confirmed')
  }
  return response
}
// Called only by the operator's explicit approval action. Never auto-retry.
export const approveTaskRecovery = (options = {}) => api.post(
  '/api/ui/tasks/recovery/approve', { confirm: RECOVERY_CONFIRMATION }, options
)
