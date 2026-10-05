// The docs' vocabulary for the search field (lib/search-terms.mjs),
// built into a static file next to the pages.
import { searchTerms } from '../lib/search-terms.mjs'

export function GET() {
  return new Response(JSON.stringify(searchTerms()), { headers: { 'Content-Type': 'application/json' } })
}
