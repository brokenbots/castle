import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/criteria.v1.ServerService': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/criteria.v1.CriteriaService': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/grpc.health.v1.Health': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    globals: true,
  },
});
