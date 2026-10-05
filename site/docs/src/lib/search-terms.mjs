// The docs' vocabulary, for completing what a reader types in the search
// field (search-suggest.ts) and for "did you mean" when nothing matches:
// - phrases: every page's title and sidebar label, its H2/H3 headings;
// - words: the words of those, the names in code (`haproxy.cfg`,
//   `wait_for_health`, `seed-controller`), the API's methods (api/proto)
//   and the extensions' names - Janus's own terms, first - then the
//   prose's words that come back at least twice; most frequent first.
// Served as search-terms.json (pages/search-terms.json.ts), loaded when
// the search opens.
import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { pageMeta } from './meta.mjs'
import { pages, repoRoot } from './structure.mjs'

const plain = (s) =>
  s
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/[`*_]/g, '')
    .trim()

export function searchTerms() {
  const phrases = new Map()
  const words = new Map()
  const addPhrase = (p) => {
    p = plain(p)
    if (p.length >= 3 && p.length <= 80) phrases.set(p, (phrases.get(p) ?? 0) + 1)
  }
  const addWord = (w, n = 1) => {
    if (/^[A-Za-z][A-Za-z0-9_.:-]{2,39}$/.test(w) && !/[.:-]$/.test(w)) words.set(w, (words.get(w) ?? 0) + n)
  }
  const prose = new Map()

  for (const page of pages) {
    const meta = pageMeta(page.file)
    for (const p of [page.title, page.label, meta.title]) if (p && p !== 'Overview') addPhrase(p)
    let fence = null
    for (const line of readFileSync(path.join(repoRoot, page.file), 'utf8').split('\n')) {
      const f = /^\s*(```+|~~~+)/.exec(line)
      if (f) {
        fence = fence ? (line.trim().startsWith(fence) ? null : fence) : f[1]
        continue
      }
      if (fence) continue
      const h = /^#{2,3} (.+)$/.exec(line)
      if (h) addPhrase(h[1])
      for (const [, code] of line.matchAll(/`([^`]+)`/g)) for (const w of code.split(/\s+/)) addWord(w.replace(/[(),;]+$/, ''))
      for (const w of plain(line).match(/[A-Za-z][A-Za-z'-]{4,}/g) ?? []) {
        const k = w.toLowerCase().replace(/'s$/, '')
        prose.set(k, (prose.get(k) ?? 0) + 1)
      }
    }
  }
  // A heading's words - not its little ones (the, and), acronyms aside.
  for (const p of phrases.keys()) for (const w of p.split(/[\s/(),:]+/)) if (w.length > 3 || /^[A-Z0-9]+$/.test(w)) addWord(w)

  // The API's methods and the extensions, named the way the docs use them.
  const protoDir = path.join(repoRoot, 'api/proto/janus/v1alpha1')
  for (const f of readdirSync(protoDir).filter((f) => f.endsWith('.proto'))) {
    for (const [, rpc] of readFileSync(path.join(protoDir, f), 'utf8').matchAll(/^\s*rpc (\w+)\s*\(/gm)) addWord(rpc, 2)
  }
  for (const e of readdirSync(path.join(repoRoot, 'extensions'), { withFileTypes: true })) {
    if (e.isDirectory()) addWord(e.name, 3)
  }

  // Janus's own terms complete before the prose's: "keep" is keepalived
  // before it's "keeps".
  for (const [w, n] of words) words.set(w, n * 10 + 5)
  for (const [w, n] of prose) if (n >= 2 && !words.has(w)) addWord(w, n)

  const ranked = (m) => [...m].sort((a, b) => b[1] - a[1] || a[0].length - b[0].length || a[0].localeCompare(b[0])).map(([t]) => t)
  // One spelling per word, case aside: the most frequent.
  const seen = new Set()
  const uniqueWords = ranked(words).filter((w) => !seen.has(w.toLowerCase()) && seen.add(w.toLowerCase()))
  return { phrases: ranked(phrases), words: uniqueWords }
}
