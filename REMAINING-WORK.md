# Remaining work

Status as of 2026-11-17, after milestones 1–5. Grouped by priority; suggested order: 1 → 2 (start the ChatGPT staging test early) → 3 → 4.

## 1. Housekeeping (do first)

**Pocketbase-MCP: about 54 uncommitted changes on `master`**

- Includes milestone 4's MCP side, the sign-in URL, all of milestone 5, the audit and chat-channel changes, the flaky-test fix, and the `.gitignore` with the two binaries removed.
- Commit on a branch, push, and open a PR.
- Decide whether `verification/` logs belong in git. Suggestion: keep the `MILESTONE-*-REPORT.md` files, ignore the raw `m4-*.txt` logs.

**pb-postgress: branch `oauth-mcp-clients`**

- Six commits, not pushed. Push it and open a PR to `master`.
- MySQL has never been tested on any of the fork changes, including the already-merged case-insensitive search fix. Run the fork tests with `PB_TEST_DB_DRIVER=mysql` before relying on it.

**Update the plan documents to match what was built**

- `MCP-SERVER-MODULE-PLAN.md` is out of date on:
  - the "sign-in app URL" (`issuerURL`) setting and the main-app redirect,
  - fork-side self-registration and storage (`_oauth2ServerClients`), hashed secrets and RS256 ID tokens,
  - refusing chat-only tools,
  - the `AccessError` audit outcomes (`not_found` / `denied` / `invalid`),
  - read-only facade enforcement for custom tools,
  - pb-postgress mode serving the protected-resource document.
- `MCP-CHAT-INTERFACE-PLAN.md` needs a matching note on channel validation.

## 2. Deployment steps (operator work)

1. **Before the fork's hashing migration runs:** back up the database and save any OAuth client secrets you still need. They can't be recovered afterwards.
2. **Set "Sign-in app URL"** to `https://pocket.dreia.org` in the `users` collection's OAuth2 server settings.
3. **ID tokens:** apps that verify them must switch from HS256 to RS256 using the new `/jwks` endpoint. Include the new signing key in backups.
4. **Self-registration:** keep it off, or turn it on with an allowed-hosts list. Every token is a full user session.
5. **Live ChatGPT test** on a public staging URL, following the steps in `verification/MILESTONE-3-REPORT.md`. Also try one self-registering client, such as Claude Code.

## 3. Milestone 6: hardening before release

- **Not built yet:** per-user rate limits and a concurrency cap for `/mcp`. The plan requires them; the code has neither.
- **Self-registration rate limits** live in each process's memory. Fine for one instance; document it or move it to the database for multi-instance setups.
- **Tests still needed:**
  - schema and instance-scope changes while running,
  - two apps in one process,
  - clean shutdown,
  - concurrency isolation (no tool-list leaks between users under load).
- **Release material:**
  - stock and fork example apps,
  - a support matrix (PocketBase versions, fork commit, Go version, databases),
  - pinned versions in `VERSIONS.md`.
- **customjs migration guide and switch-over:**
  - presets become a manifest,
  - `oauth_apps` records are imported with full callback URLs (`ImportHandClient`),
  - keep the `/mcp`, `/sse` and `/oauth/*` paths,
  - test against ChatGPT on staging first.
- **Optional fork fix:** make shutdown wait for background file deletions. That race was the root cause of the flaky `TestNativeDiscoveryRegistrationPKCE`, and could hit the fork's own tests too.

## 4. Chat (v1.1, then v1.2)

None of this exists in code yet. Plan: `MCP-CHAT-INTERFACE-PLAN.md`.

**v1.1, read-only:**

- versioned wire format and turn state machine,
- server-stored conversations,
- resumable SSE streams,
- cancellation,
- hand-written OpenAI-compatible and Anthropic provider adapters,
- a dependency-free browser client wired to `agent-panel`,
- example apps in plain JS, Vue/Nuxt and Svelte,
- turn on the `chat` and `both` channels.

**v1.2, writes:**

- proposals and approval,
- editor tools (define the real editor `ToolSpec` contract),
- uploads,
- restart recovery.

**Migration:** move one screen each in List Pocket, Event Pocket and Static Dealmakers, as separate work.

## 5. Later

- stdio/REST sidecar,
- MCP resources and prompts,
- file upload tools,
- idempotency keys for writes,
- multi-instance chat workers,
- possibly client ID metadata documents (CIMD) as an alternative to self-registration.
