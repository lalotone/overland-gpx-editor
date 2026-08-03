import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

// Paths the Go backend owns. In production it serves this bundle too, so the
// app talks to the same origin; in dev the backend is a separate process and
// these are proxied to it, which keeps the frontend URLs identical either way.
const API_PATHS = ['/files', '/gpx', '/upload', '/elevation']

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const target = env.VITE_DEV_API_TARGET || 'http://localhost:8000'

  return {
    plugins: [react()],
    server: {
      fs: {
        allow: ['..'],
      },
      proxy: Object.fromEntries(
        API_PATHS.map(path => [path, { target, changeOrigin: true }]),
      ),
    },
    build: {
      // Go embeds this directory (see web/embed.go). Keeping the build output
      // inside the web package is what makes the single binary possible —
      // //go:embed cannot reach outside its own directory.
      outDir: 'web/dist',
      // Not emptied by Vite: that would delete the committed .gitkeep that
      // keeps //go:embed working on a fresh clone. `npm run build` clears the
      // hashed assets itself first.
      emptyOutDir: false,
      rollupOptions: {
        output: {
          manualChunks: undefined,
        },
      },
    },
  }
})
