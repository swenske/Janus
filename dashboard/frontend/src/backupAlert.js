// backupAlert says what's wrong with the backups (GET /api/backups'
// alert), or nothing.
export function backupAlert(status) {
  const when = (t) => (t ? new Date(t).toLocaleString() : '–')
  const last = status?.history?.[0]
  if (status?.alert === 'failed') return `The last backup failed (${when(last?.time)}): ${last?.error}. The Controller tries again every hour.`
  if (status?.alert === 'late') {
    const ok = status.history.find((r) => !r.error && r.key)
    return `No backup since ${when(ok?.time)} - more than twice the interval: was the Controller stopped?`
  }
  return ''
}
