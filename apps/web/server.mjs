// Production server: serves the built app shell and assets, and forwards /auth, /api and /graphql to the Go API
// on the same origin. Uses only node: built-ins, which run on Node and Bun (production runs it on Bun), so the
// Docker image needs no node_modules at runtime. tests/server-parity.spec.ts checks both runtimes behave the same.
import { createServer, request as requestHTTP } from "node:http";
import { request as requestHTTPS } from "node:https";
import { readFile, stat } from "node:fs/promises";
import { dirname, join, normalize, sep } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "dist", "client");
const api = new URL(process.env.API_ORIGIN ?? "http://127.0.0.1:8080");
const requestAPI = api.protocol === "https:" ? requestHTTPS : requestHTTP;
const port = Number(process.env.PORT ?? 3000);
const host = process.env.HOST ?? "127.0.0.1";
const forwarded = /^\/(auth|api|graphql)(\/|$)/;
const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon", ".woff2": "font/woff2", ".json": "application/json", ".txt": "text/plain; charset=utf-8" };

// Stream through the configured Go origin so Go can enforce its request limits as data arrives.
function forward(req, res, { pathname, search }) {
  const headers = { ...req.headers, host: api.host };
  delete headers.connection;
  const upstream = requestAPI(api, { path: pathname + search, method: req.method, headers }, response => {
    const headers = { ...response.headers };
    delete headers.connection;
    res.writeHead(response.statusCode, headers);
    response.on("error", error => res.destroy(error));
    response.pipe(res);
  });
  upstream.on("error", error => {
    if (res.headersSent) res.destroy(error);
    else res.writeHead(502, { "content-type": "text/plain" }).end("Bad gateway");
  });
  req.on("error", error => upstream.destroy(error));
  res.on("close", () => upstream.destroy());
  req.pipe(upstream);
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
    const url = new URL(req.url, "http://localhost");
    if (forwarded.test(url.pathname)) forward(req, res, url);
    else if (req.method === "GET" || req.method === "HEAD") await serveStatic(req, res, url.pathname);
    else res.writeHead(405).end();
  } catch (error) {
    console.error(error);
    if (!res.headersSent) res.writeHead(502, { "content-type": "text/plain" }).end("Bad gateway");
  }
});

server.listen(port, host, () => console.log(`RepoOrbit web on http://${host}:${port}, API at ${api}`));
