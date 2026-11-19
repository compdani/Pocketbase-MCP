# Operations

Deploying and running the module. How it works is in
[mcp-server.md](mcp-server.md) and [chat.md](chat.md).

## Pinned versions

Updated 2026-11-19.

| Item | Pin |
|---|---|
| Root module | `github.com/compdani/pocketbase-mcp`, standard library only, `go 1.26` |
| Stock PocketBase | `ganigeorgiev.com/pocketbase/pocketbase v0.41.1` and `ganigeorgiev.com/pocketbase/dbx v1.13.0`. PocketBase moved to this host at v0.41; v0.40.1 and earlier were `github.com/pocketbase/pocketbase`. |
| Fork | `github.com/compdani/pb-postgress` `master` at `c047535c` (merge of PR #4, `oauth-mcp-clients`), tagged and pinned as `v0.41.6` |
| Go toolchain | `go1.26.5`. Modules that depend on PocketBase (adapters, `auth/stockoauth`, examples) declare `go 1.27` and switch to `go1.27.0` automatically. |
| Databases tested | SQLite (stock), PostgreSQL 14 (fork). **MySQL is untested.** |

Each adapter and example picks its PocketBase with its own `go.mod`
`replace` directive. Never use a `go.work` that includes both adapters or both
examples.

To try unmerged fork changes, point the fork modules at a local checkout for
the time being (`go mod edit -replace github.com/pocketbase/pocketbase=../path/to/pb-postgress`)
and don't commit that change.

## Deployment checklist (fork)

1. **Back up first.** The fork's migration that hashes OAuth client secrets
   can't be undone. Back up the database and save any client secrets you still
   need before it runs.
2. **Set the "Sign-in app URL"** in the `users` collection's OAuth2 server
   settings to the main app (for example `https://pocket.dreia.org`), so every
   app on the database sends sign-in there.
3. **ID tokens are now RS256.** Apps that verify them must switch from HS256
   and fetch keys from the collection's `/jwks` endpoint. Include the new
   signing key in backups.
4. **Self-registration:** leave it off, or turn it on with an allowed-hosts
   list. Every token issued is a full user session (see below).
5. **Test with real clients on a public staging URL:** ChatGPT with a
   hand-created client (steps in
   [verification/MILESTONE-3-REPORT.md](../verification/MILESTONE-3-REPORT.md)),
   and one self-registering client such as Claude Code.

## Chat providers

Managed chat providers add three private collections (`_mcpChatProviders`,
`_mcpChatProviderAuth`, `_mcpChatSettings`). API keys, header values and
ChatGPT/Codex credentials are encrypted at rest. Set a 32-character `PB_ENCRYPTION_KEY` (or
the app's configured encryption env) before adding a provider with a secret;
without it the settings API refuses to store one. Changing the key makes
existing stored secrets unreadable: re-enter provider keys and sign in Codex
again after a rotation.

The settings API is mounted under the chat prefix and is superuser-only. It
uses its own authenticator, so `Auth.AllowSuperusers` does not have to be on
for normal chat just to let an administrator manage providers. Normal chat
users can still see the `choices` in `/status` but can never read a key, header
value or token.

For a first boot, `MCP_CHAT_PROVIDERS_FILE` can seed a JSON catalog (see
[`examples/chat-providers.example.json`](../examples/chat-providers.example.json)).
Keep any file that contains `apiKey` values out of version control and mode
`0600`; the settings API is the preferred place for later changes. Codex plan
usage is offered by OpenAI for open-source and locally hosted apps; a paid or
remotely hosted deployment is expected to apply through OpenAI's interest form.

## Full session reuse

Tokens issued to an MCP client are ordinary PocketBase session tokens:

- They work against the whole PocketBase REST API, not just `/mcp`.
- The only way to revoke one is to rotate the user's token key, which also
  signs that user out of the browser and every other client. Revoking a
  single client's tokens isn't possible.
- Revoking a self-registered client stops new sign-ins; tokens it already has
  stay valid until they expire.
- The fork's identity scopes are not MCP-specific grants, so the module does
  not claim conformance with the MCP OAuth audience-binding profile.

## Self-registration rate limits

Both hosts limit self-registration (Dynamic Client Registration) per client
IP per hour:

| Host | Setting | Default | Window |
|---|---|---|---|
| Stock (`auth/stockoauth`) | `Config.RegistrationPerHourPerIP` | 10 | rolling hour |
| Fork | the auth collection's `registration.rateLimit` (counted per collection) | 10 (when 0) | clock hour |

Both use PocketBase's `RealIP()`. Behind a reverse proxy, configure
PocketBase's trusted-proxy headers, or every registration looks like it comes
from the proxy and they all share one quota.

The counts live in each process's memory: a restart resets them, and N
instances behind a load balancer allow N times the limit. For more than one
instance, rate-limit the register path at your proxy or load balancer
(`/oauth/register` on stock, `/api/collections/<collection>/oauth2-server/register`
on the fork), or keep self-registration off.

Per-user rate limits and a concurrency cap for `/mcp` itself are not built
yet ([roadmap.md](roadmap.md)).

## Running the tests

```sh
GOWORK=off go test -race ./...                               # root module
(cd adapters/pocketbase && GOWORK=off go test -race ./...)   # stock adapter
(cd frontend/client && node --test)                          # browser client
```

The fork adapter's tests need a throwaway PostgreSQL:

```sh
initdb -D /tmp/pgtest -U postgres -A trust
pg_ctl -D /tmp/pgtest -o "-p 55461 -k ''" -w start
createdb -h 127.0.0.1 -p 55461 -U postgres pbmcp_test
(cd adapters/pbpostgress && \
  PB_DB_DSN='postgres://postgres@127.0.0.1:55461/pbmcp_test?sslmode=disable' \
  GOWORK=off go test -race -p 1 ./...)
pg_ctl -D /tmp/pgtest -m fast -w stop && rm -rf /tmp/pgtest
```

The fork tests turn request logging off (`Logs.MaxDays = 0`) to avoid a data
race in the fork's own background logging; the real fix belongs in the fork.

## Migrating from customjs

customjs becomes the first app on this module. Keep what ChatGPT is
configured against:

- **Same PocketBase:** the stock adapter now builds against v0.41.1 on the
  new `ganigeorgiev.com` path. Move customjs off v0.40.1 first if it is
  still there; one binary can't contain both module paths.
- **Same paths:** `/mcp`, `/sse`, `/oauth/login`, `/oauth/token`,
  `/oauth/userinfo` and root discovery, through config.
- **Same clients:** import each `oauth_apps` record with its full callback
  URLs using `stockoauth.ImportHandClient`; secrets are re-hashed.
- **Config mapping:** `collectionPresets` and `collectionOrder` become a
  manifest, `mcp_tool_mode` becomes `toolMode`, and `mcp_write_roles` becomes
  a `field:role:<value>` policy.
- **Upstream login** (`auth_server_url`) can stay as an adapter until users
  sign in locally.
- **Test the live ChatGPT connector on a staging copy** before switching
  production.

A written migration guide is still on the [roadmap](roadmap.md).
