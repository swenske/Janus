import { Archive, CircleCheck, ExternalLink, Layers, Loader2, Puzzle, RefreshCcw, Rocket, Upload as UploadIcon, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, postJSON } from '../api.js'
import { apiURL } from '../../shared/base.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, Tabs, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { bytes, dateTime } from '../format.js'
import { useNodeStatus, usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'
import { WaitForNode } from '../waitForNode.jsx'
import { SecurityBadge } from '../../SecurityBadge.jsx'
import { securityText, securityTone } from '../../severity.js'
import { SupportBadge } from '../../SupportBadge.jsx'
import { variantNote } from '../../variants.js'

const FILES = [
  { field: 'rootfs_squashfs', label: 'rootfs.squashfs' },
  { field: 'rootfs_verity', label: 'rootfs.verity' },
  { field: 'uki_a', label: 'uki-a.efi' },
  { field: 'uki_b', label: 'uki-b.efi' },
]

export default function Update({ route = '' }) {
  const { check, overview } = useNodeStatus()
  // How the bundle reaches the node, remembered per browser: "url" (the
  // node downloads it), "relay" (the Controller downloads it and pushes
  // it to the node), "upload" (files from this computer).
  const [mode, setModeState] = useState(() => {
    try {
      return localStorage.getItem('janus.update.delivery') || 'url'
    } catch {
      return 'url'
    }
  })
  const setMode = (m) => {
    setModeState(m)
    try {
      if (m !== 'upload') localStorage.setItem('janus.update.delivery', m)
    } catch {
      // no storage: not remembered
    }
  }
  // The Controller-pushed update's progress (relay mode).
  const [relay, setRelay] = useState(null)
  const [reference, setReference] = useState('')
  const [sha, setSha] = useState('')
  const [files, setFiles] = useState({})
  const [waitHealth, setWaitHealth] = useState(true)
  const [timeout, setTimeoutS] = useState('')
  const [allowUnsigned, setAllowUnsigned] = useState(false)
  const [allowSchematic, setAllowSchematic] = useState(false)
  const [following, setFollowing] = useState(null)
  // changing: the image panel (extensions, HAProxy branch, kernel track)
  // is open; target: the update it prepared (POST /api/factory/update),
  // installed when the URL below is still its. Apps › "Add or remove
  // apps…" opens the panel (?extensions).
  const [changing, setChanging] = useState(() => new URLSearchParams(route.split('?')[1] || '').has('extensions'))
  const [target, setTarget] = useState(null)
  const installRef = useRef(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const canUpgrade = useMay()('LifecycleService/Upgrade')

  const v = overview.data?.version
  const uc = check.data
  const useLatest = () => {
    if (uc.renamed) {
      // The update renames extensions: it's built from another schematic.
      applyTarget({
        latest: uc.latest,
        source: uc.source,
        bundle_base_url: uc.bundle_base_url,
        sha256: uc.sha256,
        extensions: uc.target_extensions,
        node_extensions: uc.extensions,
        renamed: uc.renamed,
        schematic_change: true,
      })
      return
    }
    setTarget(null)
    setModeState((m) => (m === 'upload' ? 'url' : m))
    setReference(uc.bundle_base_url)
    setSha(uc.sha256 || '')
  }
  const applyTarget = useCallback((t) => {
    setTarget(t)
    setModeState((m) => (m === 'upload' ? 'url' : m))
    setReference(t.bundle_base_url)
    setSha(t.sha256 || '')
    setAllowSchematic(t.schematic_change)
    installRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [])
  const byURL = mode === 'url' || mode === 'relay'
  const targetActive = !!target && byURL && reference === target.bundle_base_url
  // Another HAProxy branch may refuse the configuration: the automatic
  // revert stays on, whatever the checkbox says.
  const branchChange = targetActive && (target.haproxy || '') !== (target.node_haproxy || '')
  const revert = waitHealth || branchChange

  const launch = async () => {
    const slot = v?.active_slot === 'A' ? 'B' : 'A'
    const ok = await confirm({
      title: 'Install this release?',
      body: (
        <>
          <p>
            The new system is written to slot <strong>{slot}</strong> (the one not running), then the node reboots into it. The current slot stays intact, so <em>Rollback</em> can
            always go back.
          </p>
          {revert ? (
            <p>
              If HAProxy isn't healthy on the new slot within {timeout || 60} s, the node reverts to the current slot on its own.
              {branchChange && !waitHealth && ' (Always, for another HAProxy branch.)'}
            </p>
          ) : (
            <p className="muted">No automatic revert: the node stays on the new slot whatever happens.</p>
          )}
          {allowUnsigned && (
            <p>
              <strong>The bundle's signature won't be checked.</strong> Whoever could alter it on its way to the node controls what the node boots - only for a development build
              you produced yourself.
            </p>
          )}
          {targetActive && target.schematic_change ? (
            <ImageChanges target={target} />
          ) : (
            allowSchematic && (
              <p>
                <strong>The bundle may be built from another image schematic:</strong> the node then boots with that schematic's extensions, HAProxy branch and kernel track, and
                loses the extensions it doesn't have.
              </p>
            )
          )}
        </>
      ),
      action: 'Install and reboot',
      danger: true,
    })
    if (!ok) return
    const health = {
      wait_for_health: revert,
      health_timeout_seconds: Number(timeout) || 0,
      insecure_skip_signature_check: allowUnsigned,
      allow_schematic_change: allowSchematic,
    }
    const result = await run(async () => {
      if (mode === 'url') return postJSON('/api/lifecycle/upgrade-url', { reference, sha256: sha, ...health })
      if (mode === 'relay') return relayUpgrade({ reference, sha256: sha, ...health }, setRelay)
      const form = new FormData()
      for (const f of FILES) form.append(f.field, files[f.field])
      form.append('sha256', sha)
      form.append('wait_for_health', String(revert))
      form.append('health_timeout_seconds', String(Number(timeout) || 0))
      form.append('insecure_skip_signature_check', String(allowUnsigned))
      form.append('allow_schematic_change', String(allowSchematic))
      const resp = await fetch(apiURL('/api/lifecycle/upgrade-upload'), { method: 'POST', body: form })
      if (!resp.ok) throw new ApiError((await resp.text()).trim(), resp.status)
      return resp.json()
    })
    setRelay(null)
    if (result) {
      toast(result.message || 'Update written - the node is rebooting')
      setFollowing({ boot: overview.data?.system?.boot_time_unix, from: v?.version })
    }
  }

  const ready = byURL ? !!reference : FILES.every((f) => files[f.field])
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
              toast(
                now && now !== following.from
                  ? `Back on ${now} (slot ${o.version?.active_slot})`
                  : `The node is back on ${now} - if that's the old version, the new one may have been reverted`,
                now !== following.from ? 'ok' : 'warn',
              )
              setFollowing(null)
            }}
          />
        </div>
      )}
      <div className="grid grid-2" style={{ marginBottom: '1rem' }}>
        <Card
          title="This node"
          icon={Archive}
          actions={
            uc &&
            canUpgrade && (
              <button className="small" onClick={() => setChanging(true)} disabled={changing}>
                <Layers size={13} /> Change the image…
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
            <dt>HAProxy</dt>
            <dd>
              <span className="mono">{v?.haproxy?.version || '…'}</span>
              {v?.haproxy?.variant && <span className="muted small"> · {variantNote(v.haproxy, 'branch')}</span>} <SupportBadge support={uc?.support} />
            </dd>
            <dt>Kernel track</dt>
            <dd>
              {v?.kernel?.variant ? (
                <>
                  <span className="mono">{v.kernel.variant}</span>
                  <span className="muted small"> · {variantNote(v.kernel, 'version')}</span>
                </>
              ) : (
                <span className="muted">{v ? 'not said by this image' : '…'}</span>
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
              Updates must be built from this schematic, so the node keeps its extensions, HAProxy branch and kernel track: they come from the image factory, not the plain GitHub
              release.
            </p>
          )}
          {uc?.support?.retired ? (
            <div className="notice danger small" style={{ marginTop: '0.6rem' }}>
              HAProxy {uc.support.variant} is no longer offered: {uc.support.last_release} is the last release with it, and no newer update will come. Move the node to another branch
              with <em>Change the image…</em> - check its configuration against that branch first.
            </div>
          ) : (
            uc?.support?.soon && (
              <div className="notice warn small" style={{ marginTop: '0.6rem' }}>
                HAProxy {uc.support.variant}'s upstream support ends on {uc.support.eol}, and the releases stop offering it then. Plan a move to a newer branch with{' '}
                <em>Change the image…</em>.
              </div>
            )
          )}
        </Card>
        <Card
          title={uc && !uc.default_schematic ? 'Latest update for this schematic' : 'Latest release'}
          icon={Rocket}
          actions={
            uc?.release_url && (
              <a href={uc.release_url} target="_blank" rel="noreferrer">
                Release notes <ExternalLink size={12} />
              </a>
            )
          }
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
              {uc.security_update && (
                <div className={`notice small ${securityTone(uc.security_update)}`}>
                  {securityText(uc.security_update)}{' '}
                  {uc.security_release && (
                    <a href={`https://github.com/swenske/Janus/releases/tag/${uc.security_release}`} target="_blank" rel="noreferrer">
                      {uc.security_release}'s notes <ExternalLink size={12} />
                    </a>
                  )}
                </div>
              )}
              {uc.message && <div className={uc.state === 'building' ? 'muted small' : 'small'}>{uc.message}</div>}
              {uc.state === 'building' && <div className="muted small">The image factory is building it - this page checks again every minute.</div>}
              {uc.renamed && (
                <div className="small">
                  {Object.entries(uc.renamed).map(([from, to]) => (
                    <div key={from}>
                      <span className="mono">{from}</span> is now called <span className="mono">{to}</span>.
                    </div>
                  ))}
                  <span className="muted">The update is built with the new names: installing it changes the node's schematic (the same extensions).</span>
                </div>
              )}
              <div className="muted small">Source: {uc.source === 'github' ? 'GitHub Releases' : 'image factory'}</div>
              {canUpgrade && (
                <div>
                  <button className="primary small" onClick={useLatest} disabled={uc.state !== 'ready' || !uc.bundle_base_url}>
                    Use this update
                  </button>
                </div>
              )}
            </div>
          )}
        </Card>
      </div>
      {changing && uc && canUpgrade && <ChangeExtensions uc={uc} onReady={applyTarget} onClose={() => setChanging(false)} />}
      <div ref={installRef} hidden={!canUpgrade}>
        <Card title="Install a release" icon={UploadIcon}>
          {targetActive && (
            <div className="notice" style={{ marginBottom: '0.75rem' }}>
              Installs <strong className="mono">{target.latest}</strong> {target.source === 'github' ? 'from its GitHub release, ' : 'built by the image factory '}
              {target.extensions.length ? (
                <>
                  with <ExtensionBadges names={target.extensions} />
                </>
              ) : (
                'without extensions'
              )}
              {target.haproxy && <>, HAProxy {target.haproxy}</>}
              {target.kernel && <>, the {target.kernel} kernel track</>}
              {target.default_schematic && ' (the default image)'}.
            </div>
          )}
          <Tabs
            tabs={[
              { id: 'url', label: 'The node downloads it' },
              { id: 'relay', label: 'The Controller pushes it' },
              { id: 'upload', label: 'Upload files' },
            ]}
            active={mode}
            onChange={setMode}
          />
          <div className="stack">
            {byURL ? (
              <>
                <p className="muted small" style={{ margin: 0 }}>
                  {mode === 'url'
                    ? 'The node fetches the release bundle from this URL itself.'
                    : "For a node that can't reach the bundle's server: this Controller downloads it and streams it to the node, which then installs it."}
                </p>
                <label className="field">
                  <span>Release bundle base URL</span>
                  <input className="mono" placeholder="https://github.com/swenske/Janus/releases/download/v…" value={reference} onChange={(e) => setReference(e.target.value)} />
                </label>
              </>
            ) : (
              <>
                <p className="muted small" style={{ margin: 0 }}>
                  The four files of a release bundle, from this computer: streamed to the node through this Controller.
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
              <input type="checkbox" checked={allowUnsigned} onChange={(e) => setAllowUnsigned(e.target.checked)} /> Accept a bundle not signed with a Janus release key (insecure -
              development builds only)
            </label>
            <label className="check">
              <input type="checkbox" checked={allowSchematic} onChange={(e) => setAllowSchematic(e.target.checked)} /> Accept a bundle built from another image schematic (changes
              the node's extensions, HAProxy branch or kernel track)
            </label>
            <p className="muted small" style={{ margin: 0 }}>
              The node checks the release signature before writing anything; the sha256 above is only an early consistency check.
            </p>
            {relay && <RelayProgress p={relay} />}
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

// ImageChanges says, in the confirmation, what the node gains and loses:
// extensions, its HAProxy branch, its kernel track.
function ImageChanges({ target }) {
  const from = target.node_extensions || []
  const to = target.extensions || []
  const renamed = target.renamed || {}
  const renames = Object.entries(renamed).filter(([old, now]) => from.includes(old) && to.includes(now))
  const added = to.filter((e) => !from.includes(e) && !renames.some(([, now]) => now === e))
  const removed = from.filter((e) => !to.includes(e) && !renames.some(([old]) => old === e))
  const exts = renames.length + added.length + removed.length > 0
  const choice = (v, what) => (v ? `${what} ${v}` : `the release's default ${what}`)
  const haproxy = (target.haproxy || '') !== (target.node_haproxy || '')
  const kernel = (target.kernel || '') !== (target.node_kernel || '')
  return (
    <>
      <p>
        <strong>The node's image changes.</strong> After the reboot it runs {to.length ? <ExtensionBadges names={to} /> : 'no extension'}.
      </p>
      <ul className="small" style={{ margin: 0 }}>
        {renames.map(([old, now]) => (
          <li key={old}>
            Renamed: {old} → {now} - the same extension under its new name.
          </li>
        ))}
        {added.length > 0 && <li>Added: {added.join(', ')}</li>}
        {removed.length > 0 && <li>Removed: {removed.join(', ')} - not in the new image, and its services stop.</li>}
        {haproxy && (
          <li>
            HAProxy: {choice(target.node_haproxy, 'branch')} → <strong>{choice(target.haproxy, 'branch')}</strong>. Another branch may refuse the running configuration: check it
            against that branch first. The automatic revert stays on - and after a configuration using what only the new branch knows, a rollback boots a HAProxy that refuses
            it.
          </li>
        )}
        {kernel && (
          <li>
            Kernel: {choice(target.node_kernel, 'track')} → <strong>{choice(target.kernel, 'track')}</strong>.
          </li>
        )}
        {!exts && !haproxy && !kernel && <li>Same extensions, HAProxy branch and kernel track: another schematic for the same image.</li>}
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
  // null until changed: the node's extensions, under the catalog's names.
  const [selected, setSelected] = useState(null)
  // The HAProxy branch and kernel track: the node's own ("" for the
  // release's default) until changed.
  const [haproxy, setHAProxy] = useState(uc.haproxy || '')
  const [kernel, setKernel] = useState(uc.kernel || '')
  const [prep, setPrep] = useState(null)
  const [busy, run] = useAction()
  const asked = useRef([])

  const offered = catalog.data?.extensions || []
  // An extension renamed since the node's image: its successor (old -> new).
  const renamed = {}
  for (const name of uc.extensions) {
    if (offered.some((e) => e.name === name)) continue
    const next = offered.find((e) => (e.replaces || []).includes(name))
    if (next) renamed[name] = next.name
  }
  const current = new Set(uc.extensions.map((n) => renamed[n] || n))
  const formerName = (name) => Object.keys(renamed).find((old) => renamed[old] === name)
  const sel = selected || current
  const list = [...offered]
  for (const name of uc.extensions) if (!offered.some((e) => e.name === name) && !renamed[name]) list.push({ name, missing: true })
  const added = [...sel].filter((n) => !current.has(n)).sort()
  const removed = [...current].filter((n) => !sel.has(n)).sort()
  const branchChanged = haproxy !== (uc.haproxy || '')
  const trackChanged = kernel !== (uc.kernel || '')
  const changed = added.length + removed.length > 0 || branchChanged || trackChanged

  const toggle = (name, on) => {
    setPrep(null)
    setSelected((s) => {
      const n = new Set(s || current)
      if (on) n.add(name)
      else n.delete(name)
      return n
    })
  }
  const ready = (r) => onReady({ ...r, renamed: { ...renamed, ...(r.renamed || {}) }, node_haproxy: uc.haproxy || '', node_kernel: uc.kernel || '' })
  // The build follower calls the latest ready (a new one every render).
  const readyRef = useRef(ready)
  useEffect(() => {
    readyRef.current = ready
  })
  const prepare = async () => {
    asked.current = { extensions: [...sel].sort(), haproxy, kernel }
    const r = await run(() => postJSON('/api/factory/update', asked.current))
    if (!r) return
    setPrep(r)
    if (r.state === 'ready' && r.bundle_base_url) ready(r)
  }

  // Follow a build: ask again every 20 s until it's ready or failed.
  useEffect(() => {
    if (prep?.state !== 'building') return undefined
    const timer = setTimeout(async () => {
      try {
        const r = await postJSON('/api/factory/update', asked.current)
        setPrep(r)
        if (r.state === 'ready' && r.bundle_base_url) readyRef.current(r)
      } catch (err) {
        setPrep({ state: 'unavailable', message: err.message })
      }
    }, 20000)
    return () => clearTimeout(timer)
  }, [prep])

  let host = ''
  try {
    host = catalog.data ? new URL(catalog.data.factory).host : ''
  } catch {
    host = catalog.data?.factory || ''
  }
  return (
    <div style={{ marginBottom: '1rem' }}>
      <Card
        title="Change the image"
        icon={Layers}
        actions={
          <button className="small ghost" onClick={onClose} title="Close">
            <X size={14} />
          </button>
        }
      >
        {catalog.error ? (
          <ErrorBox error={catalog.error} />
        ) : !catalog.data ? (
          <Loading label="Asking the image factory what it builds…" />
        ) : (
          <div className="stack">
            <p className="muted small" style={{ margin: 0 }}>
              A node runs the extensions, HAProxy and kernel built into its image. The image factory ({host}) builds the newest release, {catalog.data.version}, with what is chosen
              here and signs it with the Janus release key; installing it is the usual update below - A/B, with automatic rollback.
            </p>
            {catalog.data.haproxy?.length > 0 && (
              <div className="grid grid-2">
                <VariantSelect
                  label="HAProxy LTS branch"
                  variants={catalog.data.haproxy}
                  value={haproxy}
                  current={uc.haproxy || ''}
                  arch={uc.arch}
                  name={(v) => `HAProxy ${v.name} - ${v.version}${v.eol ? `, supported until ${v.eol.slice(0, 7)}` : ''}`}
                  follows="the newest LTS branch, following it"
                  onChange={(x) => {
                    setPrep(null)
                    setHAProxy(x)
                  }}
                />
                <VariantSelect
                  label="Kernel track"
                  variants={catalog.data.kernel}
                  value={kernel}
                  current={uc.kernel || ''}
                  arch={uc.arch}
                  name={(v) => `${v.name} - ${v.version}`}
                  follows="the release's default track"
                  onChange={(x) => {
                    setPrep(null)
                    setKernel(x)
                  }}
                />
              </div>
            )}
            {branchChanged && (
              <div className="notice warn small">
                Another HAProxy branch accepts another configuration: check the node's against it before installing (a keyword it doesn't know fails the update, which then reverts
                - its automatic revert stays on).
              </div>
            )}
            <div className="ext-list">
              {list.map((e) => {
                const unsupported = !e.missing && Array.isArray(e.arches) && !e.arches.includes(uc.arch)
                const on = sel.has(e.name)
                return (
                  <label key={e.name} className={'ext-row' + (unsupported && !on ? ' disabled' : '')}>
                    <input type="checkbox" checked={on} disabled={unsupported && !on} onChange={(ev) => toggle(e.name, ev.target.checked)} />
                    <span className="mono">{e.name}</span>
                    {e.version && <span className="muted small">{e.version}</span>}
                    {current.has(e.name) && <Badge tone="info">{formerName(e.name) ? `installed as ${formerName(e.name)}` : 'installed'}</Badge>}
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
                  {branchChanged && <span>HAProxy: {haproxy ? `branch ${haproxy}` : 'the default branch'}. </span>}
                  {trackChanged && <span>Kernel: {kernel ? `the ${kernel} track` : 'the default track'}. </span>}
                </>
              ) : (
                <span className="muted">This is the node's image now - nothing to change.</span>
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

// VariantSelect picks a HAProxy branch or kernel track: the release's
// default (unset - the node follows it), or one the release offers.
function VariantSelect({ label, variants, value, current, arch, name, follows, onChange }) {
  const def = variants.find((v) => v.default)
  return (
    <label className="field">
      <span>{label}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">
          The default - {follows}
          {def ? ` (${def.name} now)` : ''}
          {current === '' ? ' · the node' : ''}
        </option>
        {variants.map((v) => (
          <option key={v.name} value={v.name} disabled={!v.arches.includes(arch)}>
            {name(v)}
            {current === v.name ? ' · the node' : ''}
            {v.arches.includes(arch) ? '' : ` · not for ${arch}`}
          </option>
        ))}
      </select>
    </label>
  )
}

function Preparation({ prep }) {
  if (!prep) return null
  let exts = prep.extensions.length ? prep.extensions.join(', ') : 'no extension'
  if (prep.haproxy) exts += `, HAProxy ${prep.haproxy}`
  if (prep.kernel) exts += `, the ${prep.kernel} kernel track`
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
        <div className="muted small">This page asks again every 20 s. The build goes on if you leave: preparing the same image later finds it.</div>
      </div>
    )
  return (
    <div className="notice warn">
      {prep.state === 'failed' ? 'The build failed' : 'Not available'}
      {prep.message ? `: ${prep.message}` : '.'}
    </div>
  )
}

// relayUpgrade runs POST /api/lifecycle/upgrade-relay, passing each
// progress line it streams (NDJSON) to onProgress; the last line says
// whether it worked.
async function relayUpgrade(body, onProgress) {
  const resp = await fetch(apiURL('/api/lifecycle/upgrade-relay'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  if (!resp.ok) throw new ApiError((await resp.text()).trim(), resp.status)
  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let last = null
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    for (let i = buf.indexOf('\n'); i >= 0; i = buf.indexOf('\n')) {
      const line = buf.slice(0, i).trim()
      buf = buf.slice(i + 1)
      if (!line) continue
      last = JSON.parse(line)
      if (last.stage === 'error') throw new ApiError(last.message, 502)
      onProgress(last)
    }
  }
  if (last?.stage !== 'done') throw new ApiError('The connection to the Controller ended before the update did', 502)
  return { message: last.message }
}

function RelayProgress({ p }) {
  if (p.stage === 'download') {
    const n = RELAY_FILES.indexOf(p.file) + 1
    const pct = p.total ? Math.min(100, (100 * (p.bytes || 0)) / p.total) : 0
    return (
      <div className="notice">
        <div className="small">
          Pushing <span className="mono">{p.file}</span> to the node ({n}/{RELAY_FILES.length}): {bytes(p.bytes || 0)}
          {p.total ? ` / ${bytes(p.total)}` : ''}
        </div>
        <div className="bar" style={{ marginTop: '0.4rem' }}>
          <div style={{ width: `${pct}%` }} />
        </div>
      </div>
    )
  }
  return (
    <div className="notice small">
      The node: {p.stage}
      {p.percent ? ` ${p.percent}%` : ''}
      {p.message ? ` - ${p.message}` : ''}
    </div>
  )
}

const RELAY_FILES = ['rootfs.squashfs', 'rootfs.verity', 'uki-a.efi', 'uki-b.efi']

// UpdateBadge is the update state of a node, from /api/update-check.
export function UpdateBadge({ uc }) {
  if (!uc) return null
  if (uc.state === 'building') return <Badge tone="info">Building</Badge>
  if (uc.state === 'failed') return <Badge tone="danger">Build failed</Badge>
  if (uc.state !== 'ready') return <Badge tone="warn">Unavailable</Badge>
  if (uc.security_update) return <SecurityBadge severity={uc.security_update}>Security update available</SecurityBadge>
  if (uc.support?.retired && !uc.update_available) return <SupportBadge support={uc.support} />
  return uc.update_available ? <Badge tone="accent">Update available</Badge> : <Badge tone="ok">Up to date</Badge>
}
