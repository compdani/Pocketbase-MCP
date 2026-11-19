import test from 'node:test';
import assert from 'node:assert/strict';

import { ChatController, mergeConversationPage } from '../controller.mjs';
import { ChatApiError } from '../client.mjs';
import { detail, makeClient, withTimeout } from './support.mjs';

const account = (userId = 'sync-u1') => ({
  appUrl: 'https://sync.test',
  mount: '/',
  authCollectionId: 'users',
  userId,
});
const row = (id, createdAt, title = id) => ({ id, title, createdAt, updatedAt: createdAt });
const settle = (controller) => withTimeout(controller.whenSettled(), 3000, 'controller settle');
const until = async (check, label) => {
  const end = Date.now() + 3000;
  while (!check()) {
    if (Date.now() > end) throw new Error(`timed out: ${label}`);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
};

test('mergeConversationPage replaces covered rows and keeps older loaded ones', () => {
  const loaded = [
    row('c5', '2026-01-05T00:00:00Z', 'old title'),
    row('c4', '2026-01-04T00:00:00Z'),
    row('c3', '2026-01-03T00:00:00Z'),
    row('c2', '2026-01-02T00:00:00Z'),
    row('c1', '2026-01-01T00:00:00Z'),
  ];
  // c6 is new, c5 renamed, c4 deleted; the page ends at c3, so c2 and c1 (from
  // loadMore) stay and so does their cursor.
  const merged = mergeConversationPage(loaded, {
    items: [row('c6', '2026-01-06T00:00:00Z'), row('c5', '2026-01-05T00:00:00Z', 'new title'), row('c3', '2026-01-03T00:00:00Z')],
    nextCursor: 'p2',
  });
  assert.deepEqual(merged.conversations.map((c) => c.id), ['c6', 'c5', 'c3', 'c2', 'c1']);
  assert.equal(merged.conversations[1].title, 'new title');
  assert.equal(merged.nextCursor, null, 'rows from loadMore keep the current cursor');
  assert.equal(merged.covers('c4'), true);
  assert.equal(merged.covers('c1'), false);

  const whole = mergeConversationPage(loaded, { items: [row('c1', '2026-01-01T00:00:00Z')], nextCursor: '' });
  assert.deepEqual(whole.conversations.map((c) => c.id), ['c1']);
  assert.equal(whole.nextCursor, '');

  const first = mergeConversationPage([row('c2', 'x'), row('c1', 'x')], { items: [row('c2', 'x')], nextCursor: 'p2' });
  assert.deepEqual(first.conversations.map((c) => c.id), ['c2', 'c1'], 'without timestamps the loaded order decides');
  assert.equal(first.nextCursor, null);
});

test('refreshConversations shows rows created, renamed and deleted elsewhere', async () => {
  const client = makeClient();
  client.pageResult = { items: [row('c2', '2026-01-02T00:00:00Z'), row('c1', '2026-01-01T00:00:00Z')], nextCursor: '' };
  const controller = new ChatController({ client, sync: false });
  await controller.init();

  client.pageResult = { items: [row('c3', '2026-01-03T00:00:00Z'), row('c1', '2026-01-01T00:00:00Z', 'Titled')], nextCursor: '' };
  await controller.refreshConversations();
  assert.deepEqual(controller.state.conversations.map((c) => [c.id, c.title]), [['c3', 'c3'], ['c1', 'Titled']]);
  controller.dispose();
});

test('a refresh clears the view only when the open conversation is really gone', async () => {
  const client = makeClient();
  client.pageResult = { items: [row('c1', '2026-01-01T00:00:00Z')], nextCursor: '' };
  client.detailResult = detail(row('c1', '2026-01-01T00:00:00Z'), []);
  const controller = new ChatController({ client, sync: false });
  await controller.init();
  await controller.open('c1');

  // Missing from the page, but the conversation still answers: keep it.
  client.pageResult = { items: [], nextCursor: '' };
  await controller.refreshConversations();
  assert.equal(controller.state.activeConversationId, 'c1');
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c1']);

  // Missing and 404: deleted elsewhere.
  client.getConversation = async () => {
    throw new ChatApiError(404, 'not_found', 'gone');
  };
  await controller.refreshConversations();
  assert.equal(controller.state.activeConversationId, null);
  assert.deepEqual(controller.state.conversations, []);
  assert.deepEqual(controller.state.messages, []);
  controller.dispose();
});

test('a refresh that raced a local write reads the list again', async () => {
  const client = makeClient();
  client.pageResult = { items: [], nextCursor: '' };
  const controller = new ChatController({ client, sync: false });
  await controller.init();

  let release;
  const stale = new Promise((resolve) => { release = resolve; });
  let lists = 0;
  client.listConversations = async () => {
    lists += 1;
    if (lists === 1) {
      await stale;
      return { items: [], nextCursor: '' };
    }
    return { items: [row('c1', '2026-01-01T00:00:00Z')], nextCursor: '' };
  };
  const refresh = controller.refreshConversations();
  client.createResult = row('c1', '2026-01-01T00:00:00Z');
  await controller.newConversation();
  release();
  await refresh;
  assert.equal(lists, 2);
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c1'], 'the new conversation survives');
  assert.equal(controller.state.activeConversationId, 'c1');
  controller.dispose();
});

test('startDraft leaves the conversation without creating one', async () => {
  const client = makeClient();
  client.pageResult = { items: [row('c1', 'x')], nextCursor: '' };
  client.detailResult = detail(row('c1', 'x'), []);
  const controller = new ChatController({ client, sync: false });
  await controller.init();
  await controller.open('c1');
  controller.startDraft();
  assert.equal(controller.state.activeConversationId, null);
  assert.deepEqual(controller.state.conversations.map((c) => c.id), ['c1']);
  assert.equal(client.calls.some((c) => c.method === 'createConversation'), false);
  controller.dispose();
});

test('a finished turn re-reads the list', async () => {
  const client = makeClient();
  client.pageResult = { items: [row('c1', 'x')], nextCursor: '' };
  client.detailResult = detail(row('c1', 'x'), []);
  client.submitResult = { turn: { id: 't1', conversationId: 'c1', state: 'queued', createdAt: 'x', updatedAt: 'x' }, created: true };
  const controller = new ChatController({ client, sync: false });
  await controller.init();
  await controller.open('c1');
  client.pageResult = { items: [row('c1', 'x', 'Generated title')], nextCursor: '' };
  await controller.send('hello');
  await settle(controller);
  assert.equal(controller.state.conversations[0].title, 'Generated title');
  controller.dispose();
});

test('controllers for one account sync over the channel; other accounts do not', async () => {
  const clientA = makeClient();
  const clientB = makeClient();
  const clientOther = makeClient();
  for (const client of [clientA, clientB, clientOther]) client.pageResult = { items: [], nextCursor: '' };
  const sync = { focus: false, intervalMs: 0 };
  const a = new ChatController({ client: clientA, storageKey: account(), sync });
  const b = new ChatController({ client: clientB, storageKey: account(), sync });
  const other = new ChatController({ client: clientOther, storageKey: account('sync-u2'), sync });
  await Promise.all([a.init(), b.init(), other.init()]);

  clientA.createResult = row('c1', '2026-01-01T00:00:00Z');
  clientB.pageResult = { items: [row('c1', '2026-01-01T00:00:00Z')], nextCursor: '' };
  clientOther.pageResult = { items: [row('x1', '2026-01-01T00:00:00Z')], nextCursor: '' };
  await a.newConversation();
  await until(() => b.state.conversations.length === 1, 'the second controller refreshes');
  assert.equal(b.state.conversations[0].id, 'c1');
  await new Promise((resolve) => setTimeout(resolve, 400));
  assert.deepEqual(other.state.conversations, [], 'another account never refreshes on this channel');
  const lists = clientA.calls.filter((c) => c.method === 'listConversations').length;
  assert.equal(lists, 1, 'a controller ignores its own announcement');

  // After an account switch the channel follows the new account.
  await other.setAccount(account());
  clientOther.pageResult = { items: [row('c2', '2026-01-02T00:00:00Z')], nextCursor: '' };
  clientA.renameResult = row('c1', '2026-01-01T00:00:00Z', 'Renamed');
  await a.rename('c1', 'Renamed');
  await until(() => other.state.conversations[0]?.id === 'c2', 'the switched controller hears the new account');

  for (const controller of [a, b, other]) controller.dispose();
});
