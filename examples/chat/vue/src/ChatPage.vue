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
// Only used to show/hide our own "Load more" button; transcript state is the
// binding's business, not Vue's.
const canLoadMore = ref(false);

let client = null;
let controller = null;
let binding = null;

function onControllerChange() {
  canLoadMore.value = Boolean(controller?.state.nextCursor);
}

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
  binding = bindAgentPanel(element, controller, { client });
  controller.addEventListener('change', onControllerChange);

  // Login: setAccount derives the account key, aborts anything the previous
  // account owned, clears state and loads /status plus the first page of
  // conversations. A different account can never see another's rows.
  await controller.setAccount({
    appUrl: props.account.appUrl,
    mount: props.account.mount,
    authCollectionId: props.account.authCollectionId,
    userId: props.account.userId,
  });
  onControllerChange();

  // Open the most recent conversation when the account already has one. With
  // none, the first message creates one (bindAgentPanel's autoCreate).
  const [first] = controller.state.conversations;
  if (first) await controller.open(first.id);
});

function loadMore() {
  // The binding's handle, for a host-drawn load-more control: agent-panel 0.3.x
  // has no built-in one, so this button lives in the page's sidebar-end slot.
  void binding?.loadMore();
}

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
  controller?.removeEventListener('change', onControllerChange);
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
    >
      <button
        v-if="canLoadMore"
        slot="sidebar-end"
        type="button"
        class="load-more"
        @click="loadMore"
      >
        Load more
      </button>
    </agent-page>
  </main>
</template>
