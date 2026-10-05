// The docs collection: the repository's Markdown files listed in
// structure.yaml, read where they are. Each page's title, description,
// sidebar label and edit link come from there and from its own text
// (meta.mjs) - no front matter needed, which GitHub would show as a table.
import { defineCollection } from 'astro:content'
import { glob } from 'astro/loaders'
import { docsSchema } from '@astrojs/starlight/schema'
import { repo } from './lib/build-info.mjs'
import { pageMeta } from './lib/meta.mjs'
import { pageOfFile, pageOfId, pages, repoRoot, topics } from './lib/structure.mjs'

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
    sidebar: { label: page.label ?? page.title ?? meta.title },
    editUrl: `${repo}/edit/main/${page.file}`,
  }
}

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
}
