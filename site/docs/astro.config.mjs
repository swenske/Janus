// The docs site, janus.sw-servers.net/docs/: Starlight over the
// repository's own Markdown, read where it is (structure.yaml). Built in
// Docker by hack/docs-build.sh (site/docs/Dockerfile), embedded and
// served by janus-site - see docs/contributing/README.md.
import { defineConfig, passthroughImageService } from 'astro/config'
import { unified } from '@astrojs/markdown-remark'
import starlight from '@astrojs/starlight'
import mermaid from 'astro-mermaid'
import starlightImageZoom from 'starlight-image-zoom'
import starlightSidebarTopics from 'starlight-sidebar-topics'
import { base, channel, ref, repo, site } from './src/lib/build-info.mjs'
import janusFiles from './src/lib/janus-files.mjs'
import janusProblems from './src/lib/problems.mjs'
import remarkDiagrams from './src/lib/remark-diagrams.mjs'
import remarkExamples from './src/lib/remark-examples.mjs'
import remarkGitHubAlerts from './src/lib/remark-github-alerts.mjs'
import remarkRepoLinks from './src/lib/remark-repo-links.mjs'
import remarkScreenshots from './src/lib/remark-screenshots.mjs'
import remarkStripTitle from './src/lib/remark-strip-title.mjs'
import rehypeTables from './src/lib/rehype-tables.mjs'
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
      remarkPlugins: [remarkStripTitle, remarkGitHubAlerts, remarkDiagrams, remarkExamples, remarkScreenshots, [remarkRepoLinks, { base, ref, repo }]],
      rehypePlugins: [rehypeTables],
    }),
  },
  integrations: [
    // Before Starlight (astro-mermaid's own instructions).
    mermaid({ theme: 'default', autoTheme: true, enableLog: false }),
    starlight({
      title: 'Janus',
      description: 'Janus - an immutable, API-driven Linux distribution for HAProxy load balancers.',
      logo: { src: './public/favicon.svg', alt: '' },
      favicon: '/favicon.svg',
      social: [{ icon: 'github', label: 'GitHub', href: repo }],
      customCss: ['./src/styles/janus.css'],
      components: {
        Banner: './src/components/Banner.astro',
        LastUpdated: './src/components/LastUpdated.astro',
        PageTitle: './src/components/PageTitle.astro',
        Search: './src/components/Search.astro',
        SiteTitle: './src/components/SiteTitle.astro',
        SocialIcons: './src/components/SocialIcons.astro',
        ThemeProvider: './src/components/ThemeProvider.astro',
      },
      // Starlight's own Markdown transforms (asides, heading links) only
      // touch files under these folders: the pages live all over the
      // repository.
      markdown: { processedDirs: ['../..'] },
      // Shiki has no grammar for these: plain text on the site (GitHub
      // highlights `haproxy` itself).
      expressiveCode: { shiki: { langAlias: { haproxy: 'txt', nft: 'txt' } } },
      lastUpdated: false,
      credits: false,
      // Our own (src/pages/404.astro): where to go from a missing page.
      disable404Route: true,
      tableOfContents: { minHeadingLevel: 2, maxHeadingLevel: 3 },
      head: [
        { tag: 'link', attrs: { rel: 'icon', href: `${base}/favicon.ico`, sizes: '32x32' } },
        { tag: 'link', attrs: { rel: 'apple-touch-icon', href: `${base}/apple-touch-icon.png` } },
        { tag: 'meta', attrs: { name: 'theme-color', content: '#1D1C1A' } },
        ...(channel === 'next' ? [{ tag: 'meta', attrs: { name: 'robots', content: 'noindex' } }] : []),
      ],
      plugins: [starlightImageZoom(), starlightSidebarTopics(sidebarTopics(), { exclude: ['/', '/404'] })],
    }),
    janusProblems(),
    janusFiles(),
  ],
  // The screenshots are WebP already, made to measure: copied as they are.
  image: { service: passthroughImageService() },
  vite: { server: { fs: { allow: ['../..'] } } },
})
