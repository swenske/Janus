# The docs site (site/docs, Starlight), built here and never on the host:
# Node pinned by digest, npm's install scripts off. The context is a
# snapshot of the repository's sources - hack/docs-build.sh makes it, and
# passes what the build is (DOCS_CHANNEL, DOCS_VERSION...: see
# src/lib/build-info.mjs). The export stage holds the built site alone:
#   docker build -f site/docs/Dockerfile --target export -o <dir> <snapshot>
FROM node:26.10.0-trixie-slim@sha256:ec7758ee051e457b468b32bde57b0879010b325bb9862718e9615225ce4aaae1 AS deps
ENV ASTRO_TELEMETRY_DISABLED=1 NPM_CONFIG_UPDATE_NOTIFIER=false
WORKDIR /src/site/docs
COPY site/docs/package.json site/docs/package-lock.json site/docs/.npmrc ./
RUN npm ci --ignore-scripts

FROM deps AS build
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
WORKDIR /src
COPY . .
WORKDIR /src/site/docs
ARG DOCS_CHANNEL=latest
ARG DOCS_VERSION=dev
ARG DOCS_COMMIT=
ARG DOCS_DATE=
ARG DOCS_REF=main
ARG DOCS_LATEST_VERSION=
ENV DOCS_CHANNEL=$DOCS_CHANNEL DOCS_VERSION=$DOCS_VERSION DOCS_COMMIT=$DOCS_COMMIT DOCS_DATE=$DOCS_DATE \
    DOCS_REF=$DOCS_REF DOCS_LATEST_VERSION=$DOCS_LATEST_VERSION DOCS_STRICT=1
# Astro logs a page that fails to render, or a code block in an unknown
# language, and carries on: here either fails the build.
RUN npm run lint \
    && node scripts/github-index.mjs --check \
    && npm run build 2>&1 | tee /tmp/build.log \
    && ! grep -E '\[ERROR\]|\[WARN\] \[astro-expressive-code\]' /tmp/build.log \
    && npm run check

FROM scratch AS export
COPY --from=build /src/site/docs/dist /
