# Svelte 5 chat example

A thin Svelte 5 + Vite app around `<agent-page>`. It shows that the client in
`frontend/client` works unchanged in a framework app: Svelte only owns
sign-in and the mount/destroy lifecycle.

```text
src/App.svelte        the signed-in identity (token in memory only)
src/LoginForm.svelte  email/password sign-in
src/ChatPage.svelte   one ChatController per mount, bound to one <agent-page>
```

Start the server first and see what to try in the
[chat examples README](../README.md).

## Run it

Install inside this folder (the only place npm writes for this example):

```sh
cd examples/chat/svelte
npm install
npm run dev     # http://127.0.0.1:5176 (fixed port; busy port is an error)
npm run build   # production build
```

`PB_URL` and `AGENT_PANEL_DIR` work as in the
[chat examples README](../README.md#2-start-an-example).

## How the packages are found

`agent-panel` is a dependency installed by `npm install` from
[github.com/compdani/agent-panel](https://github.com/compdani/agent-panel),
pinned to `v0.5.1` in `package.json`. The chat client is this repo's own
code, which `vite.config.js` maps to folders on disk:

| Import | Resolves to |
|---|---|
| `@chat-client` | `frontend/client/index.mjs` |
| `@chat-client/agent-panel` | `frontend/client/agent-panel.mjs` |
| `agent-panel/page` | `node_modules/agent-panel/page.js` (defines `<agent-page>`), or `$AGENT_PANEL_DIR/page.js` when set |

`server.fs.allow` lists those folders, because Vite won't serve files outside
the project otherwise.

Svelte-specific details:

- `agent-panel/page` is imported once in `src/main.js`, before Svelte mounts,
  so the element exists before Svelte draws it.
- Svelte needs no custom-element rule: `<agent-page>` is a normal element,
  and `bind:this` gives `ChatPage.svelte` the instance. Svelte only sets the
  static attributes; the binding sets everything else.
- There is no `{@html}`; the element renders model text itself.
- `ChatPage.svelte` creates the controller, calls `setAccount` on sign-in and
  sign-out, binds in `onMount`, and calls `unbind()` and
  `controller.dispose()` in `onDestroy`.
