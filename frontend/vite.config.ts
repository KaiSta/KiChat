import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    // Wails serves the bundle from disk, so chasing sub-kilobyte chunk splits
    // only slows startup down.
    chunkSizeWarningLimit: 2000,
  },
})
