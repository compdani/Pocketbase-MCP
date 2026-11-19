// Node tests for serve.mjs: traversal, MIME types and the /api/ proxy.

import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';

import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { AGENT_PANEL_CDN, MIME_TYPES, parsePath, startChatServer } from './serve.mjs';

/** A stand-in agent-panel checkout, so tests never depend on the network. */
function fakeAgentPanelDir() {
  const dir = mkdtempSync(path.join(tmpdir(), 'agent-panel-'));
  writeFileSync(path.join(dir, 'page.js'), 'export {};\n');
  return dir;
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => resolve(server.address().port));
  });
}

function close(server) {
  return new Promise((resolve) => {
    server.close(() => resolve());
    // SSE test connections stay open by design; do not wait on them.
    server.closeAllConnections?.();
  });
}

/** A raw GET: fetch normalises `..`, and these tests must not. */
function rawGet(port, rawPath, headers = {}) {
  return new Promise((resolve, reject) => {
    const request = http.request({
      host: '127.0.0.1', port, path: rawPath, method: 'GET', headers,
    }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => { body += chunk; });
      response.on('end', () => resolve({ status: response.statusCode, headers: response.headers, body }));
    });
    request.on('error', reject);
    request.end();
  });
}

const withTimeout = (promise, ms = 3000, label = 'operation') => {
  let timer;
  return Promise.race([
    Promise.resolve(promise).finally(() => clearTimeout(timer)),
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} timed out after ${ms}ms`)), ms);
    }),
  ]);
};

test('parsePath rejects dot segments before any normalisation', () => {
  assert.deepEqual(parsePath('/client/controller.mjs'), { segments: ['client', 'controller.mjs'] });
  assert.ok(parsePath('/client/../../etc/passwd').error);
  assert.ok(parsePath('/client/%2e%2e/%2e%2e/etc/passwd').error);
  assert.ok(parsePath('/%2e%2e/etc/passwd').error);
  assert.ok(parsePath('/agent-panel/.git/config').error);
  assert.ok(parsePath('/client/%00/controller.mjs').error);
});

test('static files are served with the right MIME types', async (t) => {
  const app = await startChatServer({ port: 0, agentPanelDir: fakeAgentPanelDir() });
  t.after(() => close(app.server));

  const index = await rawGet(app.port, '/');
  assert.equal(index.status, 200);
  assert.equal(index.headers['content-type'], MIME_TYPES['.html']);
  assert.match(index.body, /<agent-page/);

  const appModule = await rawGet(app.port, '/app.mjs');
  assert.equal(appModule.status, 200);
  assert.equal(appModule.headers['content-type'], MIME_TYPES['.mjs']);

  const controller = await rawGet(app.port, '/client/controller.mjs');
  assert.equal(controller.status, 200);
  assert.equal(controller.headers['content-type'], MIME_TYPES['.mjs']);
  assert.match(controller.body, /export class ChatController/);

  const binding = await rawGet(app.port, '/client/agent-panel.mjs');
  assert.equal(binding.status, 200);
  assert.equal(binding.headers['content-type'], MIME_TYPES['.mjs']);

  const element = await rawGet(app.port, '/agent-panel/page.js');
  assert.equal(element.status, 200);
  assert.equal(element.headers['content-type'], MIME_TYPES['.js']);

  const css = await rawGet(app.port, '/app.css');
  assert.equal(css.status, 200);
  assert.equal(css.headers['content-type'], MIME_TYPES['.css']);
  assert.match(css.body, /agent-page/);

  const missing = await rawGet(app.port, '/does-not-exist.html');
  assert.equal(missing.status, 404);
});

test('agent-panel redirects to the pinned CDN release without AGENT_PANEL_DIR', async (t) => {
  const app = await startChatServer({ port: 0, agentPanelDir: null });
  t.after(() => close(app.server));

  const element = await rawGet(app.port, '/agent-panel/page.js');
  assert.equal(element.status, 302);
  assert.equal(element.headers.location, `${AGENT_PANEL_CDN}/page.js`);
  assert.match(AGENT_PANEL_CDN, /^https:\/\/cdn\.jsdelivr\.net\/gh\/compdani\/agent-panel@v\d+\.\d+\.\d+$/);

  assert.ok((await rawGet(app.port, '/agent-panel/.git/config')).status === 403);
});

test('path traversal is rejected, literal and percent-encoded', async (t) => {
  const app = await startChatServer({ port: 0, agentPanelDir: fakeAgentPanelDir() });
  t.after(() => close(app.server));

  for (const path of [
    '/client/../../etc/passwd',
    '/client/%2e%2e/%2e%2e/etc/passwd',
    '/%2e%2e/%2e%2e/etc/passwd',
    '/agent-panel/../frontend/client/controller.mjs',
    '/agent-panel/.git/config',
    '/client/..%2f..%2fetc/passwd',
  ]) {
    const response = await rawGet(app.port, path);
    assert.ok(
      response.status === 403 || response.status === 404,
      `${path} answered ${response.status}`,
    );
    assert.ok(!response.body.includes('root:'), `${path} leaked /etc/passwd`);
  }
});

async function startUpstream(handler) {
  const server = http.createServer(handler);
  const port = await listen(server);
  return { server, port, url: `http://127.0.0.1:${port}` };
}

test('the proxy streams SSE as it arrives and forwards the chat headers', async (t) => {
  let seenHeaders = null;
  let upstreamEnded = false;
  let releaseSecondChunk;
  const secondChunk = new Promise((resolve) => { releaseSecondChunk = resolve; });

  const upstream = await startUpstream((req, res) => {
    seenHeaders = req.headers;
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
    });
    res.write('data: {"seq":1}\n\n');
    // Wait until the test has seen the first event before ending the response.
    secondChunk.then(() => {
      upstreamEnded = true;
      res.write('data: {"seq":2}\n\n');
      res.end();
    });
  });
  t.after(() => close(upstream.server));

  const app = await startChatServer({ port: 0, pbUrl: upstream.url });
  t.after(() => close(app.server));

  const response = await withTimeout(fetch(`${app.url}/api/mcp/chat/v1/turns/t1/events`, {
    headers: {
      Authorization: 'Bearer demo-token',
      'Last-Event-ID': '4',
      'Idempotency-Key': 'key-1',
      Accept: 'text/event-stream',
    },
  }), 3000, 'proxy response');

  assert.equal(response.status, 200);
  assert.match(response.headers.get('content-type') ?? '', /text\/event-stream/);

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  const first = await withTimeout(reader.read(), 2000, 'first SSE chunk');
  const firstText = decoder.decode(first.value ?? new Uint8Array());
  assert.match(firstText, /"seq":1/);
  assert.equal(upstreamEnded, false, 'the first chunk must not wait for the upstream to end');

  releaseSecondChunk();
  let rest = '';
  for (;;) {
    const { done, value } = await withTimeout(reader.read(), 2000, 'remaining SSE chunks');
    if (done) break;
    rest += decoder.decode(value);
  }
  assert.match(rest, /"seq":2/);

  assert.equal(seenHeaders.authorization, 'Bearer demo-token');
  assert.equal(seenHeaders['last-event-id'], '4');
  assert.equal(seenHeaders['idempotency-key'], 'key-1');
  assert.equal(seenHeaders.accept, 'text/event-stream');
});

test('a POST body is forwarded (the auth endpoint)', async (t) => {
  let seen = null;
  const upstream = await startUpstream((req, res) => {
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (chunk) => { body += chunk; });
    req.on('end', () => {
      seen = { method: req.method, headers: req.headers, body };
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{"token":"t"}');
    });
  });
  t.after(() => close(upstream.server));

  const app = await startChatServer({ port: 0, pbUrl: upstream.url });
  t.after(() => close(app.server));

  const response = await fetch(`${app.url}/api/collections/users/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ identity: 'demo@example.com', password: 'password123' }),
  });
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { token: 't' });
  assert.equal(seen.method, 'POST');
  assert.equal(seen.headers['content-type'], 'application/json');
  assert.match(seen.body, /demo@example\.com/);
});

test('aborting the browser request aborts the upstream request', async (t) => {
  let upstreamClosed;
  const closed = new Promise((resolve) => { upstreamClosed = resolve; });

  const upstream = await startUpstream((req, res) => {
    req.on('close', () => upstreamClosed('request closed'));
    res.on('close', () => upstreamClosed('response closed'));
    res.writeHead(200, { 'Content-Type': 'text/event-stream' });
    res.write('data: {"seq":1}\n\n');
    // Never ends: only the client abort can stop it.
  });
  t.after(() => close(upstream.server));

  const app = await startChatServer({ port: 0, pbUrl: upstream.url });
  t.after(() => close(app.server));

  const controller = new AbortController();
  const response = await fetch(`${app.url}/api/mcp/chat/v1/turns/t1/events`, {
    signal: controller.signal,
    headers: { Accept: 'text/event-stream' },
  });
  const reader = response.body.getReader();
  await reader.read();
  controller.abort();

  const reason = await withTimeout(closed, 3000, 'upstream abort');
  assert.ok(/closed/.test(reason));
});
