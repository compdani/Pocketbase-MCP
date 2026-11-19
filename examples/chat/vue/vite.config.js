// Vite configuration for the Vue chat example.
//
// The browser only talks to this dev server. /api is proxied to PocketBase,
// and this repo's chat client is compiled straight from disk:
//
//   @chat-client              -> ../../../frontend/client/index.mjs
//   @chat-client/agent-panel  -> ../../../frontend/client/agent-panel.mjs
//
// agent-panel itself (`agent-panel/page` defines <agent-page>) is an npm
// dependency installed from github.com/compdani/agent-panel at a pinned tag.
// To work against a local agent-panel checkout instead, set AGENT_PANEL_DIR.
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

const here = path.dirname(fileURLToPath(import.meta.url));
const clientDir = path.resolve(here, '../../../frontend/client');
const agentPanelDir = process.env.AGENT_PANEL_DIR
  ? path.resolve(process.env.AGENT_PANEL_DIR)
  : null;

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
      ...(agentPanelDir
        ? [{ find: /^agent-panel\/page$/, replacement: path.join(agentPanelDir, 'page.js') }]
        : []),
    ],
  },
  server: {
    host: '127.0.0.1',
    port: 5175,
    strictPort: true,
    fs: {
      // The client (and a local agent-panel checkout, if used) live outside
      // this folder, so Vite's dev server must be allowed to read them.
      allow: [here, clientDir, ...(agentPanelDir ? [agentPanelDir] : [])],
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
