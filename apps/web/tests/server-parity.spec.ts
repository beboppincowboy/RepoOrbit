import { test, expect } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { connect, createServer as createTCPServer, type AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

// server.mjs must behave identically under Node and Bun, so every assertion runs against both.
const SHELL = '<!doctype html><title>shell</title><div id="root"></div>';
const SECRET = 'TOP-SECRET-SENTINEL';

type Seen = { method?: string; url?: string; headers: IncomingMessage['headers']; body: Buffer; complete: boolean; closed: boolean };
type Handler = (req: IncomingMessage, res: ServerResponse) => void;

async function freePort(): Promise<number> {
  const s = createTCPServer();
  await new Promise<void>(r => s.listen(0, '127.0.0.1', r));
  const { port } = s.address() as AddressInfo;
  await new Promise(r => s.close(r));
  return port;
}

// Records every upstream request; the handler is swapped per test.
async function startStub() {
  const echo: Handler = (req, res) => { req.on('end', () => res.writeHead(200, { 'content-type': 'text/plain' }).end('ok')); };
  const server = createServer((req, res) => {
    const entry: Seen = { method: req.method, url: req.url, headers: req.headers, body: Buffer.alloc(0), complete: false, closed: false };
    stub.seen.push(entry);
    const chunks: Buffer[] = [];
    req.on('data', c => chunks.push(c));
    req.on('end', () => { entry.body = Buffer.concat(chunks); entry.complete = true; });
    req.on('close', () => { entry.closed = true; });
    (stub.handler ?? echo)(req, res);
  });
  // Raw bytes per upstream socket: node's parser rejects bad framing before the handler, so `seen` alone can't show a forward.
  server.on('connection', s => s.on('data', d => stub.raw.push(d)));
  await new Promise<void>(r => server.listen(0, '127.0.0.1', r));
  // No request can arrive before the port is known, so `stub` is always set by the time the handler runs.
  const stub = { seen: [] as Seen[], raw: [] as Buffer[], handler: undefined as Handler | undefined, server, port: (server.address() as AddressInfo).port };
  return stub;
}

async function startServer(runtime: string, dir: string, apiPort: number) {
  const port = await freePort();
  const child = spawn(runtime, ['server.mjs'], { cwd: dir, env: { ...process.env, PORT: String(port), HOST: '127.0.0.1', API_ORIGIN: `http://127.0.0.1:${apiPort}` }, stdio: 'ignore' });
  let exited = false;
  child.on('exit', () => { exited = true; });
  for (let i = 0; i < 100; i++) {
    if (exited) throw new Error(`${runtime} server exited early`);
    const ok = await new Promise<boolean>(r => { const s = connect(port, '127.0.0.1', () => { s.destroy(); r(true); }); s.on('error', () => r(false)); });
    if (ok) return { child, port, url: `http://127.0.0.1:${port}` };
    await sleep(100);
  }
  child.kill();
  throw new Error(`${runtime} server did not start`);
}

// Sends raw bytes and returns everything the server writes until it closes the connection.
function rawRequest(port: number, text: string): Promise<{ head: string; body: Buffer; status: number }> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    const s = connect(port, '127.0.0.1', () => s.write(text));
    s.setTimeout(5000, () => { s.destroy(); reject(new Error('raw request timed out')); });
    s.on('data', c => chunks.push(c));
    s.on('error', reject);
    s.on('close', () => {
      const all = Buffer.concat(chunks);
      const at = all.indexOf('\r\n\r\n');
      const head = at < 0 ? all.toString() : all.subarray(0, at).toString();
      resolve({ head, body: at < 0 ? Buffer.alloc(0) : all.subarray(at + 4), status: Number(/^HTTP\/1\.\d (\d+)/.exec(head)?.[1] ?? 0) });
    });
  });
}

const get = (path: string) => `GET ${path} HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n`;

async function until(check: () => boolean, ms = 5000) {
  const end = Date.now() + ms;
  while (!check() && Date.now() < end) await sleep(25);
}

for (const runtime of ['node', 'bun']) {
  test.describe(`server.mjs under ${runtime}`, () => {
    test.describe.configure({ mode: 'serial' });
    let dir: string;
    let stub: Awaited<ReturnType<typeof startStub>>;
    let web: Awaited<ReturnType<typeof startServer>>;
    const children: ChildProcess[] = [];

    test.beforeAll(async () => {
      dir = mkdtempSync(join(tmpdir(), 'server-parity-'));
      const client = join(dir, 'dist', 'client');
      mkdirSync(join(client, 'assets'), { recursive: true });
      copyFileSync(fileURLToPath(new URL('../server.mjs', import.meta.url)), join(dir, 'server.mjs'));
      writeFileSync(join(client, 'assets', 'app-1a2b3c.js'), 'console.log(1)');
      writeFileSync(join(client, 'assets', 'font-1a2b3c.woff2'), 'woff');
      writeFileSync(join(client, '_shell.html'), SHELL);
      writeFileSync(join(dir, 'secret.txt'), SECRET);
      writeFileSync(join(dir, 'dist', 'secret.txt'), SECRET);
      stub = await startStub();
      web = await startServer(runtime, dir, stub.port);
      children.push(web.child);
    });
    test.afterAll(async () => {
      children.forEach(c => c.kill());
      stub.server.closeAllConnections();
      await new Promise(r => stub.server.close(r));
      rmSync(dir, { recursive: true, force: true });
    });
    test.beforeEach(() => { stub.seen.length = 0; stub.raw.length = 0; stub.handler = undefined; });

    test('serves hashed assets with immutable caching', async () => {
      const js = await fetch(`${web.url}/assets/app-1a2b3c.js`);
      expect(js.status).toBe(200);
      expect(js.headers.get('content-type')).toBe('text/javascript; charset=utf-8');
      expect(js.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
      const font = await fetch(`${web.url}/assets/font-1a2b3c.woff2`);
      expect(font.headers.get('content-type')).toBe('font/woff2');
    });

    test('missing asset is 404, not the shell', async () => {
      const res = await fetch(`${web.url}/assets/missing-9z.js`);
      expect(res.status).toBe(404);
      expect(await res.text()).not.toContain('shell');
    });

    test('client routes get the shell with no-cache', async () => {
      for (const path of ['/login', '/repositories/42?tab=board&issue=5']) {
        const res = await fetch(web.url + path);
        expect(res.status).toBe(200);
        expect(res.headers.get('cache-control')).toBe('no-cache');
        expect(await res.text()).toBe(SHELL);
      }
    });

    test('HEAD has headers and no body; POST is 405', async () => {
      const head = await fetch(`${web.url}/`, { method: 'HEAD' });
      expect(head.status).toBe(200);
      expect(head.headers.get('content-type')).toBe('text/html; charset=utf-8');
      expect(await head.text()).toBe('');
      expect((await fetch(`${web.url}/`, { method: 'POST', body: 'x' })).status).toBe(405);
    });

    test('malformed percent-encoding is 400; traversal never leaks', async () => {
      expect((await rawRequest(web.port, get('/%E0%A4%A'))).status).toBe(400);
      for (const path of ['/%2e%2e/secret.txt', '/assets/..%2f..%2fsecret.txt', '/..%2f..%2fsecret.txt', '/%2e%2e/%2e%2e/secret.txt']) {
        const res = await rawRequest(web.port, get(path));
        expect(res.body.toString(), path).not.toContain(SECRET);
      }
    });

    test('forwards method, raw query and body upstream', async () => {
      const res = await fetch(`${web.url}/graphql?x=1&x=2`, { method: 'POST', body: '{"query":"{a}"}' });
      expect(res.status).toBe(200);
      expect(stub.seen).toHaveLength(1);
      expect(stub.seen[0].method).toBe('POST');
      expect(stub.seen[0].url).toBe('/graphql?x=1&x=2');
      expect(stub.seen[0].body.toString()).toBe('{"query":"{a}"}');
    });

    test('keeps two Set-Cookie headers separate', async () => {
      stub.handler = (_req, res) => { res.writeHead(200, { 'set-cookie': ['a=1; Path=/', 'b=2; Path=/'] }).end('x'); };
      const res = await fetch(`${web.url}/auth/login`);
      expect(res.headers.getSetCookie()).toEqual(['a=1; Path=/', 'b=2; Path=/']);
    });

    test('passes a 302 through without following it', async () => {
      stub.handler = (_req, res) => { res.writeHead(302, { location: 'https://example.com/next' }).end(); };
      const res = await fetch(`${web.url}/auth/github`, { redirect: 'manual' });
      expect(res.status).toBe(302);
      expect(res.headers.get('location')).toBe('https://example.com/next');
    });

    test('chunked upstream response has valid framing', async () => {
      stub.handler = (_req, res) => {
        res.writeHead(200, { 'content-type': 'text/plain' });
        let n = 0;
        const t = setInterval(() => { res.write(`part${n};`); if (++n === 4) { clearInterval(t); res.end('end'); } }, 20);
      };
      const res = await rawRequest(web.port, get('/api/stream'));
      const lines = res.head.toLowerCase().split('\r\n').slice(1);
      expect(res.status).toBe(200);
      expect(lines.filter(l => l.startsWith('transfer-encoding:'))).toHaveLength(1);
      expect(lines.filter(l => l.startsWith('content-length:'))).toHaveLength(0);
      expect(lines.filter(l => l.startsWith('transfer-encoding:'))[0].split(':')[1].trim()).toBe('chunked');
      expect(res.body.toString().endsWith('0\r\n\r\n')).toBe(true);
      // Decode chunked framing by hand.
      let rest = res.body.toString();
      let decoded = '';
      for (;;) {
        const eol = rest.indexOf('\r\n');
        const size = parseInt(rest.slice(0, eol), 16);
        if (Number.isNaN(size)) throw new Error(`bad chunk size in ${JSON.stringify(rest.slice(0, 20))}`);
        if (!size) break;
        decoded += rest.slice(eol + 2, eol + 2 + size);
        rest = rest.slice(eol + 2 + size + 2);
      }
      expect(decoded).toBe('part0;part1;part2;part3;end');
    });

    test('rejects ambiguous request framing before it reaches upstream', async () => {
      const both = await rawRequest(web.port, 'POST /api/x HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\n\r\n');
      expect(both.status).toBe(400);
      const two = await rawRequest(web.port, 'POST /api/x HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nContent-Length: 5\r\nConnection: close\r\n\r\nabcd');
      expect(two.status).toBe(400);
      await sleep(200);
      expect(stub.seen).toHaveLength(0);
      expect(Buffer.concat(stub.raw)).toHaveLength(0);
    });

    test('unreachable upstream is 502 Bad gateway', async () => {
      const dead = await startServer(runtime, dir, await freePort());
      children.push(dead.child);
      const res = await fetch(`${dead.url}/api/x`);
      expect(res.status).toBe(502);
      expect(await res.text()).toContain('Bad gateway');
    });

    test('client abort mid-upload closes the upstream request', async () => {
      stub.handler = () => { /* never respond */ };
      const s = connect(web.port, '127.0.0.1');
      s.on('error', () => {});
      s.write('POST /api/upload HTTP/1.1\r\nHost: x\r\nContent-Length: 1000000\r\n\r\n' + 'a'.repeat(1000));
      await until(() => stub.seen.length > 0);
      expect(stub.seen).toHaveLength(1);
      s.destroy();
      await until(() => stub.seen[0].closed);
      expect(stub.seen[0].closed).toBe(true);
      expect(stub.seen[0].complete).toBe(false);
    });

    test('early upstream 413 reaches the client before the upload finishes', async () => {
      // Reply only after the first body chunk, with a marker a locally generated 413 could not contain.
      stub.handler = (req, res) => { req.once('data', () => res.writeHead(413, { connection: 'close' }).end('stub-413-marker')); };
      // Trickle the upload so the 413 is read before the server resets the connection over the unread body.
      const total = 4 * 1024 * 1024;
      const piece = Buffer.alloc(16 * 1024, 'a');
      let sent = 0;
      const sentAtResponse = await new Promise<number>((resolve, reject) => {
        const s = connect(web.port, '127.0.0.1');
        let text = '';
        let t: NodeJS.Timeout | undefined;
        const done = (n: number) => { clearInterval(t); s.destroy(); resolve(n); };
        s.on('error', () => {});
        s.on('connect', () => {
          s.write(`POST /api/upload HTTP/1.1\r\nHost: x\r\nContent-Length: ${total}\r\n\r\n`);
          t = setInterval(() => { if (sent < total && !s.destroyed) { s.write(piece); sent += piece.length; } }, 10);
        });
        s.on('data', c => { text += c.toString(); if (/^HTTP\/1\.\d 413/.test(text) && text.includes('stub-413-marker')) done(sent); });
        setTimeout(() => { clearInterval(t); s.destroy(); reject(new Error(`no 413 (sent ${sent})`)); }, 8000);
      });
      expect(sentAtResponse).toBeGreaterThan(0);
      expect(sentAtResponse).toBeLessThan(total);
      expect(stub.seen).toHaveLength(1);
    });

    test('/webhooks/github, /live and /ready are not proxied', async () => {
      for (const path of ['/webhooks/github', '/live', '/ready']) {
        const res = await fetch(web.url + path);
        expect(await res.text(), path).toBe(SHELL);
        expect((await fetch(web.url + path, { method: 'POST', body: '{}' })).status, path).toBe(405);
      }
      expect(stub.seen).toHaveLength(0);
      expect(Buffer.concat(stub.raw)).toHaveLength(0);
    });
  });
}
