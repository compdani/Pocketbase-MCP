<script>
  let { onLogin } = $props();

  let identity = $state('demo@example.com');
  let password = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(event) {
    event.preventDefault();
    error = '';
    busy = true;
    try {
      // PocketBase's own auth endpoint, reached through the Vite /api proxy.
      const response = await fetch('/api/collections/users/auth-with-password', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ identity: identity.trim(), password }),
      });
      const payload = await response.json().catch(() => null);
      if (!response.ok || typeof payload?.token !== 'string') {
        throw new Error(payload?.message || `Login failed (${response.status}).`);
      }
      // The parent stores the token in memory and passes it to ChatClient.
      onLogin(payload);
    } catch (err) {
      error = err?.message || 'Login failed.';
    } finally {
      busy = false;
    }
  }
</script>

<section class="login" aria-labelledby="login-title">
  <h1 id="login-title">PocketBase MCP chat · Svelte</h1>
  <p class="muted">
    Local development example. This dev server proxies <code>/api/</code> to
    PocketBase, and the offline <code>demo</code> provider answers the
    messages.
  </p>
  <form onsubmit={submit}>
    <label>
      Email
      <input bind:value={identity} type="email" autocomplete="username" required />
    </label>
    <label>
      Password
      <input bind:value={password} type="password" autocomplete="current-password" required />
    </label>
    <button type="submit" disabled={busy}>{busy ? 'Logging in…' : 'Log in'}</button>
    <p class="error" role="alert">{error}</p>
  </form>
</section>
