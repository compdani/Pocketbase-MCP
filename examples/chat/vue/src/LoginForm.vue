<script setup>
import { ref } from 'vue';

const emit = defineEmits(['login']);

const identity = ref('demo@example.com');
const password = ref('');
const error = ref('');
const busy = ref(false);

async function submit() {
  error.value = '';
  busy.value = true;
  try {
    // PocketBase's own auth endpoint, reached through the Vite /api proxy.
    const response = await fetch('/api/collections/users/auth-with-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        identity: identity.value.trim(),
        password: password.value,
      }),
    });
    const payload = await response.json().catch(() => null);
    if (!response.ok || typeof payload?.token !== 'string') {
      throw new Error(payload?.message || `Login failed (${response.status}).`);
    }
    // The parent stores the token in memory and passes it to ChatClient.
    emit('login', payload);
  } catch (err) {
    error.value = err?.message || 'Login failed.';
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <section class="login" aria-labelledby="login-title">
    <h1 id="login-title">PocketBase MCP chat · Vue</h1>
    <p class="muted">
      Local development example. This dev server proxies <code>/api/</code> to
      PocketBase, and the offline <code>demo</code> provider answers the
      messages.
    </p>
    <form @submit.prevent="submit">
      <label>
        Email
        <input v-model="identity" type="email" autocomplete="username" required />
      </label>
      <label>
        Password
        <input v-model="password" type="password" autocomplete="current-password" required />
      </label>
      <button type="submit" :disabled="busy">{{ busy ? 'Logging in…' : 'Log in' }}</button>
      <p class="error" role="alert">{{ error }}</p>
    </form>
  </section>
</template>
