export const tokenFormValues = record => record ? {
  name: record.name,
  dailyCallLimit: record.dailyCallLimit,
  validityPeriod: 'custom',
  customToken: '', // Editing a name/limit does not rotate its credential.
} : {
  name: '',
  dailyCallLimit: 500,
  validityPeriod: 'permanent',
  customToken: '',
}
