import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Run-viewer embed artifact (CRI-257). Builds packages/run-viewer/index.html
// (entry packages/run-viewer/standalone/main.tsx) into dist/runview/ with
// asset paths under /runview/ for the criteria CLI to embed and serve from
// its local loopback (CRI-279). Castle does not host this bundle (CRI-283
// removed the castle-side /runview/ route): a browser pointed at the castle
// ingress falls through to the main SPA fallback. Hash routing keeps the
// bundle servable from any root without SPA rewrites.
export default defineConfig({
  root: 'packages/run-viewer',
  base: '/runview/',
  plugins: [react()],
  build: {
    outDir: '../../dist/runview',
    emptyOutDir: true,
  },
});