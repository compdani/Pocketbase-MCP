// Zero-dependency development server for examples/chat/plain.
//
// One origin, so the browser never needs CORS:
//
//   /            examples/chat/plain (this folder)
//   /client/     frontend/client
//   /agent-panel/ agent-panel: redirected to the pinned release on jsDelivr,
//                 or served from a local checkout when AGENT_PANEL_DIR is set
//   /api/        reverse proxy to PocketBase (PB_URL), streamed
//
// The proxy pipes the upstream body straight through and flushes headers, so
// Server-Sent Events arrive as they are produced instead of being buffered. A
// browser disconnect destroys the upstream request, which is how a closed tab
// stops a chat stream.
//
// Local development only: it binds 127.0.0.1 and serves files without auth.

import http from 'node:http';
import https from 'node:https';
import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));

/**
 * The agent-panel release this example is built against
 * (github.com/compdani/agent-panel). Without AGENT_PANEL_DIR, `/agent-panel/*`
 * redirects here.
 */
export const AGENT_PANEL_VERSION = 'v0.5.1';
export const AGENT_PANEL_CDN = `https://cdn.jsdelivr.net/gh/compdani/agent-panel@${AGENT_PANEL_VERSION}`;
export const DEFAULT_PB_URL = 'http://127.0.0.1:8090';
export const DEFAULT_PORT = 5174;
export const BIND_HOST = '127.0.0.1';

export const MIME_TYPES = Object.freeze({
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.ico': 'image/x-icon',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.map': 'application/json; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.txt': 'text/plain; charset=utf-8',
  '.webmanifest': 'application/manifest+json',
});

// Dotfiles (.git, .env, …) are never served; only these extensions are.
const ALLOWED_EXTENSIONS = new Set(Object.keys(MIME_TYPES));

// Headers the browser request passes on to PocketBase. Anything else (cookies,
// origin, host, …) belongs to this origin, not to the proxied API.
const FORWARD_REQUEST_HEADERS = Object.freeze([
  'authorization',
  'content-type',
  'accept',
  'idempotency-key',
  'last-event-id',
  'content-length',
]);

const HOP_BY_HOP = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
]);

/** Resolve the served directories and the proxy target. */
export function resolveOptions(overrides = {}) {
  const appDir = path.resolve(overrides.appDir ?? HERE);
  return {
    appDir,
    clientDir: path.resolve(overrides.clientDir ?? path.join(appDir, '..', '..', '..', 'frontend', 'client')),
    // null: use the pinned release on the CDN. An explicit `agentPanelDir:
    // null` override ignores AGENT_PANEL_DIR.
    agentPanelDir: (() => {
      const dir = 'agentPanelDir' in overrides ? overrides.agentPanelDir : process.env.AGENT_PANEL_DIR;
      return dir ? path.resolve(dir) : null;
    })(),
    pbUrl: overrides.pbUrl ?? process.env.PB_URL ?? DEFAULT_PB_URL,
  };
}

function sendText(res, status, body, headers = {}) {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  const text = `${body}\n`;
  res.writeHead(status, {
    'Content-Type': 'text/plain; charset=utf-8',
    'Content-Length': Buffer.byteLength(text),
    ...headers,
  });
  res.end(text);
}

/**
 * Decode a URL path into path segments, rejecting anything that could escape a
 * served root. Rejection is a 403: the raw `..` segment is checked before any
 * normalisation, so `/client/../../etc/passwd` and `%2e%2e` variants fail here
 * rather than resolving inside a root.
 *
 * @returns {{ segments: string[] } | { error: string }}
 */
export function parsePath(rawUrl) {
  const rawPath = String(rawUrl ?? '/').split('?')[0];
  let decoded;
  try {
    decoded = decodeURIComponent(rawPath);
  } catch {
    return { error: 'Malformed path.' };
  }
  if (decoded.includes('\0')) return { error: 'Malformed path.' };

  const segments = [];
  for (const segment of decoded.split('/')) {
    if (segment === '' || segment === '.') continue;
    if (segment === '..') return { error: 'Path traversal is not allowed.' };
    if (segment.startsWith('.')) return { error: 'Hidden paths are not served.' };
    segments.push(segment);
  }
  return { segments };
}

function rootFor(segments, options) {
  if (segments[0] === 'client') {
    return { root: options.clientDir, rest: segments.slice(1) };
  }
  if (segments[0] === 'agent-panel') {
    return { root: options.agentPanelDir, rest: segments.slice(1) };
  }
  return { root: options.appDir, rest: segments };
}

async function handleStatic(req, res, options) {
  const parsed = parsePath(req.url);
  if (parsed.error) {
    sendText(res, 403, parsed.error);
    return;
  }

  if (parsed.segments[0] === 'agent-panel' && !options.agentPanelDir) {
    const rest = parsed.segments.slice(1).map(encodeURIComponent).join('/');
    res.writeHead(302, { Location: `${AGENT_PANEL_CDN}/${rest}`, 'Cache-Control': 'no-store' });
    res.end();
    return;
  }

  let { root, rest } = rootFor(parsed.segments, options);
  if (rest.length === 0) rest = ['index.html'];

  const filePath = path.resolve(root, ...rest);
  if (filePath !== root && !filePath.startsWith(`${root}${path.sep}`)) {
    sendText(res, 403, 'Path traversal is not allowed.');
    return;
  }
  const extension = path.extname(filePath).toLowerCase();
  if (!ALLOWED_EXTENSIONS.has(extension)) {
    sendText(res, 404, 'Not found.');
    return;
  }

  let info;
  try {
    info = await stat(filePath);
  } catch {
    sendText(res, 404, 'Not found.');
    return;
  }
  if (!info.isFile()) {
    sendText(res, 404, 'Not found.');
    return;
  }

  res.writeHead(200, {
    'Content-Type': MIME_TYPES[extension] ?? 'application/octet-stream',
    'Content-Length': info.size,
    'Cache-Control': 'no-store',
  });
  if (req.method === 'HEAD') {
    res.end();
    return;
  }
  const stream = createReadStream(filePath);
  stream.on('error', () => res.destroy());
  stream.pipe(res);
}

function proxyApi(req, res, options) {
  let target;
  try {
    target = new URL(req.url, options.pbUrl);
  } catch {
    sendText(res, 400, 'Bad request.');
    return;
  }

  const headers = {};
  for (const name of FORWARD_REQUEST_HEADERS) {
    const value = req.headers[name];
    if (value !== undefined) headers[name] = value;
  }

  const transport = target.protocol === 'https:' ? https : http;
  const upstream = transport.request({
    protocol: target.protocol,
    hostname: target.hostname,
    port: target.port || (target.protocol === 'https:' ? 443 : 80),
    method: req.method,
    path: `${target.pathname}${target.search}`,
    headers,
  }, (upstreamRes) => {
    const responseHeaders = {};
    for (const [name, value] of Object.entries(upstreamRes.headers)) {
      if (value !== undefined && !HOP_BY_HOP.has(name)) responseHeaders[name] = value;
    }
    res.writeHead(upstreamRes.statusCode ?? 502, responseHeaders);
    // A cache or a browser holds an SSE body until it is flushed; headers go
    // out before the first event so the client can start reading at once.
    if (typeof res.flushHeaders === 'function') res.flushHeaders();
    else if (typeof res.flush === 'function') res.flush();
    upstreamRes.pipe(res);
    upstreamRes.on('error', () => res.destroy());
  });

  upstream.on('error', (err) => {
    if (!res.headersSent) sendText(res, 502, `Bad gateway: ${err.message}`);
    else res.destroy();
  });
  // Closing the tab (or a fetch abort) must stop the upstream stream too.
  res.on('close', () => {
    if (!res.writableEnded) upstream.destroy();
  });
  req.on('error', () => upstream.destroy());
  req.pipe(upstream);
}

/** The request handler: static files first, `/api/` proxied to PocketBase. */
export function createHandler(overrides = {}) {
  const options = resolveOptions(overrides);
  return function handle(req, res) {
    const rawPath = String(req.url ?? '/').split('?')[0];
    if (rawPath === '/api' || rawPath.startsWith('/api/')) {
      proxyApi(req, res, options);
      return;
    }
    handleStatic(req, res, options).catch(() => sendText(res, 500, 'Internal error.'));
  };
}

export function createChatServer(overrides = {}) {
  return http.createServer(createHandler(overrides));
}

/** Listen on 127.0.0.1 and return the bound URL. */
export async function startChatServer(overrides = {}) {
  const server = createChatServer(overrides);
  const port = Number(overrides.port ?? process.env.PORT ?? DEFAULT_PORT);
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, BIND_HOST, resolve);
  });
  const address = server.address();
  const boundPort = typeof address === 'object' && address ? address.port : port;
  return {
    server,
    port: boundPort,
    url: `http://${BIND_HOST}:${boundPort}`,
    options: resolveOptions(overrides),
    close: () => new Promise((resolve) => server.close(() => resolve())),
  };
}

const isMain = process.argv[1] !== undefined
  && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;

if (isMain) {
  const running = await startChatServer();
  console.log(`chat demo: ${running.url}`);
  console.log(`  app         ${running.options.appDir}`);
  console.log(`  client      ${running.options.clientDir}`);
  console.log(`  agent-panel ${running.options.agentPanelDir ?? `${AGENT_PANEL_CDN} (set AGENT_PANEL_DIR for a local checkout)`}`);
  console.log(`  proxy       /api/ -> ${running.options.pbUrl}`);
  console.log('Local development only. Ctrl+C stops it.');
}
