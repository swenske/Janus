# Headless Chromium, pinned: Playwright's own image by digest, and the
# playwright npm package at the same version - the image only carries the
# browsers that version drives, so the build fails when Dependabot bumps
# one without the other. Used by make docs-smoke (hack/docs-smoke.sh) and
# make docs-screenshots (hack/docs-screenshots.sh).
FROM mcr.microsoft.com/playwright:v1.65.0-noble@sha256:6e3fb709361521eb27161606e6da6d2674e5e89fa16516cb37d6fdd91372273d
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
