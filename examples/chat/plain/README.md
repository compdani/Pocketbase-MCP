# Plain-JS chat example

A page with no build step and no dependencies:

- an email/password sign-in form that calls PocketBase directly;
- an `<agent-page>` driven by a `ChatController` through `bindAgentPanel()`;
- a **note editor** next to the chat that implements the controller's editor
  adapter, so editor proposals can be approved and applied;
- a **side panel** (the **Side panel** button): an `<agent-panel
  conversations-menu floating>` with its own controller, the way an app's
  global assistant sits beside a full chat page. `floating` draws it as a card
  over the page. Its header title is the conversation switcher (a dropdown with
  each conversation's date and Delete; New chat sits beside Close), a `✦` mark
  sits in `header-start`, and the `switcher-end` slot adds **Open in the full
  page**, which opens the panel's conversation in the `<agent-page>`. The
  switcher and the page's sidebar stay in step: see [List sync](#list-sync);
- `serve.mjs`, a tiny dev server that serves the page, the client and the
  local agent-panel on one origin and proxies `/api/` to PocketBase.

Start the server first and see what to try in the
[chat examples README](../README.md).

## Run it

From the repository root:

```sh
node examples/chat/plain/serve.mjs   # http://127.0.0.1:5174
```

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `5174` | Port on `127.0.0.1` |
| `PB_URL` | `http://127.0.0.1:8090` | Where `/api/` is proxied |
| `AGENT_PANEL_DIR` | unset: `/agent-panel/` redirects to agent-panel `v0.4.1` on jsDelivr | A local agent-panel checkout to serve at `/agent-panel/` instead |

## The note editor

The note shows its current revision, starting at `0`; typing in it bumps the
revision. Send `edit rev:<n> New text` with the revision shown and approve
the proposal. The controller then calls the page's adapter:

- `validatePatch` reports the proposal as stale if its `baseRevision` isn't
  the note's current revision;
- `applyPatch` replaces the text, bumps the revision and returns it.

The controller reports the result back to the server. The note is
page-local and is never saved anywhere.

## List sync

The page and the side panel each have their own `ChatController`, and neither
tells the other anything. Each controller re-reads its conversation list after
a turn finishes, when the tab regains focus, and once a minute while visible,
and it announces its own changes (new, rename, delete, send) on a
`BroadcastChannel` named after the account. So a chat started, renamed, titled
or deleted in one view shows up in the other, and in other tabs signed in as
the same user. **New chat** starts a draft; the conversation is created by the
first message, so pressing it twice leaves no empty conversations behind.

## The proxy

`serve.mjs` forwards `Authorization`, `Last-Event-ID`, `Idempotency-Key`,
`Content-Type` and `Accept`, streams responses unbuffered (so events arrive as
they happen), and drops the upstream request when the browser disconnects. It
refuses path traversal, including percent-encoded `..`.

## Tests

```sh
cd examples/chat/plain && node --test
```

Covers path-traversal rejection, MIME types, unbuffered event streaming,
forwarded headers, a proxied POST body and client-abort propagation.
