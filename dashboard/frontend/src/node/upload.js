// What the Update page's "Upload files" needs that a fetch doesn't give:
// the browser's progress sending the files (#627 - it showed
// "Uploading…" alone for the 28 s a bundle took at 1 MB/s), and the
// node's answer read as it comes (NDJSON) from an XMLHttpRequest.

// uploadPosition says where the browser is once `loaded` bytes of the
// request went out, files being sent in their order: which file (index,
// name), how much of it (bytes of size). The form's other fields and the
// multipart headers - a few hundred bytes - aren't counted: their share
// of a 40 MB bundle doesn't show.
export function uploadPosition(files, loaded) {
  let start = 0
  for (let i = 0; i < files.length; i++) {
    const { name, size } = files[i]
    if (loaded < start + size || i === files.length - 1) return { index: i, name, bytes: Math.max(0, Math.min(size, loaded - start)), size }
    start += size
  }
  return null
}

// ndjsonLines returns a reader of a response's text as it grows: given
// the text so far, it returns the lines completed since the last call,
// parsed - and the last line too, newline or not, once `end` says the
// response is whole.
export function ndjsonLines() {
  let seen = 0
  return (text, end = false) => {
    const out = []
    for (let i = text.indexOf('\n', seen); i >= 0; i = text.indexOf('\n', seen)) {
      const line = text.slice(seen, i).trim()
      seen = i + 1
      if (line) out.push(JSON.parse(line))
    }
    if (end) {
      const rest = text.slice(seen).trim()
      seen = text.length
      if (rest) out.push(JSON.parse(rest))
    }
    return out
  }
}
