import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The node page: a second app, served by the Controller under
// /nodes/<id>/ (dashboard/backend/internal/nodeproxy). Its hashed
// assets are the same for every node, so they live at one absolute
// path, /node-app/, cached once by the browser (immutable) rather than
// once per node under /nodes/<id>/assets/; the page itself stays under
// the node's path (its API is relative to it, src/shared/base.js).
// Built into that package's go:embed directory and committed, like the
// main SPA's build.
export default defineConfig({
  plugins: [react()],
  base: '/node-app/',
  root: 'node',
  publicDir: '../public',
  build: {
    outDir: '../../backend/internal/nodeproxy/static',
    emptyOutDir: true,
  },
})
