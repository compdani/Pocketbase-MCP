// Plain-JS demo for the PocketBase MCP chat API (protocol v1).
//
// It logs in with PocketBase's email/password auth, then binds an
// `<agent-page>` to a ChatController. Everything the page shows comes from
// controller state; everything the person does in it comes back as an event
// (see frontend/client/agent-panel.mjs).
//
// The note beside the chat is the v1.2 editor demo: it is the host page's own
// editor, reachable only through the ChatController's `editor` adapter. The
// chat-only `edit_note` tool produces an editor proposal; approving it runs
// validatePatch/applyPatch here, and nothing is ever saved to PocketBase.
//
// The optional side panel is a second `<agent-panel conversations-menu>` with
// its own ChatController for the same account, the way an app's global
// assistant panel sits beside a full chat page. Neither view tells the other
// anything: each controller re-reads its list after a turn and announces its
// own changes over a BroadcastChannel named after the account, so a chat made,
// renamed, titled or deleted in one shows up in the other (and in other tabs).
//
// Served by ./serve.mjs, which also proxies /api/ to PocketBase, so this page
// and the API share one origin and need no CORS.
//
// THE TOKEN IS KEPT IN MEMORY ONLY.
// It lives in this module's `token` variable and is handed to the client
// through getToken(). It is never written to localStorage, sessionStorage, a
// cookie or IndexedDB, so reloading the page or navigating away logs you out.
// That is deliberate here: this is a local development page, and a persisted
// bearer token would outlive the session on a shared machine and stay readable
// by any script on the origin. It also keeps the controller's account state
// tied to a live login. A real app should use PocketBase's auth store and
// decide its own persistence policy.

import '/agent-panel/page.js';
import '/agent-panel/index.js';
import { ChatClient, ChatController } from '/client/index.mjs';
import { bindAgentPanel } from '/client/agent-panel.mjs';

const loginView = document.getElementById('login');
const loginForm = document.getElementById('login-form');
const identityInput = document.getElementById('identity');
const passwordInput = document.getElementById('password');
const loginButton = document.getElementById('login-button');
const loginError = document.getElementById('login-error');
const appView = document.getElementById('app');
const who = document.getElementById('who');
const logoutButton = document.getElementById('logout');
const page = document.getElementById('page');
const side = document.getElementById('side');
const toggleSide = document.getElementById('toggle-side');
const noteText = document.getElementById('note-text');
const noteRevision = document.getElementById('note-revision');

page.renamable = true;

// --- the demo editor -------------------------------------------------------
//
// A local, unsaved draft. Its revision is the optimistic token an editor
// proposal names: a patch computed against an older revision is reported stale
// instead of overwriting what the person has typed since.
const NOTE_EDITOR_ID = 'demo-note';
let note = { revision: 0, text: '' };

function renderNote() {
  noteText.value = note.text;
  noteRevision.textContent = String(note.revision);
}

/** Any typing in the note is a new revision, as it would be in a real editor. */
function onNoteInput() {
  note = { revision: note.revision + 1, text: noteText.value };
  noteRevision.textContent = String(note.revision);
}

/**
 * The ChatController editor adapter (frontend/client/controller.mjs): snapshot
 * for a stale report, validatePatch to check the base revision, applyPatch to
 * change the local draft. Saving is not part of this contract.
 */
const editor = {
  snapshot() {
    return { editorId: NOTE_EDITOR_ID, revision: String(note.revision), content: note.text };
  },
  validatePatch(proposal) {
    const args = proposal && typeof proposal.arguments === 'object' && proposal.arguments !== null
      ? proposal.arguments
      : {};
    if (args.editorId !== undefined && String(args.editorId) !== NOTE_EDITOR_ID) return false;
    if (args.baseRevision !== undefined && String(args.baseRevision) !== String(note.revision)) {
      return false;
    }
    return true;
  },
  applyPatch(proposal) {
    const args = proposal && typeof proposal.arguments === 'object' && proposal.arguments !== null
      ? proposal.arguments
      : {};
    if (typeof args.text !== 'string') {
      throw new Error('The proposal carries no replacement text.');
    }
    note = { revision: note.revision + 1, text: args.text };
    renderNote();
    return { revision: String(note.revision), hunks: [{ id: 'note', status: 'applied' }] };
  },
};

noteText.addEventListener('input', onNoteInput);
renderNote();

/** The bearer token for this tab. Lost on reload, on purpose (see above). */
let token = '';
/** { client, controller, binding, sideController, sideBinding } while signed in. */
let session = null;

function setLoginError(message) {
  loginError.textContent = message;
}

function clearChat() {
  // The elements keep drawing what they were last given, so reset them after
  // the bindings are gone; otherwise the previous account's rows would linger.
  for (const element of [page, side]) {
    element.entries = [];
    element.conversations = [];
    element.active = '';
    element.hasMore = false;
    element.running = false;
    element.error = '';
  }
  page.setAttribute('heading', '');
  setSideOpen(false);
}

function setSideOpen(open) {
  side.hidden = !open;
  toggleSide.setAttribute('aria-pressed', String(open));
}

toggleSide.addEventListener('click', () => setSideOpen(side.hidden));
side.addEventListener('agent-close', () => setSideOpen(false));
// The switcher-end slot: open the panel's conversation in the full page.
document.getElementById('side-open-page').addEventListener('click', async () => {
  side.closeConversations();
  const id = session?.sideController.state.activeConversationId;
  if (session && id) await session.controller.open(id);
  setSideOpen(false);
});

function showLogin() {
  appView.hidden = true;
  loginView.hidden = false;
  who.textContent = '';
  clearChat();
  passwordInput.value = '';
  identityInput.focus();
}

async function signIn(auth) {
  token = auth.token;
  const record = auth.record ?? auth.admin ?? {};
  const authCollectionId = record.collectionId || record.collectionName || 'users';

  const client = new ChatClient({
    baseUrl: location.origin,
    // Read per request: logout and re-login are picked up immediately, and the
    // token never travels in a URL.
    getToken: () => token,
  });
  // The account key scopes state, and names the list-sync channel.
  const storageKey = {
    appUrl: location.origin,
    mount: '/',
    authCollectionId,
    userId: record.id,
  };
  const controller = new ChatController({ client, editor, storageKey });
  // The binding registers the proposal/notice renderers and mirrors the
  // server's attachment limits onto the composer.
  const binding = bindAgentPanel(page, controller, { client });
  // The side panel: same account and client, its own controller and view.
  // No editor adapter here: approving an edit_note proposal is the page's job.
  const sideController = new ChatController({ client, storageKey });
  const sideBinding = bindAgentPanel(side, sideController, { client });
  session = { client, controller, binding, sideController, sideBinding };

  who.textContent = record.email || record.id || 'demo user';
  loginView.hidden = true;
  appView.hidden = false;

  await Promise.all([controller.init(), sideController.init()]);
  // Open the most recent conversation when the account already has one. With
  // none, the first message creates one (bindAgentPanel's autoCreate).
  const [first] = controller.state.conversations;
  if (first) await Promise.all([controller.open(first.id), sideController.open(first.id)]);
}

async function signOut() {
  const current = session;
  session = null;
  token = '';
  if (current) {
    // Stop writing to the element first, then drop the account (which aborts
    // requests, streams and polling and clears controller state), then dispose.
    current.binding.unbind();
    current.sideBinding.unbind();
    await Promise.all([current.controller.setAccount(null), current.sideController.setAccount(null)]);
    current.controller.dispose();
    current.sideController.dispose();
  }
  showLogin();
}

loginForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  setLoginError('');
  loginButton.disabled = true;
  try {
    // PocketBase's own auth endpoint, reached through the dev proxy.
    const response = await fetch('/api/collections/users/auth-with-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        identity: identityInput.value.trim(),
        password: passwordInput.value,
      }),
    });
    const payload = await response.json().catch(() => null);
    if (!response.ok || typeof payload?.token !== 'string') {
      throw new Error(payload?.message || `Login failed (${response.status}).`);
    }
    await signIn(payload);
  } catch (error) {
    setLoginError(error?.message || 'Login failed.');
  } finally {
    loginButton.disabled = false;
  }
});

logoutButton.addEventListener('click', () => {
  void signOut();
});

showLogin();
