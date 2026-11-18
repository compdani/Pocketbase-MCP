// Node tests for agent-panel.mjs. No DOM: the element is an EventTarget with
// plain properties, and the controller is an EventTarget with a `state`.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  ATTACHMENTS_UNSUPPORTED_TEXT,
  PROPOSAL_ACTION,
  bindAgentPanel,
  conversationsFromState,
  entriesFromState,
  errorText,
  isPageElement,
  modelsFromState,
  proposalEntriesFromState,
  proposalEntryOf,
  registerProposalEntry,
} from '../agent-panel.mjs';
import { message } from './support.mjs';

// --- doubles ---------------------------------------------------------------

class FakeElement extends EventTarget {
  constructor() {
    super();
    this.entries = [];
    this.running = false;
    this.error = '';
    this.models = [];
    this.draft = '';
    this._attributes = new Map();
    this.renderers = new Map();
    this.clearedComposer = [];
    this.removedAttachments = [];
    this.attachmentStates = [];
  }

  fire(type, detail) {
    const event = new CustomEvent(type, { detail, cancelable: true });
    this.dispatchEvent(event);
    return event;
  }

  setAttribute(name, value) {
    this._attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this._attributes.has(name) ? this._attributes.get(name) : null;
  }

  removeAttribute(name) {
    this._attributes.delete(name);
  }

  hasAttribute(name) {
    return this._attributes.has(name);
  }

  defineEntry(type, render) {
    if (typeof render === 'function') this.renderers.set(type, render);
    else this.renderers.delete(type);
    return this;
  }

  emitAction(action, detail) {
    return this.fire('agent-action', { action, ...detail });
  }

  clearComposer(options) {
    this.clearedComposer.push(options);
  }

  removeAttachment(id) {
    this.removedAttachments.push(id);
  }

  setAttachmentState(id, state) {
    this.attachmentStates.push({ id, state });
  }
}

class FakePage extends FakeElement {
  constructor() {
    super();
    this.tagName = 'AGENT-PAGE';
    this.conversations = [];
    this.active = '';
    this.renamable = true;
  }
}

function makeController(state) {
  const target = new EventTarget();
  const calls = [];
  return {
    state,
    calls,
    set(patch) {
      this.state = { ...this.state, ...patch };
      target.dispatchEvent(new Event('change'));
    },
    addEventListener: target.addEventListener.bind(target),
    removeEventListener: target.removeEventListener.bind(target),
    dispatchEvent: target.dispatchEvent.bind(target),
    async send(text, options) {
      calls.push({ method: 'send', text, options });
    },
    async stop() {
      calls.push({ method: 'stop' });
    },
    async open(id) {
      calls.push({ method: 'open', id });
    },
    async newConversation(title) {
      calls.push({ method: 'newConversation', title });
      return { id: 'c-new', title: 'New chat' };
    },
    async rename(id, title) {
      calls.push({ method: 'rename', id, title });
    },
    async remove(id) {
      calls.push({ method: 'remove', id });
    },
    async decide(proposalId, decision, options) {
      calls.push({ method: 'decide', proposalId, decision, options });
    },
    async loadMore() {
      calls.push({ method: 'loadMore' });
    },
  };
}

const stateOf = (overrides = {}) => ({
  status: 'ready',
  conversations: [],
  nextCursor: '',
  activeConversationId: 'c1',
  messages: [],
  proposals: [],
  pendingUploads: [],
  turn: null,
  running: false,
  error: null,
  features: null,
  limits: null,
  models: ['model-a'],
  ...overrides,
});

const proposalOf = (overrides = {}) => ({
  id: 'p1',
  conversationId: 'c1',
  tool: 'create_items',
  location: 'server',
  arguments: {},
  argumentsHash: 'h',
  summary: 'Create item',
  state: 'pending',
  revision: 1,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  error: null,
  ...overrides,
});

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

const ERROR_TEXT_UNSUPPORTED = 'That file type is not supported.';

const transcript = () => [
  message('m1', 'user', 'Hello'),
  message('m2', 'assistant', 'Hi there.', { turnId: 't1' }),
];

// --- entry shapes ----------------------------------------------------------

test('a completed transcript maps to text entries in agent-panel shape', () => {
  const entries = entriesFromState({ messages: transcript(), turn: null, running: false });
  assert.deepEqual(entries, [
    { id: 'msg:m1', type: 'text', role: 'user', content: 'Hello' },
    { id: 'msg:m2', type: 'text', role: 'assistant', content: 'Hi there.' },
  ]);
});

test('entry fields stay inside agent-panel element.d.ts TextEntry/ToolsEntry', () => {
  const messages = [
    message('m1', 'user', 'Find contracts'),
    message('m2', 'assistant', '', {
      turnId: 't1',
      parts: [
        { type: 'text', text: 'Looking…' },
        { type: 'tool_call', callId: 'call-1', tool: 'search_contracts', arguments: {} },
      ],
    }),
    message('m3', 'tool', '', {
      turnId: 't1',
      parts: [{ type: 'tool_result', callId: 'call-1', tool: 'search_contracts', result: { results: [] } }],
    }),
  ];
  const entries = entriesFromState({ messages, turn: null, running: false });

  assert.deepEqual(entries, [
    { id: 'msg:m1', type: 'text', role: 'user', content: 'Find contracts' },
    { id: 'msg:m2', type: 'text', role: 'assistant', content: 'Looking…' },
    {
      id: 'tools:m2',
      type: 'tools',
      tools: [{ id: 'call-1', name: 'search_contracts', status: 'completed' }],
    },
  ]);

  for (const entry of entries) {
    assert.equal(typeof entry.id, 'string');
    assert.equal(typeof entry.type, 'string');
    if (entry.type === 'text') {
      assert.deepEqual(Object.keys(entry).sort(), ['content', 'id', 'role', 'type']);
      assert.equal(typeof entry.role, 'string');
      assert.equal(typeof entry.content, 'string');
    } else if (entry.type === 'tools') {
      assert.ok(Array.isArray(entry.tools));
      for (const tool of entry.tools) {
        assert.deepEqual(Object.keys(tool).sort(), ['id', 'name', 'status']);
        assert.equal(typeof tool.name, 'string');
        assert.ok(['running', 'completed', 'failed'].includes(tool.status));
      }
    }
  }
});

test('a failed tool result marks the tool failed', () => {
  const messages = [
    message('m2', 'assistant', '', {
      turnId: 't1',
      parts: [{ type: 'tool_call', callId: 'call-1', tool: 'search_contracts' }],
    }),
    message('m3', 'tool', '', {
      turnId: 't1',
      parts: [{ type: 'tool_result', callId: 'call-1', tool: 'search_contracts', isError: true }],
    }),
  ];
  const entries = entriesFromState({ messages, turn: null, running: false });
  assert.deepEqual(entries[0].tools, [
    { id: 'call-1', name: 'search_contracts', status: 'failed' },
  ]);
});

test('the live turn text appears incrementally', () => {
  const base = { messages: [], running: true };

  let entries = entriesFromState({
    ...base,
    turn: { turnId: 't1', text: 'Hel', tools: [], state: 'running' },
  });
  assert.deepEqual(entries, [
    { id: 'turn:t1', type: 'text', role: 'assistant', content: 'Hel' },
  ]);

  entries = entriesFromState({
    ...base,
    turn: { turnId: 't1', text: 'Hello', tools: [], state: 'running' },
  });
  assert.deepEqual(entries, [
    { id: 'turn:t1', type: 'text', role: 'assistant', content: 'Hello' },
  ]);
});

test('only the part of the live text the transcript lacks is added', () => {
  const messages = [
    message('m1', 'user', 'Hi', { turnId: 't1' }),
    message('m2', 'assistant', 'Hi', { turnId: 't1' }),
  ];
  const entries = entriesFromState({
    messages,
    running: true,
    turn: { turnId: 't1', text: 'Hi there', tools: [], state: 'running' },
  });
  assert.deepEqual(entries.map((entry) => entry.content ?? entry.type), ['Hi', 'Hi', ' there']);
});

test('a live tool moves from running to completed', () => {
  const base = { messages: [], running: true };

  let entries = entriesFromState({
    ...base,
    turn: {
      turnId: 't1',
      text: '',
      tools: [{ callId: 'call-1', tool: 'search_contracts', status: 'running', result: null }],
      state: 'running',
    },
  });
  assert.deepEqual(entries, [
    {
      id: 'tools:t1',
      type: 'tools',
      tools: [{ id: 'call-1', name: 'search_contracts', status: 'running' }],
    },
  ]);

  entries = entriesFromState({
    ...base,
    running: false,
    turn: {
      turnId: 't1',
      text: '',
      tools: [{ callId: 'call-1', tool: 'search_contracts', status: 'completed', result: {} }],
      state: 'completed',
    },
  });
  assert.deepEqual(entries, [
    {
      id: 'tools:t1',
      type: 'tools',
      tools: [{ id: 'call-1', name: 'search_contracts', status: 'completed' }],
    },
  ]);
});

test('tools come before the answer text when both are live', () => {
  const entries = entriesFromState({
    messages: [],
    running: true,
    turn: {
      turnId: 't1',
      text: 'I found 2 contracts.',
      tools: [{ callId: 'call-1', tool: 'search_contracts', status: 'completed' }],
      state: 'running',
    },
  });
  assert.deepEqual(entries.map((entry) => entry.type), ['tools', 'text']);
});

test('a tool still running when the turn ended shows as failed', () => {
  const entries = entriesFromState({
    messages: [],
    running: false,
    turn: {
      turnId: 't1',
      text: '',
      tools: [{ callId: 'call-1', tool: 'search_contracts', status: 'running' }],
      state: 'cancelled',
    },
  });
  assert.deepEqual(entries[0].tools, [
    { id: 'call-1', name: 'search_contracts', status: 'failed' },
  ]);
});

test('a submitted message shows as a user bubble until the transcript has it', () => {
  const state = stateOf({ messages: [] });
  const entries = entriesFromState(state, { pendingText: 'Hello' });
  assert.deepEqual(entries, [
    { id: 'pending:user', type: 'text', role: 'user', content: 'Hello' },
  ]);

  const afterReload = entriesFromState(
    stateOf({ messages: [message('m1', 'user', 'Hello')] }),
    { pendingText: 'Hello' },
  );
  assert.deepEqual(afterReload, [
    { id: 'msg:m1', type: 'text', role: 'user', content: 'Hello' },
  ]);
});

// --- error mapping ---------------------------------------------------------

test('errorText maps protocol codes to plain language', () => {
  assert.equal(errorText(null), '');
  assert.equal(errorText(undefined), '');
  assert.match(errorText({ code: 'active_turn', message: 'turn-1 is running' }), /already in progress/);
  assert.match(errorText({ code: 'unauthorized' }), /sign in again/i);
  assert.match(errorText({ code: 'forbidden' }), /access/i);
  assert.match(errorText({ code: 'resync_required' }), /reload/i);
  assert.match(errorText({ code: 'rate_limited' }), /too many requests/i);
  assert.match(errorText({ code: 'internal', message: 'panic: secret stack' }), /server/);
});

test('errorText recognises a lost network and never echoes server internals', () => {
  assert.match(errorText({ code: 'error', message: 'The server could not be reached.' }), /reach the server/);
  assert.match(errorText({ code: 'error', message: 'No event or heartbeat for 45000ms.' }), /too long/);

  const raw = 'sql: no such column: secret_token';
  const text = errorText({ code: 'boom', message: raw });
  assert.equal(text, 'Something went wrong. Try again.');
  assert.ok(!text.includes('secret_token'));
});

test('the binding writes the mapped error, and a dismissal is not undone', () => {
  const controller = makeController(stateOf({ error: { code: 'unauthorized', message: 'token expired' } }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  assert.match(element.error, /sign in again/i);

  // The element clears its own banner before firing agent-error-dismiss.
  element.error = '';
  element.fire('agent-error-dismiss', null);
  assert.equal(element.error, '');

  // A repaint of the same error must not bring the banner back.
  controller.set({ running: true });
  assert.equal(element.error, '');
});

// --- submit / stop ---------------------------------------------------------

test('agent-submit sends the text and echoes a user bubble', async () => {
  const controller = makeController(stateOf({ activeConversationId: 'c1' }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  const event = element.fire('agent-submit', {
    text: '  Hello  ',
    attachments: [],
    references: [],
    fromDraft: true,
    model: '',
    options: {},
  });
  await tick();

  assert.equal(event.defaultPrevented, false);
  assert.deepEqual(controller.calls, [{ method: 'send', text: 'Hello', options: undefined }]);
  assert.ok(element.entries.some(
    (entry) => entry.type === 'text' && entry.role === 'user' && entry.content === 'Hello',
  ));
});

test('agent-submit passes the chosen model through', async () => {
  const controller = makeController(stateOf({ activeConversationId: 'c1' }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  element.fire('agent-submit', {
    text: 'Hello',
    attachments: [],
    references: [],
    fromDraft: true,
    model: 'model-a',
    options: {},
  });
  await tick();

  assert.deepEqual(controller.calls, [
    { method: 'send', text: 'Hello', options: { model: 'model-a' } },
  ]);
});

test('a submit with attachments sends the files through the controller', async () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c1',
    features: { proposals: false, uploads: true },
  }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  const file = new File(['hello'], 'a.txt', { type: 'text/plain' });
  const event = element.fire('agent-submit', {
    text: 'Hello',
    attachments: [{ id: 'att-1', name: 'a.txt', file }],
    references: [],
    fromDraft: true,
    model: '',
    options: {},
  });
  await tick();
  await tick();

  assert.equal(event.defaultPrevented, true, 'the chips stay until the send is accepted');
  assert.deepEqual(element.clearedComposer, [{ draft: true, attachments: false }]);
  assert.deepEqual(element.attachmentStates, [{ id: 'att-1', state: { busy: true } }]);
  assert.equal(controller.calls.length, 1);
  assert.equal(controller.calls[0].text, 'Hello');
  assert.deepEqual(controller.calls[0].options.attachments, [file]);
  assert.deepEqual(element.removedAttachments, ['att-1']);
});

test('a failed attachment send keeps the chips and restores the draft', async () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c1',
    features: { proposals: false, uploads: true },
  }));
  controller.send = async () => {
    throw Object.assign(new Error('bad type'), { code: 'unsupported_media' });
  };
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  element.fire('agent-submit', {
    text: 'Hello',
    attachments: [{ id: 'att-1', name: 'a.exe', file: new File(['x'], 'a.exe') }],
    references: [],
    fromDraft: true,
    model: '',
    options: {},
  });
  await tick();
  await tick();

  assert.deepEqual(element.removedAttachments, []);
  assert.equal(element.attachmentStates.at(-1).state.error, ERROR_TEXT_UNSUPPORTED);
  assert.equal(element.draft, 'Hello');
  assert.equal(element.error, ERROR_TEXT_UNSUPPORTED);
});

test('a submit with attachments is refused when the server serves no uploads', async () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c1',
    features: { proposals: true, uploads: false },
  }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  const event = element.fire('agent-submit', {
    text: 'Hello',
    attachments: [{ id: 'att-1', name: 'a.pdf', file: new File(['x'], 'a.pdf') }],
    references: [],
    fromDraft: true,
    model: '',
    options: {},
  });
  await tick();

  assert.equal(event.defaultPrevented, true, 'the draft and files must survive');
  assert.equal(element.error, ATTACHMENTS_UNSUPPORTED_TEXT);
  assert.deepEqual(controller.calls, []);
});

test('agent-submit with no open conversation creates one, then sends', async () => {
  const controller = makeController(stateOf({ activeConversationId: null }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  element.fire('agent-submit', {
    text: 'Hello', attachments: [], references: [], fromDraft: true, model: '', options: {},
  });
  await tick();

  assert.deepEqual(controller.calls.map((call) => call.method), ['newConversation', 'send']);
  assert.equal(controller.calls[1].text, 'Hello');
});

test('agent-submit with autoCreate false reports an error instead of sending', async () => {
  const controller = makeController(stateOf({ activeConversationId: null }));
  const element = new FakeElement();
  bindAgentPanel(element, controller, { autoCreate: false });

  element.fire('agent-submit', {
    text: 'Hello', attachments: [], references: [], fromDraft: true, model: '', options: {},
  });
  await tick();

  assert.deepEqual(controller.calls, []);
  assert.notEqual(element.error, '');
});

test('agent-stop stops the controller', async () => {
  const controller = makeController(stateOf());
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  element.fire('agent-stop', null);
  await tick();
  assert.deepEqual(controller.calls, [{ method: 'stop' }]);
});

// --- going live on a page --------------------------------------------------

test('isPageElement tells a page from a panel', () => {
  assert.equal(isPageElement(new FakeElement()), false);
  assert.equal(isPageElement(new FakePage()), true);
  assert.equal(isPageElement({ tagName: 'AGENT-PANEL' }), false);
});

test('conversationsFromState maps the sidebar list and drops items without an id', () => {
  const conversations = conversationsFromState(stateOf({
    conversations: [
      { id: 'c1', title: 'One', updatedAt: '2026-01-02T03:04:05Z', createdAt: 'earlier' },
      { id: 7, title: 9, updated: 5 },
      { title: 'no id' },
    ],
  }));
  assert.deepEqual(conversations, [
    { id: 'c1', title: 'One', updated: '2026-01-02T03:04:05Z' },
    { id: 7, title: '', updated: 5 },
  ]);
});

test('a page gets the sidebar, the active row and the heading', () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c2',
    conversations: [
      { id: 'c1', title: 'One', updatedAt: '2026-01-02T03:04:05Z' },
      { id: 'c2', title: 'Two', updatedAt: '2026-01-03T03:04:05Z' },
    ],
  }));
  const page = new FakePage();
  bindAgentPanel(page, controller);

  assert.deepEqual(page.conversations, [
    { id: 'c1', title: 'One', updated: '2026-01-02T03:04:05Z' },
    { id: 'c2', title: 'Two', updated: '2026-01-03T03:04:05Z' },
  ]);
  assert.equal(page.active, 'c2');
  assert.equal(page.getAttribute('heading'), 'Two');
  assert.deepEqual(page.models, ['model-a']);
});

test('page sidebar events call the controller', async () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c2',
    conversations: [
      { id: 'c1', title: 'One', updatedAt: 'x' },
      { id: 'c2', title: 'Two', updatedAt: 'x' },
    ],
  }));
  const page = new FakePage();
  const binding = bindAgentPanel(page, controller);

  const selected = page.fire('agent-select-conversation', {
    id: 'c1',
    conversation: { id: 'c1', title: 'One' },
  });
  assert.equal(selected.defaultPrevented, true, 'the binding owns navigation');
  await tick();

  page.fire('agent-new-conversation', null);
  page.fire('agent-delete-conversation', { id: 'c2', conversation: { id: 'c2' } });
  const renamed = page.fire('agent-rename', { title: '  Renamed  ' });
  await tick();

  assert.equal(renamed.defaultPrevented, false);
  assert.deepEqual(controller.calls, [
    { method: 'open', id: 'c1' },
    { method: 'newConversation', title: undefined },
    { method: 'remove', id: 'c2' },
    { method: 'rename', id: 'c2', title: 'Renamed' },
  ]);

  await binding.loadMore();
  assert.deepEqual(controller.calls.at(-1), { method: 'loadMore' });
});

test('a rename with no open conversation is cancelled', () => {
  const controller = makeController(stateOf({ activeConversationId: null }));
  const page = new FakePage();
  bindAgentPanel(page, controller);

  const event = page.fire('agent-rename', { title: 'Renamed' });
  assert.equal(event.defaultPrevented, true);
  assert.deepEqual(controller.calls, []);
});

test('unbind removes every listener and stops writing', async () => {
  const controller = makeController(stateOf({ activeConversationId: 'c1' }));
  const page = new FakePage();
  const binding = bindAgentPanel(page, controller);

  const before = structuredClone(page.entries);
  binding.unbind();

  page.fire('agent-submit', {
    text: 'Hello', attachments: [], references: [], fromDraft: true, model: '', options: {},
  });
  page.fire('agent-stop', null);
  page.fire('agent-new-conversation', null);
  controller.set({ running: true, messages: [message('m9', 'assistant', 'late')] });
  await tick();

  assert.deepEqual(controller.calls, []);
  assert.deepEqual(page.entries, before);
  assert.equal(page.running, false);
});

test('modelsFromState keeps only model id strings', () => {
  assert.deepEqual(modelsFromState(stateOf({ models: ['a', '', 7, 'b'] })), ['a', 'b']);
  assert.deepEqual(modelsFromState(null), []);
});

// --- proposals -------------------------------------------------------------

test('a pending proposal maps to an entry with plain words and buttons', () => {
  const entry = proposalEntryOf(proposalOf({ revision: 3, summary: 'Create an item' }));
  assert.deepEqual(entry, {
    id: 'proposal:p1',
    type: 'proposal',
    proposalId: 'p1',
    tool: 'create_items',
    summary: 'Create an item',
    state: 'pending',
    location: 'server',
    revision: 3,
    error: '',
    status: 'Waiting for your approval.',
    detail: '',
    canDecide: true,
  });
});

test('every proposal state has a distinct, honest line', () => {
  const lines = [
    [proposalOf({ state: 'executing' }), /server/],
    [proposalOf({ state: 'succeeded' }), /server applied/i],
    [proposalOf({ state: 'failed', failure: 'Rule rejected the record.' }), /could not be applied.*Rule rejected/],
    [proposalOf({ state: 'unknown' }), /may or may not have applied this change — check before retrying/],
    [proposalOf({ state: 'approved', location: 'editor' }), /applying in the editor/],
    [proposalOf({ state: 'applied', location: 'editor' }), /not saved yet/],
    [proposalOf({ state: 'stale', location: 'editor' }), /editor changed/],
    [proposalOf({ state: 'rejected' }), /rejected/i],
    [proposalOf({ state: 'expired' }), /expired/i],
  ];
  for (const [proposal, pattern] of lines) {
    const entry = proposalEntryOf(proposal);
    assert.match(entry.status, pattern);
    assert.equal(entry.canDecide, false);
  }
  const applied = proposalEntryOf(proposalOf({ state: 'applied', location: 'editor' }));
  assert.ok(!/^saved/i.test(applied.status));
});

test('proposal entries keep a conflict message and arrive after the transcript', () => {
  const entries = entriesFromState(stateOf({
    messages: transcript(),
    proposals: [
      proposalOf({ id: 'p1', state: 'pending' }),
      proposalOf({ id: 'p2', state: 'stale', error: { code: 'stale', message: 'revision changed' } }),
    ],
  }));
  assert.deepEqual(entries.map((entry) => entry.id), ['msg:m1', 'msg:m2', 'proposal:p1', 'proposal:p2']);
  assert.match(entries[3].error, /out of date/i);
  assert.deepEqual(proposalEntriesFromState(null), []);
});

test('notices become system lines and attachments ride on the user message', () => {
  const messages = [
    message('m1', 'user', 'See the file', {
      turnId: 't1',
      parts: [
        { type: 'text', text: 'See the file' },
        { type: 'attachment', uploadId: 'u1', name: 'notes.txt', mime: 'text/plain' },
      ],
    }),
    message('m2', 'notice', '', {
      parts: [{
        type: 'proposal_outcome',
        proposalId: 'p1',
        state: 'rejected',
        tool: 'create_items',
        text: 'The change was rejected.',
      }],
    }),
  ];
  const entries = entriesFromState({ messages, turn: null, running: false });
  assert.deepEqual(entries, [
    {
      id: 'msg:m1',
      type: 'text',
      role: 'user',
      content: 'See the file',
      references: [{ type: 'attachment', id: 'u1', label: 'notes.txt', attachment: true, mime: 'text/plain' }],
    },
    {
      id: 'notice:m2:0',
      type: 'notice',
      content: 'The change was rejected.',
      proposalId: 'p1',
      state: 'rejected',
      tool: 'create_items',
      isError: false,
    },
  ]);
});

test('a user message with only attachments is a chip-only entry', () => {
  const messages = [
    message('m1', 'user', '', {
      parts: [{ type: 'attachment', uploadId: 'u1', name: 'shot.png', mime: 'image/png' }],
    }),
  ];
  const entries = entriesFromState({ messages, turn: null, running: false });
  assert.deepEqual(entries, [
    {
      id: 'msg:m1:attachments',
      type: 'text',
      role: 'user',
      content: '',
      references: [{ type: 'attachment', id: 'u1', label: 'shot.png', attachment: true, mime: 'image/png' }],
    },
  ]);
});

test('proposal-decision calls decide with the entry id', async () => {
  const controller = makeController(stateOf({ proposals: [proposalOf()] }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  assert.ok(element.renderers.has('proposal'));
  assert.ok(element.renderers.has('notice'));

  const event = element.fire('agent-action', {
    action: PROPOSAL_ACTION,
    id: 'p1',
    decision: 'reject',
  });
  await tick();

  assert.equal(event.defaultPrevented, true);
  assert.deepEqual(controller.calls, [
    { method: 'decide', proposalId: 'p1', decision: 'reject', options: undefined },
  ]);
});

test('an unknown agent-action is ignored', async () => {
  const controller = makeController(stateOf());
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  element.fire('agent-action', { action: 'something-else', id: 'p1' });
  await tick();
  assert.deepEqual(controller.calls, []);
});

test('an attachment chip click downloads through the client', async () => {
  const downloads = [];
  const controller = makeController(stateOf());
  const element = new FakeElement();
  bindAgentPanel(element, controller, {
    client: {
      async downloadUpload(id) {
        downloads.push(id);
        return new Blob(['bytes']);
      },
    },
  });

  element.fire('agent-reference-open', {
    reference: { type: 'attachment', id: 'u1', label: 'notes.txt' },
  });
  await tick();
  assert.deepEqual(downloads, ['u1']);

  // The same path is available to a host-drawn chip through agent-action.
  element.fire('agent-action', { action: 'attachment-download', id: 'u2', name: 'a.bin' });
  await tick();
  assert.deepEqual(downloads, ['u1', 'u2']);
});

test('composer attachment attributes follow the server limits', () => {
  const controller = makeController(stateOf({
    features: { proposals: true, uploads: true },
    limits: {
      maxMessageChars: 2000,
      maxUploadBytes: 1048576,
      maxAttachmentsPerTurn: 3,
      attachmentTypes: ['text/plain', 'image/png'],
    },
  }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  assert.equal(element.getAttribute('no-attachments'), null);
  assert.equal(element.getAttribute('accept'), 'text/plain,image/png');
  assert.equal(element.getAttribute('max-file-size'), '1048576');
  assert.equal(element.getAttribute('max-files'), '3');

  controller.set({ features: { proposals: true, uploads: false } });
  assert.equal(element.getAttribute('no-attachments'), 'true');

  controller.set({
    features: { proposals: true, uploads: true },
    limits: { maxMessageChars: 2000, attachmentTypes: [] },
  });
  assert.equal(element.getAttribute('no-attachments'), 'true', 'no types means no attachments');
});

test('registerProposalEntry registers both custom renderers and tolerates doubles', () => {
  const element = new FakeElement();
  assert.equal(registerProposalEntry(element), true);
  assert.deepEqual([...element.renderers.keys()], ['proposal', 'notice']);
  assert.equal(registerProposalEntry({}), false);

  const plain = new FakeElement();
  bindAgentPanel(plain, makeController(stateOf()), { registerEntries: false });
  assert.equal(plain.renderers.size, 0);
});

test('unbind also removes proposal and attachment listeners', async () => {
  const controller = makeController(stateOf({ proposals: [proposalOf()] }));
  const element = new FakeElement();
  const binding = bindAgentPanel(element, controller, {
    client: { async downloadUpload() { throw new Error('must not be called'); } },
  });
  binding.unbind();

  element.fire('agent-action', { action: PROPOSAL_ACTION, id: 'p1', decision: 'approve' });
  element.fire('agent-reference-open', { reference: { type: 'attachment', id: 'u1' } });
  await tick();

  assert.deepEqual(controller.calls, []);
  assert.equal(element.error, '');
});
