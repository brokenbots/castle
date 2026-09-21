import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Second static root for the standalone run-viewer (CRI-257). Builds
// packages/run-viewer/standalone/index.html into dist/runview/ with asset
// paths under /runview/ so the deploy recipe's Caddy handle block for
// /runview/* can point at the directory as-is. Hash routing keeps the root
// servable without SPA rewrites.
export default defineConfig({
  root: 'packages/run-viewer',
  base: '/runview/',
  plugins: [react()],
  build: {
    outDir: '../../dist/runview',
    emptyOutDir: true,
  },
});