// Shared helpers for the node --test suite. No network: every fetch is a fake
// and every response body is a ReadableStream the test controls.

export const tick = (ms = 0) => new Promise((resolve) => setTimeout(resolve, ms));

export function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Fail a test instead of hanging when an await never settles. */
export function withTimeout(promise, ms = 3000, label = 'operation') {
  let timer;
  return Promise.race([
    Promise.resolve(promise).finally(() => clearTimeout(timer)),
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} timed out after ${ms}ms`)), ms);
    }),
  ]);
}

const encoder = new TextEncoder();

export function byteStream(text, { chunkSize } = {}) {
  const bytes = encoder.encode(text);
  const size = (chunkSize ?? bytes.length) || 1;
  return new ReadableStream({
    start(controller) {
      for (let i = 0; i < bytes.length; i += size) {
        controller.enqueue(bytes.slice(i, i + size));
      }
      controller.close();
    },
  });
}

/** A stream split into exactly two chunks at `offset` (every byte boundary). */
export function streamSplitAt(text, offset) {
  const bytes = encoder.encode(text);
  const at = Math.min(Math.max(offset, 0), bytes.length);
  return new ReadableStream({
    start(controller) {
      controller.enqueue(bytes.slice(0, at));
      controller.enqueue(bytes.slice(at));
      controller.close();
    },
  });
}

/** A stream of the given chunks, as strings encoded independently. */
export function chunkStream(chunks) {
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}

/** A stream that yields `text`, then stays open until cancelled. */
export function openStream(text = '') {
  const bytes = encoder.encode(text);
  return new ReadableStream({
    start(controller) {
      if (bytes.length) controller.enqueue(bytes);
    },
  });
}

export async function collect(iterable) {
  const out = [];
  for await (const value of iterable) out.push(value);
  return out;
}

// --- chat protocol fixtures ---

export function event(seq, type, data, { turn = 't1', conv = 'c1', v = '1' } = {}) {
  const envelope = {
    v,
    seq,
    conversationId: conv,
    turnId: turn,
    type,
    at: new Date(Date.UTC(2026, 0, 2, 3, 4, 5, seq)).toISOString(),
  };
  if (data !== undefined) envelope.data = data;
  return envelope;
}

export function frame(envelope) {
  return `id: ${envelope.seq}\nevent: ${envelope.type}\ndata: ${JSON.stringify(envelope)}\n\n`;
}

export function frames(events) {
  return events.map(frame).join('');
}

export function message(id, role, text, { conversationId = 'c1', turnId = '', parts } = {}) {
  return {
    id,
    conversationId,
    ...(turnId ? { turnId } : {}),
    role,
    parts: parts ?? [{ type: 'text', text }],
    createdAt: '2026-01-02T03:04:05Z',
  };
}

// --- fetch fixtures ---

export function jsonResponse(value, status = 200, headers = {}) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8', ...headers },
  });
}

export function errorResponse(status, code, message) {
  return jsonResponse({ error: { code, message } }, status);
}

export function sseResponse(events, { status = 200, chunkSize = 5 } = {}) {
  const text = typeof events === 'string' ? events : frames(events);
  return new Response(byteStream(text, { chunkSize }), {
    status,
    headers: { 'Content-Type': 'text/event-stream' },
  });
}

export function fakeFetch(handler) {
  const calls = [];
  const impl = async (url, init = {}) => {
    const call = {
      url,
      init,
      method: init.method ?? 'GET',
      headers: Object.fromEntries(
        Object.entries(init.headers ?? {}).map(([k, v]) => [k.toLowerCase(), v]),
      ),
      body: init.body,
    };
    calls.push(call);
    return handler(call);
  };
  impl.calls = calls;
  return impl;
}

export const STATUS = {
  version: '1',
  models: ['model-a'],
  features: { proposals: false, uploads: false },
  limits: { maxMessageChars: 16000 },
};

// --- controller fixtures ---

export function makeClient(overrides = {}) {
  const calls = [];
  const client = {
    calls,
    statusResult: { ...STATUS },
    pageResult: { items: [], nextCursor: '' },
    detailResult: null,
    submitResult: null,
    turnResult: null,
    cancelResult: null,
    proposalResult: null,
    decideResult: null,
    outcomeResult: null,
    uploadResult: null,
    uploadMetaResult: null,
    downloadResult: null,
    uploads: 0,

    async status() {
      calls.push({ method: 'status' });
      return client.statusResult;
    },
    async listConversations(options = {}) {
      calls.push({ method: 'listConversations', options });
      return client.pageResult;
    },
    async createConversation(options = {}) {
      calls.push({ method: 'createConversation', options });
      return client.createResult;
    },
    async getConversation(id, options = {}) {
      calls.push({ method: 'getConversation', id, options });
      return client.detailResult;
    },
    async renameConversation(id, title) {
      calls.push({ method: 'renameConversation', id, title });
      return client.renameResult;
    },
    async deleteConversation(id) {
      calls.push({ method: 'deleteConversation', id });
      return null;
    },
    async submitTurn(id, options) {
      calls.push({ method: 'submitTurn', id, options });
      return client.submitResult;
    },
    async getTurn(id) {
      calls.push({ method: 'getTurn', id });
      return client.turnResult;
    },
    async cancelTurn(id) {
      calls.push({ method: 'cancelTurn', id });
      return client.cancelResult;
    },
    async getProposal(id) {
      calls.push({ method: 'getProposal', id });
      return client.proposalResult;
    },
    async decideProposal(id, options) {
      calls.push({ method: 'decideProposal', id, options });
      return client.decideResult;
    },
    async reportOutcome(id, options) {
      calls.push({ method: 'reportOutcome', id, options });
      return client.outcomeResult;
    },
    async uploadFile(conversationId, file, name) {
      client.uploads += 1;
      const call = { method: 'uploadFile', conversationId, file, name };
      calls.push(call);
      if (typeof client.uploadResult === 'function') return client.uploadResult(file, name, client.uploads);
      return client.uploadResult ?? {
        id: `u${client.uploads}`,
        conversationId,
        name: name || file?.name || 'upload',
        mime: file?.type || 'text/plain',
        kind: 'text',
        size: file?.size ?? 0,
        sha256: 'sha256:x',
        createdAt: 'x',
      };
    },
    async getUploadMeta(id) {
      calls.push({ method: 'getUploadMeta', id });
      return client.uploadMetaResult;
    },
    async downloadUpload(id) {
      calls.push({ method: 'downloadUpload', id });
      return client.downloadResult;
    },
    async streamTurn(id, options) {
      calls.push({ method: 'streamTurn', id, after: options.after });
      return { lastSeq: options.after ?? 0, terminal: true };
    },
  };
  return Object.assign(client, overrides);
}

export const detail = (conversation, messages = [], activeTurn = null, proposals = undefined) => ({
  conversation,
  messages,
  nextCursor: '',
  activeTurn,
  ...(proposals === undefined ? {} : { proposals }),
});
