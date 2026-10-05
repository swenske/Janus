# site/docs

The docs site, [janus.sw-servers.net/docs](https://janus.sw-servers.net/docs/):
[Starlight](https://starlight.astro.build/) over the repository's own
Markdown - `docs/` and a few READMEs, read where they are.
[`structure.yaml`](structure.yaml) says which file is which page.

```sh
make docs-build   # the site, in Docker, into site/backend/docsdist/latest
make docs-dev     # a dev server on http://localhost:4321/docs/
```

How the docs are written and checked: [Contributing](../../docs/contributing/README.md).
