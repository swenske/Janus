import { usePoll } from '../hooks.jsx'

function started(b) {
  return new Date(Number(b.started_unix_ns) / 1e6).toLocaleString()
}

// BootSelect picks this boot (followed live) or a previous one, from the
// node's LOGS partition. Nothing to pick on a node without one, or
// before its first reboot.
export default function BootSelect({ value, onChange }) {
  const resp = usePoll('/api/log-boots', { every: 0 })
  const boots = resp.data?.available ? (resp.data.boots || []).filter((b) => b.offset < 0) : []
  if (boots.length === 0) return null
  return (
    <select value={value} onChange={(e) => onChange(Number(e.target.value))} aria-label="Boot">
      <option value={0}>This boot (live)</option>
      {boots.map((b) => (
        <option key={b.offset} value={b.offset}>
          {b.offset === -1 ? 'Previous boot' : `Boot ${b.offset}`} (started {started(b)})
        </option>
      ))}
    </select>
  )
}
