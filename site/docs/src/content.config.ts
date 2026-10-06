// The docs collection: the repository's Markdown files listed in
// structure.yaml, read where they are. Each page's title, description,
// sidebar label and edit link come from there and from its own text
// (meta.mjs) - no front matter needed, which GitHub would show as a table.
import { existsSync, readFileSync } from 'node:fs'
import { defineCollection } from 'astro:content'
import { glob } from 'astro/loaders'
import { docsSchema, i18nSchema } from '@astrojs/starlight/schema'
import { repo } from './lib/build-info.mjs'
import { pageMeta } from './lib/meta.mjs'
import { pageOfFile, pageOfId, pages, repoRoot, topics } from './lib/structure.mjs'

// When each file last changed: hack/docs-build.sh writes it from git
// (lastupdated.tsv: path, tab, ISO date) - the build itself has no git.
const lastUpdatedFile = `${repoRoot}/site/docs/lastupdated.tsv`
const lastUpdated = new Map(
  existsSync(lastUpdatedFile)
    ? readFileSync(lastUpdatedFile, 'utf8')
        .split('\n')
        .filter(Boolean)
        .map((line) => line.split('\t'))
    : [],
)

const files = glob({
  base: repoRoot,
  pattern: pages.map((p) => p.file),
  generateId: ({ entry }) => pageOfFile(entry).id,
})

function fields(id: string) {
  const page = pageOfId(id)
  const meta = pageMeta(page.file)
  const topic = topics.find((t) => t.id === page.topic)
  return {
    title: page.title ?? meta.title,
    description: meta.description || page.description || topic.description,
    sidebar: { label: page.label ?? page.title ?? meta.title, ...(page.badge ? { badge: { text: page.badge, variant: 'caution' } } : {}) },
    editUrl: `${repo}/edit/main/${page.file}`,
    ...(lastUpdated.has(page.file) ? { lastUpdated: new Date(lastUpdated.get(page.file)) } : {}),
  }
}

// Starlight reads overrides of its UI strings from an i18n collection:
// none here, but it has to exist - a plugin's own strings (the image
// zoom's) otherwise make Astro warn that it doesn't.
const i18n = defineCollection({ loader: () => [{ id: 'en' }], schema: i18nSchema() })

export const collections = {
  docs: defineCollection({
    schema: docsSchema(),
    loader: {
      name: 'janus-docs',
      load: (ctx) =>
        files.load({
          ...ctx,
          parseData: ((entry: { id: string; data: Record<string, unknown> }) =>
            ctx.parseData({ ...entry, data: { ...fields(entry.id), ...entry.data } })) as typeof ctx.parseData,
        }),
    },
  }),
  i18n,
}
