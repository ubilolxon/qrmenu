import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The dev server proxies the API so the SPA and the API share one origin.
// That matters for the refresh cookie: it is SameSite=Strict and scoped to
// /api/v1/admin/auth, so it only travels on same-origin requests.
const api = process.env.QRMENU_API ?? 'http://localhost:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': api,
      // Guest pages and QR images. Regex keys, because a plain '/m' prefix
      // would also capture Vite's own '/main.tsx'.
      '^/(m|t)/': api,
    },
  },
})
