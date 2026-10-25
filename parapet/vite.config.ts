import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/criteria.v1.ServerService': {
        target: process.env.VITE_CASTLE_URL || 'http://catch/',
        changeOrigin: true,
      },
      '/criteria.v1.CriteriaService': {
        target: process.env.VITE_CASTLE_URL || 'http://catch/',
        changeOrigin: true,
      },
      // castle.v1 surfaces the console rides (CRI-195 login, KB-327
      // settings console): proxy them alongside the criteria services.
      '/castle.v1.ConsoleService': {
        target: process.env.VITE_CASTLE_URL || 'http://catch/',
        changeOrigin: true,
      },
      '/castle.v1.ProjectService': {
        target: process.env.VITE_CASTLE_URL || 'http://catch/',
        changeOrigin: true,
      },
      '/grpc.health.v1.Health': {
        target: process.env.VITE_CASTLE_URL || 'http://catch/',
        changeOrigin: true,
      },
    },
  },
  test: {
    // Custom environment: jsdom + Node's fetch/AbortController primitives.
    // See src/test/jsdomWithNodeFetch.ts for why this seam exists.
    environment: './src/test/jsdomWithNodeFetch.ts',
    setupFiles: ['./src/test/setup.ts'],
    globals: true,
  },
});