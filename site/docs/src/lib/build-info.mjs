// What this build is (set by hack/docs-build.sh): its channel - latest,
// the newest release's docs at /docs/, or next, main's at /docs/next/ -
// its version, commit and date, and the ref GitHub links point at.
const env = process.env

export const channel = env.DOCS_CHANNEL === 'next' ? 'next' : 'latest'
export const base = channel === 'next' ? '/docs/next' : '/docs'
export const version = env.DOCS_VERSION || 'dev'
export const commit = env.DOCS_COMMIT || ''
export const date = env.DOCS_DATE || ''
export const ref = env.DOCS_REF || 'main'
export const latestVersion = env.DOCS_LATEST_VERSION || ''
export const site = 'https://janus.sw-servers.net'
export const repo = 'https://github.com/swenske/Janus'
