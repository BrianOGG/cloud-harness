import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// The harness API sends no CORS headers, so the dev/preview server proxies
// API calls to the harness `serve` process on 127.0.0.1:19421 (same origin).
// changeOrigin stays false so the harness sees the browser's Host: it refuses
// a request whose Origin differs from its Host. A string target would enable
// changeOrigin.
const harness = { target: 'http://127.0.0.1:19421', changeOrigin: false };
const proxy = { '/v1': harness, '/healthz': harness };

export default defineConfig({
  base: '/ui/',
  plugins: [react()],
  server: { host: '127.0.0.1', port: 5173, proxy },
  preview: { host: '127.0.0.1', port: 5173, proxy },
});
