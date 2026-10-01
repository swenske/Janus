import { Archive, CircleCheck, ExternalLink, Loader2, Puzzle, RefreshCcw, Rocket, Upload as UploadIcon, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, Tabs, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
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
  // changing: the extensions panel is open; target: the update it prepared
  // (POST /api/factory/update), installed when the URL below is still its.
  const [changing, setChanging] = useState(false)
  const [target, setTarget] = useState(null)
  const installRef = useRef(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()

  const v = overview.data?.version
  const uc = check.data
  const useLatest = () => {
    setTarget(null)
    setMode('url')
    setReference(uc.bundle_base_url)
    setSha(uc.sha256 || '')
  }
  const applyTarget = useCallback((t) => {
    setTarget(t)
    setMode('url')
    setReference(t.bundle_base_url)
    setSha(t.sha256 || '')
    setAllowSchematic(t.schematic_change)
    installRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [])
  const targetActive = !!target && mode === 'url' && reference === target.bundle_base_url

  const launch = async () => {
    const slot = v?.active_slot === 'A' ? 'B' : 'A'
    const ok = await confirm({
      title: 'Install this release?',
      body: (
        <>
          <p>
            The new system is written to slot <strong>{slot}</strong> (the one not running), then the node reboots into it. The current slot stays intact, so{' '}
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
          {targetActive && target.schematic_change ? (
            <ExtensionChanges from={target.node_extensions} to={target.extensions} />
          ) : (
            allowSchematic && (
              <p>
                <strong>The bundle may be built from another image schematic:</strong> the node then boots with that schematic's extensions, and loses the ones it doesn't have.
              </p>
            )
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
        <Card
          title="This node"
          icon={Archive}
          actions={
            uc && (
              <button className="small" onClick={() => setChanging(true)} disabled={changing}>
                <Puzzle size={13} /> Change extensions…
              </button>
            )
          }
        >
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
      {changing && uc && <ChangeExtensions uc={uc} onReady={applyTarget} onClose={() => setChanging(false)} />}
      <div ref={installRef}>
        <Card title="Install a release" icon={UploadIcon}>
          {targetActive && (
            <div className="notice" style={{ marginBottom: '0.75rem' }}>
              Installs <strong className="mono">{target.latest}</strong>{' '}
              {target.source === 'github' ? 'from its GitHub release, ' : 'built by the image factory '}
              {target.extensions.length ? (
                <>
                  with <ExtensionBadges names={target.extensions} />
                </>
              ) : (
                'without extensions (the default image)'
              )}
              .
            </div>
          )}
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
      </div>
    </>
  )
}

function ExtensionBadges({ names }) {
  return (
    <span className="row" style={{ display: 'inline-flex', gap: '0.35rem', flexWrap: 'wrap', verticalAlign: 'middle' }}>
      {names.map((e) => (
        <Badge key={e} tone="accent">
          <Puzzle size={11} /> {e}
        </Badge>
      ))}
    </span>
  )
}

// ExtensionChanges says, in the confirmation, what the node gains and loses.
function ExtensionChanges({ from, to }) {
  const added = to.filter((e) => !from.includes(e))
  const removed = from.filter((e) => !to.includes(e))
  return (
    <>
      <p>
        <strong>The node's extensions change.</strong> After the reboot it runs {to.length ? <ExtensionBadges names={to} /> : 'no extension'}.
      </p>
      <ul className="small" style={{ margin: 0 }}>
        {added.length > 0 && <li>Added: {added.join(', ')}</li>}
        {removed.length > 0 && <li>Removed: {removed.join(', ')} - not in the new image, and its services stop.</li>}
      </ul>
    </>
  )
}

// ChangeExtensions picks another set of extensions for the node and asks
// the image factory (through the Controller) for an update built with
// them - the newest release, signed with the Janus release key. A build
// that doesn't exist yet is started and followed; once ready, onReady
// fills in the installation below, which stays the usual A/B update.
function ChangeExtensions({ uc, onReady, onClose }) {
  const catalog = usePoll('/api/factory/catalog', { every: 0 })
  const [selected, setSelected] = useState(() => new Set(uc.extensions))
  const [prep, setPrep] = useState(null)
  const [busy, run] = useAction()
  const asked = useRef([])

  const current = new Set(uc.extensions)
  const offered = catalog.data?.extensions || []
  const list = [...offered]
  for (const name of uc.extensions) if (!offered.some((e) => e.name === name)) list.push({ name, missing: true })
  const added = [...selected].filter((n) => !current.has(n)).sort()
  const removed = [...current].filter((n) => !selected.has(n)).sort()
  const changed = added.length + removed.length > 0

  const toggle = (name, on) => {
    setPrep(null)
    setSelected((s) => {
      const n = new Set(s)
      if (on) n.add(name)
      else n.delete(name)
      return n
    })
  }
  const prepare = async () => {
    asked.current = [...selected].sort()
    const r = await run(() => postJSON('/api/factory/update', { extensions: asked.current }))
    if (!r) return
    setPrep(r)
    if (r.state === 'ready' && r.bundle_base_url) onReady(r)
  }

  // Follow a build: ask again every 20 s until it's ready or failed.
  useEffect(() => {
    if (prep?.state !== 'building') return undefined
    const timer = setTimeout(async () => {
      try {
        const r = await postJSON('/api/factory/update', { extensions: asked.current })
        setPrep(r)
        if (r.state === 'ready' && r.bundle_base_url) onReady(r)
      } catch (err) {
        setPrep({ state: 'unavailable', message: err.message })
      }
    }, 20000)
    return () => clearTimeout(timer)
  }, [prep, onReady])

  let host = ''
  try {
    host = catalog.data ? new URL(catalog.data.factory).host : ''
  } catch {
    host = catalog.data?.factory || ''
  }
  return (
    <div style={{ marginBottom: '1rem' }}>
      <Card
        title="Change extensions"
        icon={Puzzle}
        actions={
          <button className="small ghost" onClick={onClose} title="Close">
            <X size={14} />
          </button>
        }
      >
        {catalog.error ? (
          <ErrorBox error={catalog.error} />
        ) : !catalog.data ? (
          <Loading label="Asking the image factory for its extensions…" />
        ) : (
          <div className="stack">
            <p className="muted small" style={{ margin: 0 }}>
              A node runs the extensions built into its image. The image factory ({host}) builds the newest release, {catalog.data.version}, with the extensions chosen
              here and signs it with the Janus release key; installing it is the usual update below - A/B, with automatic rollback.
            </p>
            <div className="ext-list">
              {list.map((e) => {
                const unsupported = !e.missing && Array.isArray(e.arches) && !e.arches.includes(uc.arch)
                const on = selected.has(e.name)
                return (
                  <label key={e.name} className={'ext-row' + (unsupported && !on ? ' disabled' : '')}>
                    <input type="checkbox" checked={on} disabled={unsupported && !on} onChange={(ev) => toggle(e.name, ev.target.checked)} />
                    <span className="mono">{e.name}</span>
                    {e.version && <span className="muted small">{e.version}</span>}
                    {current.has(e.name) && <Badge tone="info">installed</Badge>}
                    {unsupported && <Badge tone="warn">not for {uc.arch}</Badge>}
                    {e.missing && <Badge tone="warn">not in {catalog.data.version}</Badge>}
                    {e.description && <span className="ext-desc muted small">{e.description}</span>}
                  </label>
                )
              })}
            </div>
            <div className="small">
              {changed ? (
                <>
                  {added.length > 0 && <span>Adds {added.join(', ')}. </span>}
                  {removed.length > 0 && <span>Removes {removed.join(', ')}. </span>}
                </>
              ) : (
                <span className="muted">These are the node's extensions now - nothing to change.</span>
              )}
            </div>
            <Preparation prep={prep} />
            <div className="row">
              <button className="primary" disabled={!changed || busy || prep?.state === 'building'} onClick={prepare}>
                {prep?.state === 'failed' ? <RefreshCcw size={15} /> : <Rocket size={15} />}{' '}
                {busy ? 'Asking the image factory…' : prep?.state === 'failed' ? 'Try again' : 'Prepare the update'}
              </button>
            </div>
          </div>
        )}
      </Card>
    </div>
  )
}

function Preparation({ prep }) {
  if (!prep) return null
  const exts = prep.extensions.length ? prep.extensions.join(', ') : 'no extension'
  if (prep.state === 'ready' && prep.bundle_base_url)
    return (
      <div className="notice">
        <span className="row" style={{ gap: '0.4rem' }}>
          <CircleCheck size={15} /> <span className="mono">{prep.latest}</span> with {exts} is ready - it's filled in below: check it and install.
        </span>
      </div>
    )
  if (prep.state === 'building')
    return (
      <div className="notice">
        <span className="row" style={{ gap: '0.4rem' }}>
          <Loader2 size={15} className="spin" /> The image factory is building {prep.latest || 'the newest release'} with {exts} - usually 5 to 15 minutes.
        </span>
        <div className="muted small">This page asks again every 20 s. The build goes on if you leave: preparing the same extensions later finds it.</div>
      </div>
    )
  return (
    <div className="notice warn">
      {prep.state === 'failed' ? 'The build failed' : 'Not available'}
      {prep.message ? `: ${prep.message}` : '.'}
    </div>
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
