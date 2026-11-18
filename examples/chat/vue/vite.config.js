// Vite configuration for the Vue chat example.
//
// The browser only talks to this dev server. /api is proxied to PocketBase,
// and both local packages are compiled straight from disk, so nothing is
// copied into node_modules and no npm publish is needed:
//
//   @chat-client              -> ../../../frontend/client/index.mjs
//   @chat-client/agent-panel  -> ../../../frontend/client/agent-panel.mjs
//   agent-panel/page          -> $AGENT_PANEL_DIR/page.js (defines <agent-page>)
//
// AGENT_PANEL_DIR defaults to /Volumes/mini512/Components/agent-panel and can
// point at any local agent-panel checkout.
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

const here = path.dirname(fileURLToPath(import.meta.url));
const clientDir = path.resolve(here, '../../../frontend/client');
const agentPanelDir = path.resolve(
  process.env.AGENT_PANEL_DIR ?? '/Volumes/mini512/Components/agent-panel',
);

export default defineConfig({
  plugins: [
    vue({
      template: {
        compilerOptions: {
          // <agent-page> is a custom element: compile it as an unknown HTML
          // tag, not as a Vue component, and keep its light-DOM children
          // (including slot="…") as they are.
          isCustomElement: (tag) => tag.startsWith('agent-'),
        },
      },
    }),
  ],
  resolve: {
    alias: [
      // Most specific first: both aliases are plain paths, not node_modules.
      { find: /^@chat-client\/agent-panel$/, replacement: path.join(clientDir, 'agent-panel.mjs') },
      { find: /^@chat-client$/, replacement: path.join(clientDir, 'index.mjs') },
      { find: /^agent-panel\/page$/, replacement: path.join(agentPanelDir, 'page.js') },
    ],
  },
  server: {
    host: '127.0.0.1',
    port: 5175,
    strictPort: true,
    fs: {
      // The client and agent-panel live outside this folder, so Vite's dev
      // server must be allowed to read and transform them.
      allow: [here, clientDir, agentPanelDir],
    },
    proxy: {
      // http-proxy streams the upstream response: no compression, no body
      // transform, so Server-Sent Events arrive as they are produced.
      // Authorization, Last-Event-ID and Idempotency-Key pass through as-is.
      '/api': {
        target: process.env.PB_URL ?? 'http://127.0.0.1:8090',
        changeOrigin: false,
      },
    },
  },
});
