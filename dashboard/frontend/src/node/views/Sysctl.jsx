import { Activity, ChevronDown, ChevronRight, ExternalLink, FlaskConical, History, Info, Lightbulb, Lock, Pencil, RotateCcw, ShieldAlert, ShieldCheck, SlidersHorizontal, Undo2, X } from 'lucide-react'
import { Fragment, useMemo, useState } from 'react'
import { postJSON } from '../api.js'
import { bytes, dateTime } from '../format.js'
import { useCountdown, usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'
import { Badge, Card, Empty, ErrorBox, Loading, PageHeader, useAction, useConfirm, useToast } from '../../shared/ui.jsx'

// System › Sysctl: the kernel parameters HAProxy depends on, changed on
// trial and kept once applied (internal/sysctl, docs/guide/kernel-tuning.md),
// and the CIS benchmark's, which nothing changes. The node checks every
// change against its whitelist and bounds; the checks here only spare a
// round trip.

const DOCS = 'https://janus.sw-servers.net/docs/guide/kernel-tuning.md'

const COMPONENTS = { pair: ['low', 'high'], triple: ['min', 'default', 'max'] }
// The node's own names for them, in its messages.
const COMPONENT_NAMES = { pair: ['the low end', 'the high end'], triple: ['the minimum', 'the default', 'the maximum'] }

// fmtNum shows a bound in its unit: bytes in binary steps.
function fmtNum(n, unit) {
  if (unit === 'bytes') return bytes(n, n % 1024 === 0 ? 0 : 1)
  return Number(n).toLocaleString('en-US')
}

// allowedText describes the values a parameter may take.
function allowedText(p) {
  if (p.kind === 'enum') return p.allowed.join(' · ')
  if (p.kind === 'ports') return `up to ${p.max_items} ports or ranges, ${p.bounds[0].min}–${p.bounds[0].max}`
  const names = COMPONENTS[p.kind]
  const parts = p.bounds.map((b, i) => `${names ? names[i] + ' ' : ''}${fmtNum(b.min, p.unit)}–${fmtNum(b.max, p.unit)}`)
  return parts.join(' · ') + (p.unit && p.unit !== 'bytes' ? ` ${p.unit}` : '')
}

// validate is the node's form and bounds checks, for instant feedback.
function validate(p, raw) {
  const s = String(raw).trim()
  if (p.kind === 'ports') {
    if (s === '') return null
    const items = s.split(',').map((x) => x.trim())
    if (items.length > p.max_items) return `${p.max_items} ports or ranges at most`
    for (const it of items) {
      const m = /^(\d+)(?:\s*-\s*(\d+))?$/.exec(it)
      if (!m) return `"${it}" isn't a port or a range`
      const lo = Number(m[1])
      const hi = m[2] ? Number(m[2]) : lo
      if (hi < lo) return `"${it}" isn't a port or a range`
      for (const port of [lo, hi]) if (port < p.bounds[0].min || port > p.bounds[0].max) return `port ${port} isn't between ${p.bounds[0].min} and ${p.bounds[0].max}`
    }
    return null
  }
  const parts = s.split(/\s+/).filter(Boolean)
  const want = { int: 1, enum: 1, pair: 2, triple: 3 }[p.kind]
  if (parts.length !== want) return want === 1 ? 'one integer expected' : `${want} integers expected, separated by spaces`
  if (!parts.every((x) => /^-?\d+$/.test(x))) return 'integers only'
  const nums = parts.map(Number)
  if (p.kind === 'enum') return p.allowed.includes(nums[0]) ? null : `must be one of ${p.allowed.join(', ')}`
  for (let i = 0; i < nums.length; i++) {
    const b = p.bounds[i]
    if (nums[i] < b.min || nums[i] > b.max) {
      const what = COMPONENT_NAMES[p.kind] ? COMPONENT_NAMES[p.kind][i] : 'the value'
      return `${what} must be between ${fmtNum(b.min, p.unit)} and ${fmtNum(b.max, p.unit)}`
    }
  }
  if (p.kind === 'triple' && !(nums[0] <= nums[1] && nums[1] <= nums[2])) return 'the minimum, default and maximum must be in that order'
  return null
}

// normalize puts a value the way the node compares them.
function normalize(p, raw) {
  const s = String(raw).trim()
  return p.kind === 'ports' ? s.replace(/\s+/g, '') : s.split(/\s+/).filter(Boolean).join(' ')
}

const shown = (v) => (v === '' || v == null ? '(none)' : v)

function mmss(seconds) {
  const m = Math.floor(seconds / 60)
  const s = String(seconds % 60).padStart(2, '0')
  return `${m}:${s}`
}

const APPLIES = {
  immediately: 'at once',
  new_connections: 'to new connections',
  haproxy_reload: 'when HAProxy opens its listeners - its next reload',
}

function stateBadge(p) {
  if (p.missing) return <Badge>absent</Badge>
  if (p.on_trial) return <Badge tone="warn" dot>on trial</Badge>
  if (p.saved) return <Badge tone="accent">saved</Badge>
  if (p.value !== p.default) return <Badge tone="info">differs</Badge>
  return <Badge>default</Badge>
}

export default function Sysctl() {
  const list = usePoll('/api/system/sysctl', { every: 5000 })
  const may = useMay()
  const canApply = may('SystemService/SysctlApply')
  const d = list.data

  if (list.error && !d) return <ErrorBox error={list.error} />
  if (!d) return <Loading />
  const editable = d.parameters.filter((p) => p.class === 'editable')
  const others = d.parameters.filter((p) => p.class !== 'editable')
  return (
    <>
      <PageHeader
        title="Sysctl"
        subtitle="The kernel parameters HAProxy depends on - tested on trial, kept once applied, back to Janus's defaults any time. The CIS benchmark's stay locked."
        actions={
          <a className="button small" href={DOCS} target="_blank" rel="noreferrer">
            Kernel tuning guide <ExternalLink size={12} />
          </a>
        }
      />
      <div className="stack">
        {list.error && <ErrorBox error={list.error} />}
        {!d.managed && (
          <div className="notice">
            This janusd doesn&apos;t run a Janus node: the values are those of the machine it runs on, and nothing can be changed from here.
          </div>
        )}
        {d.trial && <TrialBanner trial={d.trial} onDone={list.reload} />}
        <Tuning params={editable} managed={d.managed && canApply} trial={d.trial} onApplied={list.reload} />
        {d.observation && <Observed obs={d.observation} />}
        <CIS cis={d.cis} />
        <ReadOnly params={others} />
        {may('SystemService/SysctlHistory') && <HistoryCard key={d.trial ? d.trial.started_unix : 'none'} />}
      </div>
    </>
  )
}

function TrialBanner({ trial, onDone }) {
  const left = useCountdown(trial.revert_at_unix)
  const may = useMay()
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const n = trial.changes.length

  const apply = async () => {
    const ok = await confirm({
      title: 'Apply the values on trial?',
      body: (
        <>
          <p>The node saves them and applies them at every boot, after its CIS baseline.</p>
          <Changes changes={trial.changes} />
        </>
      ),
      action: 'Apply',
      wide: true,
    })
    if (!ok) return
    await run(() => postJSON('/api/system/sysctl/confirm', {}), 'Applied - the node keeps these values')
    onDone()
  }
  const cancel = () =>
    run(async () => {
      await postJSON('/api/system/sysctl/cancel', {})
      onDone()
    }, 'Cancelled - the previous values are back')

  return (
    <div className="notice warn stack" style={{ gap: '0.5rem' }}>
      <div className="row">
        <FlaskConical size={16} />
        <strong>
          {n} {n === 1 ? 'parameter' : 'parameters'} on trial
        </strong>
        <span className="small">
          - back to the previous values in <span className="mono">{mmss(left)}</span> unless applied
          {trial.actor?.name ? ` · tested by ${trial.actor.name}` : ''}
          {trial.haproxy_reloaded ? ' · HAProxy reloaded' : ''}
        </span>
        <span className="grow" />
        {may('SystemService/SysctlCancel') && (
          <button className="small" disabled={busy} onClick={cancel}>
            <Undo2 size={14} /> Cancel now
          </button>
        )}
        {may('SystemService/SysctlConfirm') && (
          <button className="primary small" disabled={busy} onClick={apply}>
            Apply
          </button>
        )}
      </div>
      <Changes changes={trial.changes} />
    </div>
  )
}

function Changes({ changes }) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Parameter</th>
            <th>Before</th>
            <th>After</th>
          </tr>
        </thead>
        <tbody>
          {changes.map((c) => (
            <tr key={c.name}>
              <td className="mono">{c.name}</td>
              <td className="mono">{shown(c.old)}</td>
              <td className="mono">
                {shown(c.new)}
                {c.reset && <span className="muted"> (default)</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Tuning({ params, managed, trial, onApplied }) {
  const [edits, setEdits] = useState({}) // name -> draft value
  const [serverErrors, setServerErrors] = useState({}) // name ("" for the request) -> message
  const [open, setOpen] = useState({})
  const [minutes, setMinutes] = useState('10')
  const [reload, setReload] = useState(true)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const byName = useMemo(() => Object.fromEntries(params.map((p) => [p.name, p])), [params])

  const pending = Object.entries(edits)
    .map(([name, value]) => ({ p: byName[name], value }))
    .filter(({ p }) => p)
  const errors = Object.fromEntries(pending.map(({ p, value }) => [p.name, validate(p, value)]).filter(([, e]) => e))
  const changed = pending.filter(({ p, value }) => normalize(p, value) !== p.value)
  const needsReload = changed.some(({ p }) => p.applies === 'haproxy_reload')
  const timeout = Math.min(60, Math.max(1, Number(minutes) || 10)) * 60

  const edit = (name, value) => {
    setEdits((e) => ({ ...e, [name]: value }))
    setServerErrors((e) => ({ ...e, [name]: undefined, '': undefined }))
  }
  const drop = (name) =>
    setEdits((e) => {
      const next = { ...e }
      delete next[name]
      return next
    })

  // submit puts changes on trial - after showing them - and reports the
  // node's refusals next to their parameters.
  const submit = async (body, rows, title, action) => {
    const ok = await confirm({
      title,
      body: (
        <>
          <p>
            The node checks them, then applies them <strong>on trial</strong>: unless you apply them within {timeout / 60} minutes, it puts the previous values back by itself.
            {body.reload_haproxy && needsReloadIn(rows) && ' HAProxy reloads, without dropping connections, to open its listeners with them - and again if they go back.'}
          </p>
          <Changes changes={rows} />
        </>
      ),
      action,
      wide: true,
    })
    if (!ok) return false
    const r = await run(() => postJSON('/api/system/sysctl/apply', { ...body, confirm_timeout_seconds: timeout }))
    if (!r) return false
    if (!r.accepted) {
      setServerErrors(Object.fromEntries((r.errors || []).map((e) => [e.name, e.message])))
      toast('Refused - nothing changed', 'danger')
      return false
    }
    toast('On trial - apply the values to keep them')
    onApplied()
    return true
  }

  const test = async () => {
    const rows = changed.map(({ p, value }) => ({ name: p.name, old: p.value, new: normalize(p, value) }))
    if (await submit({ changes: changed.map(({ p, value }) => ({ name: p.name, value })), reload_haproxy: reload }, rows, `Test ${rows.length === 1 ? 'this change' : `these ${rows.length} changes`}?`, 'Test on trial')) {
      setEdits({})
    }
  }
  const resetOne = (p) => submit({ changes: [{ name: p.name, reset: true }], reload_haproxy: reload }, [{ name: p.name, old: p.value, new: p.default, reset: true }], `Reset ${p.name}?`, 'Reset on trial')
  const differing = params.filter((p) => !p.missing && p.value !== p.default)
  const suggested = params.filter((p) => p.recommendation)
  const resetAll = () =>
    submit(
      { reset_all: true, reload_haproxy: reload },
      differing.map((p) => ({ name: p.name, old: p.value, new: p.default, reset: true })),
      'Reset every parameter to its default?',
      'Reset all on trial',
    )

  return (
    <Card
      title="HAProxy tuning"
      icon={SlidersHorizontal}
      actions={
        managed && (
          <button className="small" disabled={busy || differing.length === 0} onClick={resetAll} title="Back to Janus's defaults - on trial">
            <RotateCcw size={14} /> Reset all to defaults…
          </button>
        )
      }
    >
      <p className="small muted" style={{ marginTop: 0 }}>
        The only parameters the node lets anyone change - each within its bounds. A change goes on trial first; applied, it&apos;s saved and set again at every boot.
        {trial && ' Testing more while a trial runs adds to it.'}
      </p>
      {serverErrors[''] && <ErrorBox error={serverErrors['']} />}
      {suggested.length > 0 && (
        <div className="notice" style={{ marginBottom: '0.6rem' }}>
          <Lightbulb size={14} style={{ verticalAlign: '-2px' }} /> {suggested.length === 1 ? 'A value is' : `${suggested.length} values are`} suggested for this node, from its memory and what it observed -
          never applied by themselves{managed ? ': use one, test it on trial, then apply it.' : '.'}
        </div>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th style={{ width: '1.5rem' }} />
              <th>Parameter</th>
              <th>Value</th>
              <th>Janus default</th>
              <th>Allowed</th>
              <th>State</th>
              {managed && <th />}
            </tr>
          </thead>
          <tbody>
            {params.map((p) => {
              const editing = p.name in edits
              const err = (editing && errors[p.name]) || serverErrors[p.name]
              return (
                <Fragment key={p.name}>
                  <tr>
                    <td>
                      <button className="ghost icon" aria-label={open[p.name] ? 'Hide details' : 'Show details'} onClick={() => setOpen((o) => ({ ...o, [p.name]: !o[p.name] }))}>
                        {open[p.name] ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                      </button>
                    </td>
                    <td>
                      <div className="mono">{p.name}</div>
                      <div className="small muted">{p.summary}</div>
                    </td>
                    <td className="mono" style={{ minWidth: '9rem' }}>
                      {editing ? (
                        <div className="stack" style={{ gap: '0.25rem' }}>
                          <input
                            className="mono"
                            aria-label={`New value of ${p.name}`}
                            value={edits[p.name]}
                            onChange={(e) => edit(p.name, e.target.value)}
                            style={{ borderColor: err ? 'var(--danger)' : undefined }}
                          />
                          {err && <span className="small" style={{ color: 'var(--danger)', fontFamily: 'var(--font)' }}>{err}</span>}
                        </div>
                      ) : p.missing ? (
                        <span className="muted">({p.missing})</span>
                      ) : (
                        <>
                          {shown(p.value)}
                          {err && <div className="small" style={{ color: 'var(--danger)', fontFamily: 'var(--font)' }}>{err}</div>}
                          {p.recommendation && (
                            <div className="row small" style={{ gap: '0.25rem', marginTop: '0.25rem', fontFamily: 'var(--font)' }}>
                              <Badge tone="accent">
                                <Lightbulb size={11} /> <span className="mono">{p.recommendation.value}</span> suggested
                              </Badge>
                              <button className="ghost small" onClick={() => setOpen((o) => ({ ...o, [p.name]: true }))} aria-label={`Why ${p.recommendation.value} for ${p.name}`}>
                                Why
                              </button>
                              {managed && (
                                <button className="ghost small" onClick={() => edit(p.name, p.recommendation.value)} aria-label={`Use ${p.recommendation.value} for ${p.name}`}>
                                  Use
                                </button>
                              )}
                            </div>
                          )}
                        </>
                      )}
                    </td>
                    <td className="mono">
                      {shown(p.default)}
                      {p.default_dynamic && <div className="small muted" style={{ fontFamily: 'var(--font)' }}>the kernel&apos;s, at boot</div>}
                    </td>
                    <td className="small">{allowedText(p)}</td>
                    <td>{stateBadge(p)}</td>
                    {managed && (
                      <td className="nowrap">
                        {editing ? (
                          <button className="ghost icon" title="Drop this change" aria-label={`Drop the change of ${p.name}`} onClick={() => drop(p.name)}>
                            <X size={14} />
                          </button>
                        ) : (
                          <>
                            <button className="ghost icon" title="Change" aria-label={`Change ${p.name}`} disabled={!!p.missing} onClick={() => edit(p.name, p.value)}>
                              <Pencil size={14} />
                            </button>
                            <button className="ghost icon" title="Reset to Janus's default" aria-label={`Reset ${p.name}`} disabled={busy || !!p.missing || p.value === p.default} onClick={() => resetOne(p)}>
                              <RotateCcw size={14} />
                            </button>
                          </>
                        )}
                      </td>
                    )}
                  </tr>
                  {open[p.name] && (
                    <tr>
                      <td />
                      <td colSpan={managed ? 6 : 5}>
                        <Details p={p} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
      {managed && pending.length > 0 && (
        <div className="row" style={{ marginTop: '0.8rem' }}>
          <span className="small">
            {changed.length} {changed.length === 1 ? 'change' : 'changes'}
          </span>
          <span className="grow" />
          {needsReload && (
            <label className="check small" title="somaxconn and ip_nonlocal_bind reach HAProxy when it opens its listeners">
              <input type="checkbox" checked={reload} onChange={(e) => setReload(e.target.checked)} /> Reload HAProxy for them
            </label>
          )}
          <label className="row small" title="How long the node waits for you to apply the values before putting the previous ones back">
            Trial
            <input className="mono" aria-label="Trial minutes" inputMode="numeric" style={{ width: '3.5rem' }} value={minutes} onChange={(e) => setMinutes(e.target.value.replace(/\D/g, ''))} />
            min
          </label>
          <button disabled={busy} onClick={() => setEdits({})}>
            Discard
          </button>
          <button className="primary" disabled={busy || changed.length === 0 || Object.keys(errors).length > 0} onClick={test}>
            <FlaskConical size={15} /> Test on trial…
          </button>
        </div>
      )}
    </Card>
  )
}

function needsReloadIn(rows) {
  return rows.some((r) => /somaxconn|ip_nonlocal_bind/.test(r.name))
}

function Details({ p }) {
  return (
    <div className="stack small" style={{ gap: '0.4rem', padding: '0.2rem 0 0.4rem' }}>
      {p.recommendation && <Suggestion rec={p.recommendation} />}
      {p.effect && (
        <div>
          <strong>HAProxy:</strong> {p.effect}
        </div>
      )}
      {p.risk && (
        <div>
          <strong>Risk:</strong> {p.risk}
        </div>
      )}
      {p.applies && (
        <div>
          <strong>Applies:</strong> {APPLIES[p.applies]}
        </div>
      )}
      <div className="muted">
        Kernel default: <span className="mono">{p.kernel_default || '-'}</span>
        {p.haproxy_value && (
          <>
            {' '}
            · HAProxy recommends <span className="mono">{p.haproxy_value}</span>
          </>
        )}
        {p.saved && (
          <>
            {' '}
            · saved: <span className="mono">{shown(p.saved_value)}</span>
          </>
        )}
      </div>
      {p.warnings.map((w) => (
        <div key={w} className="notice warn">
          {w}
        </div>
      ))}
      <div className="row">
        {p.sources.map((s) => (
          <a key={s.url} href={s.url} target="_blank" rel="noreferrer">
            {s.title} <ExternalLink size={11} />
          </a>
        ))}
      </div>
    </div>
  )
}

// Suggestion is why a value is suggested: the rule, what it measured,
// where it comes from.
function Suggestion({ rec }) {
  return (
    <div className="notice">
      <div>
        <Lightbulb size={13} style={{ verticalAlign: '-2px' }} /> <strong>Suggested for this node:</strong> <span className="mono">{rec.value}</span>{' '}
        <span className="muted">- never applied by itself</span>
      </div>
      <div style={{ marginTop: '0.3rem' }}>{rec.rule}</div>
      <ul style={{ margin: '0.3rem 0 0', paddingLeft: '1.1rem' }}>
        {rec.measured.map((m) => (
          <li key={m.name}>
            <strong>{m.name}:</strong> {m.value}
            {m.window && <span className="muted"> - {m.window}</span>}
          </li>
        ))}
      </ul>
      <div className="row" style={{ marginTop: '0.3rem' }}>
        {rec.sources.map((s) => (
          <a key={s.url} href={s.url} target="_blank" rel="noreferrer">
            {s.title} <ExternalLink size={11} />
          </a>
        ))}
      </div>
    </div>
  )
}

// Observed is what the suggestions rest on: the signals the node
// watches, by hour.
function Observed({ obs }) {
  const [open, setOpen] = useState(false)
  const recurring = obs.signals.filter((s) => s.hours >= obs.min_hours).length
  return (
    <Card
      title="What the node observed"
      icon={Activity}
      actions={
        recurring > 0 ? (
          <Badge tone="warn" dot>
            {recurring} recurring
          </Badge>
        ) : (
          <Badge tone="ok" dot>
            nothing recurring
          </Badge>
        )
      }
    >
      <p className="small" style={{ marginTop: 0 }}>
        What the suggestions rest on, besides the node&apos;s memory: the kernel&apos;s counters and gauges, by hour, since {dateTime(obs.since_unix * 1000)}. A signal counts once
        seen in {obs.min_hours} different hours of the last {obs.window_hours / 24} days - never one peak.
      </p>
      <button className="small" onClick={() => setOpen(!open)}>
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />} {open ? 'Hide' : 'Show'} the {obs.signals.length} signals
      </button>
      {open && (
        <div className="table-wrap" style={{ marginTop: '0.6rem' }}>
          <table>
            <thead>
              <tr>
                <th>Signal</th>
                <th>Seen in</th>
                <th>At its highest</th>
                <th>Last seen</th>
              </tr>
            </thead>
            <tbody>
              {obs.signals.map((s) => (
                <tr key={s.id}>
                  <td>
                    <div>{s.title}</div>
                    <div className="small muted">
                      {s.measure} Counts when: {s.seen.charAt(0).toLowerCase() + s.seen.slice(1)}
                    </div>
                  </td>
                  <td className="nowrap">
                    {s.hours >= obs.min_hours ? (
                      <Badge tone="warn">
                        {s.hours} {s.hours === 1 ? 'hour' : 'hours'}
                      </Badge>
                    ) : (
                      <span className={s.hours ? '' : 'muted'}>
                        {s.hours} {s.hours === 1 ? 'hour' : 'hours'}
                      </span>
                    )}
                  </td>
                  <td className="small">{s.peak || <span className="muted">-</span>}</td>
                  <td className="small nowrap">{s.last_seen_unix ? dateTime(s.last_seen_unix * 1000) : <span className="muted">never</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}

function CIS({ cis }) {
  const [open, setOpen] = useState(false)
  const ok = cis.compliant === cis.total
  return (
    <Card
      title="CIS hardening"
      icon={ok ? ShieldCheck : ShieldAlert}
      actions={
        ok ? (
          <Badge tone="ok" dot>
            {cis.compliant}/{cis.total} compliant
          </Badge>
        ) : (
          <Badge tone="danger" dot>
            {cis.total - cis.compliant} not compliant
          </Badge>
        )
      }
    >
      <p className="small" style={{ marginTop: 0 }}>
        <Lock size={13} style={{ verticalAlign: '-2px' }} /> {cis.benchmark}, {cis.profile} - its kernel-parameter controls. Locked: no call changes them, every boot writes them again after
        the saved values, and every change is refused if it would break one.
      </p>
      {!ok && <div className="notice danger">Some controls don&apos;t hold: changes are refused until they do - a reboot writes the benchmark&apos;s values again.</div>}
      <button className="small" onClick={() => setOpen(!open)}>
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />} {open ? 'Hide' : 'Show'} the {cis.total} controls
      </button>
      {open && (
        <div className="table-wrap" style={{ marginTop: '0.6rem' }}>
          <table>
            <thead>
              <tr>
                <th>Control</th>
                <th>Key</th>
                <th>Value</th>
                <th>Expected</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {cis.controls.map((c) => (
                <tr key={c.key}>
                  <td className="nowrap">
                    {c.ids.join(', ')}
                    {c.level === 2 && <span className="muted small"> L2</span>}
                  </td>
                  <td className="mono">
                    {c.key}
                    {c.interfaces && <div className="small muted" style={{ fontFamily: 'var(--font)' }}>and on every interface</div>}
                  </td>
                  <td className="mono">{shown(c.value)}</td>
                  <td className="mono">{c.want.join(' or ')}</td>
                  <td>{c.compliant ? <Badge tone="ok">compliant</Badge> : <Badge tone="danger">{c.problems.join('; ')}</Badge>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}

function ReadOnly({ params }) {
  if (params.length === 0) return null
  return (
    <Card title="Shown, not changeable" icon={Info}>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Parameter</th>
              <th>Value</th>
              <th>Why it stays</th>
            </tr>
          </thead>
          <tbody>
            {params.map((p) => (
              <tr key={p.name}>
                <td>
                  <div className="mono">{p.name}</div>
                  <div className="small muted">{p.summary}</div>
                </td>
                <td className="mono">{p.missing ? <span className="muted">({p.missing})</span> : shown(p.value)}</td>
                <td className="small">
                  {p.class === 'forbidden' && <Badge tone="danger">forbidden</Badge>} {p.why}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  )
}

const ACTIONS = {
  trial: ['Tested', 'warn'],
  confirm: ['Applied', 'ok'],
  cancel: ['Cancelled', undefined],
  revert: ['Reverted - not applied in time', 'info'],
  'boot-refused': ['Refused at boot', 'danger'],
}

function HistoryCard() {
  const h = usePoll('/api/system/sysctl/history?limit=50', { every: 15000 })
  return (
    <Card title="History" icon={History}>
      {h.error && <ErrorBox error={h.error} />}
      {!h.data && !h.error && <Loading />}
      {h.data && h.data.entries.length === 0 && <Empty>No change yet: every parameter is as Janus ships it.</Empty>}
      {h.data && h.data.entries.length > 0 && (
        <div className="table-wrap" style={{ maxHeight: '28rem' }}>
          <table>
            <thead>
              <tr>
                <th>When</th>
                <th>Who</th>
                <th>What</th>
                <th>Changes</th>
              </tr>
            </thead>
            <tbody>
              {h.data.entries.map((e, i) => {
                const [label, tone] = ACTIONS[e.action] || [e.action]
                return (
                  <tr key={i}>
                    <td className="nowrap small">{dateTime(e.time_unix * 1000)}</td>
                    <td className="small">
                      {e.actor.name}
                      {e.actor.roles.length > 0 && <span className="muted"> ({e.actor.roles.join(', ')})</span>}
                      {e.actor.via && <div className="muted">via {e.actor.via}</div>}
                    </td>
                    <td>
                      <Badge tone={tone}>{label}</Badge>
                    </td>
                    <td className="small">
                      {e.changes.map((c) => (
                        <div key={c.name}>
                          <span className="mono">{c.name}</span>: <span className="mono">{shown(c.old)}</span> → <span className="mono">{shown(c.new)}</span>
                          {c.reset && <span className="muted"> (default)</span>}
                        </div>
                      ))}
                      {e.detail && <div className="muted">{e.detail}</div>}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}
