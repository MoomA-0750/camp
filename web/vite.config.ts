/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 開発中は campd serve へ中継する。Cookie を同一オリジンに保つため、
// 認証もログイン画面もこのプロキシ越しに動く。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8787', changeOrigin: false },
      '/login': { target: 'http://127.0.0.1:8787', changeOrigin: false },
      '/healthz': { target: 'http://127.0.0.1:8787', changeOrigin: false },
    },
  },
  // emptyOutDir は使わない。dist/.gitkeep まで消えてしまい、
  // fresh clone で //go:embed がコンパイルを通らなくなる（web/embed.go 参照）。
  // 代わりに build スクリプトが dist/assets だけ消す。
  build: { outDir: 'dist', emptyOutDir: false },
  test: { environment: 'jsdom', globals: true, setupFiles: ['./src/test-setup.ts'] },
})
