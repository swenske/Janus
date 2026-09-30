import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The per-node page: a second app, served by each node's own mTLS-gated
// listener (dashboard/backend/internal/nodeproxy) rather than the main
// port - see nodeproxy.go's package doc for why the two pages are
// separate origins. Built into that package's go:embed directory and
// committed, like the main SPA's build.
export default defineConfig({
  plugins: [react()],
  root: 'node',
  publicDir: '../public',
  build: {
    outDir: '../../backend/internal/nodeproxy/static',
    emptyOutDir: true,
  },
})
