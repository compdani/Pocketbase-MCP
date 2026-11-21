import test from 'node:test';
import assert from 'node:assert/strict';

import {
  AfterTerminalError,
  ForeignTurnError,
  GapError,
  InvalidEventError,
  TOOL_STATUSES,
  applyEvent,
  createTurnView,
  isTerminalState,
  viewFromTranscript,
} from '../reducer.mjs';
import { event, message } from './support.mjs';

function applyAll(view, events) {
  let out = view;
  for (const e of events) out = applyEvent(out, e);
  return out;
}

test('the happy path reduces text, usage and terminal state', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.queued'),
    event(2, 'turn.started'),
    event(3, 'text.delta', { text: 'héllo ' }),
    event(4, 'text.delta', { text: '🌍 wörld' }),
    event(5, 'usage.updated', { inputTokens: 10, outputTokens: 7 }),
    event(6, 'turn.completed'),
  ]);

  assert.equal(view.turnId, 't1');
  assert.equal(view.text, 'héllo 🌍 wörld');
  assert.equal(view.state, 'completed');
  assert.equal(isTerminalState(view.state), true);
  assert.deepEqual(view.usage, { inputTokens: 10, outputTokens: 7 });
  assert.equal(view.lastSeq, 6);
});

test('duplicates are ignored and return the same view object', () => {
  const events = [
    event(1, 'turn.started'),
    event(2, 'text.delta', { text: 'ab' }),
    event(3, 'text.delta', { text: 'cd' }),
  ];
  const first = applyAll(createTurnView(), events);
  assert.equal(first.text, 'abcd');

  let replayed = first;
  for (const e of events) {
    const next = applyEvent(replayed, e);
    assert.equal(next, replayed, 'a duplicate must not produce a new view');
    replayed = next;
  }
  assert.equal(replayed.text, 'abcd');
  assert.equal(replayed.lastSeq, 3);

  const done = applyEvent(replayed, event(4, 'turn.completed'));
  assert.equal(applyEvent(done, event(4, 'turn.completed')), done, 'a duplicate terminal is harmless');
});

test('a sequence gap throws GapError and changes nothing', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.started'),
    event(2, 'text.delta', { text: 'a' }),
  ]);

  assert.throws(
    () => applyEvent(view, event(4, 'text.delta', { text: 'b' })),
    (err) => err instanceof GapError && err.have === 2 && err.got === 4 && err.turnId === 't1',
  );
  assert.equal(view.text, 'a');
  assert.equal(view.lastSeq, 2);

  assert.throws(
    () => applyEvent(createTurnView('t2'), event(2, 'turn.started', undefined, { turn: 't2' })),
    (err) => err instanceof GapError && err.have === 0 && err.got === 2,
  );
});

test('an event for another turn throws ForeignTurnError', () => {
  const view = applyEvent(createTurnView(), event(1, 'turn.started'));
  assert.throws(
    () => applyEvent(view, event(2, 'text.delta', { text: 'x' }, { turn: 'other' })),
    (err) => err instanceof ForeignTurnError
      && err.eventTurnId === 'other'
      && err.viewTurnId === 't1',
  );
});

test('events after a terminal state are refused', () => {
  const cases = [
    { name: 'completed', term: event(2, 'turn.completed'), state: 'completed', next: event(3, 'text.delta', { text: 'late' }) },
    { name: 'failed', term: event(2, 'turn.failed', { code: 'provider_error', message: 'no' }), state: 'failed', next: event(3, 'turn.completed') },
    { name: 'cancelled', term: event(2, 'turn.cancelled', { reason: 'user' }), state: 'cancelled', next: event(3, 'text.delta', { text: 'late' }) },
    { name: 'interrupted', term: event(2, 'turn.failed', { code: 'interrupted', message: 'restart' }), state: 'interrupted', next: event(3, 'turn.completed') },
  ];

  for (const tc of cases) {
    const view = applyAll(createTurnView(), [event(1, 'turn.started'), tc.term]);
    assert.equal(view.state, tc.state, tc.name);
    assert.throws(
      () => applyEvent(view, tc.next),
      (err) => err instanceof AfterTerminalError && err.state === tc.state,
      tc.name,
    );
    assert.equal(view.lastSeq, 2, tc.name);
  }
});

test('tools move through running, completed and failed', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.started'),
    event(2, 'tool.started', { callId: 'a', tool: 'search', arguments: { q: 'x' } }),
    event(3, 'tool.started', { callId: 'b', tool: 'fetch' }),
    event(4, 'tool.completed', { callId: 'a', tool: 'search', result: { n: 1 } }),
    event(5, 'tool.failed', { callId: 'b', tool: 'fetch', message: 'boom' }),
  ]);

  assert.equal(view.tools.length, 2);
  assert.deepEqual(view.tools[0], {
    callId: 'a',
    tool: 'search',
    arguments: { q: 'x' },
    status: TOOL_STATUSES.COMPLETED,
    result: { n: 1 },
    message: '',
  });
  assert.deepEqual(view.tools[1], {
    callId: 'b',
    tool: 'fetch',
    status: TOOL_STATUSES.FAILED,
    result: null,
    message: 'boom',
  });

  // A result without a prior start is represented too, so an outcome is never lost.
  const orphan = applyAll(createTurnView('t2'), [
    event(1, 'turn.started', undefined, { turn: 't2' }),
    event(2, 'tool.completed', { callId: 'z', tool: 'search', result: true }, { turn: 't2' }),
  ]);
  assert.equal(orphan.tools.length, 1);
  assert.equal(orphan.tools[0].status, 'completed');
  assert.equal(orphan.tools[0].result, true);
});

test('a duplicate tool start replaces its previous state, like the Go reducer', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.started'),
    event(2, 'tool.started', { callId: 'a', tool: 'search' }),
    event(3, 'tool.completed', { callId: 'a', result: { n: 1 } }),
    event(4, 'tool.started', { callId: 'a', tool: 'search' }),
  ]);
  assert.deepEqual(view.tools, [
    { callId: 'a', tool: 'search', status: 'running', result: null, message: '' },
  ]);
});

test('turn.failed with an interrupted code maps to interrupted', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.started'),
    event(2, 'turn.failed', { code: 'interrupted', message: 'server restart' }),
  ]);
  assert.equal(view.state, 'interrupted');
  assert.deepEqual(view.error, { code: 'interrupted', message: 'server restart' });
  assert.equal(isTerminalState(view.state), true);
});

test('malformed events are rejected', () => {
  const view = applyEvent(createTurnView(), event(1, 'turn.started'));

  assert.throws(
    () => applyEvent(view, event(2, 'text.delta', undefined)),
    (err) => err instanceof InvalidEventError && /without data/.test(err.message),
  );
  assert.throws(
    () => applyEvent(view, { ...event(2, 'text.delta', { text: 'x' }), data: 'not json' }),
    (err) => err instanceof InvalidEventError,
  );
  assert.throws(
    () => applyEvent(view, event(2, 'text.delta', { text: 5 })),
    (err) => err instanceof InvalidEventError,
  );
  assert.throws(
    () => applyEvent(view, event(2, 'future.event', {})),
    (err) => err instanceof InvalidEventError && /unknown event type/.test(err.message),
  );
  assert.throws(
    () => applyEvent(view, { ...event(2, 'turn.completed'), seq: 0 }),
    (err) => err instanceof InvalidEventError,
  );

  // The rejected events never moved the view.
  assert.equal(view.lastSeq, 1);
  assert.equal(view.state, 'running');
});

test('data may arrive as the JSON string from an SSE frame', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.started'),
    { ...event(2, 'text.delta'), data: '{"text":"from a string"}' },
  ]);
  assert.equal(view.text, 'from a string');
});

test('views are immutable snapshots', () => {
  const first = applyEvent(createTurnView(), event(1, 'turn.started'));
  const second = applyEvent(first, event(2, 'text.delta', { text: 'hi' }));
  assert.notEqual(second, first);
  assert.equal(first.text, '');
  assert.equal(first.lastSeq, 1);
  assert.equal(second.text, 'hi');

  const third = applyEvent(second, event(3, 'tool.started', { callId: 'a', tool: 't' }));
  assert.equal(second.tools.length, 0);
  assert.equal(third.tools.length, 1);
});

test('a fresh view matches the zero Go TurnView', () => {
  assert.deepEqual(createTurnView('t1'), {
    turnId: 't1',
    state: '',
    text: '',
    tools: [],
    usage: null,
    error: null,
    proposals: [],
    lastSeq: 0,
  });
});

test('viewFromTranscript rebuilds text and tool outcomes', () => {
  const turn = { id: 't1', conversationId: 'c1', state: 'running' };
  const messages = [
    message('m1', 'user', 'question', { turnId: 't1' }),
    message('m2', 'assistant', 'part one ', { turnId: 't1' }),
    message('m2', 'assistant', 'part two', {
      turnId: 't1',
      parts: [
        { type: 'text', text: 'part two' },
        { type: 'tool_call', callId: 'a', tool: 'search' },
      ],
    }),
    message('m3', 'tool', '', {
      turnId: 't1',
      parts: [{ type: 'tool_result', callId: 'a', result: { n: 1 } }],
    }),
    message('m4', 'assistant', 'not this turn', { turnId: 'other' }),
  ];

  const view = viewFromTranscript(turn, messages);
  assert.equal(view.turnId, 't1');
  assert.equal(view.state, 'running');
  assert.equal(view.text, 'part one part two');
  assert.equal(view.tools.length, 1);
  assert.equal(view.tools[0].status, 'completed');
  assert.deepEqual(view.tools[0].result, { n: 1 });
  assert.equal(view.lastSeq, 0);
});

// --- proposals ---

function proposal(overrides = {}) {
  return {
    id: 'p1',
    conversationId: 'c1',
    turnId: 't1',
    tool: 'create_items',
    location: 'server',
    arguments: { data: { title: 'Buy milk' } },
    argumentsHash: 'sha256:abc',
    state: 'pending',
    revision: 1,
    ...overrides,
  };
}

test('proposal.created and proposal.updated reduce by id, never duplicating', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'turn.started'),
    event(2, 'proposal.created', { proposal: proposal() }),
    event(3, 'proposal.updated', { proposal: proposal({ state: 'executing', revision: 2 }) }),
    event(4, 'proposal.updated', { proposal: proposal({ state: 'succeeded', revision: 3 }) }),
    event(5, 'text.delta', { text: 'done' }),
  ]);

  assert.equal(view.proposals.length, 1);
  assert.equal(view.proposals[0].id, 'p1');
  assert.equal(view.proposals[0].state, 'succeeded');
  assert.equal(view.proposals[0].revision, 3);
  assert.equal(view.text, 'done');
  assert.equal(view.lastSeq, 5);
});

test('an older proposal revision that arrives late cannot undo a newer state', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'proposal.created', { proposal: proposal({ state: 'succeeded', revision: 4 }) }),
    event(2, 'proposal.updated', { proposal: proposal({ state: 'pending', revision: 2 }) }),
    event(3, 'proposal.updated', { proposal: proposal({ state: 'executing', revision: 4 }) }),
  ]);

  assert.equal(view.proposals.length, 1);
  assert.equal(view.proposals[0].state, 'executing', 'equal revision wins, older loses');
  assert.equal(view.proposals[0].revision, 4);
});

test('two proposals keep their first-seen order', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'proposal.created', { proposal: proposal({ id: 'a' }) }),
    event(2, 'proposal.created', { proposal: proposal({ id: 'b' }) }),
    event(3, 'proposal.updated', { proposal: proposal({ id: 'a', revision: 2, state: 'rejected' }) }),
  ]);
  assert.deepEqual(view.proposals.map((p) => p.id), ['a', 'b']);
  assert.equal(view.proposals[0].state, 'rejected');
});

test('an invalid proposal payload is a broken stream', () => {
  const view = applyEvent(createTurnView(), event(1, 'turn.started'));
  const bad = [
    { proposal: null },
    { proposal: { ...proposal(), id: '' } },
    { proposal: { ...proposal(), location: 'browser' } },
    { proposal: { ...proposal(), state: 'done' } },
    { proposal: { ...proposal(), revision: 0 } },
    { proposal: { ...proposal(), arguments: 'not json' } },
    { proposal: { ...proposal(), arguments: [] } },
    { proposal: { ...proposal(), hunks: [{ id: 'h', status: 'maybe' }] } },
  ];

  for (const data of bad) {
    assert.throws(
      () => applyEvent(view, event(2, 'proposal.created', data)),
      (err) => err instanceof InvalidEventError,
      JSON.stringify(data),
    );
  }
  assert.equal(view.lastSeq, 1);
  assert.deepEqual(view.proposals, []);
});

test('an editor proposal moves through approved, applied and stale', () => {
  const view = applyAll(createTurnView(), [
    event(1, 'proposal.created', {
      proposal: proposal({ location: 'editor', tool: 'edit_note', target: 'demo-note' }),
    }),
    event(2, 'proposal.updated', { proposal: proposal({ location: 'editor', state: 'approved', revision: 2 }) }),
    event(3, 'proposal.updated', {
      proposal: proposal({
        location: 'editor',
        state: 'applied',
        revision: 3,
        editorRevision: '7',
        hunks: [{ id: 'h1', status: 'applied' }],
      }),
    }),
  ]);
  assert.equal(view.proposals[0].state, 'applied');
  assert.equal(view.proposals[0].editorRevision, '7');
  assert.deepEqual(view.proposals[0].hunks, [{ id: 'h1', status: 'applied' }]);
});


test('tool arguments survive transcript resync and are copied from event data', () => {
  const argumentsValue = { filter: { active: true } };
  const view = applyEvent(createTurnView(), event(1, 'tool.started', {
    callId: 'a', tool: 'search', arguments: argumentsValue,
  }));
  argumentsValue.filter.active = false;
  assert.deepEqual(view.tools[0].arguments, { filter: { active: true } });
  const restored = viewFromTranscript({ id: 't1', state: 'completed' }, [
    message('m1', 'assistant', '', { turnId: 't1', parts: [
      { type: 'tool_call', callId: 'a', tool: 'search', arguments: { q: 'x' } },
    ] }),
    message('m2', 'tool', '', { turnId: 't1', parts: [
      { type: 'tool_result', callId: 'a', result: false },
    ] }),
  ]);
  assert.deepEqual(restored.tools[0].arguments, { q: 'x' });
  assert.equal(restored.tools[0].result, false);
});
