// Production server: serves the built app shell and assets, and forwards /auth, /api and /graphql to the Go API
// on the same origin. Uses only Node built-ins so the Docker image needs no node_modules at runtime.
import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { dirname, join, normalize, sep } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "dist", "client");
const api = process.env.API_ORIGIN ?? "http://127.0.0.1:8080";
const port = Number(process.env.PORT ?? 3000);
const host = process.env.HOST ?? "127.0.0.1";
const forwarded = /^\/(auth|api|graphql)(\/|$)/;
const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon", ".woff2": "font/woff2", ".json": "application/json", ".txt": "text/plain; charset=utf-8" };

// Relays the request unchanged (Origin, Cookie and X-CSRF-Token matter to the API). Redirects pass through to the browser.
async function forward(req, res) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  const headers = { ...req.headers };
  delete headers.host;
  const upstream = await fetch(new URL(req.url, api), {
    method: req.method,
    headers,
    body: chunks.length ? Buffer.concat(chunks) : undefined,
    redirect: "manual",
  });
  const body = Buffer.from(await upstream.arrayBuffer());
  const outHeaders = {};
  upstream.headers.forEach((value, name) => {
    if (!["set-cookie", "content-encoding", "content-length", "transfer-encoding"].includes(name)) outHeaders[name] = value;
  });
  const cookies = upstream.headers.getSetCookie();
  if (cookies.length) outHeaders["set-cookie"] = cookies;
  outHeaders["content-length"] = body.length;
  res.writeHead(upstream.status, outHeaders);
  res.end(req.method === "HEAD" ? undefined : body);
}

// Built files are served as-is; every other GET gets the shell, and the client router renders the route.
async function serveStatic(req, res, pathname) {
  let file;
  try {
    file = normalize(join(root, decodeURIComponent(pathname)));
  } catch {
    res.writeHead(400).end();
    return;
  }
  if (file !== root && !file.startsWith(root + sep)) {
    res.writeHead(403).end();
    return;
  }
  const info = await stat(file).catch(() => null);
  // A missing build asset is a 404, not the shell: a stale tab after a deploy must fail loudly, not run HTML as JavaScript.
  if (!info?.isFile() && pathname.startsWith("/assets/")) {
    res.writeHead(404).end();
    return;
  }
  const target = info?.isFile() ? file : join(root, "_shell.html");
  const body = await readFile(target);
  const immutable = target.includes(`${sep}assets${sep}`);
  res.writeHead(200, { "content-type": types[target.slice(target.lastIndexOf("."))] ?? "application/octet-stream", "cache-control": immutable ? "public, max-age=31536000, immutable" : "no-cache" });
  res.end(req.method === "HEAD" ? undefined : body);
}

const server = createServer(async (req, res) => {
  try {
    const { pathname } = new URL(req.url, "http://localhost");
    if (forwarded.test(pathname)) await forward(req, res);
    else if (req.method === "GET" || req.method === "HEAD") await serveStatic(req, res, pathname);
    else res.writeHead(405).end();
  } catch (error) {
    console.error(error);
    if (!res.headersSent) res.writeHead(502, { "content-type": "text/plain" }).end("Bad gateway");
  }
});

server.listen(port, host, () => console.log(`RepoOrbit web on http://${host}:${port}, API at ${api}`));
