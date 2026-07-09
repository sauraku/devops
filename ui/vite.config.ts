import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: '../internal/api/ui/dist',
    emptyOutDir: true,
  },
  base: '/',
  server: {
    proxy: {
      '/api': 'http://localhost:8787',
      '/login': 'http://localhost:8787',
      '/logout': 'http://localhost:8787',
    },
  },
})
