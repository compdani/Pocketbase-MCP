<script setup>
// The chat screen: one ChatController per mount, bound to one <agent-page>.
//
// All chat logic lives in frontend/client (ChatClient, ChatController,
// bindAgentPanel). This component only wires it to Vue's lifecycle:
//
//   mount   -> create client + controller, bind, setAccount (login), init/open
//   unmount -> unbind, dispose
//
// The page is a custom element, so its data is set as DOM properties by
// bindAgentPanel, never through Vue bindings. Rendering model text is the
// element's job; this example contains no innerHTML or v-html.
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { ChatClient, ChatController } from '@chat-client';
import { bindAgentPanel } from '@chat-client/agent-panel';

const props = defineProps({
  account: { type: Object, required: true },
  // () => string, read before every request (the token lives in App's memory).
  getToken: { type: Function, required: true },
});
const emit = defineEmits(['logout']);

const page = ref(null);
const loggingOut = ref(false);

let client = null;
let controller = null;
let binding = null;

onMounted(async () => {
  const element = page.value;
  // The page owns the title UI; bindAgentPanel syncs the heading attribute.
  element.renamable = true;

  client = new ChatClient({
    baseUrl: location.origin,
    // Called before every request, so logout and re-login are picked up at
    // once and the token never travels in a URL.
    getToken: props.getToken,
  });
  controller = new ChatController({ client });
  // The binding registers the proposal/notice renderers, mirrors the server's
  // attachment limits onto the composer, and downloads attachment chips
  // through the client passed here.
  // It also keeps the sidebar in step: rows, the active one, and "Load more"
  // (agent-panel's has-more) while the server has another page. The
  // controller re-reads the list after each turn, on focus, and when another
  // tab or element for this account changes it.
  binding = bindAgentPanel(element, controller, { client });

  // Login: setAccount derives the account key, aborts anything the previous
  // account owned, clears state and loads /status plus the first page of
  // conversations. A different account can never see another's rows.
  await controller.setAccount({
    appUrl: props.account.appUrl,
    mount: props.account.mount,
    authCollectionId: props.account.authCollectionId,
    userId: props.account.userId,
  });

  // Open the most recent conversation when the account already has one. With
  // none, the first message creates one (bindAgentPanel's autoCreate).
  const [first] = controller.state.conversations;
  if (first) await controller.open(first.id);
});

async function logout() {
  loggingOut.value = true;
  try {
    // Stop writing to the element first, then drop the account: setAccount
    // aborts requests, streams and polling, and clears controller state. The
    // parent then unmounts this component, whose cleanup disposes it.
    binding?.unbind();
    await controller?.setAccount(null);
  } finally {
    loggingOut.value = false;
    emit('logout');
  }
}

onBeforeUnmount(() => {
  // unbind() is idempotent, so calling it twice (logout, then unmount) is fine.
  binding?.unbind();
  controller?.dispose();
  controller = null;
  binding = null;
  client = null;
});
</script>

<template>
  <main class="app">
    <header class="topbar">
      <span class="muted">Signed in as <strong>{{ account.email }}</strong></span>
      <button type="button" :disabled="loggingOut" @click="logout">
        {{ loggingOut ? 'Logging out…' : 'Log out' }}
      </button>
    </header>

    <agent-page
      ref="page"
      placeholder="Ask anything…"
      empty-title="How can I help?"
      empty-text="Send a message. The offline demo provider answers it."
    />
  </main>
</template>
