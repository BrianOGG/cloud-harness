import assert from 'node:assert/strict';
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import { createServer, loadConfigFromFile, type ProxyOptions } from 'vite';

// The harness refuses a request whose Origin differs from its Host, so the
// dev/preview proxy has to forward the browser's Host unchanged.
const loaded = await loadConfigFromFile({ command: 'serve', mode: 'development' }, 'vite.config.ts');
assert.ok(loaded, 'vite.config.ts did not load');
const { server, preview } = loaded.config;
assert.ok(server?.proxy && Object.keys(server.proxy).length > 0, 'dev server has no proxy');
assert.deepEqual(preview?.proxy, server.proxy, 'preview and dev proxies differ');

const seen: { url?: string; host?: string; origin?: string }[] = [];
const harness = http.createServer((req, res) => {
  seen.push({ url: req.url, host: req.headers.host, origin: req.headers.origin });
  res.writeHead(201, { 'Content-Type': 'application/json' }).end('{}');
});
await new Promise<void>((resolve) => harness.listen(0, '127.0.0.1', resolve));
const target = `http://127.0.0.1:${(harness.address() as AddressInfo).port}`;

// Only the target changes; every other option comes from vite.config.ts.
const proxy: Record<string, ProxyOptions> = {};
for (const [path, options] of Object.entries(server.proxy)) {
  assert.equal(typeof options, 'object', `proxy ${path} is a string target, which rewrites Host`);
  proxy[path] = { ...(options as ProxyOptions), target };
}
const vite = await createServer({
  ...loaded.config,
  configFile: false,
  logLevel: 'silent',
  server: { ...server, port: 0, strictPort: false, proxy },
});
await vite.listen();
try {
  const browserHost = `127.0.0.1:${(vite.httpServer!.address() as AddressInfo).port}`;
  for (const path of Object.keys(proxy)) {
    const status = await new Promise<number | undefined>((resolve, reject) => {
      http
        .request({ host: '127.0.0.1', port: browserHost.split(':')[1], path, method: 'POST', headers: { Origin: `http://${browserHost}` } }, (res) => {
          res.resume();
          res.on('end', () => resolve(res.statusCode));
        })
        .on('error', reject)
        .end();
    });
    assert.equal(status, 201, `${path} was not proxied`);
    assert.deepEqual(seen.pop(), { url: path, host: browserHost, origin: `http://${browserHost}` }, `${path} reached the harness with a rewritten Host`);
  }
} finally {
  await vite.close();
  harness.close();
}
console.log('proxy tests passed');
