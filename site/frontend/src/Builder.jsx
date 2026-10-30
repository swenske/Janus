import { AlertTriangle, Check, Copy, Cpu, Download, ExternalLink, FlaskConical, Hammer, Layers, Loader2, Package, PackagePlus, RefreshCcw, Rocket, Terminal } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { requirementError, toggleExtension } from '@shared/requires.js'
import { Badge, Card, ErrorBox, Loading, useToast } from '@shared/ui.jsx'
import { DOCS, REPO, bytes, getJSON, postJSON } from './api.js'
import { extensionRequestURL, selectablePlatform, testerIssueURL } from './builder.js'

// The image builder, after factory.talos.dev: where the image runs, which
// release, which HAProxy branch and kernel track, which extensions -
// giving a schematic ID, then the images. The choices live in the URL, so
// a configuration can be shared.

function readQuery() {
  const q = new URLSearchParams(window.location.search)
  return {
    platform: q.get('platform') || '',
    version: q.get('version') || '',
    haproxy: q.get('haproxy') || '',
    kernel: q.get('kernel') || '',
    console: q.get('console') || '',
    extensions: (q.get('extensions') || '').split(',').filter(Boolean),
  }
}

function writeQuery(s) {
  const q = new URLSearchParams()
  if (s.platform) q.set('platform', s.platform)
  if (s.version) q.set('version', s.version)
  if (s.haproxy) q.set('haproxy', s.haproxy)
  if (s.kernel) q.set('kernel', s.kernel)
  if (s.console) q.set('console', s.console)
  if (s.extensions.length) q.set('extensions', s.extensions.join(','))
  const url = `/builder${q.toString() ? `?${q}` : ''}`
  window.history.replaceState(null, '', url)
}

const compact = new Intl.NumberFormat('en', { notation: 'compact' })

// Downloads is how often images with a choice were fetched in the last
// days: platforms' images (from this page, or a Controller creating a
// machine) and nodes' updates. The site counts what, never who
// (docs/image-factory.md).
function Downloads({ images = 0, updates = 0, days }) {
  const n = images + updates
  if (!n) return null
  const what = [images && `${images} image${images === 1 ? '' : 's'}`, updates && `${updates} update${updates === 1 ? '' : 's'}`].filter(Boolean).join(' and ')
  const said = `${what} downloaded in the last ${days} days`
  return (
    <span className="downloads" title={said}>
      <Download size={11} aria-hidden="true" />
      <span aria-hidden="true">{compact.format(n)}</span>
      <span className="sr-only">{said}</span>
    </span>
  )
}

// SuspendedPlatform is a target variants.mk suspends: shown, never
// selectable, saying why in plain text (read on a phone and by a screen
// reader alike), with a "Need tester" link to its issue form.
function SuspendedPlatform({ p }) {
  const id = `suspended-${p.id}`
  return (
    <div className="choice suspended" role="group" aria-labelledby={`${id}-name`} aria-describedby={`${id}-why`}>
      <span className="choice-title" id={`${id}-name`}>
        {p.name} {p.experimental && <Badge tone="warn">experimental</Badge>}
      </span>
      <span className="small">{p.description}</span>
      <span className="small suspended-why" id={`${id}-why`}>
        Suspended: not verified on real hardware yet - no image is built until someone can test it.
      </span>
      <span className="choice-meta">
        <Cpu size={12} aria-hidden="true" /> {p.arch}
      </span>
      <a className="badge accent need-tester" href={testerIssueURL(p.target)} target="_blank" rel="noopener">
        <FlaskConical size={12} aria-hidden="true" />
        Need tester
        <span className="sr-only">: offer to test {p.name} (a GitHub issue form, in a new tab)</span>
        <ExternalLink size={11} aria-hidden="true" />
      </a>
    </div>
  )
}

function Step({ n, title, done, children }) {
  return (
    <section className={`step-card ${done ? 'done' : ''}`}>
      <div className="step-head">
        <span className="step-n">{done ? <Check size={14} /> : n}</span>
        <h2>{title}</h2>
      </div>
      {children}
    </section>
  )
}

// VariantChoice picks a HAProxy branch or kernel track: the release's
// default (unset - the image follows it from release to release), or
// one of those it offers, pinned.
function VariantChoice({ variants, value, arch, onChange, follows, name, until, counts, days }) {
  const def = variants.find((v) => v.default)
  return (
    <div className="choice-grid">
      <button className={`choice ${value === '' ? 'selected' : ''}`} onClick={() => onChange('')}>
        <span className="choice-title">The default</span>
        <span className="muted small">{follows}</span>
        <span className="choice-meta">
          {name(def)} · {def.version}
        </span>
      </button>
      {variants.map((v) => {
        const ok = v.arches.includes(arch)
        return (
          <button key={v.name} disabled={!ok} className={`choice ${value === v.name ? 'selected' : ''} ${ok ? '' : 'disabled'}`} onClick={() => onChange(v.name)}>
            <span className="choice-title">{name(v)}</span>
            <span className="muted small">{until(v)}</span>
            <span className="choice-meta">
              {v.version} <Downloads {...counts?.[v.name]} days={days} />
            </span>
            {!ok && <span className="small warn-text">amd64 only</span>}
          </button>
        )
      })}
    </div>
  )
}

function CopyBlock({ text, label }) {
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      toast(`${label} copied`)
    } catch {
      toast('Select the text and copy it', 'warn')
    }
  }
  return (
    <div className="copy-block">
      <pre>{text}</pre>
      <button className="small ghost" onClick={copy} title={`Copy ${label}`}>
        <Copy size={14} />
      </button>
    </div>
  )
}

export default function Builder() {
  const [choice, setChoice] = useState(readQuery)
  const [platforms, setPlatforms] = useState(null)
  const [versions, setVersions] = useState(null)
  const [catalog, setCatalog] = useState(null)
  const [schematic, setSchematic] = useState(null)
  const [image, setImage] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  // What ticking or unticking an extension did to the others it requires
  // or that require it.
  const [extNote, setExtNote] = useState('')
  // How often each choice was downloaded (GET /api/v1/stats/downloads):
  // nothing shown without it.
  const [stats, setStats] = useState(null)
  const toast = useToast()

  useEffect(() => {
    getJSON('/api/v1/stats/downloads?days=30')
      .then(setStats)
      .catch(() => setStats(null))
  }, [])
  const days = stats?.days

  useEffect(() => {
    Promise.all([getJSON('/api/v1/platforms'), getJSON('/api/v1/versions')])
      .then(([p, v]) => {
        setPlatforms(p)
        setVersions(v)
        // A link or bookmark naming a suspended (or unknown) platform
        // selects nothing, and the address stops saying it does.
        setChoice((c) => {
          if (!c.platform || selectablePlatform(p, c.platform)) return c
          const next = { ...c, platform: '' }
          writeQuery(next)
          return next
        })
      })
      .catch(setError)
  }, [])

  const update = (patch) =>
    setChoice((c) => {
      const next = { ...c, ...patch }
      writeQuery(next)
      return next
    })

  const platform = platforms?.find((p) => p.id === selectablePlatform(platforms, choice.platform))
  // The newest release unless asked for another: never a pre-release (for
  // testing) by default.
  const latest = versions?.find((v) => !v.prerelease)
  const version = versions?.find((v) => v.version === choice.version) || (choice.version ? null : latest)
  const arch = platform?.arch

  // The release's extension catalog.
  useEffect(() => {
    if (!version) return
    setCatalog(null)
    getJSON(`/api/v1/versions/${version.version}/extensions`).then(setCatalog).catch(setError)
  }, [version])

  const available = useMemo(() => (catalog?.extensions || []).filter((e) => !arch || e.arches.includes(arch)), [catalog, arch])
  const chosen = choice.extensions.filter((e) => available.some((a) => a.name === e))
  // An extension without one it requires (a shared link, say): the image
  // factory's own refusal, before asking it.
  const missing = requirementError(chosen, catalog?.extensions, version?.version)
  // A branch or track this release offers for this architecture, else the
  // default.
  const offered = (list, name) => (list || []).some((v) => v.name === name && (!arch || v.arches.includes(arch)))
  const haproxy = offered(catalog?.haproxy, choice.haproxy) ? choice.haproxy : ''
  const kernel = offered(catalog?.kernel, choice.kernel) ? choice.kernel : ''
  // The console: screen alone, when the release offers the choice.
  const consoleChoice = (catalog?.console || []).includes(choice.console) ? choice.console : ''

  // The schematic of the current choices (posted so the site knows it).
  useEffect(() => {
    if (!version || !catalog) return
    setSchematic(null)
    setImage(null)
    const customization = { extensions: chosen }
    if (haproxy) customization.haproxy = haproxy
    if (kernel) customization.kernel = kernel
    if (consoleChoice) customization.console = consoleChoice
    postJSON('/api/v1/schematics', { customization })
      .then(setSchematic)
      .catch(setError)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [version, catalog, chosen.join(','), haproxy, kernel, consoleChoice])

  const loadImage = useCallback(() => {
    if (!schematic || !version || !arch) return Promise.resolve()
    return getJSON(`/api/v1/images/${schematic.id}/${version.version}/${arch}`).then(setImage).catch(setError)
  }, [schematic, version, arch])

  useEffect(() => {
    loadImage()
  }, [loadImage])

  // While it builds, follow it.
  useEffect(() => {
    if (image?.state !== 'building') return undefined
    const t = setInterval(loadImage, 10000)
    return () => clearInterval(t)
  }, [image?.state, loadImage])

  const build = async () => {
    setBusy(true)
    try {
      const st = await postJSON(`/api/v1/images/${schematic.id}/${version.version}/${arch}`)
      setImage(st)
      if (st.state === 'building') toast('Build started - it takes a few minutes')
    } catch (err) {
      toast(err, 'danger')
    } finally {
      setBusy(false)
    }
  }

  // The page keeps its heading while it loads, or when it can't.
  if (error && !platforms)
    return (
      <div className="builder">
        <h1>Image builder</h1>
        <ErrorBox error={error} />
      </div>
    )
  if (!platforms || !versions)
    return (
      <div className="builder">
        <h1>Image builder</h1>
        <Loading />
      </div>
    )

  const groups = [...new Set(platforms.map((p) => p.group))]
  const mainFile = image?.files?.find((f) => f.name === platform?.file)
  const bundle = (image?.files || []).filter((f) => ['rootfs.squashfs', 'rootfs.verity', 'uki-a.efi', 'uki-b.efi', 'rootfs.squashfs.sha256'].includes(f.name))
  const bundleBase = bundle[0]?.url.replace(/\/[^/]+$/, '')

  return (
    <div className="builder">
      <div className="builder-intro">
        <h1>Image builder</h1>
        <p className="muted">
          Pick where Janus runs, which release, its HAProxy branch and kernel track, and the optional extensions you want. Your choices form a <strong>schematic</strong>: the same choices
          always give the same ID, and nodes built from it get updates built from it - extensions, branch and track included.
        </p>
        {stats?.total && stats.total.images + stats.total.updates > 0 && (
          <p className="muted small">
            In the last {days} days: {stats.total.images.toLocaleString('en')} images and {stats.total.updates.toLocaleString('en')} updates downloaded.
          </p>
        )}
      </div>
      <ErrorBox error={error} />

      <Step n={1} title="Where will it run?" done={!!platform}>
        {groups.map((g) => (
          <div key={g} className="platform-group">
            <div className="muted small group-label">{g}</div>
            <div className="choice-grid">
              {platforms
                .filter((p) => p.group === g)
                .map((p) =>
                  p.suspended ? (
                    <SuspendedPlatform key={p.id} p={p} />
                  ) : (
                  <button key={p.id} className={`choice ${choice.platform === p.id ? 'selected' : ''}`} onClick={() => update({ platform: p.id })}>
                    <span className="choice-title">
                      {p.name} {p.experimental && <Badge tone="warn">experimental</Badge>}
                    </span>
                    <span className="muted small">{p.description}</span>
                    <span className="choice-meta">
                      <Cpu size={12} /> {p.arch} <Downloads images={stats?.platforms?.[p.id]} days={days} />
                    </span>
                  </button>
                  ),
                )}
            </div>
          </div>
        ))}
      </Step>

      {platform && (
        <Step n={2} title="Which release?" done={!!version}>
          <div className="row">
            <select aria-label="Release" value={version?.version || ''} onChange={(e) => update({ version: e.target.value })}>
              {versions.map((v) => (
                <option key={v.version} value={v.version}>
                  {v.version}
                  {v === latest ? ' (latest)' : ''}
                  {v.prerelease ? ' (pre-release)' : ''}
                  {v.schematics ? '' : ' - default image only'}
                </option>
              ))}
            </select>
            {version && (
              <a href={version.url} className="small">
                Release notes <ExternalLink size={12} />
              </a>
            )}
          </div>
          {version?.prerelease && (
            <div className="notice warn" style={{ marginTop: '0.75rem' }}>
              <strong>{version.version} is a pre-release</strong>, published to be tested before it becomes a release: it may still change or be withdrawn, and it isn&apos;t
              supported. Nodes are never offered it as an update.
            </div>
          )}
        </Step>
      )}

      {platform && version && (
        <Step n={3} title="HAProxy and kernel" done={!!catalog}>
          {!catalog ? (
            <Loading />
          ) : !catalog.haproxy?.length ? (
            <p className="muted">{version.version} predates the choice: its images come with its one HAProxy and kernel.</p>
          ) : (
            <div className="stack">
              <div>
                <div className="muted small group-label">
                  <Layers size={12} /> HAProxy LTS branch
                </div>
                <VariantChoice
                  variants={catalog.haproxy}
                  value={haproxy}
                  arch={arch}
                  onChange={(v) => update({ haproxy: v })}
                  follows="The newest LTS branch - the image moves to the next one with the release that makes it the default"
                  name={(v) => `HAProxy ${v.name}`}
                  until={(v) => `Stays on ${v.name}${v.eol ? ` - supported upstream until ${v.eol.slice(0, 7)}` : ''}`}
                  counts={stats?.haproxy}
                  days={days}
                />
              </div>
              <div>
                <div className="muted small group-label">
                  <Cpu size={12} /> Kernel track
                </div>
                <VariantChoice
                  variants={catalog.kernel}
                  value={kernel}
                  arch={arch}
                  onChange={(v) => update({ kernel: v })}
                  follows="The release's default track"
                  name={(v) => `${v.name.charAt(0).toUpperCase()}${v.name.slice(1)}`}
                  until={(v) => `kernel.org's newest ${v.name} release, from branch to branch`}
                  counts={stats?.kernel}
                  days={days}
                />
              </div>
              <p className="muted small" style={{ margin: 0 }}>
                Another HAProxy branch accepts another configuration: <a href={`${DOCS}/haproxy-config.md#haproxy-branches`}>what differs between branches</a>.
              </p>
              {catalog.console?.includes('screen') && (
                <div>
                  <div className="muted small group-label">
                    <Terminal size={12} /> Console
                  </div>
                  <div className="choice-grid">
                    <button className={`choice ${consoleChoice === '' ? 'selected' : ''}`} onClick={() => update({ console: '' })}>
                      <span className="choice-title">Serial and screen</span>
                      <span className="muted small">The default: what a hypervisor's serial console shows, what the Controller reads of its machines</span>
                      <span className="choice-meta">
                        console=tty0 console=ttyS0 <Downloads {...stats?.console?.serial} days={days} />
                      </span>
                    </button>
                    <button className={`choice ${consoleChoice === 'screen' ? 'selected' : ''}`} onClick={() => update({ console: 'screen' })}>
                      <span className="choice-title">Screen only</span>
                      <span className="muted small">
                        No serial console: an emulated serial port costs CPU for every byte written to it. The first boot's credentials show on the screen alone - for a node you install by hand and watch
                        another way.
                      </span>
                      <span className="choice-meta">
                        console=tty0 <Downloads {...stats?.console?.screen} days={days} />
                      </span>
                    </button>
                  </div>
                </div>
              )}
            </div>
          )}
        </Step>
      )}

      {platform && version && (
        <Step n={4} title="Extensions" done={!!catalog}>
          {!catalog ? (
            <Loading />
          ) : catalog.extensions.length === 0 ? (
            <p className="muted">{version.version} predates optional extensions: it offers the default image only. Pick a newer release to add extensions.</p>
          ) : (
            <div className="choice-grid">
              {catalog.extensions.map((e) => {
                const ok = e.arches.includes(arch)
                const on = chosen.includes(e.name)
                // The card is the toggle (no checkbox): a button pressed
                // or not, its project link beside it - a link can't sit
                // inside a button.
                return (
                  <div key={e.name} className={`choice ext ${on ? 'selected' : ''} ${ok ? '' : 'disabled'}`}>
                    <button
                      type="button"
                      className="ext-toggle"
                      aria-pressed={on}
                      disabled={!ok}
                      onClick={() => {
                        const r = toggleExtension(chosen, e.name, !on, catalog.extensions)
                        setExtNote(r.note)
                        update({ extensions: r.extensions })
                      }}
                    >
                      <span className="row">
                        <span className="choice-title">
                          <Package size={14} aria-hidden="true" /> {e.name}
                        </span>
                        <span className="muted small mono nowrap">
                          {e.version}
                          <Downloads {...stats?.extensions?.[e.name]} days={days} />
                        </span>
                        <span className={`ext-check ${on ? 'on' : ''}`} aria-hidden="true">
                          <Check size={13} />
                        </span>
                      </span>
                      <span className="muted small">{e.description}</span>
                      {e.requires?.length > 0 && <span className="small">Requires {e.requires.join(', ')}</span>}
                      {!ok && <span className="small warn-text">Not available for {arch}</span>}
                    </button>
                    {e.homepage && (
                      <a className="small ext-link" href={e.homepage}>
                        Project page<span className="sr-only"> of {e.name}</span> <ExternalLink size={11} aria-hidden="true" />
                      </a>
                    )}
                  </div>
                )
              })}
              <a className="choice ext-request" href={extensionRequestURL()} target="_blank" rel="noopener">
                <span className="choice-title">
                  <PackagePlus size={14} aria-hidden="true" /> Need something else…
                </span>
                <span className="muted small">An extension this release doesn't have: tell us what it is and what it's for.</span>
                <span className="small ext-request-go">
                  Request an extension <ExternalLink size={11} aria-hidden="true" />
                  <span className="sr-only">(a GitHub issue form, in a new tab)</span>
                </span>
              </a>
            </div>
          )}
          {catalog && missing && (
            <p className="small warn-text" role="alert">
              <AlertTriangle size={13} /> {missing}
            </p>
          )}
          {catalog && !missing && extNote && (
            <p className="small muted" role="status">
              {extNote}
            </p>
          )}
        </Step>
      )}

      {platform && version && schematic && (
        <Step n={5} title="Your image" done={image?.state === 'ready'}>
          <div className="grid grid-2">
            <Card title="Schematic">
              <div className="stack">
                <div>
                  <div className="muted small">ID</div>
                  <CopyBlock text={schematic.id} label="Schematic ID" />
                </div>
                <div>
                  <div className="muted small">Definition</div>
                  <CopyBlock text={schematic.yaml.trim()} label="Schematic" />
                </div>
                {schematic.default && <p className="muted small">No extension, the default HAProxy and kernel: this is the default schematic of the official releases.</p>}
              </div>
            </Card>
            <Card title={`${platform.name} · ${version.version} · ${arch}`}>
              {!image ? (
                <Loading />
              ) : image.state === 'ready' ? (
                <div className="stack">
                  {mainFile ? (
                    <a className="button primary big" href={mainFile.url}>
                      <Download size={17} /> Download {platform.file} {mainFile.size ? <span className="small">({bytes(mainFile.size)})</span> : null}
                    </a>
                  ) : (
                    <div className="notice warn">This release doesn&apos;t publish a {platform.name} image.</div>
                  )}
                  {mainFile?.sha256 && <div className="muted small mono break">sha256 {mainFile.sha256}</div>}
                  {platform.docs && (
                    // The page's permalink on the docs site: the file's path, docs/ dropped.
                    <a href={`${DOCS}/${platform.docs.replace(/^docs\//, '')}`} className="small">
                      How to install on {platform.name}
                    </a>
                  )}
                </div>
              ) : image.state === 'building' ? (
                <div className="stack">
                  <div className="row">
                    <Loader2 size={16} className="spin" /> Building - usually 5 to 15 minutes. This page follows it.
                  </div>
                  {image.run_url && (
                    <a href={image.run_url} className="small">
                      Build log <ExternalLink size={11} />
                    </a>
                  )}
                </div>
              ) : image.state === 'unavailable' ? (
                <div className="notice warn">
                  <AlertTriangle size={14} /> {image.message}
                </div>
              ) : (
                <div className="stack">
                  {image.state === 'failed' && (
                    <div className="notice warn">
                      <AlertTriangle size={14} /> The last build {image.message?.replace('the build ', '')}.{' '}
                      {image.run_url && <a href={image.run_url}>Build log</a>}
                    </div>
                  )}
                  <p className="muted" style={{ margin: 0 }}>
                    This combination hasn&apos;t been built yet. The build runs on the project&apos;s build machine, signs the image with the Janus release key, and publishes it here.
                  </p>
                  <div>
                    <button className="primary" disabled={busy} onClick={build}>
                      {image.state === 'failed' ? <RefreshCcw size={15} /> : <Hammer size={15} />} {image.state === 'failed' ? 'Retry the build' : 'Build this image'}
                    </button>
                  </div>
                </div>
              )}
            </Card>
          </div>

          {image?.state === 'ready' && arch === 'amd64' && (
            <Card title="Updates" icon={Rocket} className="updates-card">
              <p className="muted" style={{ marginTop: 0 }}>
                Nodes know their schematic and only accept updates built from it. The Janus Controller finds them on this site by itself; with janusctl:
              </p>
              {bundleBase ? (
                <CopyBlock text={`janusctl lifecycle upgrade -wait-for-health ${bundleBase}`} label="Upgrade command" />
              ) : (
                <p className="muted small">The update bundle of this release isn&apos;t published.</p>
              )}
              <details className="small">
                <summary>All files</summary>
                <table>
                  <tbody>
                    {image.files.map((f) => (
                      <tr key={f.name}>
                        <td>
                          <a href={f.url} className="mono">
                            {f.name}
                          </a>
                        </td>
                        <td className="muted">{bytes(f.size)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </details>
            </Card>
          )}
          <p className="muted small">
            See <a href={`${DOCS}/image-factory.md`}>image schematics and extensions</a> for what each extension, branch and track brings, and how updates keep them.
          </p>
        </Step>
      )}
    </div>
  )
}
