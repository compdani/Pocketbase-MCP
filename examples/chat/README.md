# Chat examples

Three small front ends for the chat API. All of them use the
dependency-free client in [frontend/client](../../frontend/client) and the
`<agent-page>` web component from a local `agent-panel` checkout. The
framework apps only own the login form and the mount/unmount lifecycle; the
chat logic is in the client.

| Example | Port | Extra |
|---|---|---|
| [plain/](plain/) | 5174 | No build step. Has a note editor, so it can apply editor proposals. |
| [vue/](vue/) | 5175 | Vue 3 + Vite. Notes on using it in Nuxt (untested). |
| [svelte/](svelte/) | 5176 | Svelte 5 + Vite. |

These are **for local development only**: they bind `127.0.0.1`, serve files
without authentication, and are meant to run against the offline demo
provider.

## 1. Start the server

The stock example has a `demo` provider that needs no API key or network and
answers with canned text. From the repository root:

```sh
cd examples/stock && GOWORK=off go build -o /tmp/pbmcp-stock-demo . && cd ../..

MCP_CHAT_PROVIDER=demo MCP_CHAT_PROPOSALS=true MCP_ALLOW_WRITES=true \
  MCP_CHAT_UPLOADS=true MCP_SEED_DEMO=true MCP_CHAT_TITLE_MODEL=auto \
  MCP_MANIFEST="$PWD/examples/stock/manifest.json" \
  /tmp/pbmcp-stock-demo serve --http 127.0.0.1:8090 --dir /tmp/pbmcp-chat-demo
```

- `MCP_SEED_DEMO=true` creates the user `demo@example.com` / `password123`
  and one contract. The example also creates the `items` and `reports`
  collections the manifest names.
- `MCP_CHAT_PROPOSALS=true` turns on proposals (it needs
  `MCP_ALLOW_WRITES=true`) and registers the editor tool `edit_note`.
- `MCP_CHAT_UPLOADS=true` turns on attachments.
- `MCP_CHAT_TITLE_MODEL=auto` titles new conversations (off by default).
- Leave `MCP_CHAT_MODEL` unset; the demo uses `demo-1`.

The log confirms it: `chat enabled with the OFFLINE DEMO provider … at
/api/mcp/chat/v1`.

## 2. Start an example

See each example's README. Every one proxies `/api` to the server on
`PB_URL` (default `http://127.0.0.1:8090`) and streams events through
unbuffered. They use
[agent-panel](https://github.com/compdani/agent-panel) `v0.5.1` (the binding
needs `v0.4.0` or later; 0.5.0 adds the context ring): Vue and
Svelte install it with `npm install`, and the plain example loads it from
jsDelivr. Set `AGENT_PANEL_DIR` to use a local agent-panel checkout instead.

## 3. Things to try

Sign in as `demo@example.com` / `password123`, then:

| Send | What happens |
|---|---|
| `hello` | A short reply streams in a few pieces. |
| `search contracts` | The model calls `search_contracts`; the panel shows the tool running, then done. |
| `create A demo item` | The model proposes `create_items`. A card says "Waiting for your approval." **Approve** creates the record ("The server applied this change."); **Reject** doesn't. A note line records the outcome. |
| `edit rev:0 New text` | The model proposes `edit_note`. Only the plain example can apply it: approving changes the note and bumps its revision ("Applied in the editor — not saved yet"). Use the revision the note shows; an old one is reported as out of date. Vue and Svelte show the card, but approving reports `editor_unavailable`. |
| a file, via the paperclip, paste or drop | It's uploaded and shown as a chip on your message; clicking the chip downloads it. The demo replies with the file's name. Limits come from `/status`. |

| `long` (send it three or four times) | With the server started with `MCP_CHAT_CONTEXT_WINDOW=4000 MCP_CHAT_COMPACT_MODEL=auto`, each reply is long, so the context fills up: a ring appears next to Send (amber, then red), the oldest messages are summarized after a turn, and a divider marks where the summary ends. Click the ring, then **Compact conversation**, to summarize at once. |

**New chat**, the sidebar, rename and delete all work. **New chat** starts a
draft: the first message creates the conversation. The sidebar ends with
agent-panel's built-in **Load more** while the server has another page.

**The list stays in sync.** Open the plain example's **Side panel** (an
`<agent-panel conversations-menu floating>` with its own controller: its header
title is the conversation switcher, with New chat beside Close), or a second tab:
a chat started, renamed or deleted in one view appears in the other. Each
controller re-reads the list after every turn, on focus and once a minute,
and tells the others for the same account over a `BroadcastChannel`.

**Titles.** Start the server with `MCP_CHAT_TITLE_MODEL=auto` and a new
conversation is titled from its first message (the demo provider uses the
message's first words). The title is stored before the turn's last event, so
both views show it when the reply finishes. In managed provider mode, an
administrator can pick a quicker model with `POST
/api/mcp/chat/v1/providers/actions/title` instead.

## Notes

- **Sign-in stays in memory.** The token is never put in `localStorage`,
  `sessionStorage`, a cookie or IndexedDB, so reloading the page signs you
  out.
- **Nothing is ever shown as saved.** A server proposal says "applied"; an
  editor proposal says "not saved yet", because saving goes through the app's
  own API.
- **Proposals survive a reload.** Approving re-checks the user and runs the
  stored arguments, so approving after a reload is safe.
- **Files never travel as bytes in the transcript.** It stores the upload ID,
  name and type; the chip downloads through the authenticated client, and the
  token never goes in a URL.
- **The model menu** lists what `/status` reports (`demo-1`). The default
  entry sends no model, so the server uses its own.
