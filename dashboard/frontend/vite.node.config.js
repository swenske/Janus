import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The node page: a second app, served by the Controller under
// /nodes/<id>/ (dashboard/backend/internal/nodeproxy) - its assets are
// relative (base './') for that. Built into that package's go:embed
// directory and committed, like the main SPA's build.
export default defineConfig({
  plugins: [react()],
  base: './',
  root: 'node',
  publicDir: '../public',
  build: {
    outDir: '../../backend/internal/nodeproxy/static',
    emptyOutDir: true,
  },
})
