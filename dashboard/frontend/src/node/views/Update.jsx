import { Archive, ExternalLink, Puzzle, Rocket, Upload as UploadIcon } from 'lucide-react'
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
  const check = usePoll('/api/update-check', { every: 60000 })
  const overview = usePoll('/api/system/overview', { every: 0 })
  const [mode, setMode] = useState('url')
  const [reference, setReference] = useState('')
  const [sha, setSha] = useState('')
  const [files, setFiles] = useState({})
  const [waitHealth, setWaitHealth] = useState(true)
  const [timeout, setTimeoutS] = useState('')
  const [allowUnsigned, setAllowUnsigned] = useState(false)
  const [allowSchematic, setAllowSchematic] = useState(false)
  const [following, setFollowing] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()

  const v = overview.data?.version
  const uc = check.data
  const useLatest = () => {
    setMode('url')
    setReference(uc.bundle_base_url)
    setSha(uc.sha256 || '')
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
          {allowSchematic && (
            <p>
              <strong>The bundle may be built from another image schematic:</strong> the node then boots with that schematic's extensions, and loses the ones it doesn't have.
            </p>
          )}
        </>
      ),
      action: 'Install and reboot',
      danger: true,
    })
    if (!ok) return
    const health = { wait_for_health: waitHealth, health_timeout_seconds: Number(timeout) || 0, insecure_skip_signature_check: allowUnsigned, allow_schematic_change: allowSchematic }
    const result = await run(async () => {
      if (mode === 'url') return postJSON('/api/lifecycle/upgrade-url', { reference, sha256: sha, ...health })
      const form = new FormData()
      for (const f of FILES) form.append(f.field, files[f.field])
      form.append('sha256', sha)
      form.append('wait_for_health', String(waitHealth))
      form.append('health_timeout_seconds', String(Number(timeout) || 0))
      form.append('insecure_skip_signature_check', String(allowUnsigned))
      form.append('allow_schematic_change', String(allowSchematic))
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
            <dt>Image schematic</dt>
            <dd>
              {uc ? (
                <span className="mono" title={uc.schematic_id}>
                  {uc.schematic_id.slice(0, 12)}
                  {uc.default_schematic && <span className="muted"> (default)</span>}
                </span>
              ) : (
                '…'
              )}
            </dd>
            <dt>Extensions</dt>
            <dd>
              {uc?.extensions?.length ? (
                <span className="row" style={{ gap: '0.35rem', flexWrap: 'wrap' }}>
                  {uc.extensions.map((e) => (
                    <Badge key={e} tone="accent">
                      <Puzzle size={11} /> {e}
                    </Badge>
                  ))}
                </span>
              ) : (
                <span className="muted">none</span>
              )}
            </dd>
          </dl>
          {uc && !uc.default_schematic && (
            <p className="muted small" style={{ marginBottom: 0 }}>
              Updates must be built from this schematic, so the node keeps its extensions: they come from the image factory, not the plain GitHub release.
            </p>
          )}
        </Card>
        <Card
          title={uc && !uc.default_schematic ? 'Latest update for this schematic' : 'Latest release'}
          icon={Rocket}
          actions={uc?.release_url && (
            <a href={uc.release_url} target="_blank" rel="noreferrer">
              Release notes <ExternalLink size={12} />
            </a>
          )}
        >
          {check.error ? (
            <div className="muted">Unavailable: {String(check.error.message)}</div>
          ) : !uc ? (
            <div className="muted">…</div>
          ) : (
            <div className="stack">
              <div className="spread">
                <span className="mono">{uc.latest || '–'}</span>
                <UpdateBadge uc={uc} />
              </div>
              {uc.published_at && <div className="muted small">Published {dateTime(Date.parse(uc.published_at))}</div>}
              {uc.message && <div className={uc.state === 'building' ? 'muted small' : 'small'}>{uc.message}</div>}
              {uc.state === 'building' && <div className="muted small">The image factory is building it - this page checks again every minute.</div>}
              <div className="muted small">Source: {uc.source === 'github' ? 'GitHub Releases' : 'image factory'}</div>
              <div>
                <button className="primary small" onClick={useLatest} disabled={uc.state !== 'ready' || !uc.bundle_base_url}>
                  Use this update
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
          <label className="check">
            <input type="checkbox" checked={allowSchematic} onChange={(e) => setAllowSchematic(e.target.checked)} /> Accept a bundle built from another image schematic (changes the node's extensions)
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

// UpdateBadge is the update state of a node, from /api/update-check.
export function UpdateBadge({ uc }) {
  if (!uc) return null
  if (uc.state === 'building') return <Badge tone="info">Building</Badge>
  if (uc.state === 'failed') return <Badge tone="danger">Build failed</Badge>
  if (uc.state !== 'ready') return <Badge tone="warn">Unavailable</Badge>
  return uc.update_available ? <Badge tone="accent">Update available</Badge> : <Badge tone="ok">Up to date</Badge>
}
