import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'

// The site shares the Controller's design system: dashboard/frontend/
// src/shared (tokens, cards, badges, theme toggle), imported as
// "@shared". dedupe keeps one React - the shared files would otherwise
// resolve it from dashboard/frontend/node_modules. The build goes
// straight into site/backend/static, embedded by the Go server and
// committed like the Controller's (a plain `go build` needs no Node.js).
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@shared': fileURLToPath(new URL('../../dashboard/frontend/src/shared', import.meta.url)) },
    dedupe: ['react', 'react-dom', 'lucide-react'],
  },
  server: {
    fs: { allow: ['..', '../../dashboard/frontend/src/shared'] },
    proxy: { '/api': 'http://127.0.0.1:8080', '/image': 'http://127.0.0.1:8080' },
  },
  build: {
    outDir: '../backend/static',
    emptyOutDir: true,
  },
})
