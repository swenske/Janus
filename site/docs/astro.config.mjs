// The docs site, janus.sw-servers.net/docs/: Starlight over the
// repository's own Markdown, read where it is (structure.yaml). Built in
// Docker by hack/docs-build.sh (site/docs/Dockerfile), embedded and
// served by janus-site - see docs/contributing/README.md.
import { defineConfig } from 'astro/config'
import { unified } from '@astrojs/markdown-remark'
import starlight from '@astrojs/starlight'
import mermaid from 'astro-mermaid'
import starlightSidebarTopics from 'starlight-sidebar-topics'
import { base, channel, ref, repo, site } from './src/lib/build-info.mjs'
import janusProblems from './src/lib/problems.mjs'
import remarkDiagrams from './src/lib/remark-diagrams.mjs'
import remarkGitHubAlerts from './src/lib/remark-github-alerts.mjs'
import remarkRepoLinks from './src/lib/remark-repo-links.mjs'
import remarkStripTitle from './src/lib/remark-strip-title.mjs'
import { sidebarTopics } from './src/lib/structure.mjs'

export default defineConfig({
  // Only the latest channel is the canonical, indexed one: without
  // `site`, next gets no canonical link and no sitemap.
  site: channel === 'latest' ? site : undefined,
  base,
  trailingSlash: 'always',
  markdown: {
    // Astro 7's own Markdown engine runs no remark plugins: unified() does.
    // These run before astro-mermaid's and Starlight's own.
    processor: unified({
      remarkPlugins: [remarkStripTitle, remarkGitHubAlerts, remarkDiagrams, [remarkRepoLinks, { base, ref, repo }]],
    }),
  },
  integrations: [
    // Before Starlight (astro-mermaid's own instructions).
    mermaid({ theme: 'default', autoTheme: true, enableLog: false }),
    starlight({
      title: 'Janus',
      description: 'Janus - an immutable, API-driven Linux distribution for HAProxy load balancers.',
      social: [{ icon: 'github', label: 'GitHub', href: repo }],
      // Starlight's own Markdown transforms (asides, heading links) only
      // touch files under these folders: the pages live all over the
      // repository.
      markdown: { processedDirs: ['../..'] },
      // Shiki has no grammar for these: plain text on the site (GitHub
      // highlights `haproxy` itself).
      expressiveCode: { shiki: { langAlias: { haproxy: 'txt', nft: 'txt' } } },
      lastUpdated: false,
      credits: false,
      tableOfContents: { minHeadingLevel: 2, maxHeadingLevel: 3 },
      head: channel === 'next' ? [{ tag: 'meta', attrs: { name: 'robots', content: 'noindex' } }] : [],
      plugins: [starlightSidebarTopics(sidebarTopics(), { exclude: ['/'] })],
    }),
    janusProblems(),
  ],
  vite: { server: { fs: { allow: ['../..'] } } },
})
