import { Archive, ExternalLink, Rocket, Upload as UploadIcon } from 'lucide-react'
import { useState } from 'react'
import { ApiError, postJSON } from '../api.js'
import { Badge, Card, PageHeader, Tabs, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import { WaitForNode } from '../waitForNode.jsx'

const FILES = [
  { field: 'rootfs_squashfs', label: 'rootfs.squashfs' },
  { field: 'rootfs_verity', label: 'rootfs.verity' },
  { field: 'uki_a', label: 'uki-a.efi' },
  { field: 'uki_b', label: 'uki-b.efi' },
]

export default function Update() {
  const release = usePoll('/api/latest-release', { every: 600000 })
  const overview = usePoll('/api/system/overview', { every: 0 })
  const [mode, setMode] = useState('url')
  const [reference, setReference] = useState('')
  const [sha, setSha] = useState('')
  const [files, setFiles] = useState({})
  const [waitHealth, setWaitHealth] = useState(true)
  const [timeout, setTimeoutS] = useState('')
  const [allowUnsigned, setAllowUnsigned] = useState(false)
  const [following, setFollowing] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()

  const v = overview.data?.version
  const rel = release.data
  const updateAvailable = v?.version && rel?.tag_name && v.version !== rel.tag_name
  const useLatest = () => {
    setMode('url')
    setReference(rel.bundle_base_url)
    setSha(rel.sha256 || '')
  }

  const launch = async () => {
    const target = v?.active_slot === 'A' ? 'B' : 'A'
    const ok = await confirm({
      title: 'Install this release?',
      body: (
        <>
          <p>
            The new system is written to slot <strong>{target}</strong> (the one not running), then the node reboots into it. The current slot stays intact, so{' '}
            <em>Rollback</em> can always go back.
          </p>
          {waitHealth ? (
            <p>If HAProxy isn't healthy on the new slot within {timeout || 60} s, the node reverts to the current slot on its own.</p>
          ) : (
            <p className="muted">No automatic revert: the node stays on the new slot whatever happens.</p>
          )}
          {allowUnsigned && (
            <p>
              <strong>The bundle's signature won't be checked.</strong> Whoever could alter it on its way to the node controls what the node boots - only for a development build you produced yourself.
            </p>
          )}
        </>
      ),
      action: 'Install and reboot',
      danger: true,
    })
    if (!ok) return
    const health = { wait_for_health: waitHealth, health_timeout_seconds: Number(timeout) || 0, insecure_skip_signature_check: allowUnsigned }
    const result = await run(async () => {
      if (mode === 'url') return postJSON('/api/lifecycle/upgrade-url', { reference, sha256: sha, ...health })
      const form = new FormData()
      for (const f of FILES) form.append(f.field, files[f.field])
      form.append('sha256', sha)
      form.append('wait_for_health', String(waitHealth))
      form.append('health_timeout_seconds', String(Number(timeout) || 0))
      form.append('insecure_skip_signature_check', String(allowUnsigned))
      const resp = await fetch('/api/lifecycle/upgrade-upload', { method: 'POST', body: form })
      if (!resp.ok) throw new ApiError((await resp.text()).trim(), resp.status)
      return resp.json()
    })
    if (result) {
      toast(result.message || 'Update written - the node is rebooting')
      setFollowing({ boot: overview.data?.system?.boot_time_unix, from: v?.version })
    }
  }

  const ready = mode === 'url' ? !!reference : FILES.every((f) => files[f.field])
  return (
    <>
      <PageHeader title="Update" subtitle="Install a new Janus release on this node, A/B with automatic rollback" />
      {following && (
        <div style={{ marginBottom: '1rem' }}>
          <WaitForNode
            previousBoot={following.boot}
            label="Installing and rebooting"
            onBack={(o) => {
              const now = o.version?.version
              toast(now && now !== following.from ? `Back on ${now} (slot ${o.version?.active_slot})` : `The node is back on ${now} - if that's the old version, the new one may have been reverted`, now !== following.from ? 'ok' : 'warn')
              setFollowing(null)
              overview.reload()
            }}
          />
        </div>
      )}
      <div className="grid grid-2" style={{ marginBottom: '1rem' }}>
        <Card title="This node" icon={Archive}>
          <dl className="kv">
            <dt>Running</dt>
            <dd className="mono">{v?.version || '…'}</dd>
            <dt>Active slot</dt>
            <dd>{v?.active_slot ? <Badge tone="info">slot {v.active_slot}</Badge> : <span className="muted">not an A/B boot - updates need an installed node</span>}</dd>
          </dl>
        </Card>
        <Card title="Latest release" icon={Rocket} actions={rel?.html_url && <a href={rel.html_url} target="_blank" rel="noreferrer">Release notes <ExternalLink size={12} /></a>}>
          {release.error ? (
            <div className="muted">Unavailable: {String(release.error.message)}</div>
          ) : !rel ? (
            <div className="muted">…</div>
          ) : (
            <div className="stack">
              <div className="spread">
                <span className="mono">{rel.tag_name}</span>
                {updateAvailable ? <Badge tone="accent">Update available</Badge> : <Badge tone="ok">Up to date</Badge>}
              </div>
              {rel.published_at && <div className="muted small">Published {dateTime(Date.parse(rel.published_at))}</div>}
              <div>
                <button className="primary small" onClick={useLatest}>
                  Use this release
                </button>
              </div>
            </div>
          )}
        </Card>
      </div>
      <Card title="Install a release" icon={UploadIcon}>
        <Tabs
          tabs={[
            { id: 'url', label: 'From a URL (the node downloads it)' },
            { id: 'upload', label: 'Upload files (relayed by the Controller)' },
          ]}
          active={mode}
          onChange={setMode}
        />
        <div className="stack">
          {mode === 'url' ? (
            <label className="field">
              <span>Release bundle base URL</span>
              <input className="mono" placeholder="https://github.com/swenske/Janus/releases/download/v…" value={reference} onChange={(e) => setReference(e.target.value)} />
            </label>
          ) : (
            <>
              <p className="muted small" style={{ margin: 0 }}>
                For a node that can't reach the Internet: the four files of a release bundle are streamed to it through this Controller.
              </p>
              <div className="grid grid-2">
                {FILES.map((f) => (
                  <label key={f.field} className="field">
                    <span className="mono">{f.label}</span>
                    <input type="file" onChange={(e) => setFiles((all) => ({ ...all, [f.field]: e.target.files[0] }))} />
                  </label>
                ))}
              </div>
            </>
          )}
          <label className="field">
            <span>rootfs.squashfs sha256 (recommended)</span>
            <input className="mono" value={sha} onChange={(e) => setSha(e.target.value)} placeholder="64 hex characters" />
          </label>
          <div className="row">
            <label className="check">
              <input type="checkbox" checked={waitHealth} onChange={(e) => setWaitHealth(e.target.checked)} /> Revert automatically if HAProxy isn't healthy after the reboot
            </label>
            {waitHealth && <input type="number" min={0} style={{ width: '9rem' }} placeholder="timeout (60 s)" value={timeout} onChange={(e) => setTimeoutS(e.target.value)} />}
          </div>
          <label className="check">
            <input type="checkbox" checked={allowUnsigned} onChange={(e) => setAllowUnsigned(e.target.checked)} /> Accept a bundle not signed with a Janus release key (insecure - development builds only)
          </label>
          <p className="muted small" style={{ margin: 0 }}>
            The node checks the release signature before writing anything; the sha256 above is only an early consistency check.
          </p>
          <div>
            <button className="danger solid" disabled={busy || !ready || !!following || !v?.active_slot} onClick={launch}>
              <Rocket size={15} /> {busy ? (mode === 'upload' ? 'Uploading…' : 'Installing…') : 'Install and reboot…'}
            </button>
          </div>
        </div>
      </Card>
    </>
  )
}
