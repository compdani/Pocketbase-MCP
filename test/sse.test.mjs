import test from 'node:test';
import assert from 'node:assert/strict';

import { SSEError, parseSSE, utf8ByteLength } from '../sse.mjs';
import {
  byteStream,
  chunkStream,
  collect,
  openStream,
  streamSplitAt,
  tick,
} from './support.mjs';

const SAMPLE = [
  ': ping',
  '',
  'id: 1',
  'event: turn.queued',
  'data: {"v":"1","seq":1,"text":"héllo 🌍"}',
  '',
  'id: 2',
  'event: text.delta',
  'data: line one',
  'data: line two',
  '',
  'id: 3',
  'event: turn.completed',
  'data: {"v":"1","seq":3}',
].join('\n');

const EXPECTED = [
  { id: '1', event: 'turn.queued', data: '{"v":"1","seq":1,"text":"héllo 🌍"}' },
  { id: '2', event: 'text.delta', data: 'line one\nline two' },
  { id: '3', event: 'turn.completed', data: '{"v":"1","seq":3}' },
];

test('a stream split at every byte offset parses identically', async () => {
  const length = new TextEncoder().encode(SAMPLE).length;
  for (let offset = 0; offset <= length; offset += 1) {
    const parsed = await collect(parseSSE(streamSplitAt(SAMPLE, offset)));
    assert.deepEqual(parsed, EXPECTED, `split at byte ${offset}`);
  }
});

test('a stream delivered one byte at a time keeps multi-byte characters intact', async () => {
  const parsed = await collect(parseSSE(byteStream(SAMPLE, { chunkSize: 1 })));
  assert.deepEqual(parsed, EXPECTED);
});

test('CRLF and lone CR are line endings', async () => {
  const crlf = SAMPLE.replace(/\n/g, '\r\n');
  const cr = SAMPLE.replace(/\n/g, '\r');
  assert.deepEqual(await collect(parseSSE(byteStream(crlf, { chunkSize: 3 }))), EXPECTED);
  assert.deepEqual(await collect(parseSSE(byteStream(cr, { chunkSize: 2 }))), EXPECTED);
});

test('a CR at a chunk boundary does not split CRLF into an extra blank line', async () => {
  const text = 'data: a\r\ndata: b\r\n\r\ndata: c\r\n\r\n';
  for (let offset = 0; offset <= text.length; offset += 1) {
    const chunks = [text.slice(0, offset), text.slice(offset)];
    const parsed = await collect(parseSSE(chunkStream(chunks)));
    assert.deepEqual(parsed, [
      { id: '', event: 'message', data: 'a\nb' },
      { id: '', event: 'message', data: 'c' },
    ], `split at char ${offset}`);
  }
});

test('comments are reported through onComment and never become frames', async () => {
  const comments = [];
  const parsed = await collect(parseSSE(
    byteStream(': ping\n\n:ping-two\r\ndata: x\n\n: tail'),
    { onComment: (comment) => comments.push(comment) },
  ));
  assert.deepEqual(comments, ['ping', 'ping-two', 'tail']);
  assert.deepEqual(parsed, [{ id: '', event: 'message', data: 'x' }]);
});

test('a final frame without a trailing blank line is dispatched', async () => {
  const parsed = await collect(parseSSE(byteStream('id: 9\nevent: turn.completed\ndata: {"ok":true}')));
  assert.deepEqual(parsed, [{ id: '9', event: 'turn.completed', data: '{"ok":true}' }]);
});

test('a data-less frame at the end of the stream dispatches nothing', async () => {
  const parsed = await collect(parseSSE(byteStream('id: 4\n')));
  assert.deepEqual(parsed, []);
});

test('the event id persists across frames until a new id is set', async () => {
  const parsed = await collect(parseSSE(byteStream('id: 7\ndata: a\n\ndata: b\n\nid: 8\ndata: c\n\n')));
  assert.deepEqual(parsed.map((f) => f.id), ['7', '7', '8']);
});

test('a large chunk of many small frames does not trip the per-event limit', async () => {
  const text = 'data: x\n\n'.repeat(2000);
  const parsed = await collect(parseSSE(byteStream(text), { maxEventBytes: 64 }));
  assert.equal(parsed.length, 2000);
  assert.deepEqual(parsed[1999], { id: '', event: 'message', data: 'x' });
});

test('an event larger than maxEventBytes is rejected', async () => {
  const text = `data: ${'x'.repeat(200)}\n\n`;
  await assert.rejects(
    collect(parseSSE(byteStream(text), { maxEventBytes: 32 })),
    (err) => err instanceof SSEError && err.code === 'event_too_large',
  );
});

test('maxEventBytes counts UTF-8 bytes, not code units', async () => {
  // Four emoji are 16 bytes but only 8 code units.
  const text = 'data: 🌍🌍🌍🌍\n\n';
  await assert.rejects(
    collect(parseSSE(byteStream(text), { maxEventBytes: 10 })),
    (err) => err.code === 'event_too_large',
  );
  const ok = await collect(parseSSE(byteStream('data: 🌍\n\n'), { maxEventBytes: 20 }));
  assert.deepEqual(ok, [{ id: '', event: 'message', data: '🌍' }]);
});

test('utf8ByteLength matches TextEncoder', () => {
  for (const value of ['', 'abc', 'é', '🌍', 'héllo 🌍', '\u0000\u007f\u0080']) {
    assert.equal(utf8ByteLength(value), new TextEncoder().encode(value).length, value);
  }
});

test('an aborted read rejects with the abort reason', async () => {
  const controller = new AbortController();
  const iterator = parseSSE(openStream('data: first\n\n'), { signal: controller.signal });
  const first = await iterator.next();
  assert.deepEqual(first.value, { id: '', event: 'message', data: 'first' });

  const pending = iterator.next();
  await tick(1);
  controller.abort();
  await assert.rejects(pending, (err) => err.name === 'AbortError');
});

test('an already aborted signal rejects before reading', async () => {
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(
    collect(parseSSE(byteStream('data: x\n\n'), { signal: controller.signal })),
    (err) => err.name === 'AbortError',
  );
});

test('breaking out of the loop cancels the stream', async () => {
  let cancelled = false;
  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode('data: 1\n\ndata: 2\n\n'));
    },
    cancel() {
      cancelled = true;
    },
  });
  for await (const frame of parseSSE(stream)) {
    assert.equal(frame.data, '1');
    break;
  }
  await tick(1);
  assert.equal(cancelled, true);
});

test('a non-stream argument is a TypeError', async () => {
  await assert.rejects(collect(parseSSE(null)), TypeError);
});
