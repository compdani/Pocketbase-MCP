# Vue 3 chat example

A thin Vue 3 + Vite app around `<agent-page>`. It shows that the client in
`frontend/client` works unchanged in a framework app: Vue only owns sign-in
and the mount/unmount lifecycle.

```text
src/App.vue        the signed-in identity (token in memory only)
src/LoginForm.vue  email/password sign-in
src/ChatPage.vue   one ChatController per mount, bound to one <agent-page>
```

Start the server first and see what to try in the
[chat examples README](../README.md).

## Run it

Install inside this folder (the only place npm writes for this example):

```sh
cd examples/chat/vue
npm install
npm run dev     # http://127.0.0.1:5175 (fixed port; busy port is an error)
npm run build   # production build
```

`PB_URL` and `AGENT_PANEL_DIR` work as in the
[chat examples README](../README.md#2-start-an-example).

## How the packages are found

`agent-panel` is a dependency installed by `npm install` from
[github.com/compdani/agent-panel](https://github.com/compdani/agent-panel),
pinned to `v0.5.0` in `package.json`. The chat client is this repo's own
code, which `vite.config.js` maps to folders on disk:

| Import | Resolves to |
|---|---|
| `@chat-client` | `frontend/client/index.mjs` |
| `@chat-client/agent-panel` | `frontend/client/agent-panel.mjs` |
| `agent-panel/page` | `node_modules/agent-panel/page.js` (defines `<agent-page>`), or `$AGENT_PANEL_DIR/page.js` when set |

`server.fs.allow` lists those folders, because Vite won't serve files outside
the project otherwise.

Vue-specific details:

- `agent-panel/page` is imported once in `src/main.js`, before Vue mounts, so
  the element exists before Vue draws it.
- The compiler treats every `agent-*` tag as a custom element:
  `isCustomElement: (tag) => tag.startsWith('agent-')`. Without that, Vue
  would try to resolve `<agent-page>` as a component.
- No Vue bindings carry transcript data and there is no `v-html`; the
  element renders model text itself.
- `ChatPage.vue` creates the controller, calls `setAccount` on sign-in and
  sign-out, binds in `onMounted`, and calls `unbind()` and
  `controller.dispose()` in `onBeforeUnmount`.

## Using it in Nuxt (untested)

The same `ChatPage.vue` works in Nuxt with three changes:

1. **Render it in the browser only:** wrap the chat screen in `<ClientOnly>`
   (or set `ssr: false`). Keep the sign-in form outside it.
2. **Config:** add `"agent-panel": "github:compdani/agent-panel#v0.5.0"` to
   the app's dependencies, then set the custom-element rule, the `/api` proxy
   and the chat-client aliases in `nuxt.config.ts`:

   ```ts
   import { fileURLToPath } from 'node:url';

   const appDir = fileURLToPath(new URL('.', import.meta.url));
   const clientDir = fileURLToPath(new URL('../../../frontend/client', import.meta.url));
   const pbUrl = process.env.PB_URL ?? 'http://127.0.0.1:8090';

   export default defineNuxtConfig({
     vue: { compilerOptions: { isCustomElement: (tag: string) => tag.startsWith('agent-') } },
     nitro: { devProxy: { '/api': { target: pbUrl, changeOrigin: false } } },
     vite: {
       resolve: {
         alias: [
           { find: /^@chat-client\/agent-panel$/, replacement: `${clientDir}/agent-panel.mjs` },
           { find: /^@chat-client$/, replacement: `${clientDir}/index.mjs` },
         ],
       },
       server: { fs: { allow: [appDir, clientDir] } },
     },
   });
   ```

3. **Import `agent-panel/page`** at the top of a client-only component or
   plugin, so it runs before the element is drawn.

This repository doesn't test Nuxt. Check that events stream in your Nuxt
version before relying on its proxy.
