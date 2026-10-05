// Search, before and while typing (components/Search.astro). Pagefind's
// own interface shows results as you type, from an index built with the
// site and searched in the browser. This adds, still with no service:
// - before anything is typed, suggestions - the most read pages, a door
//   per audience;
// - while typing, the completion of what's being typed, in the field
//   itself (greyed; → or Tab takes it), from the docs' vocabulary
//   (search-terms.json: titles, headings, names in code, the API's
//   methods, the extensions);
// - "did you mean", when a word typed isn't the docs' but one close to
//   it is: Pagefind forgives the typo, this shows the spelling.

type Terms = { phrases: string[]; words: string[] }

const base = import.meta.env.BASE_URL.replace(/\/$/, '')
let terms: Promise<Terms> | null = null
const loadTerms = () =>
  (terms ??= fetch(`${base}/search-terms.json`)
    .then((r) => r.json() as Promise<Terms>)
    .catch(() => ({ phrases: [], words: [] })))

// completion is what to add to the typed value: the rest of a phrase it
// begins, else of a word its last word begins.
export function completion(value: string, t: Terms): string {
  if (value.trim().length < 2 || /\s$/.test(value)) return ''
  const v = value.toLowerCase()
  for (const p of t.phrases) if (p.length > value.length && p.toLowerCase().startsWith(v)) return p.slice(value.length)
  const last = value.split(/\s+/).pop() ?? ''
  if (last.length < 2) return ''
  const l = last.toLowerCase()
  for (const w of t.words) if (w.length > last.length && w.toLowerCase().startsWith(l)) return w.slice(last.length)
  return ''
}

function distance(a: string, b: string, max: number): number {
  if (Math.abs(a.length - b.length) > max) return max + 1
  let prev = Array.from({ length: b.length + 1 }, (_, i) => i)
  for (let i = 1; i <= a.length; i++) {
    const cur = [i]
    let best = i
    for (let j = 1; j <= b.length; j++) {
      cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1))
      best = Math.min(best, cur[j])
    }
    if (best > max) return max + 1
    prev = cur
  }
  return prev[b.length]
}

// correction is the query with each unknown word replaced by the
// vocabulary's closest one (the most frequent among the closest), or ''
// when there's nothing to correct.
export function correction(query: string, t: Terms): string {
  const known = new Set(t.words.map((w) => w.toLowerCase()))
  let changed = false
  const out = query.split(/(\s+)/).map((part) => {
    const w = part.toLowerCase()
    if (w.length < 4 || /\s/.test(part) || known.has(w)) return part
    const max = w.length <= 5 ? 1 : 2
    let best = ''
    let bestD = max + 1
    for (const cand of t.words) {
      const d = distance(w, cand.toLowerCase(), max)
      if (d < bestD) {
        best = cand
        bestD = d
        if (d === 1) break
      }
    }
    if (!best) return part
    changed = true
    return best
  })
  return changed ? out.join('') : ''
}

function setValue(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

function attach(root: HTMLElement, input: HTMLInputElement) {
  input.dataset.janus = '1'
  input.setAttribute('aria-autocomplete', 'inline')
  const ui = root.querySelector('.pagefind-ui') ?? root

  const template = document.getElementById('janus-search-suggestions') as HTMLTemplateElement | null
  const panel = template?.content.firstElementChild?.cloneNode(true) as HTMLElement | undefined
  if (panel) ui.after(panel)
  // Above the results, under the field: inside Pagefind's form, before
  // its results drawer (a node of ours there survives its updates).
  const didYouMean = document.createElement('p')
  didYouMean.className = 'janus-didyoumean'
  didYouMean.hidden = true
  const drawer = ui.querySelector('.pagefind-ui__drawer')
  if (drawer) drawer.before(didYouMean)
  else (panel ?? ui).after(didYouMean)

  // The completion, drawn over the field: the typed text, invisible, then
  // the rest, greyed.
  const ghost = document.createElement('div')
  ghost.className = 'janus-ghost'
  ghost.setAttribute('aria-hidden', 'true')
  const typed = document.createElement('span')
  const rest = document.createElement('span')
  rest.className = 'janus-ghost-rest'
  ghost.append(typed, rest)
  input.insertAdjacentElement('afterend', ghost)

  let suffix = ''
  const place = () => {
    const s = getComputedStyle(input)
    Object.assign(ghost.style, {
      left: `${input.offsetLeft}px`,
      top: `${input.offsetTop}px`,
      width: `${input.offsetWidth}px`,
      height: `${input.offsetHeight}px`,
      paddingLeft: s.paddingLeft,
      paddingRight: s.paddingRight,
      paddingTop: s.paddingTop,
      paddingBottom: s.paddingBottom,
      borderStyle: 'solid',
      borderColor: 'transparent',
      borderWidth: s.borderWidth,
      font: s.font,
      letterSpacing: s.letterSpacing,
      lineHeight: s.lineHeight,
    })
  }
  const update = async () => {
    const t = await loadTerms()
    const value = input.value
    if (panel) panel.hidden = value.trim() !== ''
    const caretAtEnd = input.selectionStart === value.length && input.selectionEnd === value.length
    suffix = caretAtEnd ? completion(value, t) : ''
    typed.textContent = value
    rest.textContent = suffix
    place()
    // No room to show it whole: no completion drawn.
    ghost.hidden = !suffix || input.scrollWidth > input.clientWidth
    didYouMean.hidden = true
  }
  input.addEventListener('input', update)
  input.addEventListener('focus', update)
  input.addEventListener('keyup', (e) => {
    if (e.key === 'ArrowLeft' || e.key === 'Home' || e.key === 'End') update()
  })
  input.addEventListener('keydown', (e) => {
    if (!suffix || ghost.hidden) return
    const atEnd = input.selectionStart === input.value.length
    if ((e.key === 'Tab' && !e.shiftKey) || (e.key === 'ArrowRight' && atEnd)) {
      e.preventDefault()
      setValue(input, input.value + suffix)
    }
  })

  // "Did you mean": once Pagefind has answered the query.
  let timer: ReturnType<typeof setTimeout> | undefined
  new MutationObserver(() => {
    clearTimeout(timer)
    timer = setTimeout(async () => {
      const query = input.value.trim()
      if (query === '' || !ui.querySelector('.pagefind-ui__message')) {
        didYouMean.hidden = true
        return
      }
      const fixed = correction(query, await loadTerms())
      didYouMean.hidden = !fixed
      if (!fixed) return
      didYouMean.replaceChildren('Did you mean ')
      const b = document.createElement('button')
      b.type = 'button'
      b.textContent = fixed
      b.addEventListener('click', () => {
        setValue(input, fixed)
        input.focus()
      })
      didYouMean.append(b, '?')
    }, 250)
  }).observe(ui, { childList: true, subtree: true, characterData: true })

  update()
}

const root = document.getElementById('starlight__search')
if (root) {
  const find = () => {
    const input = root.querySelector<HTMLInputElement>('input.pagefind-ui__search-input')
    if (input && !input.dataset.janus) attach(root, input)
  }
  new MutationObserver(find).observe(root, { childList: true, subtree: true })
  find()
}
