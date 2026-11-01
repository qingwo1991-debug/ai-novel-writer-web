import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { readFileSync } from 'node:fs'

const pkg = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf-8')) as { version: string }

/** Standalone browser renderer: no Electron plugins, preload polyfills, or Node shims. */
export default defineConfig({
  base: '/',
  plugins: [tailwindcss(), react()],
  publicDir: 'public',
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
    // Also select web for builds run with a custom --mode (for example staging).
    'import.meta.env.VITE_RUNTIME': JSON.stringify('web'),
  },
  server: {
    host: '127.0.0.1',
    port: 5180,
    strictPort: true,
    watch: { ignored: ['**/docs/**'] },
    // Development still uses the backend's cookie/CSRF boundary via the same browser origin.
    proxy: { '/api': { target: process.env.WEB_API_ORIGIN ?? 'http://127.0.0.1:3000', changeOrigin: false } },
  },
  build: {
    outDir: 'dist-web',
    emptyOutDir: true,
    rollupOptions: {
      onwarn(warning, defaultHandler) {
        if (warning.code === 'INEFFECTIVE_DYNAMIC_IMPORT') return
        if (warning.message?.includes('Invalid key')) return
        defaultHandler(warning)
      },
    },
  },
})
