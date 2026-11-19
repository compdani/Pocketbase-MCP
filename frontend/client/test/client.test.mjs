import test from 'node:test';
import assert from 'node:assert/strict';

import {
  ChatApiError,
  ChatClient,
  ChatNetworkError,
  ChatProtocolError,
  ResyncRequiredError,
  newIdempotencyKey,
} from '../client.mjs';
import {
  STATUS,
  errorResponse,
  event,
  fakeFetch,
  frames,
  jsonResponse,
  sseResponse,
} from './support.mjs';

const conversation = { id: 'c1', title: 'One', createdAt: '2026-01-02T03:04:05Z', updatedAt: '2026-01-02T03:04:05Z' };
const turn = { id: 't1', conversationId: 'c1', state: 'queued', createdAt: 'x', updatedAt: 'x' };

test('getToken is called before every request and the token only travels in the header', async () => {
  let issued = 0;
  const fetch = fakeFetch((call) => {
    if (call.url.endsWith('/status')) return jsonResponse(STATUS);
    throw new Error(`unexpected ${call.url}`);
  });
  const client = new ChatClient({
    baseUrl: 'https://api.test/',
    getToken: () => `tok-${(issued += 1)}`,
    fetch,
  });

  const first = await client.status();
  const second = await client.status();
  assert.equal(first.version, '1');
  assert.equal(second.version, '1');
  assert.equal(issued, 2, 'getToken runs per request, not once per client');

  assert.equal(fetch.calls[0].url, 'https://api.test/api/mcp/chat/v1/status');
  assert.equal(fetch.calls[0].headers.authorization, 'Bearer tok-1');
  assert.equal(fetch.calls[1].headers.authorization, 'Bearer tok-2');
  for (const call of fetch.calls) {
    assert.equal(call.url.includes('tok-'), false, 'no token in a URL');
    assert.equal(call.url.includes('token='), false);
    assert.equal(call.url.includes('access_token='), false);
  }
});

test('an async getToken is awaited for every request', async () => {
  let issued = 0;
  const fetch = fakeFetch(() => jsonResponse(STATUS));
  const client = new ChatClient({
    baseUrl: '',
    getToken: async () => `async-${(issued += 1)}`,
    fetch,
  });
  await client.status();
  await client.status();
  assert.equal(issued, 2);
  assert.equal(fetch.calls[0].headers.authorization, 'Bearer async-1');
  assert.equal(fetch.calls[1].headers.authorization, 'Bearer async-2');
});

test('path segments are percent-encoded and never treated as separators', async () => {
  const fetch = fakeFetch(() => jsonResponse({
    conversation,
    messages: [],
    nextCursor: '',
    activeTurn: null,
  }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await client.getConversation('a/b c');
  assert.equal(fetch.calls[0].url, 'https://api.test/api/mcp/chat/v1/conversations/a%2Fb%20c');

  await client.getTurn('ü%?');
  assert.equal(fetch.calls[1].url, 'https://api.test/api/mcp/chat/v1/turns/%C3%BC%25%3F');

  await client.cancelTurn('t 1');
  assert.equal(fetch.calls[2].url, 'https://api.test/api/mcp/chat/v1/turns/t%201/cancel');
  assert.equal(fetch.calls[2].method, 'POST', 'cancel has no body and still POSTs');
});

test('list queries carry only the fields the caller set', async () => {
  const fetch = fakeFetch(() => jsonResponse({ items: [], nextCursor: '' }));
  const client = new ChatClient({ baseUrl: '', getToken: () => 't', fetch });

  await client.listConversations({ limit: 10, cursor: 'abc', target: 'rec/1' });
  assert.equal(
    fetch.calls[0].url,
    '/api/mcp/chat/v1/conversations?limit=10&cursor=abc&target=rec%2F1',
  );
  await client.listConversations();
  assert.equal(fetch.calls[1].url, '/api/mcp/chat/v1/conversations');
});

test('the error envelope becomes a ChatApiError with its code', async () => {
  const fetch = fakeFetch(() => errorResponse(404, 'not_found', 'Not found.'));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await assert.rejects(
    client.getConversation('missing'),
    (err) => err instanceof ChatApiError
      && err.status === 404
      && err.code === 'not_found'
      && err.message === 'Not found.',
  );
});

test('a 409 resync_required becomes ResyncRequiredError', async () => {
  const fetch = fakeFetch(() => errorResponse(
    409,
    'resync_required',
    'The event cursor is no longer available; reload the conversation.',
  ));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await assert.rejects(
    client.streamTurn('t1', { after: 2 }),
    (err) => err instanceof ResyncRequiredError
      && err instanceof ChatApiError
      && err.status === 409
      && err.code === 'resync_required'
      && /reload the conversation/.test(err.message),
  );
});

test('a 409 active_turn stays an ordinary ChatApiError', async () => {
  const fetch = fakeFetch(() => errorResponse(409, 'active_turn', 'The conversation already has an active turn.'));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await assert.rejects(
    client.submitTurn('c1', { text: 'hi' }),
    (err) => err instanceof ChatApiError
      && !(err instanceof ResyncRequiredError)
      && err.code === 'active_turn',
  );
});

test('submitTurn sends an Idempotency-Key and reuses an explicit one', async () => {
  const fetch = fakeFetch(() => jsonResponse({ turn, created: true }, 202));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  const generated = await client.submitTurn('c1', { text: 'hello' });
  assert.deepEqual(generated, { turn, created: true });
  const key = fetch.calls[0].headers['idempotency-key'];
  assert.match(key, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.deepEqual(JSON.parse(fetch.calls[0].body), { text: 'hello' });
  assert.equal(fetch.calls[0].method, 'POST');

  await client.submitTurn('c1', { text: 'again', model: 'm1', idempotencyKey: 'fixed-key' });
  assert.equal(fetch.calls[1].headers['idempotency-key'], 'fixed-key');
  assert.deepEqual(JSON.parse(fetch.calls[1].body), { text: 'again', model: 'm1' });
});

test('submitTurn sends a reasoning effort only when one is named', async () => {
  const fetch = fakeFetch(() => jsonResponse({ turn, created: true }, 202));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await client.submitTurn('c1', { text: 'think', model: 'gpt-5', effort: 'high' });
  await client.submitTurn('c1', { text: 'default', model: 'gpt-5', effort: '' });
  assert.deepEqual(JSON.parse(fetch.calls[0].body), { text: 'think', model: 'gpt-5', effort: 'high' });
  assert.deepEqual(JSON.parse(fetch.calls[1].body), { text: 'default', model: 'gpt-5' });
});

test('submitTurn sends a page context object and refuses other shapes', async () => {
  const fetch = fakeFetch(() => jsonResponse({ turn, created: true }, 202));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await client.submitTurn('c1', { text: 'here', context: { route: '/campaigns/7' } });
  await client.submitTurn('c1', { text: 'none', context: null });
  assert.deepEqual(JSON.parse(fetch.calls[0].body), { text: 'here', context: { route: '/campaigns/7' } });
  assert.deepEqual(JSON.parse(fetch.calls[1].body), { text: 'none' });
  await assert.rejects(client.submitTurn('c1', { text: 'bad', context: ['x'] }), TypeError);
  assert.equal(fetch.calls.length, 2);
});

test('newIdempotencyKey produces distinct UUIDs', () => {
  const keys = new Set(Array.from({ length: 50 }, () => newIdempotencyKey()));
  assert.equal(keys.size, 50);
  for (const key of keys) {
    assert.match(key, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  }
});

test('streamTurn sends Last-Event-ID only when resuming', async () => {
  const fetch = fakeFetch(() => sseResponse([event(5, 'turn.completed')]));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await client.streamTurn('t1', { after: 4 });
  assert.equal(fetch.calls[0].headers['last-event-id'], '4');
  assert.equal(fetch.calls[0].headers.accept, 'text/event-stream');
  assert.match(fetch.calls[0].url, /\?after=4$/);

  await client.streamTurn('t1');
  assert.equal(fetch.calls[1].headers['last-event-id'], undefined);
  assert.equal(fetch.calls[1].url, 'https://api.test/api/mcp/chat/v1/turns/t1/events');
});

test('streamTurn parses envelopes, reports heartbeats and resolves with lastSeq/terminal', async () => {
  const events = [
    event(1, 'turn.started'),
    event(2, 'text.delta', { text: 'hi' }),
    event(3, 'turn.completed'),
  ];
  const body = `: ping\n\n${frames(events)}`;
  const fetch = fakeFetch(() => sseResponse(body, { chunkSize: 3 }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  const seen = [];
  const heartbeats = [];
  const result = await client.streamTurn('t1', {
    onEvent: (e) => seen.push(e),
    onHeartbeat: (comment) => heartbeats.push(comment),
  });

  assert.deepEqual(seen.map((e) => e.seq), [1, 2, 3]);
  assert.equal(seen[1].data.text, 'hi');
  assert.deepEqual(heartbeats, ['ping']);
  assert.deepEqual(result, { lastSeq: 3, terminal: true });
});

test('a stream that ends without a terminal event resolves terminal:false', async () => {
  const fetch = fakeFetch(() => sseResponse([event(1, 'turn.started'), event(2, 'text.delta', { text: 'x' })]));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  const result = await client.streamTurn('t1');
  assert.deepEqual(result, { lastSeq: 2, terminal: false });
});

test('an event envelope with an unsupported version is refused', async () => {
  const fetch = fakeFetch(() => sseResponse([event(1, 'turn.started', undefined, { v: '2' })]));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await assert.rejects(
    client.streamTurn('t1'),
    (err) => err instanceof ChatProtocolError && err.code === 'unsupported_version',
  );
});

test('a malformed event body is refused', async () => {
  const fetch = fakeFetch(() => sseResponse('data: not-json\n\n'));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await assert.rejects(client.streamTurn('t1'), ChatProtocolError);
});

test('network failures become ChatNetworkError', async () => {
  const fetch = fakeFetch(() => {
    throw new TypeError('fetch failed');
  });
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await assert.rejects(
    client.status(),
    (err) => err instanceof ChatNetworkError && err.cause instanceof TypeError,
  );
});

test('an aborted request rejects with the abort reason, not a network error', async () => {
  const fetch = fakeFetch((call) => new Promise((_, reject) => {
    call.init.signal.addEventListener('abort', () => reject(call.init.signal.reason), { once: true });
  }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  const controller = new AbortController();
  const pending = client.status({ signal: controller.signal });
  controller.abort();
  await assert.rejects(pending, (err) => err.name === 'AbortError');
});

test('deleteConversation returns null for 204', async () => {
  const fetch = fakeFetch(() => new Response(null, { status: 204 }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  assert.equal(await client.deleteConversation('c1'), null);
  assert.equal(fetch.calls[0].method, 'DELETE');
});

test('createConversation sends only the fields it was given', async () => {
  const fetch = fakeFetch(() => jsonResponse(conversation, 201));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  assert.deepEqual(await client.createConversation(), conversation);
  assert.deepEqual(JSON.parse(fetch.calls[0].body), {});
  await client.createConversation({ title: 'T', target: 'rec/1' });
  assert.deepEqual(JSON.parse(fetch.calls[1].body), { title: 'T', target: 'rec/1' });
});

test('renameConversation PATCHes the title', async () => {
  const fetch = fakeFetch(() => jsonResponse({ ...conversation, title: 'New' }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  const renamed = await client.renameConversation('c1', 'New');
  assert.equal(renamed.title, 'New');
  assert.equal(fetch.calls[0].method, 'PATCH');
  assert.deepEqual(JSON.parse(fetch.calls[0].body), { title: 'New' });
});

test('a wrong prefix normalizes to an absolute path', async () => {
  const fetch = fakeFetch(() => jsonResponse(STATUS));
  const client = new ChatClient({
    baseUrl: 'https://api.test/',
    prefix: 'api/chat/',
    getToken: () => 't',
    fetch,
  });
  await client.status();
  assert.equal(fetch.calls[0].url, 'https://api.test/api/chat/status');
});

// --- proposals ---

const proposal = (overrides = {}) => ({
  id: 'p1',
  conversationId: 'c1',
  tool: 'create_items',
  location: 'server',
  arguments: { data: { title: 'Buy milk' } },
  argumentsHash: 'sha256:abc',
  state: 'pending',
  revision: 1,
  createdAt: '2026-01-02T03:04:05Z',
  updatedAt: '2026-01-02T03:04:05Z',
  ...overrides,
});

test('getProposal reads one proposal from its envelope', async () => {
  const fetch = fakeFetch(() => jsonResponse({ proposal: proposal() }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  assert.deepEqual(await client.getProposal('p1'), proposal());
  assert.equal(fetch.calls[0].url, 'https://api.test/api/mcp/chat/v1/proposals/p1');
  assert.equal(fetch.calls[0].method, 'GET');
});

test('decideProposal sends the reviewed revision and parses replayed', async () => {
  const fetch = fakeFetch(() => jsonResponse({ proposal: proposal({ state: 'succeeded', revision: 2 }) }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  const result = await client.decideProposal('p1', { decision: 'approve', revision: 3 });
  assert.deepEqual(result, {
    proposal: proposal({ state: 'succeeded', revision: 2 }),
    replayed: false,
  });
  assert.equal(fetch.calls[0].method, 'POST');
  assert.equal(fetch.calls[0].url, 'https://api.test/api/mcp/chat/v1/proposals/p1/decision');
  assert.deepEqual(JSON.parse(fetch.calls[0].body), { decision: 'approve', revision: 3 });

  await client.decideProposal('p1', {
    decision: 'reject',
    revision: 2,
    arguments: { data: { title: 'Edited' } },
  });
  assert.deepEqual(JSON.parse(fetch.calls[1].body), {
    decision: 'reject',
    revision: 2,
    arguments: { data: { title: 'Edited' } },
  });

  await assert.rejects(client.decideProposal('p1', { decision: 'maybe', revision: 1 }), TypeError);
  await assert.rejects(client.decideProposal('p1', { decision: 'approve' }), TypeError);
});

test('a replayed decision is reported as replayed', async () => {
  const fetch = fakeFetch(() => jsonResponse({
    proposal: proposal({ state: 'rejected', revision: 2 }),
    replayed: true,
  }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  const result = await client.decideProposal('p1', { decision: 'reject', revision: 2 });
  assert.equal(result.replayed, true);
});

test('a 409 decision conflict carries the stored proposal on the error', async () => {
  const stored = proposal({ state: 'executing', revision: 5 });
  const fetch = fakeFetch(() => jsonResponse({
    error: { code: 'conflict', message: 'The proposal is out of date.' },
    proposal: stored,
  }, 409));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await assert.rejects(
    client.decideProposal('p1', { decision: 'approve', revision: 1 }),
    (err) => err instanceof ChatApiError
      && err.status === 409
      && err.code === 'conflict'
      && err.proposal.revision === 5
      && err.proposal.state === 'executing',
  );
});

test('a 409 outcome conflict carries the stored proposal too', async () => {
  const stored = proposal({ location: 'editor', state: 'stale', revision: 4 });
  const fetch = fakeFetch(() => jsonResponse({
    error: { code: 'conflict', message: 'The proposal is not approved.' },
    proposal: stored,
  }, 409));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await assert.rejects(
    client.reportOutcome('p1', { revision: 2, status: 'applied', editorRevision: '9' }),
    (err) => err instanceof ChatApiError && err.proposal.state === 'stale',
  );
});

test('reportOutcome sends only the fields it was given', async () => {
  const fetch = fakeFetch(() => jsonResponse({ proposal: proposal({ location: 'editor', state: 'applied' }) }));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  await client.reportOutcome('p1', {
    revision: 2,
    status: 'applied',
    editorRevision: 'rev-9',
    hunks: [{ id: 'h1', status: 'applied' }],
  });
  assert.deepEqual(JSON.parse(fetch.calls[0].body), {
    revision: 2,
    status: 'applied',
    editorRevision: 'rev-9',
    hunks: [{ id: 'h1', status: 'applied' }],
  });

  await client.reportOutcome('p1', { revision: 3, status: 'stale' });
  assert.deepEqual(JSON.parse(fetch.calls[1].body), { revision: 3, status: 'stale' });

  await client.reportOutcome('p1', { revision: 3, status: 'failed', message: 'boom' });
  assert.deepEqual(JSON.parse(fetch.calls[2].body), { revision: 3, status: 'failed', message: 'boom' });

  await assert.rejects(client.reportOutcome('p1', { status: 'applied' }), TypeError);
});

// --- uploads ---

test('uploadFile sends multipart/form-data with the server field names', async () => {
  const upload = {
    id: 'u1',
    conversationId: 'c1',
    name: 'notes.txt',
    mime: 'text/plain',
    kind: 'text',
    size: 5,
    sha256: 'sha256:x',
    createdAt: 'x',
  };
  const fetch = fakeFetch(() => jsonResponse({ upload }, 201));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });

  const file = new Blob(['hello'], { type: 'text/plain' });
  const result = await client.uploadFile('c1', file, 'notes.txt');

  assert.deepEqual(result, upload);
  assert.equal(fetch.calls[0].method, 'POST');
  assert.equal(fetch.calls[0].url, 'https://api.test/api/mcp/chat/v1/uploads');
  assert.equal(fetch.calls[0].headers.authorization, 'Bearer t');
  assert.equal(
    fetch.calls[0].headers['content-type'],
    undefined,
    'the multipart boundary is left to fetch',
  );

  const form = fetch.calls[0].body;
  assert.ok(form instanceof FormData);
  assert.equal(form.get('conversationId'), 'c1');
  const sent = form.get('file');
  assert.equal(sent.name, 'notes.txt');
  assert.equal(sent.type, 'text/plain');
  assert.equal(await sent.text(), 'hello');
});

test('uploadFile defaults the file name and refuses a missing conversation', async () => {
  const fetch = fakeFetch(() => jsonResponse({ upload: { id: 'u1' } }, 201));
  const client = new ChatClient({ baseUrl: '', getToken: () => 't', fetch });

  await client.uploadFile('c1', new Blob(['x'], { type: 'text/plain' }));
  assert.equal(fetch.calls[0].body.get('file').name, 'upload');

  await assert.rejects(client.uploadFile('', new Blob(['x'])), TypeError);
  await assert.rejects(client.uploadFile('c1', null), TypeError);
});

test('an upload the server refuses keeps its typed code', async () => {
  const fetch = fakeFetch(() => errorResponse(415, 'unsupported_media', 'The file type is not supported.'));
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
  await assert.rejects(
    client.uploadFile('c1', new Blob(['x'], { type: 'application/zip' }), 'x.zip'),
    (err) => err instanceof ChatApiError && err.status === 415 && err.code === 'unsupported_media',
  );
});

test('getUploadMeta and downloadUpload use authenticated requests, never a token URL', async () => {
  const upload = { id: 'u1', conversationId: 'c1', name: 'a.txt', mime: 'text/plain' };
  const fetch = fakeFetch((call) => {
    if (call.url.endsWith('/meta')) return jsonResponse({ upload });
    return new Response(new Blob(['file bytes'], { type: 'text/plain' }), { status: 200 });
  });
  const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 'secret-token', fetch });

  assert.deepEqual(await client.getUploadMeta('u1'), upload);
  assert.equal(fetch.calls[0].url, 'https://api.test/api/mcp/chat/v1/uploads/u1/meta');

  const blob = await client.downloadUpload('u1');
  assert.equal(blob instanceof Blob, true);
  assert.equal(await blob.text(), 'file bytes');
  assert.equal(fetch.calls[1].url, 'https://api.test/api/mcp/chat/v1/uploads/u1');
  assert.equal(fetch.calls[1].headers.authorization, 'Bearer secret-token');
  for (const call of fetch.calls) {
    assert.equal(call.url.includes('secret-token'), false);
    assert.equal(call.url.includes('token'), false);
  }
});

test('submitTurn includes attachments only when it has some', async () => {
  const fetch = fakeFetch(() => jsonResponse({ turn, created: true }, 202));
  const client = new ChatClient({ baseUrl: '', getToken: () => 't', fetch });

  await client.submitTurn('c1', { text: 'see this', attachments: ['u1', 'u2'] });
  assert.deepEqual(JSON.parse(fetch.calls[0].body), {
    text: 'see this',
    attachments: ['u1', 'u2'],
  });

  await client.submitTurn('c1', { text: 'plain' });
  assert.deepEqual(JSON.parse(fetch.calls[1].body), { text: 'plain' });
});

test('getConversation passes the conversation proposals through', async () => {
  const fetch = fakeFetch(() => jsonResponse({
    conversation,
    messages: [],
    nextCursor: '',
    activeTurn: null,
    proposals: [proposal()],
  }));
  const client = new ChatClient({ baseUrl: '', getToken: () => 't', fetch });
  const detailResult = await client.getConversation('c1');
  assert.deepEqual(detailResult.proposals, [proposal()]);
});

test('an ordinary ChatApiError carries no proposal', async () => {
  const fetch = fakeFetch(() => errorResponse(404, 'not_found', 'Not found.'));
  const client = new ChatClient({ baseUrl: '', getToken: () => 't', fetch });
  await assert.rejects(
    client.getProposal('missing'),
    (err) => err instanceof ChatApiError && err.proposal === null,
  );
});
