import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// The harness API sends no CORS headers, so the dev/preview server proxies
// API calls to the harness `serve` process on 127.0.0.1:19421 (same origin).
const proxy = {
  '/v1': 'http://127.0.0.1:19421',
  '/healthz': 'http://127.0.0.1:19421',
};

export default defineConfig({
  base: '/ui/',
  plugins: [react()],
  server: { host: '127.0.0.1', port: 5173, proxy },
  preview: { host: '127.0.0.1', port: 5173, proxy },
});
