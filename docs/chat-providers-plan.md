# Chat model providers: multiple configs and Codex plans

Status: implemented for review, 2026-11-18. Target: chat v1.3. This supersedes the
single-provider setup in [chat.md](chat.md#8-model-providers).

**Decision summary**

- A named provider catalog, stored in three private PocketBase collections and
  managed by superusers; static Go config and the current single-provider path
  remain supported.
- Each turn selects a provider and model; the turn records both, and `/status`
  gains labelled choices while keeping the old flat model list.
- Built-in kinds: `openai` (Chat Completions or Responses), `anthropic` and
  `codex` (ChatGPT plan / Codex subscription).
- Port `list_pocket`'s proven Codex OAuth and Responses work, but implement
  RS256 verification with the standard library, keeping the module
  dependency-free.
- No per-user provider accounts, no public OAuth callback, no vendor SDKs.

**Implementation note (2026-11-18).** The backend, OpenAI Responses adapter,
Codex OAuth/token manager, catalog/admin service, both adapter stores, turns
migration, frontend picker and example seeding are implemented. Not done in this
pass: a full example settings panel (the API is available), a live OpenAI
Codex sign-in (tested against a mock authorization server), fork tests against
PostgreSQL, and Codex CLI `auth.json` import.

**Review note (2026-11-18).** A review pass fixed: `POST /providers` silently
replacing an existing entry (now `409`); a partial secret re-entry saving the
other, unreadable secret as empty; a stale Codex loopback goroutine failing a
newer sign-in attempt; and a Codex manager keeping its old issuer after
`oauthBaseURL` changed. One open design question remains. A saved key follows
the entry's `baseURL`, so a superuser who changes the URL (or tests a saved id
against a new URL) sends the existing key to the new host. Requiring the key
to be re-entered when the host changes would close that gap.

## 1. Why

Chat today has one `provider.Provider` and one default model, fixed when
`EnableChat` builds the runner:

- `RunnerConfig.Provider` is a single instance and `RunnerConfig.Model` is a
  single string. `SubmitOptions.Model` can pick a different model name, but the
  same provider (and the same base URL, key and wire protocol) handles it.
- `/status` returns a flat `models` list. A model id is not tied to a provider,
  so two OpenAI-compatible gateways cannot offer the same model.
- The examples build that one provider from `MCP_CHAT_*` environment variables.
- There is no OpenAI Responses adapter, so the ChatGPT/Codex plan route used in
  `list_pocket` cannot be used from this module.

The goal is a named provider catalog, managed in the app rather than in env
vars, with per-turn selection. Concretely:

1. Any number of provider entries, each with its own kind, base URL, key,
   headers, wire protocol and model allowlist.
2. Multiple plain OpenAI-compatible configs (gateways, local proxies, separate
   accounts) side by side.
3. Anthropic entries alongside them.
4. A `codex` entry that signs in with a ChatGPT plan (the Codex subscription
   route already working in `list_pocket`) and refreshes its own tokens.
5. Per-turn choice of provider and model, recorded on the turn.
6. A superuser-only management API and a small settings UI, so adding a key or
   signing in does not require a redeploy.

The current single-provider API keeps working. Chat is still optional and an
MCP-only installation still needs none of this.

### Non-goals for this version

- **Per-end-user provider accounts.** Providers are installation-level, like
  PocketBase settings. One signed-in Codex account is shared by every chat user
  of that mount. Per-user accounts are a separate feature with much larger
  ownership and quota questions.
- **Cross-process catalog sync.** Each process reloads its own catalog and sees
  writes made through its own admin API. Several chat processes are already
  unsupported (single-instance chat), so no pub/sub is added.
- **A public OAuth callback.** Sign-in uses a loopback listener for local
  browsers and a paste-the-redirect-URL flow for remote servers, as
  `list_pocket` does. No public callback route is added to the chat mount.
- **Provider-level rate limits, budgets or health checks.** The runner's global
  and per-user concurrency limits remain the only limits.
- **Vendor SDKs or new module dependencies.** In particular, Codex ID-token
  validation is rewritten on the standard library instead of using
  `golang-jwt/jwt/v5` from `list_pocket`.

## 2. Current state, and what changes

| Piece | Today | After |
|---|---|---|
| `chat/provider/provider.go` | Small provider port: one request in, chunks out | Unchanged |
| `chat/provider/openai` | Chat Completions only; base URL, key, extra headers, name | Adds `WireAPI` (`chat` or `responses`), a token source and extra body |
| `chat/provider/anthropic` | Messages API | Unchanged, usable through the catalog |
| `chat/provider/scripted` | Test provider | Unchanged |
| `chat/runner.go` | One provider in `RunnerConfig`, one model | Optional `ModelRouter`; resolves provider+model once per turn |
| `chat/wire.go` `Turn` | `model` only | Adds `provider`, empty for legacy rows |
| `chat/http.go` `/status` | `models []string` | Adds `choices` and `defaultChoice`; keeps `models` |
| `chat/http.go` turn body | `model` | Adds `provider` |
| `examples/stock`, `examples/fork` | `MCP_CHAT_PROVIDER` switch | Managed catalog, or a static config list; env remains a legacy fallback |
| `adapters/*/chat.go` | `Provider` + `Model` required | Adds `Providers` / `ManageProviders` |
| Adapters' chat store | Six collections | Adds `provider` to turns, additively |
| New `chat/provider/catalog` | — | Config model, router, admin service |
| New `chat/provider/codex` | — | ChatGPT-plan OAuth, tokens, model list |

### What is reused from `list_pocket`

| Source | Reuse | Adaptation |
|---|---|---|
| `internal/flow/chatgpt/oauth.go` | PKCE, dynamic client, token exchange, refresh rotation, revoke, JWKS, `ListModels` | Remove the JWT dependency; verify RS256 with `crypto/rsa` + `crypto/sha256` |
| `internal/flow/chatgpt/store.go` | Token lifecycle and status | Replace the PocketBase record store with a `codex.Store` interface implemented by the adapter |
| `internal/flow/chatgpt/login.go` | Loopback listener and callback delivery | Keep; no PocketBase in the package |
| `internal/flow/llm/responses.go` | Responses request/response translation and SSE parsing | Adapt to `provider.Request` / `provider.Chunk`, including streaming tool calls |
| `cmd/ai_settings.go` | Sign-in state machine, model cache, import/logout UX | Move into `catalog.Admin`; superuser auth instead of app roles |

## 3. Design overview

```text
superuser settings UI
        │  bearer superuser token
        ▼
chat admin routes ──> catalog.Admin ──> adapter provider store (encrypted)
        │                                  │
        │                                  └── codex credentials
        └── reload ──> catalog.Router (atomic snapshot)
                              │
                              │ Choices / Default / Resolve
                              ▼
chat HTTP ──> chat.Runner ──> concrete provider.Provider for one turn
```

Four concepts:

- **Provider config** — one named entry: id, kind, base URL, key or sign-in,
  model allowlist, default model and advanced request settings.
- **Model choice** — a public `(provider, model)` pair, optionally labelled,
  returned by `GET /status`. This is what the browser picker shows.
- **Resolved model** — a concrete `provider.Provider` plus the wire model name
  for one turn. Resolved before the turn is stored, so an unknown choice is a
  request error, not a failed turn.
- **Catalog** — the immutable in-memory snapshot of enabled providers. A reload
  builds a new snapshot and swaps it atomically. Turns already running keep the
  provider instance they started with.

### 3.1 File layout

New packages and files, chosen so the dependency direction stays
`adapter -> catalog -> chat -> provider` and no package imports `chat` in a
cycle:

```text
chat/choice.go                       ModelChoice, ResolvedModel, ModelRouter, errors
chat/admin.go                        ProviderAdmin and its public DTOs
chat/runner.go                       Router, Target, resolved job, turn provider
chat/wire.go                         Turn.Provider
chat/http.go                         choices in /status, provider in submit, admin routes
chat/provider/openai/responses.go    Responses request/stream implementation
chat/provider/codex/                 OAuth, credentials, manager, JWT verification, models
chat/provider/catalog/               Provider config, Router, Admin, Store interface
adapters/pocketbase/providerstore.go Provider collections, encryption, CRUD
adapters/pocketbase/chat.go          EnableChat wiring and managed-mode setup
adapters/pocketbase/chatstore.go     turns schema: additive provider field
adapters/pbpostgress/*               the same provider store and wiring
frontend/client/*                    choices, provider-aware controller, picker
examples/stock|fork|chat/*           managed setup, settings panel, docs
```

`chat/provider/catalog` imports `chat` to implement `chat.ModelRouter` and
`chat.ProviderAdmin`; `chat` imports only `chat/provider`, so there is no cycle.
The adapter imports both and is the only place that knows about PocketBase
records.

## 4. Provider config model

```go
// chat/provider/catalog
type Kind string

const (
    KindOpenAI    Kind = "openai"    // Chat Completions or Responses
    KindAnthropic Kind = "anthropic" // Messages API
    KindCodex     Kind = "codex"     // ChatGPT plan: OAuth + Responses
)

type Model struct {
    ID    string `json:"id"`
    Label string `json:"label,omitempty"`
}

type Provider struct {
    ID           string            // stable slug, immutable
    Label        string            // display name
    Kind         Kind
    Enabled      bool
    BaseURL      string            // empty means the kind's default
    WireAPI      string            // openai only: "chat" (default) or "responses"
    Models       []Model           // explicit allowlist
    DefaultModel string            // empty means Models[0]
    Headers      map[string]string // extra request headers
    ExtraBody    map[string]any    // optional gateway-specific fields
    APIKey       string            `json:"-"` // runtime only, never serialized
    OAuthBaseURL string            // codex only; default https://auth.openai.com
}
```

Validation rules:

- `ID` matches `^[a-z0-9][a-z0-9_-]{0,62}$` and is immutable after creation.
- An omitted `enabled` in a create or a static file means true; the admin input
  uses a pointer so an explicit false can disable an entry.
- `Kind` is one of the three above; `WireAPI` is only meaningful for `openai`.
- `BaseURL` must be `http` or `https`; a trailing slash is normalized.
- An enabled provider has at least one model; `DefaultModel` is in `Models` or
  empty (then the first model is the default).
- `Headers` values may not contain CR/LF. `Authorization` is allowed but is
  treated as a secret on the way out (never returned by the admin API).
- `ExtraBody` is for non-secret gateway tuning. A secret belongs in `APIKey` or
  a write-only header, not in `ExtraBody`.
- A `codex` entry must have credentials before it appears in `/status`; the
  admin view shows it as "not signed in" instead.
- A provider that fails to build is excluded from choices and carries a
  configuration error in the admin view. Startup logs the failure without a
  secret.

## 5. Storage

Three new private base collections, provisioned only in managed mode, using the
same collection prefix as chat (`_mcpChat` by default). All have nil API rules,
are never added to the manifest, and are scoped by `(namespace, mount)` so two
apps can share a fork database:

| Collection | Holds | Key / index |
|---|---|---|
| `_mcpChatProviders` | id/slug, label, kind, enabled, base URL, OAuth base URL, wire API, models, default model, headers, extra body, encrypted API key, key hint | unique `(namespace, mount, slug)` |
| `_mcpChatProviderAuth` | per-provider Codex credentials (encrypted), client id, email, scopes, expiry, last error | unique `(namespace, mount, slug)` |
| `_mcpChatSettings` | small JSON values: `hostId`, `defaultChoice` | unique `(namespace, mount, key)` |

**Secrets at rest.** API keys, header values and OAuth credentials are
encrypted with PocketBase's existing encryption helper:

```go
key := os.Getenv(app.EncryptionEnv()) // normally PB_ENCRYPTION_KEY
cipher, err := security.Encrypt(raw, key)
```

If no encryption key is configured, the admin service refuses to store a secret
and returns a clear configuration error. It never silently stores plaintext.
Existing legacy providers (constructed in Go) do not use this store and are
unaffected. Tests assert that no plaintext key appears in a raw record.

**What crosses the API.** The admin API never returns a key, header value or
OAuth token. It returns `hasApiKey`, a last-four hint, header names, and Codex
status (email, plan, expiry). Chat `/status` returns only the choices, labels
and attachment support.

**Model lists.** `Models` is an explicit allowlist, not a live feed. A
`models/refresh` admin action calls the provider's `/models` endpoint and either
returns candidates for review or replaces the list when asked. This keeps
`/status` deterministic and prevents a gateway's model list from silently
widening access.

### 5.1 Model discovery, compared with `list_pocket`

The fetch mechanics are the same ones `list_pocket` uses:

| Kind | Request | Filter | Label |
|---|---|---|---|
| `openai` | `GET {baseURL}/models` with the provider key | every `data[].id` (also `models[].id` for compatible gateways) | `name` if present, else the id |
| `codex` | `GET {base}/models` with a fresh plan token | `visibility` absent or `"list"` (hidden entries skipped) | `display_name`, else the id |
| `anthropic` | `GET {base}/models` when the endpoint exists | every returned model | `display_name`, else the id; otherwise the config's explicit list |

`catalog.Admin` fetches on demand; it does not cache a live feed, and an
explicit refresh updates the allowlist after creation. The difference is
**when the result is used**:

- `list_pocket` returns the fetched list directly to the settings UI on every
  load (with `?refresh=1` to bypass its cache).
- This plan treats the fetch as an admin action that populates the provider's
  explicit `Models` allowlist. `/status` serves the stored list, so a chat page
  load never fans out to provider APIs, and a gateway cannot silently add a
  model to the picker.
- If `Models` is empty when an OpenAI-compatible/Anthropic provider is created,
  or when Codex sign-in completes, the admin service tries to fetch and save
  the list. The settings UI also calls `models/refresh` with `save: true` when
  the user clicks refresh.
- Optional later mode: a per-provider `modelsMode: "auto"` with a TTL cache that
  updates the choices without an admin click. It is not in v1 because it makes
  the picker depend on provider availability and can silently widen the
  allowlist.

So the answer to "does it grab models like `list_pocket`?" is yes for the
request itself, the Codex visibility filter and the display names. The only
deliberate difference is that the fetched list is stored and reviewed instead
of being served live to every chat client.

**Sources.** `EnableChat` accepts exactly one source:

1. **Managed** (`ManageProviders: true`) — DB-backed, admin routes on. If
   `Providers` is also set, it seeds missing entries on first boot only; after
   that the DB is authoritative.
2. **Static** (`Providers` non-empty) — built at startup, no admin routes. Good
   for tests and small deployments that keep config in a file.
3. **Legacy** (`Provider` + `Model`) — exactly today's behavior. No new
   collections, no admin routes.

Setting both legacy and catalog fields is a startup error, so a host never has
to guess which one wins. Managed mode may start with zero providers: `/status`
returns no choices and turn submissions return 400 until the first provider is
added. That is the fresh-install bootstrap, not a startup failure.

`EnableChat` stops requiring `Provider` when a catalog is configured. The
adapter builds one `catalog.Router` and passes it to both the runner and the
handler. The handler's optional admin surface is passed separately:

```go
// adapter ChatConfig additions
Providers       []catalog.Provider // static list, or first-boot seed
ManageProviders bool               // provision the three collections and mount admin routes
ProviderStore   catalog.Store      // optional custom store; nil means the adapter's

// pbmcp.ChatOptions additions, set by the adapter
Router        chat.ModelRouter    // choices and per-turn resolution
ProviderAdmin chat.ProviderAdmin  // nil disables the admin routes
AdminAuth     chat.Authenticator  // superuser-only authenticator for those routes
```

A host that supplies its own `Runner` is responsible for giving that runner a
router; the adapter only wires the runner it builds itself. In catalog modes
the adapter derives the legacy `ChatOptions.Models` list from the router's
choices when the host did not set one, so old clients keep seeing a model list.

A custom `provider.Provider` implementation (including the stock example's
offline `demo`) is not a catalog kind. It keeps using the legacy
`Provider` + `Model` path. Managed/static catalogs cover the built-in kinds;
a custom-factory hook can be added later if apps need custom providers beside
the built-ins.

## 6. Selection semantics

`/status` exposes choices. A turn submission resolves them as follows:

| Request | Result |
|---|---|
| no provider, no model | the configured default choice |
| provider set, no model | that provider's `DefaultModel` |
| provider and model set | must be in that provider's `Models` |
| no provider, model set | the default provider if it offers the model; otherwise the unique provider that offers it; otherwise 400 |
| unknown, disabled, not signed in, or ambiguous | 400 `invalid_request`, before a turn is stored |

Resolution happens once, in the submit request, and the concrete provider is
carried into the turn worker. A catalog reload while the turn is running does
not change its provider. A deleted provider stops new turns; existing
conversations remain readable and can continue with another choice.

The resolved `provider` and `model` are recorded on the turn. The `model` field
already exists; `provider` is new and empty for turns created before the
upgrade. The idempotency key still wins: retrying a submission with a different
choice returns the originally stored turn unchanged.

## 7. Runner changes

New neutral types in `chat` (not in the catalog package):

```go
type ModelChoice struct {
    ID          string   `json:"id"`
    Provider    string   `json:"provider"`
    Model       string   `json:"model"`
    Label       string   `json:"label,omitempty"`
    Group       string   `json:"group,omitempty"`
    Default     bool     `json:"default,omitempty"`
    Attachments []string `json:"attachmentTypes,omitempty"`
}

type ResolvedModel struct {
    Choice   ModelChoice
    Provider provider.Provider // concrete instance for this turn
    Model    string            // wire model name
}

type ModelRouter interface {
    Choices(ctx context.Context) ([]ModelChoice, error)
    Default(ctx context.Context) (ModelChoice, error)
    Resolve(ctx context.Context, providerID, model string) (ResolvedModel, error)
    SupportsAttachment(mimeType string) bool // union across ready choices
}

var (
    ErrNoModelConfigured = errors.New("chat: no model provider is configured")
    ErrModelUnknown      = errors.New("chat: unknown model choice")
    ErrModelAmbiguous    = errors.New("chat: model is offered by several providers")
)
```

`RunnerConfig` gains `Router ModelRouter`. `Provider` and `Model` remain the
legacy fallback. `NewRunner` accepts either a router or a provider, not neither.

`SubmitOptions` gains `Provider string` and `Target *ResolvedModel`:

- The HTTP handler resolves the choice first, so it can validate attachments
  and so an unknown choice never creates a turn. It passes the resolved target.
- Direct runner callers may leave `Target` nil; the runner resolves through its
  router.
- Without a router, everything behaves as today: the configured `Provider` is
  used, `Target` is nil, and `Turn.Provider` stays empty.

`turnJob` stores the resolved provider so every model step in one turn uses the
same instance. `buildRequest` takes the model from the job. `Runner` gains
`Choices`, `DefaultChoice` and `SupportsAttachment` helpers for the handler.

`SupportsAttachment` remains on the runner for compatibility. With a router it
returns the union across ready choices; without one it asks the single provider
as today.

## 8. Turn schema migration

The adapters currently refuse an existing chat collection that is missing a
field. Adding `provider` to turns therefore needs a real upgrade path:

- Keep the safety checks: base collection, nil API rules, correct field types,
  correct indexes, and refusal of incompatible collections.
- When an existing collection is otherwise compatible, **add known missing
  fields** in the same provisioning transaction and log the upgrade. This is
  limited to fields the current spec owns; an unknown extra field is still
  allowed, as today.
- Add `provider` (text) to the turns spec in both adapters.
- Add a migration test that starts from a v1.2 schema (six collections, turns
  without `provider`), runs `NewChatStore`, and asserts: the field exists,
  existing turn rows still read, and a new turn can record a provider.

Provider collections are new, so no existing data is touched. A conflicting
collection with one of their names refuses startup, as chat collections do.

## 9. HTTP contract

### 9.1 `GET /status`

Keep the existing fields. Add choices, default and per-choice attachments:

```json
{
  "version": "1",
  "models": ["gpt-5", "claude-sonnet-4", "gpt-5-codex"],
  "choices": [
    {
      "id": "openai-personal::gpt-5",
      "provider": "openai-personal",
      "model": "gpt-5",
      "label": "GPT-5",
      "group": "OpenAI (personal)",
      "default": true,
      "attachmentTypes": ["text/plain", "image/png", "application/pdf"]
    },
    {
      "id": "codex-work::gpt-5-codex",
      "provider": "codex-work",
      "model": "gpt-5-codex",
      "label": "GPT-5 Codex",
      "group": "ChatGPT plan",
      "attachmentTypes": ["text/plain", "image/png", "application/pdf"]
    }
  ],
  "defaultChoice": "openai-personal::gpt-5",
  "features": { "proposals": true, "uploads": true },
  "limits": { "...": "as today; attachmentTypes is the default choice's set" }
}
```

The `id` is an opaque stable string for picker round-tripping. The proposed
format is `providerID + "::" + model`, split at the first `::`; provider IDs
cannot contain `::`. Clients must treat it as opaque.

`models` stays a de-duplicated flat list for old clients. `limits.attachmentTypes`
becomes the default choice's support, so an old client behaves exactly as it did
with a single provider. New clients use the selected choice's list.

### 9.2 `POST /conversations/{id}/turns`

```json
{
  "text": "…",
  "provider": "codex-work",
  "model": "gpt-5-codex",
  "attachments": ["…"]
}
```

Both `provider` and `model` are optional. The resolution table in section 6
applies. The handler resolves the target before checking attachment types, so a
file accepted for one provider is rejected consistently for another. The turn
response gains `"provider"` when one was resolved.

Unknown or ambiguous choices return `400` with the existing error envelope and
a safe message ("That model isn't available. Pick one from status."). No
provider internals, base URLs or key state appear in the message.

### 9.3 Provider admin routes

Mounted under the existing chat prefix only in managed mode. They use a separate
authenticator that requires a PocketBase superuser, so they work even when the
chat surface itself rejects superusers (`Auth.AllowSuperusers` off). Every
response uses the chat error envelope and never contains a secret.

| Method and path | Does |
|---|---|
| `GET /providers` | List provider views and the default choice |
| `POST /providers` | Create one; body includes a write-only API key. An existing id is `409` |
| `GET /providers/{id}` | One provider view |
| `PATCH /providers/{id}` | Update; omitted key means unchanged, empty string clears |
| `DELETE /providers/{id}` | Remove config, credentials and any default pointing at it |
| `POST /providers/actions/test` | Test an unsaved config (reachability, auth, model list) |
| `POST /providers/{id}/test` | Test a saved config |
| `POST /providers/{id}/models/refresh` | Fetch `/models`; replace the allowlist only with `{"save": true}` |
| `POST /providers/actions/default` | Set `{provider, model}` as the default choice |
| `POST /providers/{id}/signin` | Start Codex sign-in; returns `{authUrl, expiresAt}` |
| `POST /providers/{id}/signin/complete` | Finish with `{url: "<pasted redirect>"}` |
| `POST /providers/{id}/signin/cancel` | Abandon the pending attempt |
| `POST /providers/{id}/signout` | Revoke the refresh token and clear local credentials |
| `POST /providers/{id}/import` | Import existing Codex credentials (optional) |

Admin auth rules:

- Missing/invalid token: `401`.
- Valid token but not a superuser: `403`.
- The pending sign-in is bound to the superuser who started it and expires
  after 10 minutes. Another superuser cannot complete it.
- A normal chat user never sees these routes as usable; they are not listed in
  `/status`.

The handler dispatches `/providers...` to `AdminAuth` **before** the normal chat
authenticator. Otherwise a superuser could be rejected by the user-facing chat
gate (`AllowSuperusers` off) and never reach the settings surface. The admin
path does not apply the chat policy, does not create a chat caller for the
normal routes, and does not widen any user's chat access.

## 10. Provider kinds

### 10.1 `openai` — OpenAI-compatible

Existing behavior plus:

- `WireAPI: "chat"` (default) keeps the current Chat Completions adapter.
- `WireAPI: "responses"` uses the new Responses adapter.
- `Headers` already exist; `ExtraBody` is merged into the request body last for
  gateway-specific fields.
- A `TokenSource` override lets a provider obtain a bearer token per request
  (used by Codex; also useful for short-lived gateway tokens):

  ```go
  type TokenSource interface {
      Token(ctx context.Context, forceRefresh bool) (string, error)
  }
  ```

  The provider calls `Token(ctx, false)` for the request; on a 401 it calls
  `Token(ctx, true)` and retries the same body once.
- The catalog supplies a shared `*http.Client` with a bounded response-header
  wait; stream bodies stay bounded by the turn context, not by a total request
  timeout. This avoids a provider hanging a worker forever without breaking
  long streams.

Multiple entries are independent: different base URL, key, headers, model list
and wire API. This is the "multiple OpenAI-compatible configs" requirement.

### 10.2 `anthropic`

Existing Messages adapter, built from a catalog entry. Multiple entries work the
same way. The model list is explicit because not every Anthropic-compatible
gateway exposes `/models`.

### 10.3 `codex` — ChatGPT plan (the Codex subscription route)

This is the work already proven in `list_pocket`:

- OAuth 2.0 authorization code with PKCE against `https://auth.openai.com`.
- First sign-in uses the dynamic agent client; the issued `client_id` is stored
  and reused. `ext_agent_host_id` is a stable opaque URN per install.
- Scopes include `openid profile email offline_access resource.invoke
  chatgpt.tokens.use.direct`; the plan scope is required.
- Token exchange, rotating refresh tokens, revocation, JWKS discovery and ID
  token validation (issuer, audience, expiry, nonce, subject).
- Model list from `GET /models`, skipping entries whose `visibility` is set and
  is not `"list"`, with display names.
- Requests use the Responses wire API. The tested base URL is
  `https://api.openai.com/v1` (configurable), with `stream: true`,
  `store: false`, system as `instructions`, full history in `input`, and no
  `temperature` / `top_p` / `max_output_tokens`.

Token handling:

- A `codex.Manager` per provider owns credentials and a single-flight refresh
  mutex. Refreshes happen 10 minutes before expiry and on a forced 401 retry.
- The manager is kept across catalog reloads, so an old and a new provider
  instance can never refresh the same rotating token twice.
- Refresh writes the rotated token back through the store before the new access
  token is used.
- A 401 from the Responses endpoint forces one refresh and one retry of the
  same request body.
- Signing out revokes the refresh token, clears it locally, reloads the router,
  and removes the choice from `/status`.

Sign-in UX:

1. A superuser starts sign-in from the settings UI.
2. The server creates a pending attempt and tries to listen on
   `127.0.0.1:1455` (ephemeral fallback). The settings UI opens the returned
   auth URL in the superuser's browser.
3. On a browser running on the same machine as the server, the loopback
   listener receives the redirect. On a remote browser the redirect points at
   that browser's own machine, where no listener exists; the superuser copies
   the full redirect URL from the address bar and pastes it into the settings
   UI, and the server validates state and exchanges the code.
4. The credentials are encrypted and stored, the model list is refreshed if it
   was empty, and the router reloads.
5. Pending attempts are single-use, expire after 10 minutes, and can be
   cancelled. A restart drops them; the user starts again.

Hosting note (repeat in docs and the settings UI): OpenAI offers ChatGPT plan
usage for open-source and locally hosted apps. A paid or remotely hosted
deployment is expected to apply through OpenAI's interest form. The module
should not imply that any deployment may share plan tokens.

## 11. OpenAI Responses adapter

Add `chat/provider/openai/responses.go` behind `Config.WireAPI`:

| `provider.Request` | Responses request |
|---|---|
| `System` | `instructions` |
| user text | `input_text` content part |
| user image / PDF / text attachment | `input_image` / `input_file` with data URLs |
| assistant text | input message with `output_text` |
| assistant tool calls | `function_call` items |
| tool result | `function_call_output` with `call_id` |
| `Tools` | `{type: "function", name, description, parameters, strict: false}` |
| `store` | always `false` for the plan route |
| `stream` | always `true` |
| `MaxOutputTokens` | omitted by default; opt-in for a gateway that allows it |

Streaming:

- `response.output_text.delta` → text chunks.
- `response.output_item.done` with a `function_call` → one complete tool-call
  chunk (id, name, arguments). Arguments are validated by the runner, as today.
- `response.completed` → usage + done.
- `response.failed`, `response.incomplete`, `error` → one error chunk.
- A stream that ends before `response.completed` is an error, never a silent
  success.
- Context cancellation closes the body promptly.

The adapter keeps the existing 32 MiB request cap and attachment limits.

## 12. Frontend

`ChatStatus` gains `choices` and `defaultChoice`; `models` stays. A choice maps
directly to agent-panel's existing `AgentModelChoice` shape (`id`, `label`,
`group`, `description`, `disabled`):

- `modelsFromState` returns `AgentModelChoice[]` when choices exist, else the
  old string list. `agent-panel` already accepts objects, so no element change
  is needed.
- `ChatController` state gains `choices`, `defaultChoice` and `choice`. `send`
  accepts a choice id, resolves it to `{provider, model}`, and passes both to
  `ChatClient.submitTurn`.
- A conversation remembers its last choice in controller memory (not
  persisted), so reopening it in the same session restores the picker. No
  server-side conversation default is added in this version.
- The settings UI is app-specific. `examples/chat/plain` gets a small
  superuser-only section: provider list, add/edit form, test button, model
  refresh, Codex sign-in with copy/paste, sign-out and default choice. Secrets
  are only ever typed into the form and sent over the authenticated request;
  they are never read back.
- `ChatController` remains token-agnostic; the settings page uses the normal
  PocketBase superuser token, not the chat user's token.

## 13. Security

- **Admin gate:** superuser only, through a dedicated authenticator on the
  admin routes. The normal chat auth and policy are unchanged.
- **Secrets:** AES-256-GCM through PocketBase's `security.Encrypt`; encrypted at
  rest, never returned, never logged. Refuse to store secrets without an
  encryption key. Rotating the encryption env key makes existing stored secrets
  unreadable; document that an operator must re-enter keys and sign in Codex
  again after a key change (or re-encrypt offline).
- **Key masking:** only the last four characters may be shown. Header values are
  write-only; the API returns names only.
- **OAuth:** random state/nonce/verifier, PKCE S256, single pending attempt,
  10-minute TTL, bound to the initiating superuser, single use. ID tokens are
  verified against the published JWKS, issuer, audience, expiry, nonce and
  subject. No access or refresh token ever appears in a URL, log, transcript or
  turn record; the short-lived authorization code appears only in the redirect
  the admin pastes, and is exchanged once.
- **SSRF:** base URLs are superuser-controlled. Only `http`/`https` is accepted,
  and the docs state that a superuser can point a provider at an internal
  service. Chat users cannot set a base URL.
- **Error leakage:** model-facing and browser-facing errors stay generic. Test
  results and admin errors may name the provider and HTTP status, but never a
  key or token.
- **Token lifetime:** the chat worker still never outlives the caller's
  PocketBase token; provider tokens are independent and server-side.

## 14. Compatibility and migration

- Legacy `RunnerConfig{Provider, Model}` and `SubmitOptions.Model` keep their
  exact behavior. `Turn.Provider` is omitted when empty.
- `/status` and turn responses are additive. No existing field changes shape.
- New collections exist only in managed mode. Static and legacy modes provision
  nothing new.
- v1.2 chat stores upgrade in place by adding the `provider` field to turns, as
  described in section 8.
- The examples keep `MCP_CHAT_*` as a legacy fallback. On first boot with
  managed mode, if no provider rows exist and the env is set, the example seeds
  one entry, logs it, and ignores the env on later boots. New setups use the
  settings UI or a checked-in `chat-providers.json` with secrets supplied
  separately.
- No vendor SDK is added; the root module and chat subpackages remain
  standard-library only.

A non-secret `chat-providers.json` for static or first-boot seeding can look
like this. Keys are entered through the admin API. A static loader that keeps
secrets in a separate `0600` file sets `Provider.APIKey` in Go after decoding;
neither the admin view nor `/status` ever includes it:

```json
{
  "providers": [
    {
      "id": "openai-personal",
      "kind": "openai",
      "label": "OpenAI (personal)",
      "baseURL": "https://api.openai.com/v1",
      "models": [{ "id": "gpt-5" }],
      "defaultModel": "gpt-5"
    },
    {
      "id": "gateway",
      "kind": "openai",
      "label": "Internal gateway",
      "baseURL": "https://gateway.example/v1",
      "models": [{ "id": "openai/gpt-5" }],
      "defaultModel": "openai/gpt-5"
    },
    {
      "id": "codex-work",
      "kind": "codex",
      "label": "Codex (work plan)",
      "models": [{ "id": "gpt-5-codex", "label": "GPT-5 Codex" }],
      "defaultModel": "gpt-5-codex"
    }
  ]
}
```

## 15. Testing

| Area | Tests |
|---|---|
| `catalog.Router` | choices ordering, default, provider/model/ambiguous/unknown resolution, disabled/unready exclusion, atomic reload, attachment union |
| `catalog.Admin` | create/update/delete validation, key unchanged/clear/replace, redaction, default update, model refresh save vs review |
| Provider store | encrypted round-trip, no plaintext in DB, unique slug, namespace/mount isolation, cascade delete, seeding, missing-key refusal |
| OpenAI Responses | request mapping, text/tool/usage chunks, attachments, 401 refresh retry, incomplete/failed/truncated streams, cancellation |
| Codex auth | PKCE/state/nonce, token exchange, refresh rotation single-flight, forced refresh, revoke, JWKS/RS256 validation failures, nonce/issuer/audience/expiry, model list, loopback and pasted callback, timeout/cancel |
| Runner | resolve once per turn, provider recorded, legacy path, routerless behavior, max steps with a resolved provider |
| HTTP | status choices/default, provider in turn body, 400 for unknown/ambiguous, attachment check per choice, admin 401/403, CRUD, sign-in flow against a mock auth server |
| Adapters and stores | v1.2 → v1.3 schema upgrade, managed collections in both adapters, restart recovery still scoped, no manifest leak; `chat/storetest` round-trips `Turn.Provider` for custom stores |
| Frontend | choices mapping, opaque id round-trip, provider+model in `submitTurn`, picker objects, settings form, no secret rendering |
| Manual | real Codex sign-in locally and via pasted callback; a turn with a tool call and an attachment; key rotation without restart; sign-out removes the choice |

Commands (from `docs/operations.md`):

```sh
GOWORK=off go test -race ./...
(cd adapters/pocketbase && GOWORK=off go test -race ./...)
(cd frontend/client && node --test)
# fork adapter against a throwaway PostgreSQL, as documented
```

## 16. Phases

### Phase 1 — Core routing, no new provider kinds

- Add `ModelChoice`, `ModelRouter`, `ResolvedModel`, errors.
- Add `Router`/`Provider`/`Target` to the runner and handler, `provider` to
  `Turn`, and `/status.choices` + `defaultChoice`.
- Add a minimal static `catalog.Router` built from `[]catalog.Provider` (no
  database, no admin routes), so the routing path is testable end to end.
- Keep the legacy path and tests for it.

Done when: two OpenAI-compatible entries in Go config can be selected per turn
and the turn records which one answered.

### Phase 2 — Managed catalog, admin API and storage

- `catalog.Admin`, provider store interface and PocketBase implementation.
- Three private collections, encryption, redaction, seeding, reload.
- Separate superuser admin authenticator and routes in the chat handler.
- Both adapters: `ManageProviders`, additive turns migration.
- Tests and docs for storage/migration.

Done when: a superuser can add two gateway configs and an Anthropic config in
the settings API, switch between them without a restart, and no secret is
readable back or present in plaintext in the database.

### Phase 3 — OpenAI Responses adapter

- `WireAPI`, `TokenSource`, `ExtraBody`.
- Responses request mapping and SSE decoder, tool calls, usage, attachments.
- Tests against a fake Responses server.

Done when: an OpenAI-compatible Responses endpoint works with tools and
attachments, using a plain API key.

### Phase 4 — Codex plan

- `chat/provider/codex`: credentials, OAuth, stdlib JWT verification, token
  manager, model list, loopback and pasted callback.
- Catalog kind wiring, sign-in/sign-out/import admin routes, status views.
- Docs and hosting note.
- Tests, including a mock authorization server.

Done when: a superuser signs in a ChatGPT plan, sees Codex models in `/status`,
runs a tool-using turn with an attachment, survives an access-token expiry, and
signs out cleanly.

### Phase 5 — Frontend, examples and docs

- Client types, controller choices/provider, agent-panel mapping.
- Plain example settings section; stock/fork examples seed from managed config
  and keep the env fallback. The stock `demo` provider stays on the legacy path,
  since it is a custom provider rather than a catalog kind.
- Rewrite `docs/chat.md` section 8, update `operations.md`, `roadmap.md`,
  `frontend/client/README.md` and the example READMEs.
- Frontend tests.

Done when: the picker shows labelled choices from both kinds, submits the
provider, and the settings page covers add/edit/test/sign-in without exposing
secrets.

### Phase 6 — Hardening

- Race tests for reload vs in-flight turns and token refresh.
- Failure UX: provider down, revoked token, deleted provider, invalid config.
- Log review for secret leakage; redaction tests.
- Manual staging pass with a real Codex sign-in and a gateway.
- Optional: per-conversation default choice, import from Codex CLI
  `~/.codex/auth.json` after verifying the token audience.

Phases 1–2 are independently shippable and keep the current examples working.

## 17. Open questions and assumptions

Assumptions to confirm before Phase 2:

1. **Installation-level, superuser-managed providers.** This is modeled on
   PocketBase settings. If each end user must bring their own Codex account,
   the ownership and UI model change substantially and should be a separate
   design.
2. **Superuser-only management.** App roles can be added later through a
   custom `AdminAuth` authenticator; v1 uses PocketBase superusers.
3. **Encryption key.** Reusing `PB_ENCRYPTION_KEY` (via
   `app.EncryptionEnv()`) is acceptable, and managed secret storage requires it.
4. **Codex base URL.** `https://api.openai.com/v1` with the Responses API is the
   tested route. A deployment may override `BaseURL`.
5. **No public OAuth callback.** Loopback plus pasted redirect URL is enough for
   both local and remote servers.
6. **Explicit model allowlists.** `/models` refresh is an admin action, not a
   live feed.
7. **No per-conversation default on the server.** The client remembers the last
   choice per conversation in memory; a server-side default can be added later
   if apps need it.
8. **Import scope.** Start with import of the module's own credential JSON
   (client id, tokens, scopes, expiry). Codex CLI `auth.json` import is a later
   compatibility check, after verifying the token audience and account id.
