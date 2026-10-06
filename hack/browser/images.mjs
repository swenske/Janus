// Screenshots as WebP, and how much two of them differ - encoded,
// decoded and compared by Chromium itself (canvas), so the image needs
// no native image library. `page` is any blank page.

const QUALITY = 0.86

export async function toWebp(page, png) {
  const b64 = await page.evaluate(
    async ({ b64, q }) => {
      const img = new Image()
      img.src = 'data:image/png;base64,' + b64
      await img.decode()
      const c = document.createElement('canvas')
      c.width = img.naturalWidth
      c.height = img.naturalHeight
      c.getContext('2d').drawImage(img, 0, 0)
      const url = await new Promise((resolve) =>
        c.toBlob((blob) => {
          const r = new FileReader()
          r.onload = () => resolve(r.result)
          r.readAsDataURL(blob)
        }, 'image/webp', q),
      )
      return url.slice(url.indexOf(',') + 1)
    },
    { b64: png.toString('base64'), q: QUALITY },
  )
  return Buffer.from(b64, 'base64')
}

// diff compares two images (PNG or WebP): the share of their pixels that
// changed, outside the ignored rectangles (in the images' pixels), and a
// picture of where.
//
// Chromium doesn't draw a page's text exactly the same way twice: a
// glyph lands a fraction of a pixel aside, a line of text one pixel
// lower, the page otherwise identical - no change worth an image. So
// the images are compared at 1/`scale` of their size (the screenshots'
// device pixels back to CSS pixels: a glyph's ink stays, its
// antialiasing goes), and a pixel counts as changed past `delta` on a
// channel - WebP's own artefacts stay below it, a different text or
// colour doesn't - and only when no pixel within `shift` of it in the
// other image matches, both ways.
export async function diff(page, a, b, { ignore = [], delta = 48, shift = 2, scale = 1 } = {}) {
  return page.evaluate(
    async ({ a, b, ignore, delta, shift, scale }) => {
      const load = async (b64) => {
        const img = new Image()
        img.src = 'data:;base64,' + b64
        await img.decode()
        const c = document.createElement('canvas')
        c.width = Math.round(img.naturalWidth / scale)
        c.height = Math.round(img.naturalHeight / scale)
        const g = c.getContext('2d', { willReadFrequently: true })
        g.imageSmoothingQuality = 'high'
        g.drawImage(img, 0, 0, c.width, c.height)
        return { c, g, px: g.getImageData(0, 0, c.width, c.height).data }
      }
      ignore = ignore.map((r) => ({ x: Math.floor(r.x / scale), y: Math.floor(r.y / scale), width: Math.ceil(r.width / scale), height: Math.ceil(r.height / scale) }))
      const [x, y] = await Promise.all([load(a), load(b)])
      if (x.c.width !== y.c.width || x.c.height !== y.c.height) return { ratio: 1, size: true }
      const w = x.c.width
      const h = x.c.height
      const close = (p, o, q, i) => Math.max(Math.abs(p[o] - q[i]), Math.abs(p[o + 1] - q[i + 1]), Math.abs(p[o + 2] - q[i + 2])) <= delta
      // near: does p's pixel at (px, py) match one of q's around it?
      const near = (p, q, px, py) => {
        const o = (py * w + px) * 4
        for (let dy = -shift; dy <= shift; dy++) {
          const yy = py + dy
          if (yy < 0 || yy >= h) continue
          for (let dx = -shift; dx <= shift; dx++) {
            const xx = px + dx
            if (xx >= 0 && xx < w && close(p, o, q, (yy * w + xx) * 4)) return true
          }
        }
        return false
      }
      const skipped = new Uint8Array(w * h)
      for (const r of ignore)
        for (let py = Math.max(0, r.y); py < Math.min(h, r.y + r.height); py++) skipped.fill(1, py * w + Math.max(0, r.x), py * w + Math.min(w, r.x + r.width))
      const out = y.g.createImageData(w, h)
      let changed = 0
      let counted = 0
      for (let py = 0; py < h; py++)
        for (let px = 0; px < w; px++) {
          const i = py * w + px
          const o = i * 4
          out.data[o + 3] = 255
          if (skipped[i]) {
            out.data[o + 2] = 90
            continue
          }
          counted++
          if (close(x.px, o, y.px, o) || (near(y.px, x.px, px, py) && near(x.px, y.px, px, py))) {
            const l = (y.px[o] + y.px[o + 1] + y.px[o + 2]) / 12
            out.data[o] = out.data[o + 1] = out.data[o + 2] = l
          } else {
            changed++
            out.data[o] = 255
          }
        }
      y.g.putImageData(out, 0, 0)
      const png = y.c.toDataURL('image/png')
      return { ratio: counted ? changed / counted : 0, changed, png: png.slice(png.indexOf(',') + 1) }
    },
    { a: a.toString('base64'), b: b.toString('base64'), ignore, delta, shift, scale },
  )
}
