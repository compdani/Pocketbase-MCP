import test from 'node:test';
import assert from 'node:assert/strict';

import { ChatController, accountKeyOf, attachmentTypesFor, resolveChoice } from '../controller.mjs';
import { ChatApiError, ChatNetworkError, ResyncRequiredError } from '../client.mjs';
import { GapError } from '../reducer.mjs';
import {
  STATUS,
  detail,
  event,
  makeClient,
  message,
  tick,
  withTimeout,
} from './support.mjs';

const accountA = {
  appUrl: 'https://a.test',
  mount: '/',
  authCollectionId: 'users',
  userId: 'u1',
};
const accountB = { ...accountA, userId: 'u2' };

const conversation = (id, title = id) => ({ id, title, createdAt: 'x', updatedAt: 'x' });
const turn = (state, overrides = {}) => ({
  id: 't1',
  conversationId: 'c1',
  state,
  createdAt: 'x',
  updatedAt: 'x',
  ...overrides,
});

const settle = (controller) => withTimeout(controller.whenSettled(), 3000, 'controller settle');

test('init loads status, models, limits and the first conversation page', async () => {
  const client = makeClient();
  client.pageResult = { items: [conversation('c1', 'One')], nextCursor: 'page-2' };

  const controller = new ChatController({ client });
  const before = controller.state;
  assert.equal(before.status, 'idle');

  const changes = [];
  controller.addEventListener('change', () => changes.push(controller.state));

  await controller.init();

  assert.notEqual(controller.state, before, 'state is replaced, never mutated');
  assert.equal(before.status, 'idle');
  assert.equal(controller.state.status, 'ready');
  assert.deepEqual(controller.state.models, ['model-a']);
  assert.deepEqual(controller.state.limits, { maxMessageChars: 16000 });
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c1']);
  assert.equal(controller.state.nextCursor, 'page-2');
  assert.ok(changes.length >= 2, 'loading and ready both emit change');
});

test('init loads catalog choices and send resolves provider and model', async () => {
  const client = makeClient();
  client.statusResult = {
    ...STATUS,
    models: ['m1'],
    choices: [
      { id: 'openai::m1', provider: 'openai', model: 'm1', label: 'Model One', group: 'OpenAI', default: true },
    ],
    defaultChoice: 'openai::m1',
  };
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };
  client.streamTurn = async () => ({ lastSeq: 0, terminal: true });

  const controller = new ChatController({ client });
  await controller.init();
  assert.deepEqual(controller.state.choices.map((c) => c.id), ['openai::m1']);
  assert.equal(controller.state.defaultChoice, 'openai::m1');
  assert.equal(controller.state.choice, 'openai::m1');

  await controller.open('c1');
  await controller.send('hello', { model: 'openai::m1' });
  await settle(controller);

  const submit = client.calls.find((c) => c.method === 'submitTurn');
  assert.equal(submit.options.provider, 'openai');
  assert.equal(submit.options.model, 'm1');
  assert.equal(controller.state.choice, 'openai::m1');
});

test('resolveChoice does not silently switch an unknown picker value', () => {
  const choices = [
    { id: 'a::m1', provider: 'a', model: 'm1', default: true },
    { id: 'b::m2', provider: 'b', model: 'm2' },
  ];
  assert.equal(resolveChoice(choices, 'b::m2')?.provider, 'b');
  assert.equal(resolveChoice(choices, 'm2')?.provider, 'b');
  assert.equal(resolveChoice(choices, 'missing'), null);
  assert.equal(resolveChoice(choices, '')?.provider, 'a');
});

test('init surfaces an API error in state instead of throwing', async () => {
  const client = makeClient();
  client.status = async () => {
    const err = new Error('Authentication is required.');
    err.code = 'unauthorized';
    err.status = 401;
    throw err;
  };
  const controller = new ChatController({ client });
  await controller.init();
  assert.equal(controller.state.status, 'error');
  assert.deepEqual(controller.state.error, {
    code: 'unauthorized',
    message: 'Authentication is required.',
  });
});

test('send streams to terminal and then reloads the canonical transcript', async () => {
  const client = makeClient();
  client.pageResult = { items: [conversation('c1')], nextCursor: '' };
  client.submitResult = { turn: turn('queued'), created: true };
  client.detailResult = detail(conversation('c1'), [
    message('m1', 'user', 'hello'),
    message('m2', 'assistant', 'hi', { turnId: 't1' }),
  ]);

  const streamed = [];
  client.streamTurn = async (id, options) => {
    const events = [
      event(1, 'turn.queued'),
      event(2, 'turn.started'),
      event(3, 'text.delta', { text: 'hi' }),
      event(4, 'turn.completed'),
    ];
    for (const e of events) {
      streamed.push(e.seq);
      await options.onEvent(e);
    }
    return { lastSeq: 4, terminal: true };
  };

  const controller = new ChatController({ client, reconnect: { initialDelayMs: 5 } });
  await controller.init();
  await controller.open('c1');
  const result = await controller.send('hello');

  assert.deepEqual(result, { turn: turn('queued'), created: true });
  await settle(controller);

  assert.deepEqual(streamed, [1, 2, 3, 4]);
  assert.equal(controller.state.turn.state, 'completed');
  assert.equal(controller.state.turn.text, 'hi');
  assert.equal(controller.state.turn.lastSeq, 4);
  assert.equal(controller.state.running, false);
  assert.deepEqual(controller.state.messages.map((m) => m.id), ['m1', 'm2']);

  const calls = client.calls.filter((call) => call.method === 'getConversation');
  assert.equal(calls.length, 2, 'one read on open, one canonical reload after terminal');
});

test('a lost POST response is retried with the same Idempotency-Key', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  const keys = [];
  let submits = 0;
  client.submitTurn = async (id, options) => {
    submits += 1;
    keys.push(options.idempotencyKey);
    if (submits === 1) throw new ChatNetworkError('the response was lost');
    return { turn: turn('queued'), created: true };
  };
  client.streamTurn = async () => ({ lastSeq: 0, terminal: true });

  const controller = new ChatController({ client, reconnect: { initialDelayMs: 5 } });
  await controller.init();
  await controller.open('c1');
  const result = await controller.send('hello');
  await settle(controller);

  assert.equal(submits, 2, 'one retry, no duplicate submit');
  assert.equal(keys[0], keys[1], 'the retry reuses the key');
  assert.ok(keys[0]);
  assert.equal(result.created, true);
});

test('a disconnect mid-stream reconnects from lastSeq without duplicating text', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };

  const afters = [];
  client.streamTurn = async (id, options) => {
    afters.push(options.after);
    if (afters.length === 1) {
      await options.onEvent(event(1, 'turn.started'));
      await options.onEvent(event(2, 'text.delta', { text: 'ab' }));
      return { lastSeq: 2, terminal: false };
    }
    await options.onEvent(event(3, 'text.delta', { text: 'cd' }));
    await options.onEvent(event(4, 'turn.completed'));
    return { lastSeq: 4, terminal: true };
  };

  const controller = new ChatController({ client, reconnect: { initialDelayMs: 5 } });
  await controller.init();
  await controller.open('c1');
  await controller.send('hello');
  await settle(controller);

  assert.deepEqual(afters, [0, 2], 'the reconnect resumes after the last seen sequence');
  assert.equal(controller.state.turn.text, 'abcd');
  assert.equal(controller.state.turn.lastSeq, 4);
  assert.equal(controller.state.running, false);
});

test('an idle stream reconnects with backoff instead of hanging', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };

  const afters = [];
  client.streamTurn = (id, options) => {
    afters.push(options.after);
    if (afters.length === 1) {
      options.onEvent(event(1, 'turn.started'));
      return new Promise((resolve, reject) => {
        const abort = () => reject(options.signal.reason ?? Object.assign(new Error('aborted'), { name: 'AbortError' }));
        if (options.signal.aborted) abort();
        else options.signal.addEventListener('abort', abort, { once: true });
      });
    }
    return (async () => {
      await options.onEvent(event(2, 'turn.completed'));
      return { lastSeq: 2, terminal: true };
    })();
  };

  const controller = new ChatController({
    client,
    reconnect: { initialDelayMs: 5 },
    idleTimeoutMs: 25,
  });
  await controller.init();
  await controller.open('c1');
  await controller.send('hello');
  await settle(controller);

  assert.deepEqual(afters, [0, 1], 'idle reconnect resumes after the last event');
  assert.equal(controller.state.turn.state, 'completed');
  assert.equal(controller.state.running, false);
});

test('a heartbeat resets the idle timer', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };

  let streams = 0;
  let beats = 0;
  client.streamTurn = (id, options) => {
    streams += 1;
    if (streams > 1) throw new Error('an idle reconnect happened despite heartbeats');
    return new Promise((resolve, reject) => {
      const beat = setInterval(() => {
        beats += 1;
        options.onHeartbeat('ping');
      }, 10);
      const finish = setTimeout(() => {
        clearInterval(beat);
        options.onEvent(event(1, 'turn.completed'));
        resolve({ lastSeq: 1, terminal: true });
      }, 100);
      options.signal.addEventListener('abort', () => {
        clearInterval(beat);
        clearTimeout(finish);
        reject(options.signal.reason ?? Object.assign(new Error('aborted'), { name: 'AbortError' }));
      }, { once: true });
    });
  };

  const controller = new ChatController({
    client,
    reconnect: { initialDelayMs: 5 },
    idleTimeoutMs: 50,
  });
  await controller.init();
  await controller.open('c1');
  await controller.send('hello');
  await settle(controller);

  assert.equal(streams, 1);
  assert.ok(beats >= 3, `expected several heartbeats, got ${beats}`);
  assert.equal(controller.state.turn.state, 'completed');
});

test('resync_required reloads canonical state and does not replay the stream', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), [
    message('m1', 'user', 'question'),
    message('m2', 'assistant', 'done', { turnId: 't1' }),
  ]);
  client.submitResult = { turn: turn('queued'), created: true };
  client.turnResult = turn('completed');

  let streams = 0;
  client.streamTurn = async () => {
    streams += 1;
    throw new ResyncRequiredError();
  };

  const controller = new ChatController({
    client,
    reconnect: { initialDelayMs: 5 },
    idleTimeoutMs: 1000,
  });
  await controller.init();
  await controller.open('c1');
  await controller.send('hello');
  await settle(controller);

  assert.equal(streams, 1, 'a resync is not a reconnect');
  assert.equal(controller.state.turn.state, 'completed');
  assert.equal(controller.state.turn.text, 'done');
  assert.equal(controller.state.messages.length, 2);
  assert.equal(controller.state.running, false);
  assert.equal(client.calls.filter((c) => c.method === 'getTurn').length, 1);
});

test('a resync with a gap polls the turn until it is terminal, then reloads once', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), [
    message('m1', 'user', 'question'),
    message('m2', 'assistant', 'done', { turnId: 't1' }),
  ]);
  client.submitResult = { turn: turn('queued'), created: true };
  client.streamTurn = async () => {
    throw new GapError('t1', 0, 3);
  };

  let polls = 0;
  client.getTurn = async () => {
    polls += 1;
    return polls === 1 ? turn('running') : turn('completed');
  };

  const controller = new ChatController({ client, reconnect: { initialDelayMs: 5 } });
  await controller.init();
  await controller.open('c1');
  await controller.send('hello');
  await settle(controller);

  assert.equal(polls, 2, 'polled until terminal');
  assert.equal(controller.state.turn.state, 'completed');
  assert.equal(controller.state.turn.text, 'done');
  assert.equal(controller.state.messages.length, 2);
  assert.equal(controller.state.running, false);
});

test('stop cancels on the server before aborting the local stream', async () => {
  const client = makeClient();
  const log = [];
  client.detailResult = detail(conversation('c1'), [], turn('running'));
  client.streamTurn = (id, options) => {
    log.push('stream');
    return new Promise((resolve, reject) => {
      const abort = () => {
        log.push('abort');
        reject(options.signal.reason ?? Object.assign(new Error('aborted'), { name: 'AbortError' }));
      };
      if (options.signal.aborted) abort();
      else options.signal.addEventListener('abort', abort, { once: true });
    });
  };
  client.cancelTurn = async () => {
    await tick(15);
    log.push('cancel');
    return turn('cancelled');
  };

  const controller = new ChatController({ client, idleTimeoutMs: 1000 });
  await controller.init();
  await controller.open('c1');
  assert.equal(controller.state.running, true);
  await controller.stop();

  assert.deepEqual(log, ['stream', 'cancel', 'abort'], 'cancel precedes the local abort');
  assert.equal(controller.state.running, false);
  assert.equal(controller.state.turn.state, 'cancelled');
  await settle(controller);
});

test('an account switch clears state synchronously and ignores the old account response', async () => {
  const client = makeClient();
  client.pageResult = { items: [conversation('new-conv')], nextCursor: '' };
  const oldDetail = deferredDetail();
  client.getConversation = async () => oldDetail.promise;

  const controller = new ChatController({
    client,
    storageKey: accountA,
    reconnect: { initialDelayMs: 5 },
  });
  assert.equal(controller.accountKey, accountKeyOf(accountA));

  const opening = controller.open('old-conv');
  await tick(2);
  assert.equal(controller.state.activeConversationId, 'old-conv');

  const switching = controller.setAccount(accountB);
  assert.equal(controller.state.activeConversationId, null, 'state clears before init');
  assert.deepEqual(controller.state.conversations, []);
  assert.deepEqual(controller.state.messages, []);
  assert.equal(controller.state.turn, null);
  assert.equal(controller.state.error, null);

  await switching;
  oldDetail.resolve(detail(conversation('old-conv'), [
    message('old', 'assistant', 'OLD ACCOUNT'),
  ]));
  await opening;
  await settle(controller);

  assert.equal(controller.state.activeConversationId, null);
  assert.deepEqual(controller.state.messages, []);
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['new-conv']);
  assert.equal(controller.accountKey, accountKeyOf(accountB));
});

test('account keys cannot collide across field boundaries', () => {
  const one = { appUrl: 'a|b', mount: 'c', authCollectionId: 'd', userId: 'e' };
  const two = { appUrl: 'a', mount: 'b|c', authCollectionId: 'd', userId: 'e' };
  assert.notEqual(accountKeyOf(one), accountKeyOf(two));
  assert.equal(accountKeyOf(null), null);
  assert.throws(() => accountKeyOf({ appUrl: 'a', mount: '/', authCollectionId: 'users' }), TypeError);
});

test('a stale open response never overwrites a newer open', async () => {
  const client = makeClient();
  const a = deferredDetail();
  const b = deferredDetail();
  client.getConversation = async (id) => (id === 'A' ? a.promise : b.promise);

  const controller = new ChatController({ client });
  const openingA = controller.open('A');
  const openingB = controller.open('B');

  b.resolve(detail(conversation('B'), [message('b', 'assistant', 'B')]));
  await openingB;
  assert.equal(controller.state.activeConversationId, 'B');

  a.resolve(detail(conversation('A'), [message('a', 'assistant', 'A')]));
  await openingA;
  assert.equal(controller.state.activeConversationId, 'B');
  assert.deepEqual(controller.state.messages.map((m) => m.id), ['b']);
});

test('open attaches to an active turn and streams it from 0', async () => {
  const client = makeClient();
  client.detailResult = detail(
    conversation('c1'),
    [message('m1', 'user', 'question')],
    turn('running'),
  );

  let afterSeen = null;
  client.streamTurn = async (id, options) => {
    afterSeen = options.after;
    await options.onEvent(event(1, 'turn.started'));
    await options.onEvent(event(2, 'text.delta', { text: 'live' }));
    await options.onEvent(event(3, 'turn.completed'));
    return { lastSeq: 3, terminal: true };
  };

  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');
  assert.equal(controller.state.running, true);
  assert.equal(afterSeen, 0, 'an attached turn replays from the beginning');

  await settle(controller);
  assert.equal(controller.state.turn.state, 'completed');
  assert.equal(controller.state.turn.text, 'live');
  assert.equal(controller.state.running, false);
});

test('dispose aborts streams and requests and stops all updates', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), [], turn('running'));
  let aborted = false;
  client.streamTurn = (id, options) => new Promise((resolve, reject) => {
    const abort = () => {
      aborted = true;
      reject(options.signal.reason ?? Object.assign(new Error('aborted'), { name: 'AbortError' }));
    };
    if (options.signal.aborted) abort();
    else options.signal.addEventListener('abort', abort, { once: true });
  });

  const controller = new ChatController({ client, idleTimeoutMs: 1000 });
  await controller.init();
  await controller.open('c1');
  assert.equal(controller.state.running, true);

  const frozen = controller.state;
  controller.dispose();
  await settle(controller);

  assert.equal(aborted, true);
  assert.equal(controller.state, frozen, 'no update is applied after dispose');
  await controller.init();
  assert.equal(controller.state, frozen);
});

test('loadMore, newConversation, rename and remove keep list state coherent', async () => {
  const client = makeClient();
  client.pageResult = { items: [conversation('c1')], nextCursor: 'p2' };
  const controller = new ChatController({ client });
  await controller.init();

  client.pageResult = { items: [conversation('c2'), conversation('c1')], nextCursor: '' };
  await controller.loadMore();
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c1', 'c2']);
  assert.equal(controller.state.nextCursor, '');

  client.createResult = conversation('c3', 'New');
  const created = await controller.newConversation('New');
  assert.equal(created.id, 'c3');
  assert.equal(controller.state.activeConversationId, 'c3');
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c3', 'c1', 'c2']);

  client.renameResult = { ...conversation('c3'), title: 'Renamed' };
  await controller.rename('c3', 'Renamed');
  assert.equal(controller.state.conversations[0].title, 'Renamed');

  const removed = await controller.remove('c3');
  assert.equal(removed, true);
  assert.equal(controller.state.activeConversationId, null);
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c1', 'c2']);
});

test('send without an open conversation is a programming error', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  const controller = new ChatController({ client });
  await controller.init();
  await assert.rejects(controller.send('hello'), /requires an open conversation/);
  await controller.open('c1');
  await assert.rejects(controller.send('   '), /non-empty text/);
});

test('reconnect stops after maxAttempts and reports an error', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };
  let streams = 0;
  client.streamTurn = async () => {
    streams += 1;
    throw new ChatNetworkError('down');
  };

  const controller = new ChatController({
    client,
    reconnect: { initialDelayMs: 5, maxDelayMs: 10, maxAttempts: 2 },
  });
  await controller.init();
  await controller.open('c1');
  await controller.send('hello');
  await settle(controller);

  assert.equal(streams, 3, 'the initial stream plus two reconnect attempts');
  assert.equal(controller.state.running, false);
  assert.ok(controller.state.error);
  assert.equal(controller.state.error.code, 'error');
});

test('a failed submit keeps the error in state', async () => {
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), []);
  client.submitTurn = async () => {
    const err = new Error('The request is invalid.');
    err.code = 'invalid_request';
    err.status = 400;
    throw err;
  };

  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');
  const result = await controller.send('hello');
  await settle(controller);

  assert.equal(result, null);
  assert.equal(controller.state.running, false);
  assert.deepEqual(controller.state.error, {
    code: 'invalid_request',
    message: 'The request is invalid.',
  });
});

function deferredDetail() {
  let resolve;
  const promise = new Promise((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

// --- proposals, editor adapter and attachments ---

const proposal = (overrides = {}) => ({
  id: 'p1',
  conversationId: 'c1',
  turnId: 't1',
  tool: 'create_items',
  location: 'server',
  arguments: { data: { title: 'Buy milk' } },
  argumentsHash: 'sha256:abc',
  summary: 'Create an item',
  state: 'pending',
  revision: 1,
  createdAt: '2026-01-02T03:04:05Z',
  updatedAt: '2026-01-02T03:04:05Z',
  ...overrides,
});

const UPLOAD_LIMITS = {
  maxMessageChars: 16000,
  maxUploadBytes: 1024,
  maxAttachmentsPerTurn: 2,
  attachmentTypes: ['text/plain', 'image/png'],
};

function uploadLimitsClient() {
  const client = makeClient();
  client.statusResult = {
    version: '1',
    models: ['model-a'],
    features: { proposals: true, uploads: true },
    limits: UPLOAD_LIMITS,
  };
  return client;
}

const textFile = (name = 'notes.txt', text = 'hello') => {
  const blob = new Blob([text], { type: 'text/plain' });
  blob.name = name;
  return blob;
};

async function openedWith(client, proposals) {
  client.detailResult = detail(conversation('c1'), [], null, proposals);
  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');
  return controller;
}

test('init exposes features and the attachment limits from /status', async () => {
  const client = uploadLimitsClient();
  const controller = new ChatController({ client });
  await controller.init();

  assert.deepEqual(controller.state.features, { proposals: true, uploads: true });
  assert.deepEqual(controller.state.limits, UPLOAD_LIMITS);
  assert.deepEqual(controller.state.proposals, []);
  assert.deepEqual(controller.state.pendingUploads, []);
  assert.deepEqual(controller.state.transcript, []);
});

test('open loads the conversation proposals and live events merge by newest revision', async () => {
  const client = makeClient();
  const controller = await openedWith(client, [proposal()]);
  assert.deepEqual(controller.state.proposals.map((p) => [p.id, p.revision]), [['p1', 1]]);

  // A live event for a new proposal, then a newer revision of the first one.
  client.streamTurn = async (id, options) => {
    await options.onEvent(event(1, 'turn.started'));
    await options.onEvent(event(2, 'proposal.created', { proposal: proposal({ id: 'p2' }) }));
    await options.onEvent(event(3, 'proposal.updated', {
      proposal: proposal({ id: 'p1', state: 'executing', revision: 2 }),
    }));
    await options.onEvent(event(4, 'turn.completed'));
    return { lastSeq: 4, terminal: true };
  };
  client.submitResult = { turn: turn('queued'), created: true };
  client.detailResult = detail(conversation('c1'), []);
  await controller.send('create one');
  await settle(controller);

  assert.deepEqual(
    controller.state.proposals.map((p) => [p.id, p.state, p.revision]),
    [['p1', 'executing', 2], ['p2', 'pending', 1]],
    'no duplicate for p1, both proposals present',
  );
});

test('a decided proposal stays visible through the terminal transcript reload', async () => {
  const client = makeClient();
  const controller = await openedWith(client, [proposal()]);
  client.submitResult = { turn: turn('queued'), created: true };
  client.decideResult = { proposal: proposal({ state: 'succeeded', revision: 2 }), replayed: false };
  client.streamTurn = async () => ({ lastSeq: 0, terminal: true });

  await controller.decide('p1', 'approve');
  assert.equal(controller.state.proposals[0].state, 'succeeded');

  // The server only returns proposals that can still be decided, so the
  // succeeded one is absent from the reload; the view keeps it.
  client.detailResult = detail(conversation('c1'), [], null, []);
  await controller.send('what happened?');
  await settle(controller);

  assert.deepEqual(
    controller.state.proposals.map((p) => [p.id, p.state]),
    [['p1', 'succeeded']],
  );

  // A fresh open is authoritative again and clears the terminal entry.
  client.detailResult = detail(conversation('c1'), [], null, []);
  await controller.open('c1');
  assert.deepEqual(controller.state.proposals, []);
});

test('decide approves at the reviewed revision and adopts the server result', async () => {
  const client = makeClient();
  const controller = await openedWith(client, [proposal()]);
  client.decideResult = { proposal: proposal({ state: 'succeeded', revision: 2, result: { id: 'r1' } }), replayed: false };

  const result = await controller.decide('p1', 'approve');
  assert.equal(result.replayed, false);
  assert.equal(result.proposal.state, 'succeeded');
  assert.equal(controller.state.proposals[0].state, 'succeeded');
  assert.equal(controller.state.proposals[0].result.id, 'r1');

  const call = client.calls.find((c) => c.method === 'decideProposal');
  assert.equal(call.id, 'p1');
  assert.deepEqual(call.options, { decision: 'approve', revision: 1, arguments: undefined, signal: undefined });
});

test('decide reject reports the rejection and a replayed decision is marked', async () => {
  const client = makeClient();
  const controller = await openedWith(client, [proposal()]);
  client.decideResult = { proposal: proposal({ state: 'rejected', revision: 2 }), replayed: true };

  const result = await controller.decide('p1', 'reject');
  assert.equal(result.replayed, true);
  assert.equal(controller.state.proposals[0].state, 'rejected');
  assert.equal(client.calls.find((c) => c.method === 'decideProposal').options.decision, 'reject');
});

test('a 409 decision adopts the stored proposal and records a non-fatal error', async () => {
  const client = makeClient();
  const controller = await openedWith(client, [proposal()]);
  client.decideProposal = async () => {
    const err = new ChatApiError(409, 'conflict', 'The proposal is out of date.');
    err.proposal = proposal({ state: 'executing', revision: 4 });
    throw err;
  };

  const result = await controller.decide('p1', 'approve');
  assert.equal(result.error.code, 'conflict');
  assert.equal(result.proposal.revision, 4);
  assert.equal(controller.state.proposals.length, 1);
  assert.equal(controller.state.proposals[0].revision, 4);
  assert.equal(controller.state.proposals[0].state, 'executing');
  assert.deepEqual(controller.state.proposals[0].error, {
    code: 'conflict',
    message: 'The proposal is out of date.',
  });
  assert.equal(controller.state.error, null, 'a conflict does not fail the whole view');
});

test('an unknown proposal or an invalid decision is a programming error', async () => {
  const client = makeClient();
  const controller = await openedWith(client, [proposal()]);
  await assert.rejects(controller.decide('missing', 'approve'), /no longer available/);
  await assert.rejects(controller.decide('p1', 'maybe'), TypeError);
  assert.equal(client.calls.filter((c) => c.method === 'decideProposal').length, 0);
});

function editorFixture() {
  const editorProposal = proposal({
    id: 'e1',
    tool: 'edit_note',
    location: 'editor',
    target: 'demo-note',
    arguments: { editorId: 'demo-note', baseRevision: '0', text: 'Hello' },
  });
  const approved = { ...editorProposal, state: 'approved', revision: 2 };
  return { editorProposal, approved };
}

function editorClient(proposalState) {
  const client = makeClient();
  client.decideResult = { proposal: proposalState.approved, replayed: false };
  client.reportOutcome = async (id, options) => {
    client.calls.push({ method: 'reportOutcome', id, options });
    return {
      proposal: {
        ...proposalState.approved,
        state: options.status,
        revision: 3,
        editorRevision: options.editorRevision,
        hunks: options.hunks,
      },
      replayed: false,
    };
  };
  return client;
}

test('approving an editor proposal validates, applies and reports applied', async () => {
  const { editorProposal, approved } = editorFixture();
  const client = editorClient({ approved });
  const calls = [];
  const editor = {
    snapshot: () => ({ editorId: 'demo-note', revision: '5' }),
    validatePatch: (p) => {
      calls.push(['validate', p.id]);
      return { applicable: true };
    },
    applyPatch: (p) => {
      calls.push(['apply', p.id]);
      return { revision: '9', hunks: [{ id: 'h1', status: 'applied' }] };
    },
  };

  client.detailResult = detail(conversation('c1'), [], null, [editorProposal]);
  const controller = new ChatController({ client, editor });
  await controller.init();
  await controller.open('c1');

  const result = await controller.decide('e1', 'approve');
  assert.deepEqual(calls, [['validate', 'e1'], ['apply', 'e1']]);
  assert.equal(result.proposal.state, 'applied');
  assert.equal(result.proposal.editorRevision, '9');
  assert.equal(controller.state.proposals[0].state, 'applied');

  const report = client.calls.find((c) => c.method === 'reportOutcome');
  assert.deepEqual(report.options, {
    revision: 2,
    status: 'applied',
    editorRevision: '9',
    hunks: [{ id: 'h1', status: 'applied' }],
    message: undefined,
    signal: undefined,
  });
  assert.equal(controller.state.running, false);
});

test('a stale patch reports stale, never applied', async () => {
  const { editorProposal, approved } = editorFixture();
  const client = editorClient({ approved });
  let applied = false;
  const editor = {
    snapshot: () => ({ revision: '7' }),
    validatePatch: () => false,
    applyPatch: () => {
      applied = true;
      return { revision: '9' };
    },
  };

  client.detailResult = detail(conversation('c1'), [], null, [editorProposal]);
  const controller = new ChatController({ client, editor });
  await controller.init();
  await controller.open('c1');

  const result = await controller.decide('e1', 'approve');
  assert.equal(applied, false, 'applyPatch is not called for a stale patch');
  assert.equal(result.proposal.state, 'stale');
  const report = client.calls.find((c) => c.method === 'reportOutcome');
  assert.equal(report.options.status, 'stale');
  assert.equal(report.options.editorRevision, '7');
});

test('a throwing applyPatch reports failed with its message', async () => {
  const { editorProposal, approved } = editorFixture();
  const client = editorClient({ approved });
  const editor = {
    snapshot: () => ({ revision: '7' }),
    validatePatch: () => true,
    applyPatch: () => {
      throw new Error('the editor is read-only');
    },
  };

  client.detailResult = detail(conversation('c1'), [], null, [editorProposal]);
  const controller = new ChatController({ client, editor });
  await controller.init();
  await controller.open('c1');

  await controller.decide('e1', 'approve');
  const report = client.calls.find((c) => c.method === 'reportOutcome');
  assert.equal(report.options.status, 'failed');
  assert.equal(report.options.message, 'the editor is read-only');
  assert.equal(controller.state.proposals[0].state, 'failed');
});

test('approving an editor proposal without an adapter is refused client-side', async () => {
  const { editorProposal } = editorFixture();
  const client = makeClient();
  client.detailResult = detail(conversation('c1'), [], null, [editorProposal]);
  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');

  await assert.rejects(
    controller.decide('e1', 'approve'),
    (err) => err instanceof ChatApiError && err.code === 'editor_unavailable',
  );
  assert.equal(client.calls.filter((c) => c.method === 'decideProposal').length, 0);
  assert.deepEqual(controller.state.proposals[0].error, {
    code: 'editor_unavailable',
    message: 'Approving an editor change requires an editor adapter.',
  });
});

test('attach uploads to the open conversation and keeps the uploads pending', async () => {
  const client = uploadLimitsClient();
  client.createResult = conversation('c1');
  const controller = new ChatController({ client });
  await controller.init();

  const uploads = await controller.attach([textFile()]);
  assert.equal(uploads.length, 1);
  assert.equal(uploads[0].id, 'u1');
  assert.equal(client.calls.some((c) => c.method === 'createConversation'), true);
  assert.equal(client.calls.find((c) => c.method === 'uploadFile').conversationId, 'c1');
  assert.deepEqual(controller.state.pendingUploads.map((u) => u.id), ['u1']);
  assert.equal(controller.state.activeConversationId, 'c1');
});

test('attach refuses an unsupported type, an oversized file and too many files', async () => {
  const client = uploadLimitsClient();
  client.detailResult = detail(conversation('c1'), []);
  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');

  await assert.rejects(
    controller.attach([new Blob(['x'], { type: 'application/zip' })]),
    (err) => err instanceof ChatApiError && err.code === 'unsupported_media',
  );
  await assert.rejects(
    controller.attach([textFile('big.txt', 'x'.repeat(2048))]),
    (err) => err instanceof ChatApiError && err.code === 'payload_too_large',
  );
  await assert.rejects(
    controller.attach([textFile('a.txt'), textFile('b.txt'), textFile('c.txt')]),
    (err) => err instanceof ChatApiError && err.code === 'invalid_attachment',
  );
  assert.equal(client.calls.filter((c) => c.method === 'uploadFile').length, 0);
});

test('attach refuses files when /status reports uploads off', async () => {
  const client = makeClient();
  client.statusResult = {
    version: '1',
    models: [],
    features: { proposals: true, uploads: false },
    limits: { maxMessageChars: 100 },
  };
  const controller = new ChatController({ client });
  await controller.init();
  await assert.rejects(controller.attach([textFile()]), /not enabled/);
});

test('send uploads Blob attachments and submits every id once', async () => {
  const client = uploadLimitsClient();
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };
  client.streamTurn = async () => ({ lastSeq: 0, terminal: true });
  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');

  const [upload] = await controller.attach([textFile('one.txt')]);
  await controller.send('see these', { attachments: [upload.id, textFile('two.txt')] });
  await settle(controller);

  const submit = client.calls.find((c) => c.method === 'submitTurn');
  assert.deepEqual(submit.options.attachments, ['u1', 'u2']);
  assert.equal(client.calls.filter((c) => c.method === 'uploadFile').length, 2);
  assert.deepEqual(controller.state.pendingUploads, [], 'sent uploads leave the composer');
});

test('the transcript exposes notice messages and attachment parts', async () => {
  const client = makeClient();
  const messages = [
    message('m1', 'user', 'see attached', {
      parts: [
        { type: 'text', text: 'see attached' },
        { type: 'attachment', uploadId: 'u1', name: 'notes.txt', mime: 'text/plain' },
      ],
    }),
    {
      id: 'm2',
      conversationId: 'c1',
      role: 'notice',
      createdAt: '2026-01-02T03:04:06Z',
      parts: [{
        type: 'proposal_outcome',
        proposalId: 'p1',
        state: 'succeeded',
        tool: 'create_items',
        text: '[Proposal p1 "Create an item": approved and applied.]',
      }],
    },
  ];
  client.detailResult = detail(conversation('c1'), messages);
  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');

  const entries = controller.state.transcript;
  assert.deepEqual(entries.map((e) => e.id), ['m1:0', 'm1:1', 'm2:0']);
  assert.equal(entries[1].type, 'attachment');
  assert.equal(entries[1].uploadId, 'u1');
  assert.equal(entries[1].role, 'user');
  assert.equal(entries[2].type, 'proposal_outcome');
  assert.equal(entries[2].role, 'notice');
  assert.equal(entries[2].proposalId, 'p1');
  assert.equal(entries[2].state, 'succeeded');
});

test('an account switch clears proposals and pending uploads', async () => {
  const client = uploadLimitsClient();
  client.detailResult = detail(conversation('c1'), [], null, [proposal()]);
  const controller = new ChatController({
    client,
    storageKey: accountA,
    editor: { snapshot: () => ({ revision: '1' }), applyPatch: () => ({ revision: '2' }) },
  });
  await controller.init();
  await controller.open('c1');
  await controller.attach([textFile()]);
  assert.equal(controller.state.proposals.length, 1);
  assert.equal(controller.state.pendingUploads.length, 1);

  const switching = controller.setAccount(accountB);
  assert.deepEqual(controller.state.proposals, []);
  assert.deepEqual(controller.state.pendingUploads, []);
  assert.deepEqual(controller.state.transcript, []);
  await switching;
});

test('attachmentTypesFor prefers the choice list over the union', () => {
  const limits = { attachmentTypes: ['text/plain', 'image/png'] };
  assert.deepEqual(attachmentTypesFor(limits), ['text/plain', 'image/png']);
  assert.deepEqual(attachmentTypesFor(limits, { id: 'a::m', attachmentTypes: ['text/plain'] }), ['text/plain']);
  assert.deepEqual(attachmentTypesFor(limits, { id: 'a::m' }), ['text/plain', 'image/png']);
  assert.equal(attachmentTypesFor(null), null);
});

test('attach checks the union, or the named model; send checks the model it sends to', async () => {
  const client = uploadLimitsClient();
  client.statusResult = {
    ...client.statusResult,
    choices: [
      { id: 'a::m1', provider: 'a', model: 'm1', default: true, attachmentTypes: ['text/plain'] },
      { id: 'b::m2', provider: 'b', model: 'm2', attachmentTypes: ['text/plain', 'image/png'] },
    ],
    defaultChoice: 'a::m1',
  };
  client.detailResult = detail(conversation('c1'), []);
  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');

  const png = () => new Blob(['x'], { type: 'image/png' });
  // The union accepts a PNG even though the default model can't read it.
  assert.equal((await controller.attach([png()])).length, 1);
  controller.clearAttachments();
  await assert.rejects(
    controller.attach([png()], { model: 'a::m1' }),
    (err) => err instanceof ChatApiError && err.code === 'unsupported_media',
  );
  await assert.rejects(
    controller.send('look', { model: 'a::m1', attachments: [png()] }),
    (err) => err instanceof ChatApiError && err.code === 'unsupported_media',
  );
  assert.equal((await controller.attach([png()], { model: 'b::m2' })).length, 1);
});

test('send carries page context only when the server accepts it', async () => {
  for (const accepts of [true, false]) {
    const client = makeClient();
    client.statusResult = { ...STATUS, features: { ...STATUS.features, context: accepts } };
    client.detailResult = detail(conversation('c1'), []);
    client.submitResult = { turn: turn('queued'), created: true };
    client.streamTurn = async () => ({ lastSeq: 0, terminal: true });
    let reads = 0;
    const controller = new ChatController({
      client,
      context: async () => {
        reads += 1;
        return { route: '/campaigns/7' };
      },
    });
    await controller.init();
    await controller.open('c1');

    await controller.send('one');
    await settle(controller);
    await controller.send('two', { context: { route: '/override' } });
    await settle(controller);

    const sent = client.calls.filter((c) => c.method === 'submitTurn').map((c) => c.options.context);
    if (accepts) {
      assert.deepEqual(sent, [{ route: '/campaigns/7' }, { route: '/override' }]);
      assert.equal(reads, 1);
    } else {
      assert.deepEqual(sent, [undefined, undefined]);
      assert.equal(reads, 0);
    }
  }
});

test('the context option must be a function', () => {
  assert.throws(() => new ChatController({ client: makeClient(), context: { route: '/' } }), TypeError);
});

test('send passes an effort only when the resolved choice lists it', async () => {
  const client = makeClient();
  client.statusResult = {
    ...STATUS,
    models: ['reasoner', 'plain'],
    choices: [
      { id: 'a::reasoner', provider: 'a', model: 'reasoner', default: true, efforts: ['low', 'high'] },
      { id: 'a::plain', provider: 'a', model: 'plain' },
    ],
    defaultChoice: 'a::reasoner',
  };
  client.detailResult = detail(conversation('c1'), []);
  client.submitResult = { turn: turn('queued'), created: true };
  client.streamTurn = async () => ({ lastSeq: 0, terminal: true });

  const controller = new ChatController({ client });
  await controller.init();
  await controller.open('c1');

  await controller.send('one', { effort: 'high' });
  await settle(controller);
  await controller.send('two', { model: 'a::plain', effort: 'high' });
  await settle(controller);
  await controller.send('three', { model: 'a::reasoner', effort: 'xhigh' });
  await settle(controller);

  const efforts = client.calls.filter((c) => c.method === 'submitTurn').map((c) => c.options.effort);
  assert.deepEqual(efforts, ['high', undefined, undefined]);
});
