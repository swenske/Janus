import { AlertTriangle, Check, Copy, Cpu, Download, ExternalLink, Hammer, Loader2, Package, RefreshCcw, Rocket } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Badge, Card, ErrorBox, Loading, useToast } from '@shared/ui.jsx'
import { DOCS, REPO, bytes, getJSON, postJSON } from './api.js'

// The image builder, after factory.talos.dev: where the image runs, which
// release, which extensions - giving a schematic ID, then the images. The
// choices live in the URL, so a configuration can be shared.

function readQuery() {
  const q = new URLSearchParams(window.location.search)
  return {
    platform: q.get('platform') || '',
    version: q.get('version') || '',
    extensions: (q.get('extensions') || '').split(',').filter(Boolean),
  }
}

function writeQuery(s) {
  const q = new URLSearchParams()
  if (s.platform) q.set('platform', s.platform)
  if (s.version) q.set('version', s.version)
  if (s.extensions.length) q.set('extensions', s.extensions.join(','))
  const url = `/builder${q.toString() ? `?${q}` : ''}`
  window.history.replaceState(null, '', url)
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
  const toast = useToast()

  useEffect(() => {
    Promise.all([getJSON('/api/v1/platforms'), getJSON('/api/v1/versions')])
      .then(([p, v]) => {
        setPlatforms(p)
        setVersions(v)
      })
      .catch(setError)
  }, [])

  const update = (patch) =>
    setChoice((c) => {
      const next = { ...c, ...patch }
      writeQuery(next)
      return next
    })

  const platform = platforms?.find((p) => p.id === choice.platform)
  const version = versions?.find((v) => v.version === choice.version) || (choice.version ? null : versions?.[0])
  const arch = platform?.arch

  // The release's extension catalog.
  useEffect(() => {
    if (!version) return
    setCatalog(null)
    getJSON(`/api/v1/versions/${version.version}/extensions`).then(setCatalog).catch(setError)
  }, [version])

  const available = useMemo(() => (catalog?.extensions || []).filter((e) => !arch || e.arches.includes(arch)), [catalog, arch])
  const chosen = choice.extensions.filter((e) => available.some((a) => a.name === e))

  // The schematic of the current choices (posted so the site knows it).
  useEffect(() => {
    if (!version || !catalog) return
    setSchematic(null)
    setImage(null)
    postJSON('/api/v1/schematics', { customization: { extensions: chosen } })
      .then(setSchematic)
      .catch(setError)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [version, catalog, chosen.join(',')])

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

  if (error && !platforms) return <ErrorBox error={error} />
  if (!platforms || !versions) return <Loading />

  const groups = [...new Set(platforms.map((p) => p.group))]
  const mainFile = image?.files?.find((f) => f.name === platform?.file)
  const bundle = (image?.files || []).filter((f) => ['rootfs.squashfs', 'rootfs.verity', 'uki-a.efi', 'uki-b.efi', 'rootfs.squashfs.sha256'].includes(f.name))
  const bundleBase = bundle[0]?.url.replace(/\/[^/]+$/, '')

  return (
    <div className="builder">
      <div className="builder-intro">
        <h1>Image builder</h1>
        <p className="muted">
          Pick where Janus runs, which release, and the optional extensions you want. Your choices form a <strong>schematic</strong>: the same choices always give the same ID, and nodes
          built from it get updates built from it - extensions included.
        </p>
      </div>
      <ErrorBox error={error} />

      <Step n={1} title="Where will it run?" done={!!platform}>
        {groups.map((g) => (
          <div key={g} className="platform-group">
            <div className="muted small group-label">{g}</div>
            <div className="choice-grid">
              {platforms
                .filter((p) => p.group === g)
                .map((p) => (
                  <button key={p.id} className={`choice ${choice.platform === p.id ? 'selected' : ''}`} onClick={() => update({ platform: p.id })}>
                    <span className="choice-title">
                      {p.name} {p.experimental && <Badge tone="warn">experimental</Badge>}
                    </span>
                    <span className="muted small">{p.description}</span>
                    <span className="choice-meta">
                      <Cpu size={12} /> {p.arch}
                    </span>
                  </button>
                ))}
            </div>
          </div>
        ))}
      </Step>

      {platform && (
        <Step n={2} title="Which release?" done={!!version}>
          <div className="row">
            <select value={version?.version || ''} onChange={(e) => update({ version: e.target.value })}>
              {versions.map((v, i) => (
                <option key={v.version} value={v.version}>
                  {v.version}
                  {i === 0 ? ' (latest)' : ''}
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
        </Step>
      )}

      {platform && version && (
        <Step n={3} title="Extensions" done={!!catalog}>
          {!catalog ? (
            <Loading />
          ) : catalog.extensions.length === 0 ? (
            <p className="muted">{version.version} predates optional extensions: it offers the default image only. Pick a newer release to add extensions.</p>
          ) : (
            <div className="choice-grid">
              {catalog.extensions.map((e) => {
                const ok = e.arches.includes(arch)
                const on = chosen.includes(e.name)
                return (
                  <label key={e.name} className={`choice ext ${on ? 'selected' : ''} ${ok ? '' : 'disabled'}`}>
                    <span className="row">
                      <input
                        type="checkbox"
                        disabled={!ok}
                        checked={on}
                        onChange={() => update({ extensions: on ? chosen.filter((x) => x !== e.name) : [...chosen, e.name] })}
                      />
                      <span className="choice-title">
                        <Package size={14} /> {e.name}
                      </span>
                      <span className="muted small mono">{e.version}</span>
                    </span>
                    <span className="muted small">{e.description}</span>
                    {!ok && <span className="small warn-text">Not available for {arch}</span>}
                    {e.homepage && (
                      <a className="small" href={e.homepage} onClick={(ev) => ev.stopPropagation()}>
                        Project page <ExternalLink size={11} />
                      </a>
                    )}
                  </label>
                )
              })}
            </div>
          )}
        </Step>
      )}

      {platform && version && schematic && (
        <Step n={4} title="Your image" done={image?.state === 'ready'}>
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
                {schematic.default && <p className="muted small">No extension: this is the default schematic of the official releases.</p>}
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
                    <a href={`${REPO}/blob/main/${platform.docs}`} className="small">
                      How to install on {platform.name} <ExternalLink size={11} />
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
            See <a href={`${DOCS}/image-factory.md`}>image schematics and extensions</a> for what each extension does and how updates keep them.
          </p>
        </Step>
      )}
    </div>
  )
}
