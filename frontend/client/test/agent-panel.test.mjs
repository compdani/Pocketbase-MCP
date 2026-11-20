// Node tests for agent-panel.mjs. No DOM: the element is an EventTarget with
// plain properties, and the controller is an EventTarget with a `state`.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  ATTACHMENTS_UNSUPPORTED_TEXT,
  PROPOSAL_ACTION,
  bindAgentPanel,
  conversationsFromState,
  effortOptionFromState,
  entriesFromState,
  errorCode,
  errorText,
  hasConversationList,
  isPageElement,
  modelsFromState,
  proposalEntriesFromState,
  proposalErrorText,
  proposalEntryOf,
  registerProposalEntry,
  CONTEXT_MARKER_TYPE,
  contextUsageFromState,
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
    async compact() {
      calls.push({ method: 'compact' });
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

test('errorText explains the failure codes a failed turn records', () => {
  // chat/runner.go: provider_error, max_steps, internal_error. They used to
  // fall through to the generic line.
  assert.match(errorText({ code: 'provider_error', message: 'The model request failed.' }), /provider refused/i);
  assert.match(errorText({ code: 'max_steps', message: 'The turn exceeded the maximum number of steps.' }), /too many steps/i);
  assert.match(errorText({ code: 'internal_error', message: 'The turn failed.' }), /server/);
  for (const code of ['provider_error', 'max_steps', 'internal_error']) {
    assert.notEqual(errorText({ code }), 'Something went wrong. Try again.');
  }
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

test('agent-stop stops a running turn, and nothing else', async () => {
  const controller = makeController(stateOf());
  const element = new FakeElement();
  bindAgentPanel(element, controller);

  element.fire('agent-stop', null);
  await tick();
  assert.deepEqual(controller.calls, [], 'no turn is running: nothing to cancel');

  controller.set({ running: true, turn: { turnId: 't1', state: 'running' } });
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

test('modelsFromState returns labelled catalog choices', () => {
  assert.deepEqual(
    modelsFromState(
      stateOf({
        models: ['m1'],
        choices: [
          { id: 'openai::m1', model: 'm1', label: 'Model One', group: 'OpenAI' },
          { id: '', model: 'ignored' },
        ],
      }),
    ),
    [{ id: 'openai::m1', label: 'Model One', group: 'OpenAI', disabled: false }],
  );
});

test('modelsFromState keeps only model id strings', () => {
  assert.deepEqual(modelsFromState(stateOf({ models: ['a', '', 7, 'b'] })), ['a', 'b']);
  assert.deepEqual(modelsFromState(null), []);
});

// --- proposals -------------------------------------------------------------

test('a pending proposal maps to an entry with plain words and buttons', () => {
  const source = proposalOf({ revision: 3, summary: 'Create an item' });
  const entry = proposalEntryOf(source);
  const { error: _error, ...data } = source;
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
    warnings: [],
    canDecide: true,
    proposal: data,
  });
  const warned = proposalEntryOf(proposalOf({ warnings: [' The workflow has no trigger ', '', 7] }));
  assert.deepEqual(warned.warnings, ['The workflow has no trigger']);
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
  assert.deepEqual([...element.renderers.keys()], ['proposal', 'notice', 'context-marker']);
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

test('effortOptionFromState offers the selected choice\'s efforts', () => {
  const state = {
    choices: [
      { id: 'a::reasoner', provider: 'a', model: 'reasoner', default: true, efforts: ['low', 'medium', 'high'] },
      { id: 'a::plain', provider: 'a', model: 'plain' },
    ],
    choice: 'a::reasoner',
  };
  const option = effortOptionFromState(state, '');
  assert.equal(option.id, 'effort');
  assert.deepEqual(option.choices.map((c) => c.value), ['', 'low', 'medium', 'high']);
  assert.equal(option.choices[0].label, 'Model default');
  assert.equal(effortOptionFromState(state, 'a::plain'), null);
  assert.equal(effortOptionFromState(state, 'plain'), null);
  assert.ok(effortOptionFromState(state, 'reasoner'));
  assert.equal(effortOptionFromState({ choices: [] }, ''), null);
});

test('the binding shows the effort menu only for models with efforts and sends the pick', async () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c1',
    choices: [
      { id: 'a::reasoner', provider: 'a', model: 'reasoner', default: true, efforts: ['low', 'high'] },
      { id: 'a::plain', provider: 'a', model: 'plain' },
    ],
    choice: 'a::reasoner',
  }));
  const element = new FakeElement();
  element.model = '';
  element.options = [{ id: 'hostOption', label: 'Host toggle' }];
  bindAgentPanel(element, controller);
  assert.deepEqual(element.options.map((o) => o.id), ['hostOption', 'effort'], 'host options are kept');

  element.fire('agent-submit', {
    text: 'Think hard', attachments: [], references: [], fromDraft: true,
    model: '', options: { hostOption: false, effort: 'high' },
  });
  await tick();
  assert.deepEqual(controller.calls.at(-1), { method: 'send', text: 'Think hard', options: { effort: 'high' } });

  // Switching to a model without efforts removes the entry.
  element.fire('agent-model-change', { model: 'a::plain', previous: '' });
  element.model = 'a::plain';
  await tick();
  assert.deepEqual(element.options.map((o) => o.id), ['hostOption']);

  // "Model default" sends no effort at all.
  element.fire('agent-submit', {
    text: 'Plain', attachments: [], references: [], fromDraft: true, model: 'a::plain', options: { hostOption: false },
  });
  await tick();
  assert.deepEqual(controller.calls.at(-1), { method: 'send', text: 'Plain', options: { model: 'a::plain' } });
});

class FakeSwitcherPanel extends FakeElement {
  constructor() {
    super();
    this.tagName = 'AGENT-PANEL';
    this.conversations = [];
    this.active = '';
    this.hasMore = false;
  }

  openConversations() {}

  closeConversations() {}
}

test('agent-panel 0.4 gets the conversation list but keeps its own heading', async () => {
  const controller = makeController(stateOf({
    activeConversationId: 'c2',
    nextCursor: 'p2',
    conversations: [
      { id: 'c1', title: 'One', updatedAt: 'x' },
      { id: 'c2', title: 'Two', updatedAt: 'y' },
    ],
  }));
  const panel = new FakeSwitcherPanel();
  panel.setAttribute('heading', 'Assistant');
  assert.equal(isPageElement(panel), false);
  assert.equal(hasConversationList(panel), true);
  bindAgentPanel(panel, controller);

  assert.deepEqual(panel.conversations.map((c) => c.id), ['c1', 'c2']);
  assert.equal(panel.active, 'c2');
  assert.equal(panel.hasMore, true);
  assert.equal(panel.getAttribute('heading'), 'Assistant', 'the panel heading is the host\'s');

  const selected = panel.fire('agent-select-conversation', { id: 'c1', conversation: { id: 'c1' } });
  assert.equal(selected.defaultPrevented, true);
  panel.fire('agent-delete-conversation', { id: 'c2', conversation: { id: 'c2' } });
  panel.fire('agent-load-more', null);
  panel.fire('agent-rename', { title: 'Ignored' });
  await tick();
  assert.deepEqual(controller.calls.map((c) => c.method), ['open', 'remove', 'loadMore']);

  controller.set({ nextCursor: '' });
  assert.equal(panel.hasMore, false);
});

test('New chat starts a draft when the controller supports it', async () => {
  const controller = makeController(stateOf({ activeConversationId: 'c1' }));
  controller.startDraft = () => {
    controller.calls.push({ method: 'startDraft' });
  };
  const panel = new FakeSwitcherPanel();
  bindAgentPanel(panel, controller);
  panel.fire('agent-new-conversation', null);
  assert.deepEqual(controller.calls, [{ method: 'startDraft' }]);

  const eager = makeController(stateOf({ activeConversationId: 'c1' }));
  eager.startDraft = () => eager.calls.push({ method: 'startDraft' });
  const other = new FakeSwitcherPanel();
  bindAgentPanel(other, eager, { autoCreate: false });
  other.fire('agent-new-conversation', null);
  await tick();
  assert.deepEqual(eager.calls, [{ method: 'newConversation', title: undefined }]);
});

// --- context window ----------------------------------------------------------

/** An element whose prototype defines contextUsage, as agent-panel 0.5 does. */
class FakeContextElement extends FakeElement {
  get contextUsage() { return this._contextUsage ?? null; }

  set contextUsage(value) { this._contextUsage = value; }
}

const contextOf = (overrides = {}) => ({
  tokens: 72000, window: 100000, estimated: true, summarizedThrough: '', summarizedAt: '',
  compacting: false, canCompact: true, ...overrides,
});

test('contextUsageFromState maps the controller context and hides an unknown window', () => {
  assert.equal(contextUsageFromState(stateOf()), null);
  assert.equal(contextUsageFromState(stateOf({ context: contextOf({ window: 0 }) })), null);
  assert.deepEqual(contextUsageFromState(stateOf({ context: contextOf({ summarizedAt: '2026-11-19T10:00:00Z' }) })), {
    used: 72000, window: 100000, estimated: true, compacting: false, canCompact: true, summarizedAt: '2026-11-19T10:00:00Z',
  });
});

test('the binding writes contextUsage and compacts on agent-compact', async () => {
  const controller = makeController(stateOf({ context: contextOf() }));
  const element = new FakeContextElement();
  bindAgentPanel(element, controller);
  assert.equal(element.contextUsage.used, 72000);
  element.fire('agent-compact');
  await tick();
  assert.deepEqual(controller.calls.map((c) => c.method), ['compact']);
  controller.set({ context: null });
  assert.equal(element.contextUsage, null);

  // An element without contextUsage gets no expando and no button by default.
  const old = new FakeElement();
  bindAgentPanel(old, makeController(stateOf({ context: contextOf() })));
  assert.equal(Object.prototype.hasOwnProperty.call(old, 'contextUsage'), false);
});

test('entriesFromState marks where the summary ends', () => {
  const messages = [
    message('m1', 'user', 'Hello'),
    message('m2', 'assistant', 'Hi'),
    message('m3', 'user', 'More'),
  ];
  const entries = entriesFromState(stateOf({ messages, context: contextOf({ summarizedThrough: 'm2' }) }));
  const types = entries.map((e) => e.type);
  const marker = types.indexOf(CONTEXT_MARKER_TYPE);
  assert.ok(marker > 0, 'a marker is drawn');
  assert.equal(entries[marker - 1].id, 'msg:m2');
  assert.equal(entries[marker + 1].id, 'msg:m3');
  assert.equal(entriesFromState(stateOf({ messages })).some((e) => e.type === CONTEXT_MARKER_TYPE), false);
  // A boundary that is not in the loaded page draws nothing.
  assert.equal(entriesFromState(stateOf({ messages, context: contextOf({ summarizedThrough: 'older' }) }))
    .some((e) => e.type === CONTEXT_MARKER_TYPE), false);
});

test('the new failure codes have plain text', () => {
  assert.match(errorText({ code: 'context_too_long' }), /too long for the selected model/);
  assert.match(errorText({ code: 'too_soon' }), /moments ago/);
});

test('provider failure kinds have actionable text', () => {
  assert.match(errorText({ code: 'usage_limit' }), /usage limit was reached/);
  assert.match(errorText({ code: 'provider_auth' }), /rejected its credentials/);
  assert.match(errorText({ code: 'output_limit' }), /cut off/);
  assert.match(errorText({ code: 'rate_limited' }), /Too many requests/);
});

test('a refused approval reads as fixable', () => {
  assert.equal(
    proposalErrorText({ code: 'refused', message: 'The subject line is required.' }),
    'The subject line is required. Nothing was saved; the change is waiting for your decision again.',
  );
});


// --- send outcome, errors, context chips, whole proposals --------------------

const tickAll = () => new Promise((resolve) => setTimeout(resolve, 0));

test('every submit reports how it ended, on the element and to onSend', async () => {
  const controller = makeController(stateOf());
  const element = new FakeElement();
  const results = [];
  const events = [];
  element.addEventListener('chat-send', (event) => events.push(event.detail));
  bindAgentPanel(element, controller, { onSend: (result) => results.push(result) });

  controller.send = async () => ({ turn: { id: 't1', state: 'queued' }, created: true });
  element.fire('agent-submit', { text: 'one', attachments: [], references: [], model: '', options: {} });
  await tickAll();

  controller.send = async () => {
    controller.set({ error: { code: 'turn_refused', message: 'Pick a list first.' } });
    return null;
  };
  element.fire('agent-submit', { text: 'two', attachments: [], references: [], model: '', options: {} });
  await tickAll();

  controller.send = async () => {
    controller.set({ error: { code: 'no_provider', message: "AI isn't set up yet." } });
    return null;
  };
  element.fire('agent-submit', { text: 'three', attachments: [], references: [], model: '', options: {} });
  await tickAll();
  assert.match(element.error, /isn't set up/);

  controller.set({ error: null });
  controller.send = async () => null;
  element.fire('agent-submit', { text: 'four', attachments: [], references: [], model: '', options: {} });
  await tickAll();

  assert.deepEqual(results.map((r) => r.status), ['accepted', 'refused', 'failed', 'cancelled']);
  assert.deepEqual(events.map((r) => r.status), ['accepted', 'refused', 'failed', 'cancelled']);
  assert.equal(results[2].code, 'no_provider');
  assert.match(results[2].message, /isn't set up/);
});

test('a new submit hides the previous turn\'s failure', async () => {
  const controller = makeController(stateOf({
    turn: { turnId: 't-old', state: 'failed', error: { code: 'provider_error', message: 'x' } },
  }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  assert.match(element.error, /provider refused/);
  let errorDuringSend = null;
  controller.send = async () => {
    errorDuringSend = element.error;
    return { turn: { id: 't-new', state: 'queued' }, created: true };
  };
  element.fire('agent-submit', { text: 'again', attachments: [], references: [], model: '', options: {} });
  await tickAll();
  assert.equal(errorDuringSend, '');
  // A failure of the new turn shows again.
  controller.set({ turn: { turnId: 't-new', state: 'failed', error: { code: 'max_steps', message: 'x' } } });
  assert.match(element.error, /too many steps/);
});

test('the binding reads the page context before creating the conversation', async () => {
  const controller = makeController(stateOf({ activeConversationId: null }));
  const order = [];
  controller.readContext = () => {
    order.push('context');
    return Promise.resolve({ route: '/here' });
  };
  const create = controller.newConversation;
  controller.newConversation = async (title) => {
    order.push('create');
    const created = await create.call(controller, title);
    controller.set({ activeConversationId: created.id });
    return created;
  };
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  element.fire('agent-submit', { text: 'hi', attachments: [], references: [], model: '', options: {} });
  await tickAll();
  assert.deepEqual(order, ['context', 'create']);
  const send = controller.calls.find((c) => c.method === 'send');
  assert.deepEqual(await send.options.context, { route: '/here' });
});

test('a host can replace every error line', async () => {
  const controller = makeController(stateOf({ error: { code: 'no_provider', message: 'x' } }));
  const element = new FakeElement();
  const seen = [];
  bindAgentPanel(element, controller, {
    errorText(code, text) {
      seen.push(code);
      return code === 'no_provider' ? 'La IA no está configurada.' : `[es] ${text}`;
    },
  });
  assert.equal(element.error, 'La IA no está configurada.');
  controller.set({ error: null, features: { uploads: false } });
  const file = new File(['a'], 'a.txt', { type: 'text/plain' });
  element.fire('agent-submit', { text: 'hi', attachments: [{ id: 'a1', file }], references: [], model: '', options: {} });
  assert.equal(element.error, `[es] ${ATTACHMENTS_UNSUPPORTED_TEXT}`);
  assert.ok(seen.includes('attachments_unsupported'));
  assert.equal(errorCode({ code: 'weird', message: 'fetch failed' }), 'network');
  assert.equal(errorCode({ code: 'no_provider' }), 'no_provider');
  assert.match(errorText({ code: 'no_provider' }), /isn't set up/);
});

test('stored page context becomes reference chips only through the option', () => {
  const state = stateOf({
    messages: [
      message('m1', 'user', '', { parts: [
        { type: 'text', text: 'what about this one?' },
        { type: 'context', result: { listId: 'l1', listName: 'Groceries' } },
      ] }),
    ],
  });
  const plain = entriesFromState(state);
  assert.equal(plain[0].references, undefined);
  const chips = entriesFromState(state, {
    contextReferences: (context) => [{ type: 'list', id: context.listId, label: context.listName }, { type: 'x' }],
  });
  assert.deepEqual(chips[0].references, [{ type: 'list', id: 'l1', label: 'Groceries' }]);
  const throwing = entriesFromState(state, { contextReferences: () => { throw new Error('no'); } });
  assert.equal(throwing[0].references, undefined);
});

test('a notice carries the decided proposal and its result', () => {
  const decided = proposalOf({ id: 'p9', state: 'succeeded', result: { id: 'r1' } });
  const state = stateOf({
    decidedProposals: [decided],
    messages: [
      message('m1', 'notice', '', { parts: [
        { type: 'proposal_outcome', proposalId: 'p9', state: 'succeeded', tool: 'create_items', text: 'Applied.', result: { id: 'r1' } },
      ] }),
    ],
  });
  const [notice] = entriesFromState(state);
  assert.equal(notice.type, 'notice');
  assert.deepEqual(notice.result, { id: 'r1' });
  assert.equal(notice.proposal.id, 'p9');
  assert.deepEqual(notice.proposal.result, { id: 'r1' });
  assert.equal('error' in notice.proposal, false);
});

test('a superseded send leaves no echo and no chip error', async () => {
  const controller = makeController(stateOf({ features: { proposals: false, uploads: true } }));
  const element = new FakeElement();
  const results = [];
  bindAgentPanel(element, controller, { onSend: (r) => results.push(r.status) });
  controller.send = async () => {
    controller.set({ activeConversationId: null }); // an account switch
    return null;
  };
  const file = new File(['a'], 'a.txt', { type: 'text/plain' });
  element.fire('agent-submit', { text: 'secret note', attachments: [{ id: 'a1', file }], references: [], model: '', options: {} });
  await tickAll();
  await tickAll();
  assert.deepEqual(results, ['cancelled']);
  assert.equal(element.entries.some((e) => e.id === 'pending:user'), false);
  assert.deepEqual(element.attachmentStates.at(-1), { id: 'a1', state: null });
});

test('a dismissed error does not come back when the next submit starts', async () => {
  const controller = makeController(stateOf({ error: { code: 'no_provider', message: 'x' } }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  element.fire('agent-error-dismiss', {});
  element.error = '';
  let shown = null;
  controller.send = async () => {
    shown = element.error;
    return { turn: { id: 't1', state: 'queued' }, created: true };
  };
  element.fire('agent-submit', { text: 'again', attachments: [], references: [], model: '', options: {} });
  await tickAll();
  assert.equal(shown, '');
});

test('the binding skips the context read when the server takes none', async () => {
  const controller = makeController(stateOf({ features: { proposals: false, uploads: false, context: false } }));
  let reads = 0;
  controller.readContext = () => {
    reads += 1;
    return Promise.resolve({});
  };
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  element.fire('agent-submit', { text: 'hi', attachments: [], references: [], model: '', options: {} });
  await tickAll();
  assert.equal(reads, 0);
});

test('errorText reaches proposal cards, and nothing is reported after unbind', async () => {
  const controller = makeController(stateOf({
    proposals: [proposalOf({ error: { code: 'stale', message: 'x' } })],
  }));
  const element = new FakeElement();
  const results = [];
  const binding = bindAgentPanel(element, controller, {
    errorText: (code) => `[${code}]`,
    onSend: (r) => results.push(r),
  });
  const card = element.entries.find((e) => e.type === 'proposal');
  assert.equal(card.error, '[stale]');

  controller.set({ features: { uploads: false } });
  const file = new File(['a'], 'a.txt', { type: 'text/plain' });
  element.fire('agent-submit', { text: 'hi', attachments: [{ id: 'a1', file }], references: [], model: '', options: {} });
  assert.deepEqual(results.map((r) => [r.status, r.code]), [['failed', 'attachments_unsupported']]);

  const gate = deferredOf();
  controller.send = async () => gate.promise;
  controller.set({ features: null });
  element.fire('agent-submit', { text: 'late', attachments: [], references: [], model: '', options: {} });
  binding.unbind();
  gate.resolve({ turn: { id: 't1' }, created: true });
  await tickAll();
  assert.equal(results.length, 1);
});

function deferredOf() {
  let resolve;
  const promise = new Promise((res) => {
    resolve = res;
  });
  return { promise, resolve };
}


test('a second submit is refused while the first is still on its way', async () => {
  const controller = makeController(stateOf());
  const element = new FakeElement();
  const results = [];
  bindAgentPanel(element, controller, { onSend: (r) => results.push([r.status, r.code]) });
  const gate = deferredOf();
  const sent = [];
  controller.send = async (text) => {
    sent.push(text);
    return gate.promise;
  };
  element.fire('agent-submit', { text: 'first', attachments: [], references: [], model: '', options: {} });
  assert.equal(element.running, true, 'the element refuses submits itself meanwhile');
  const second = element.fire('agent-submit', { text: 'second', attachments: [], references: [], model: '', options: {} });
  assert.equal(second.defaultPrevented, true);
  gate.resolve({ turn: { id: 't1', state: 'queued' }, created: true });
  await tickAll();
  assert.deepEqual(sent, ['first']);
  assert.deepEqual(results, [['failed', 'busy'], ['accepted', undefined]]);
  assert.equal(element.running, false);
});

test('a failed send puts the message and its references back', async () => {
  const controller = makeController(stateOf());
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  controller.send = async () => {
    controller.set({ error: { code: 'turn_refused', message: 'Pick a list.' } });
    return null;
  };
  const refs = [{ type: 'list', id: 'l1', label: 'Groceries' }];
  element.references = [];
  element.draft = '';
  element.fire('agent-submit', { text: 'add milk', attachments: [], references: refs, fromDraft: true, model: '', options: {} });
  await tickAll();
  assert.equal(element.draft, 'add milk');
  assert.deepEqual(element.references, refs);

  // Words typed meanwhile are never clobbered, and restoreDraft: false opts out.
  const other = new FakeElement();
  bindAgentPanel(other, controller, { restoreDraft: false });
  other.draft = '';
  other.fire('agent-submit', { text: 'x', attachments: [], references: [], fromDraft: true, model: '', options: {} });
  await tickAll();
  assert.equal(other.draft, '');
});

test('the composer is disabled while no provider is set up', () => {
  const controller = makeController(stateOf({ features: { ready: false, proposals: false, uploads: false } }));
  const element = new FakeElement();
  bindAgentPanel(element, controller);
  assert.equal(element.disabled, true);
  assert.match(element.error, /isn't set up/);
  controller.set({ features: { ready: true, proposals: false, uploads: false } });
  assert.equal(element.disabled, false);
  assert.equal(element.error, '');

  // A composer the host disabled stays disabled.
  const hostDisabled = new FakeElement();
  hostDisabled.disabled = true;
  bindAgentPanel(hostDisabled, makeController(stateOf({ features: { ready: true } })));
  assert.equal(hostDisabled.disabled, true);
});
