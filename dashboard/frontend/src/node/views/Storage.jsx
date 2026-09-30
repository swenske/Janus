import { Database, FolderTree, HardDrive } from 'lucide-react'
import { useState } from 'react'
import { getJSON } from '../api.js'
import DataTable from '../components/DataTable.jsx'
import { Badge, Card, ErrorBox, Meter, PageHeader, useAction } from '../../shared/ui.jsx'
import { bytes, num } from '../format.js'
import { usePoll } from '../hooks.jsx'

function DiskUsage() {
  const [path, setPath] = useState('/etc')
  const [recursive, setRecursive] = useState(true)
  const [result, setResult] = useState(null)
  const [busy, run] = useAction()
  const go = () =>
    run(async () => {
      const q = new URLSearchParams({ path, recursive: String(recursive) })
      setResult(await getJSON(`/api/files/du?${q}`))
    })
  const max = Math.max(1, ...(result || []).map((e) => Number(e.size_bytes) || 0))
  return (
    <Card title="Disk usage" icon={FolderTree}>
      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault()
          go()
        }}
      >
        <input className="mono grow" value={path} onChange={(e) => setPath(e.target.value)} placeholder="/path" />
        <label className="check">
          <input type="checkbox" checked={recursive} onChange={(e) => setRecursive(e.target.checked)} /> Per directory
        </label>
        <button className="primary" disabled={busy || !path}>
          {busy ? 'Measuring…' : 'Measure'}
        </button>
      </form>
      {result && (
        <div className="stack" style={{ marginTop: '0.8rem', gap: '0.4rem', maxHeight: '50vh', overflow: 'auto' }}>
          {[...result]
            .sort((a, b) => (Number(b.size_bytes) || 0) - (Number(a.size_bytes) || 0))
            .slice(0, 200)
            .map((e) => (
              <div key={e.path} className="stack" style={{ gap: '0.2rem' }}>
                <div className="spread">
                  <span className="mono small">{e.path}</span>
                  <span className="small">{bytes(Number(e.size_bytes) || 0)}</span>
                </div>
                <Meter value={Number(e.size_bytes) || 0} max={max} tone="ok" />
              </div>
            ))}
        </div>
      )}
      <p className="muted small">Apparent size of regular files; /proc, /sys and /dev are never walked into.</p>
    </Card>
  )
}

export default function Storage() {
  const mounts = usePoll('/api/system/mounts', { every: 30000 })
  const disks = usePoll('/api/system/disks')
  const rows = (mounts.data?.mounts || []).map((m, i) => ({
    ...m,
    __key: i,
    size: Number(m.size_bytes) || 0,
    used: (Number(m.size_bytes) || 0) - (Number(m.available_bytes) || 0),
  }))
  return (
    <>
      <PageHeader title="Storage" subtitle="Mounted filesystems, disks and space usage" />
      <div className="stack">
        <Card title="Mounts" icon={HardDrive}>
          <ErrorBox error={mounts.error} />
          <DataTable
            rows={rows}
            columns={[
              { key: 'mounted_on', label: 'Mounted on', render: (r) => <span className="mono">{r.mounted_on}</span> },
              { key: 'filesystem', label: 'Filesystem', render: (r) => <span className="mono small">{r.filesystem}</span> },
              { key: 'read_only', label: 'Mode', get: (r) => (r.read_only ? 'ro' : 'rw'), render: (r) => <Badge tone={r.read_only ? 'info' : ''}>{r.read_only ? 'read-only' : 'read-write'}</Badge> },
              { key: 'size', label: 'Size', num: true, render: (r) => (r.size ? bytes(r.size) : '–') },
              {
                key: 'used',
                label: 'Used',
                render: (r) =>
                  r.size ? (
                    <div className="stack" style={{ gap: '0.2rem', minWidth: '9rem' }}>
                      <span className="small">
                        {bytes(r.used)} ({num((r.used / r.size) * 100, 0)}%)
                      </span>
                      <Meter value={r.used} max={r.size} />
                    </div>
                  ) : (
                    '–'
                  ),
              },
            ]}
          />
        </Card>
        <div className="grid grid-2">
          <Card title="Disk I/O (since boot)" icon={Database}>
            <ErrorBox error={disks.error} />
            <DataTable
              filterable={false}
              rows={(disks.data?.disks || []).map((d) => ({ ...d, __key: d.device_name }))}
              columns={[
                { key: 'device_name', label: 'Device', render: (r) => <span className="mono">{r.device_name}</span> },
                { key: 'read_completed', label: 'Reads', num: true, render: (r) => num(r.read_completed || 0) },
                { key: 'write_completed', label: 'Writes', num: true, render: (r) => num(r.write_completed || 0) },
              ]}
            />
          </Card>
          <DiskUsage />
        </div>
      </div>
    </>
  )
}
