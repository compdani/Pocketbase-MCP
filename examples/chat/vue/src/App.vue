<script setup>
// The app shell owns the signed-in identity. Everything chat-specific lives in
// ChatPage (and, below it, frontend/client).
//
// THE TOKEN IS KEPT IN MEMORY ONLY.
// It lives in this module's `token` variable and is read through getToken()
// before every request. It is never written to localStorage, sessionStorage, a
// cookie or IndexedDB, so reloading the page logs you out. That is deliberate
// for this local development example: a persisted bearer token would outlive
// the tab on a shared machine. A real app should use PocketBase's auth store
// and decide its own persistence policy.
import { ref } from 'vue';
import LoginForm from './LoginForm.vue';
import ChatPage from './ChatPage.vue';

/** The bearer token for this tab. Lost on reload, on purpose (see above). */
let token = '';

/** `{ appUrl, mount, authCollectionId, userId, email }` while signed in. */
const account = ref(null);

function signIn(auth) {
  token = auth.token;
  const record = auth.record ?? auth.admin ?? {};
  account.value = {
    appUrl: location.origin,
    mount: '/',
    authCollectionId: record.collectionId || record.collectionName || 'users',
    userId: record.id,
    email: record.email || record.id || 'demo user',
  };
}

// Read per request: logout and re-login are picked up immediately, and the
// token never travels in a URL.
function getToken() {
  return token;
}

function signOut() {
  token = '';
  account.value = null;
}
</script>

<template>
  <LoginForm v-if="!account" @login="signIn" />
  <ChatPage v-else :account="account" :get-token="getToken" @logout="signOut" />
</template>
