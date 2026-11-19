# Plain-JS chat example

A page with no build step and no dependencies:

- an email/password sign-in form that calls PocketBase directly;
- an `<agent-page>` driven by a `ChatController` through `bindAgentPanel()`;
- a **note editor** next to the chat that implements the controller's editor
  adapter, so editor proposals can be approved and applied;
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
| `AGENT_PANEL_DIR` | unset: `/agent-panel/` redirects to agent-panel `v0.3.1` on jsDelivr | A local agent-panel checkout to serve at `/agent-panel/` instead |

## The note editor

The note shows its current revision, starting at `0`; typing in it bumps the
revision. Send `edit rev:<n> New text` with the revision shown and approve
the proposal. The controller then calls the page's adapter:

- `validatePatch` reports the proposal as stale if its `baseRevision` isn't
  the note's current revision;
- `applyPatch` replaces the text, bumps the revision and returns it.

The controller reports the result back to the server. The note is
page-local and is never saved anywhere.

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
