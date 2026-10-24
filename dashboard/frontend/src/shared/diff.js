// lineDiff returns the line-level edit script between a and b:
// [{op: ' ' | '-' | '+', text}]. Plain LCS - fine for config files of a
// few thousand lines.
export function lineDiff(a, b) {
  const x = a.split('\n')
  const y = b.split('\n')
  const n = x.length
  const m = y.length
  const lcs = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1))
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = x[i] === y[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    }
  }
  const out = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ op: ' ', text: x[i] })
      i++
      j++
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      out.push({ op: '-', text: x[i++] })
    } else {
      out.push({ op: '+', text: y[j++] })
    }
  }
  while (i < n) out.push({ op: '-', text: x[i++] })
  while (j < m) out.push({ op: '+', text: y[j++] })
  return out
}

// hunks keeps only changed lines plus `context` lines around them.
export function hunks(diff, context = 2) {
  const keep = new Array(diff.length).fill(false)
  diff.forEach((d, i) => {
    if (d.op !== ' ') for (let k = Math.max(0, i - context); k <= Math.min(diff.length - 1, i + context); k++) keep[k] = true
  })
  const out = []
  let skipped = false
  diff.forEach((d, i) => {
    if (keep[i]) {
      if (skipped && out.length) out.push({ op: '…', text: '' })
      out.push(d)
      skipped = false
    } else skipped = true
  })
  return out
}
