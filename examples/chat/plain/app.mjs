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
/** { controller, client, binding } while signed in. */
let session = null;

function setLoginError(message) {
  loginError.textContent = message;
}

function clearChat() {
  // The element keeps drawing what it was last given, so reset it after the
  // binding is gone; otherwise the previous account's rows would linger.
  page.entries = [];
  page.conversations = [];
  page.active = '';
  page.setAttribute('heading', '');
  page.running = false;
  page.error = '';
}

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
  const controller = new ChatController({
    client,
    editor,
    storageKey: {
      appUrl: location.origin,
      mount: '/',
      authCollectionId,
      userId: record.id,
    },
  });
  // The binding registers the proposal/notice renderers and mirrors the
  // server's attachment limits onto the composer.
  const binding = bindAgentPanel(page, controller, { client });
  session = { client, controller, binding };

  who.textContent = record.email || record.id || 'demo user';
  loginView.hidden = true;
  appView.hidden = false;

  await controller.init();
  // Open the most recent conversation when the account already has one. With
  // none, the first message creates one (bindAgentPanel's autoCreate).
  const [first] = controller.state.conversations;
  if (first) await controller.open(first.id);
}

async function signOut() {
  const current = session;
  session = null;
  token = '';
  if (current) {
    // Stop writing to the element first, then drop the account (which aborts
    // requests, streams and polling and clears controller state), then dispose.
    current.binding.unbind();
    await current.controller.setAccount(null);
    current.controller.dispose();
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
