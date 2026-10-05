# site/backend/docsdist

The docs site's build output, embedded by janus-site (`docs.go`):
`latest/` (served at `/docs/`) and `next/` (`/docs/next/`), made by
`make docs-build` or `make docs-site` - never committed. Without them,
janus-site builds and answers 404 under `/docs/`.
