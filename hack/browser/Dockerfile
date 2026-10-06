# Headless Chromium, pinned: Playwright's own image by digest, and the
# playwright npm package at the same version - the image only carries the
# browsers that version drives, so the build fails when Dependabot bumps
# one without the other. Used by make docs-smoke (hack/docs-smoke.sh) and
# make docs-screenshots (hack/docs-screenshots.sh).
FROM mcr.microsoft.com/playwright:v1.63.0-noble@sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27
ENV NPM_CONFIG_UPDATE_NOTIFIER=false NPM_CONFIG_FUND=false NPM_CONFIG_AUDIT=false
# The fonts the screenshots show for the system's own (fonts.conf).
RUN apt-get update && apt-get install -y --no-install-recommends fonts-inter fonts-jetbrains-mono \
    && rm -rf /var/lib/apt/lists/*
COPY fonts.conf /etc/fonts/local.conf
WORKDIR /browser
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts \
    && node -e "const fs = require('fs'), p = require('playwright').chromium.executablePath(); \
       if (!fs.existsSync(p)) { console.error('playwright ' + require('playwright/package.json').version + ' drives ' + p + ', not in this image: bump the image and the package together'); process.exit(1) }"
COPY *.mjs ./
