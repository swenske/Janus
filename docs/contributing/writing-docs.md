# Writing docs

The docs are Markdown files of this repository - `docs/` and a few
READMEs - read on GitHub as they are, and built into
[janus.sw-servers.net/docs](https://janus.sw-servers.net/docs/) by
[Starlight](https://starlight.astro.build/) (`site/docs`): one source,
two readers. The site adds the navigation, full-text search, light and
dark themes, diagrams - and checks every link before anything is
published.

## Where a page goes

`site/docs/structure.yaml` says which file is which page: its audience
(a sidebar topic - user guide, technical, developer), its section (the
URL's prefix), its group in the sidebar, and optionally its `slug`, its
`title` on the site, its sidebar `label` and a `badge`. A new page:

1. a Markdown file, in its section's folder - `docs/guide/`,
   `docs/internals/`, `docs/contributing/`, `docs/private-cloud/`;
2. its entry in `structure.yaml`;
3. `make docs-index` - `docs/README.md`, the index GitHub shows, is
   generated from `structure.yaml`.

The build fails on a `docs/` file listed nowhere (or under `exclude`).

**A published file never moves, nor a heading a published link points
at.** Release notes, Controllers already deployed, `janusctl`'s help and
every node's `haproxy.cfg` link `github.com/swenske/Janus/blob/main/
docs/<file>#<anchor>`: content can move, leaving the heading behind with
a line saying where it went. `go test ./hack/docscheck` checks every such
link in the repository still finds its file and its heading. On the
site, `/docs/<file>.md` redirects to the file's page whatever its route.

## How a page is written

- **The H1 is the title**, and the first paragraph the description
  search engines show: make it say what the page is for. No front
  matter - GitHub would show it as a table.
- **Relative links, the GitHub way**: `[VRRP](vrrp.md#applying)`,
  `[the README](../dashboard/README.md)`. The site turns a link to a page
  into its route, and a link to any other file of the repository into
  GitHub at the release's tag; a link to nothing fails the build. A
  page's path in code - `` `docs/vrrp.md` `` - becomes a link too.
- **Callouts** are GitHub's alerts - `> [!NOTE]`, `> [!TIP]`,
  `> [!IMPORTANT]`, `> [!WARNING]`, `> [!CAUTION]` - which the site shows
  as asides.
- **Diagrams** are Mermaid code blocks - GitHub draws them too - and each
  carries an `accTitle` and an `accDescr`, for screen readers and search:
  the build fails without them.

  ````text
  ```mermaid
  flowchart LR
      accTitle: What the diagram shows
      accDescr: The same, as a sentence or two.
      a --> b
  ```
  ````

- **Code blocks** name their language - `sh`, `json`, `yaml`, `hcl`,
  `haproxy`, `nft`... - for highlighting on both sides.
- **Examples that run**: a block titled with a path of `examples/`
  (` ```hcl title="examples/terraform/libvirt/main.tf" `) must be that
  file exactly - the file is what a test runs. `make docs-examples`
  copies the files in again; every file of `examples/` must be shown
  somewhere ([examples](../../examples/README.md)).
- **Generated pages** aren't edited: the [janusctl
  reference](../guide/janusctl-reference.md) comes from the command tree
  (`go test ./cmd/janusctl -run TestReferenceDoc -update`).
- **The style**: write for the reader who has to act - what it does,
  what to do - in short sentences, with examples that work as written.
  English, sentence-case headings.

## Building and checking

```sh
make docs-dev       # a dev server, http://localhost:4321/docs/ - reloads on every change
make docs-build     # the site, as CI builds it: site/backend/docsdist/latest
make docs-smoke     # every page in a real browser: both themes, a phone, search, axe
```

Node runs in Docker - `site/docs/Dockerfile`, pinned by digest - never on
the machine. `make docs-build` fails on:

- markdownlint (`site/docs/.markdownlint-cli2.jsonc`) - never run it with
  `--fix` on `docs/`: `hypervisors.md`'s scripts are compared byte for
  byte with what the Controller generates;
- a stale `docs/README.md`;
- a relative link to nothing, a diagram without its accessible text, an
  example that isn't its file, a page listed nowhere;
- anything Astro reports as an error, a code block in a language it
  doesn't know;
- the built site's audit (`scripts/check-dist.mjs`): every link and
  `#fragment` resolves, every page has one H1, a title, a description and
  a canonical link, every page is in its sidebar, the sitemap and the
  search index are whole.

`make docs-smoke` then serves it with janus-site - its real security
headers - and walks every page in Chromium: no console error, no CSP
violation, every diagram drawn, nothing wider than a phone's screen,
search, suggestions and completion working, no serious axe finding.
`ci.yml` runs both on every push and pull request, with `make
examples-check`.

## Publishing

`site-deploy.yml` builds two channels on every change to the docs and
deploys them with the site: **`/docs/`**, built from the newest release's
tag - the docs of what operators run - and **`/docs/next/`**, from main,
marked as development docs and kept out of search engines. A release
redeploys them. The docs of an older release stay readable on GitHub,
at its tag.
