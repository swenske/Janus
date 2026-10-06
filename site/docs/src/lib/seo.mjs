// What a docs page tells link previews and search engines beyond
// Starlight's own tags: its OpenGraph card - one per audience, made by
// make docs-og (hack/browser/og.mjs) - and, on the canonical channel
// only (next isn't indexed), its JSON-LD: an article, and where it sits.
import { base, channel, site } from './build-info.mjs'

const docsURL = `${site}${base}/`
const publisher = { '@type': 'Organization', name: 'Janus', url: `${site}/`, logo: { '@type': 'ImageObject', url: `${site}/icon-512.png` } }

export const cardURL = (card) => `${site}${base}/og/${card}.png`

export function cardHead(card, alt) {
  return [
    { tag: 'meta', attrs: { property: 'og:image', content: cardURL(card) } },
    { tag: 'meta', attrs: { property: 'og:image:width', content: '1200' } },
    { tag: 'meta', attrs: { property: 'og:image:height', content: '630' } },
    { tag: 'meta', attrs: { property: 'og:image:alt', content: alt } },
  ]
}

export function jsonLD(data) {
  return channel === 'latest' ? [{ tag: 'script', attrs: { type: 'application/ld+json' }, content: JSON.stringify(data) }] : []
}

// pageHead is a docs page's: its audience's card, then the article and
// its breadcrumbs - the docs, its audience, the page (unless it's the
// audience's own index).
export function pageHead({ id, title, description, topic, lastUpdated }) {
  const url = `${site}${base}/${id}/`
  const topicURL = `${site}${base}/${topic.sections[0].path}/`
  const crumbs = [{ name: 'Docs', item: docsURL }, { name: topic.label, item: topicURL }]
  if (url !== topicURL) crumbs.push({ name: title, item: url })
  return [
    ...cardHead(topic.id, `Janus documentation - ${topic.label}`),
    ...jsonLD([
      {
        '@context': 'https://schema.org',
        '@type': 'TechArticle',
        headline: title,
        description,
        url,
        inLanguage: 'en',
        image: cardURL(topic.id),
        ...(lastUpdated ? { dateModified: lastUpdated.toISOString() } : {}),
        isPartOf: { '@type': 'WebSite', name: 'Janus documentation', url: docsURL },
        publisher,
      },
      { '@context': 'https://schema.org', '@type': 'BreadcrumbList', itemListElement: crumbs.map((c, i) => ({ '@type': 'ListItem', position: i + 1, ...c })) },
    ]),
  ]
}
