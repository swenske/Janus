// The docs' Mermaid diagrams (pre.mermaid, from remark-diagrams.mjs),
// drawn in the browser - Mermaid's renderer loaded only once a diagram
// comes near the screen: 250 KB a reader who never scrolls that far
// doesn't download, and nothing heavy before the page shows. Drawn again
// in the other theme when the reader switches, in the site's colours.
//
// A diagram wider than the text column is drawn smaller to fit it - a
// sequence of four actors ends up with 6 px text. Each one has a Zoom
// button: the viewer shows the diagram on the whole screen, drawn again
// at the size asked for (sharp at any zoom), zoomed with the wheel, a
// pinch, a double click or + and -, moved by dragging or the arrow keys.
const diagrams = [...document.querySelectorAll('pre.mermaid')]

if (diagrams.length) {
  let mermaid
  let queue = Promise.resolve()
  const theme = () => (document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light')

  // Mermaid's base theme in the page's own colours (janus.css's tokens):
  // the text, the page, the greys - Mermaid's default lavender and its
  // dark theme's grey on black matched neither theme of the site.
  function themeVariables() {
    const css = getComputedStyle(document.documentElement)
    const t = (name) => css.getPropertyValue(name).trim()
    const dark = theme() === 'dark'
    const text = t('--sl-color-white')
    const page = t('--sl-color-black')
    const box = dark ? t('--sl-color-gray-5') : t('--sl-color-gray-6')
    const border = t('--sl-color-gray-4')
    const line = t('--sl-color-gray-3')
    return {
      darkMode: dark,
      background: page,
      fontFamily: t('--sl-font'),
      fontSize: '16px',
      primaryColor: box,
      primaryTextColor: text,
      primaryBorderColor: border,
      secondaryColor: box,
      tertiaryColor: page,
      lineColor: line,
      textColor: text,
      titleColor: text,
      clusterBkg: page,
      clusterBorder: border,
      edgeLabelBackground: page,
      actorBkg: box,
      actorBorder: border,
      actorTextColor: text,
      actorLineColor: border,
      signalColor: text,
      signalTextColor: text,
      labelBoxBkgColor: box,
      labelBoxBorderColor: border,
      labelTextColor: text,
      loopTextColor: text,
      noteBkgColor: t('--sl-color-accent-low'),
      noteBorderColor: t('--sl-color-accent'),
      noteTextColor: text,
      activationBkgColor: box,
      activationBorderColor: border,
      sequenceNumberColor: page,
    }
  }

  // One drawing at a time: Mermaid draws in a shared scratch element.
  // Mermaid 12 lays out with ELK and draws in a new look by default:
  // dagre and the classic look keep the diagrams as they were drawn (and
  // ELK's 500 KB unloaded).
  const render = (id, source) =>
    (queue = queue.then(async () => {
      mermaid ??= (await import('mermaid')).default
      mermaid.initialize({ startOnLoad: false, theme: 'base', themeVariables: themeVariables(), layout: 'dagre', look: 'classic' })
      return (await mermaid.render(id, source)).svg
    }))

  async function draw(pre) {
    pre.dataset.source ??= pre.textContent
    const drawn = theme()
    try {
      pre.innerHTML = await render(`diagram-${diagrams.indexOf(pre)}`, pre.dataset.source)
      addZoom(pre)
    } catch (err) {
      pre.textContent = `Syntax error in this diagram: ${err.message}`
    }
    pre.dataset.drawn = drawn
  }

  const icon = (d) =>
    `<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${d}</svg>`
  const icons = {
    zoom: icon('<path d="M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"/>'),
    in: icon('<path d="M12 5v14M5 12h14"/>'),
    out: icon('<path d="M5 12h14"/>'),
    close: icon('<path d="M18 6 6 18M6 6l12 12"/>'),
  }
  // A diagram's name: its accTitle (Mermaid's <title>).
  const label = (pre) => pre.querySelector('svg title')?.textContent.trim() || 'Diagram'

  function addZoom(pre) {
    const button = document.createElement('button')
    button.type = 'button'
    button.className = 'diagram-zoom'
    button.setAttribute('aria-label', `Zoom: ${label(pre)}`)
    button.innerHTML = `${icons.zoom}<span>Zoom</span>`
    button.addEventListener('click', () => open(pre))
    pre.append(button)
    pre.querySelector('svg')?.addEventListener('click', () => open(pre))
  }

  let viewer = null

  async function open(pre) {
    if (viewer) return
    const dialog = document.createElement('dialog')
    dialog.className = 'diagram-viewer'
    dialog.setAttribute('aria-label', label(pre))
    dialog.innerHTML = `<div class="diagram-viewer-stage"></div>
      <div class="diagram-viewer-bar">
        <button type="button" data-do="out" aria-label="Zoom out" title="Zoom out (-)">${icons.out}</button>
        <button type="button" data-do="fit" aria-label="100 %, fit the screen" title="Fit the screen (0)">100 %</button>
        <button type="button" data-do="in" aria-label="Zoom in" title="Zoom in (+)">${icons.in}</button>
        <button type="button" data-do="close" aria-label="Close" title="Close (Esc)">${icons.close}</button>
      </div>
      <p class="diagram-viewer-hint">Wheel or pinch to zoom, drag to move</p>`
    const stage = dialog.querySelector('.diagram-viewer-stage')
    const percent = dialog.querySelector('[data-do="fit"]')
    const hint = dialog.querySelector('.diagram-viewer-hint')
    const v = { pre, stage, svg: null, w: 0, h: 0, s: 1, x: 0, y: 0, fitted: true }
    viewer = v
    document.body.append(dialog)
    dialog.showModal()
    const onResize = () => v.fitted && fit()
    dialog.addEventListener('close', () => {
      removeEventListener('resize', onResize)
      dialog.remove()
      viewer = null
      pre.querySelector('.diagram-zoom')?.focus()
    })

    const apply = () => {
      if (!v.svg) return
      v.svg.setAttribute('width', v.w * v.s)
      v.svg.setAttribute('height', v.h * v.s)
      v.svg.style.transform = `translate(${v.x}px, ${v.y}px)`
      // Its name holds what it shows (axe: label-content-name-mismatch):
      // a speech user says what they see.
      percent.textContent = `${Math.round(v.s * 100)} %`
      percent.setAttribute('aria-label', `${percent.textContent}, fit the screen`)
    }
    const fit = () => {
      const r = stage.getBoundingClientRect()
      v.s = Math.min(4, Math.max(0.05, Math.min(r.width / v.w, r.height / v.h) * 0.94))
      v.x = (r.width - v.w * v.s) / 2
      v.y = (r.height - v.h * v.s) / 2
      v.fitted = true
      apply()
    }
    // Zooms by factor around the stage's point (px, py): it stays put.
    const zoomAt = (factor, px, py) => {
      const s = Math.min(8, Math.max(0.05, v.s * factor))
      v.x = px - ((px - v.x) * s) / v.s
      v.y = py - ((py - v.y) * s) / v.s
      v.s = s
      v.fitted = false
      hint.hidden = true
      apply()
    }
    const move = (dx, dy) => {
      v.x += dx
      v.y += dy
      v.fitted = false
      hint.hidden = true
      apply()
    }
    const centre = () => {
      const r = stage.getBoundingClientRect()
      return [r.width / 2, r.height / 2]
    }
    const at = (e) => {
      const r = stage.getBoundingClientRect()
      return [e.clientX - r.left, e.clientY - r.top]
    }
    v.apply = apply

    dialog.querySelector('.diagram-viewer-bar').addEventListener('click', (e) => {
      const what = e.target.closest('button')?.dataset.do
      if (what === 'in') zoomAt(1.5, ...centre())
      if (what === 'out') zoomAt(1 / 1.5, ...centre())
      if (what === 'fit') fit()
      if (what === 'close') dialog.close()
    })
    stage.addEventListener(
      'wheel',
      (e) => {
        e.preventDefault()
        zoomAt(Math.exp(-e.deltaY * (e.deltaMode === 1 ? 0.05 : 0.002)), ...at(e))
      },
      { passive: false },
    )
    stage.addEventListener('dblclick', (e) => zoomAt(2, ...at(e)))

    // One pointer drags; two pinch (the zoom follows their distance,
    // around their middle, which they also move).
    const pointers = new Map()
    let last = null
    const gesture = () => {
      const [a, b] = [...pointers.values()]
      return b ? { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2, d: Math.hypot(a.x - b.x, a.y - b.y) } : { x: a.x, y: a.y, d: 0 }
    }
    stage.addEventListener('pointerdown', (e) => {
      stage.setPointerCapture(e.pointerId)
      pointers.set(e.pointerId, { x: e.clientX, y: e.clientY })
      last = gesture()
      stage.classList.add('dragging')
    })
    stage.addEventListener('pointermove', (e) => {
      if (!pointers.has(e.pointerId)) return
      pointers.set(e.pointerId, { x: e.clientX, y: e.clientY })
      const g = gesture()
      if (last.d && g.d) {
        const r = stage.getBoundingClientRect()
        zoomAt(g.d / last.d, g.x - r.left, g.y - r.top)
      }
      move(g.x - last.x, g.y - last.y)
      last = g
    })
    const up = (e) => {
      pointers.delete(e.pointerId)
      last = pointers.size ? gesture() : null
      if (!pointers.size) stage.classList.remove('dragging')
    }
    stage.addEventListener('pointerup', up)
    stage.addEventListener('pointercancel', up)

    dialog.addEventListener('keydown', (e) => {
      const step = 60
      const keys = {
        '+': () => zoomAt(1.5, ...centre()),
        '=': () => zoomAt(1.5, ...centre()),
        '-': () => zoomAt(1 / 1.5, ...centre()),
        _: () => zoomAt(1 / 1.5, ...centre()),
        0: fit,
        ArrowLeft: () => move(step, 0),
        ArrowRight: () => move(-step, 0),
        ArrowUp: () => move(0, step),
        ArrowDown: () => move(0, -step),
      }
      if (keys[e.key] && !e.ctrlKey && !e.metaKey && !e.altKey) {
        e.preventDefault()
        keys[e.key]()
      }
    })
    addEventListener('resize', onResize)

    await show(v)
    if (viewer === v) fit()
  }

  // The viewer's own drawing of its diagram, in the current theme - its
  // own ids: a copy of the page's drawing would repeat them.
  async function show(v) {
    const svg = await render(`diagram-${diagrams.indexOf(v.pre)}-zoomed`, v.pre.dataset.source)
    if (viewer !== v) return
    v.stage.innerHTML = svg
    v.svg = v.stage.querySelector('svg')
    v.svg.style.maxWidth = 'none'
    const box = (v.svg.getAttribute('viewBox') || '').split(/[\s,]+/).map(Number)
    v.w = box[2] || v.svg.getBBox().width
    v.h = box[3] || v.svg.getBBox().height
    v.apply?.()
  }

  const near = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue
        near.unobserve(e.target)
        draw(e.target)
      }
    },
    { rootMargin: '50% 0px' },
  )
  for (const pre of diagrams) near.observe(pre)

  new MutationObserver(() => {
    for (const pre of diagrams) if (pre.dataset.drawn && pre.dataset.drawn !== theme()) draw(pre)
    if (viewer) show(viewer)
  }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
}
