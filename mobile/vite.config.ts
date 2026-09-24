import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'

export default defineConfig({
  root: fileURLToPath(new URL('./frontend', import.meta.url)),
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: false },
  server: {
    host: '127.0.0.1',
    port: 5174,
    fs: { allow: [fileURLToPath(new URL('..', import.meta.url))] },
    proxy: Object.fromEntries(
      [
        '/config',
        '/files',
        '/gpx',
        '/upload',
        '/routing',
        '/offline',
        '/map',
        '/elevation',
        '/places',
        '/pois',
        '/fuel',
        '/mobile',
      ].map((path) => [path, 'http://127.0.0.1:8000']),
    ),
  },
})
