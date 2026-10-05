// What janus-site reads from a built channel (dist/_janus/, never served
// to readers): build.json - what the build is - and redirects.json - each
// source file's page, so /docs/vrrp.md (or /docs/dashboard/README.md)
// leads to it whatever its route becomes.
import { mkdir, writeFile } from 'node:fs/promises'
import * as info from './build-info.mjs'
import { pages } from './structure.mjs'

export default function janusFiles() {
  return {
    name: 'janus-docs-files',
    hooks: {
      'astro:build:done': async ({ dir }) => {
        const out = new URL('_janus/', dir)
        await mkdir(out, { recursive: true })
        const build = {
          channel: info.channel,
          version: info.version,
          commit: info.commit,
          date: info.date,
          ref: info.ref,
          latestVersion: info.latestVersion,
        }
        const redirects = Object.fromEntries(pages.map((p) => [p.file.replace(/^docs\//, ''), `${p.id}/`]))
        await writeFile(new URL('build.json', out), JSON.stringify(build, null, 2) + '\n')
        await writeFile(new URL('redirects.json', out), JSON.stringify(redirects, null, 2) + '\n')
      },
    },
  }
}
