# MCP ↔ PocketBase Integration Review

**Purpose:** Detail every surface where MCP integrates with PocketBase today, as the basis for a
universal, configurable package that works on **stock PocketBase** and on **pb-postgress**
(the fork that already ships an OAuth2/OIDC authorization server).

**Projects inspected (read-only, all bodies cited were opened):**

| Project | Role in this review |
|---|---|
| [customjs](/Volumes/mini512/Go%20Projects/customjs) | Embedded HTTP MCP server (in-process) |
| [list_pocket](/Volumes/mini512/list_pocket) | Separate stdio MCP bridge (REST proxy) |
| [pb-postgress](/Volumes/mini512/Go%20Projects/pb-postgress) | PocketBase fork, Postgres/MySQL, built-in OAuth2 server, **no MCP** |
| [dreia_pocket](/Volumes/mini512/WebstormProjects/dreia_pocket) | Plain PocketBase app, **no MCP** (negative baseline) |

**Status:** Static inspection only. No live `initialize` / `tools/list` / `tools/call`, auth, or
OAuth round-trip was executed (tests were read, not run). Items marked **BLOCKING** must be
verified before the universal package picks its target API.

---

## 1. Verdict in one page

Only **two** MCP integrations exist, and they are architectural opposites:

| Surface | customjs | list_pocket | pb-postgress | dreia_pocket |
|---|---|---|---|---|
| MCP present | Yes, embedded | Yes, separate binary | No | No |
| Transport | Stateless Streamable-HTTP-style `POST /mcp` (+`/sse` alias) | stdio only | — | — |
| MCP SDK | None — hand-rolled JSON-RPC | `modelcontextprotocol/go-sdk v1.8.0` | — | — |
| Protocol | `2025-06-18` (also `2025-03-26`, `2024-11-05`) | SDK default | — | — |
| In PocketBase process | Yes — `OnServe` → `RegisterRoutes` | No — proxies REST to PB app | — | — |
| Tools | Dynamic per-collection + generic mode | 27 read + 5 write, hardcoded | — | — |
| Resources / prompts | Empty lists only | None registered | — | — |
| Auth | PB `RequireAuth()` + collection rules + role-gated writes | Long-lived PB user token; backend `PermRE` | — (OAuth2 server, see §5) | — |
| Data access | In-process `core.App` + REST subrequest for writes | HTTP proxy to `/mailapi/*`; backend uses direct SQL | — | — |
| PocketBase version | `v0.40.1` (`github.com/pocketbase`) | `v0.41.1` (`ganigeorgiev.com/pocketbase`) | fork itself, `v0.41.3-8-g3725e783` | declares `v0.39.6`, `replace` → fork `v0.41.4` |
| Go | 1.27 | 1.27 | 1.27.0 | 1.27.0 |

**Consequences for the universal package:**

1. There is no shared MCP codebase to extract — the two implementations share only the
   *ideas* (tools-only surface, delegate authz to PocketBase, env config). The universal
   package is a new design informed by both, not a merge.
2. The package must support **both deployment shapes**: embedded HTTP (customjs pattern)
   and sidecar stdio (list_pocket pattern). They differ in auth, lifecycle, and data access.
3. On pb-postgress it is greenfield, but the fork's built-in OAuth2 server is the natural
   remote-MCP auth story — with one critical caveat (§5): its access tokens are **full,
   unscoped PocketBase auth tokens**.

---

## 2. customjs — embedded HTTP MCP server

Source: [mcp/](/Volumes/mini512/Go%20Projects/customjs/mcp) (`config.go`, `routes.go`,
`tools.go`, `registry.go`, `search.go`, `write.go`, `contract_pdf.go`, `types.go`, `utils.go`,
plus `mcp_test.go`, `contract_pdf_test.go` — all read in full).

### 2.1 Transport & wiring

- `RegisterRoutes` ([routes.go:27-33](/Volumes/mini512/Go%20Projects/customjs/mcp/routes.go:27))
  mounts `POST/GET/DELETE` on `/mcp`, `/mcp/`, `/sse`, `/sse/`, each `.Bind(apis.RequireAuth())`.
  `/mcp` is canonical; `/sse` is a legacy alias. No stdio, no SSE transport code.
- Only `POST` does JSON-RPC. `GET`/`DELETE` return `405 Allow: POST` — *"No server-initiated
  SSE stream and no sessions"* ([routes.go:36-45](/Volumes/mini512/Go%20Projects/customjs/mcp/routes.go:36)).
  This is stateless Streamable-HTTP-style MCP.
- Dispatch ([routes.go:77-95](/Volumes/mini512/Go%20Projects/customjs/mcp/routes.go:77)):
  `initialize`, `ping`, `tools/list`, `tools/call`. `resources/list`,
  `resources/templates/list`, `prompts/list` return empty arrays and are **not** advertised
  as capabilities. `initialize` advertises only `tools.listChanged=false`.
- Wiring ([main.go:104-112](/Volumes/mini512/Go%20Projects/customjs/main.go:104)):
  `app.OnServe().BindFunc(...)` calls `registerHTTPRoutes`, then `mcp.Init(se.App)`
  (failure is only `log.Printf`, non-fatal); `registerHTTPRoutes` calls `mcp.RegisterRoutes(r)`
  ([main.go:131](/Volumes/mini512/Go%20Projects/customjs/main.go:131)). No `OnBeforeServe`.
- Hand-rolled JSON-RPC ([types.go:5-23](/Volumes/mini512/Go%20Projects/customjs/mcp/types.go:5),
  manual switch in `routes.go`). [go.mod](/Volumes/mini512/Go%20Projects/customjs/go.mod) has
  **no MCP SDK**; PocketBase `v0.40.1`, dbx `v1.12.0`, go 1.27. Server identity:
  `customjs-mcp 0.3.0`, `latestProtocolVersion=2025-06-18`,
  `supported=[2025-06-18, 2025-03-26, 2024-11-05]` ([config.go:10-15](/Volumes/mini512/Go%20Projects/customjs/mcp/config.go:10)).

### 2.2 Tool surface (dynamic, registry-driven)

Registry collections ([config.go:54-63](/Volumes/mini512/Go%20Projects/customjs/mcp/config.go:54)):
`accepted_contracts, properties, property_extras, notes, tasks, deals, contact, inquiries`
(each with preset Summary/Title/URL fields; `inquiries` has `QueryFields contact.*` + `Expand contact,extra`).

| Tool | Shape | Notes |
|---|---|---|
| `describe_collection` | `{collection?: enum}` | Always visible ([tools.go:150-166](/Volumes/mini512/Go%20Projects/customjs/mcp/tools.go:150)). Lists collections or one collection's fields/filters/sortFields/expand (+ `writableFields` if caller can write). **Leak: no collection-rule check beyond `RequireAuth`** — names/searchFields of all 8 collections disclosed to any authenticated caller ([tools.go:259-311](/Volumes/mini512/Go%20Projects/customjs/mcp/tools.go:259)). |
| `search_<c>` / `fetch_<c>` | per collection | Default mode ([tools.go:96-137](/Volumes/mini512/Go%20Projects/customjs/mcp/tools.go:96)). `search_<c>{query, limit 1–50 def 20, page, sort, +field filters, additionalProperties:false}`; `fetch_<c>{id: required}`. |
| `search` / `fetch` | generic | Only when `mcp_tool_mode=generic` ([tools.go:59-94](/Volumes/mini512/Go%20Projects/customjs/mcp/tools.go:59)). `search{collection: required, query, limit, page, sort, filters: object}`, `fetch{collection, id: required}` replace the per-collection pair. |
| `fetch_contract_pdf` | `{id: required}` | Only if `accepted_contracts` visible ([contract_pdf.go:85-113](/Volumes/mini512/Go%20Projects/customjs/mcp/contract_pdf.go:85)). Returns text + embedded resource blob (base64, `application/pdf`), `OpenWorldHint=true`. |
| `create_record` / `update_record` / `delete_record` | `{collection, data}` / `{collection, id, data}` / `{collection, id}` | Only if `canWrite` ([write.go:119-169](/Volumes/mini512/Go%20Projects/customjs/mcp/write.go:119)). `data` supports `field+`/`field-` append/remove on multi fields; file/autodate/password fields excluded. |

**Search filter derivation** ([search.go:37-66](/Volumes/mini512/Go%20Projects/customjs/mcp/search.go:37)):
from live schema — text/editor/email/url/json → `contains (~)`; select/relation → `=`
(`?=` if multiple); bool → `=`; number → `=` + `<f>_min`/`<f>_max`; date/autodate →
`<f>_from`/`<f>_to`. Unknown filter, bad enum/number/date/sort are rejected. This is the
model for the universal package's dynamic tool generation.

**No resources/prompts surface** ([routes.go:87-92](/Volumes/mini512/Go%20Projects/customjs/mcp/routes.go:87)).

### 2.3 Auth (per layer)

1. **Route:** `apis.RequireAuth()` on every method/path — PocketBase auth token required
   (`Authorization` header; tests confirm unauthenticated `ping` = 401). MCP code never
   parses the JWT itself; PocketBase validates it → `e.Auth` / `RequestInfo.HasSuperuserAuth`.
2. **tools/list visibility** ([tools.go:26-35](/Volumes/mini512/Go%20Projects/customjs/mcp/tools.go:26)):
   a collection is hidden if `!superuser && ListRule==nil && ViewRule==nil` (superuser-only).
3. **Search** ([search.go:338-364](/Volumes/mini512/Go%20Projects/customjs/mcp/search.go:338)):
   `ListRule==nil && !superuser` → tool error. Non-empty `ListRule` is compiled via
   `core.NewRecordFieldResolver` + `search.FilterData` and `AndWhere`-ed into the
   `RecordQuery` SQL, so paging/totals only count visible records. Superuser bypasses;
   `SetAllowHiddenFields(superuser)`.
4. **Fetch / PDF** ([search.go:430-441](/Volumes/mini512/Go%20Projects/customjs/mcp/search.go:430)):
   `App.FindRecordById` then `App.CanAccessRecord(record, info, ViewRule)`; failure returns
   the same not-found text (existence not leaked). `expand` re-checks each related
   collection's `ViewRule` (`nil` = superuser-only).
5. **Writes** ([write.go:80-97](/Volumes/mini512/Go%20Projects/customjs/mcp/write.go:80)):
   `canWrite` = `e.Auth.IsSuperuser()` **or** auth collection `users` with `role` ∩
   `mcp_write_roles` (env, default `['admin']`). Tool visibility uses the same check.

### 2.4 App-custom OAuth (not the fork's — customjs's own)

[authentication/oauth.go](/Volumes/mini512/Go%20Projects/customjs/authentication/oauth.go):
`GET/POST /oauth/login`, `POST /oauth/token`, `GET /oauth/userinfo` (RequireAuth),
`GET /.well-known/oauth-authorization-server|openid-configuration` (+ `/sse/.well-known/*`
aliases for MCP clients).

- Client validated against an `oauth_apps` record (client_secret compared in plain text);
  session in `oauth_sessions`, 10-min cookie. Login proxies the password to
  `$auth_server_url/api/collections/{collection}/auth-with-password`.
- Token endpoint returns `user.NewAuthToken()` — a **regular PocketBase token**
  (`expires_in 1209600`) — plus optional HS256 `id_token` (signed with `$shared_secret`)
  when scope contains `openid`.
- Token exchange ([authentication/main.go:16-70](/Volumes/mini512/Go%20Projects/customjs/authentication/main.go:16)):
  `GET /user_token` reads an `ExchangeToken` header, validates an HS256 JWT (`shared_secret`,
  `exp` required, `userId` claim), finds or **creates** the `users` record, and issues a
  PocketBase token via `apis.RecordAuthResponse`. That token then authorizes `/mcp`.

### 2.5 Data access

- **Reads:** direct in-process `core.App` — `RecordQuery(collection)` +
  `search.NewProvider(resolver).Exec`, `FindRecordById`, `ExpandRecords`. No REST proxy, no
  `app.Dao()`, no raw SQL; filter values bound as `dbx.Params`, field names only from server
  schema. Schema via `app.FindCollectionByNameOrId` / `FindCachedCollectionByNameOrId`
  ([registry.go:74-113](/Volumes/mini512/Go%20Projects/customjs/mcp/registry.go:74)).
- **Writes:** in-process REST **subrequest** through `router.BuildMux()`
  ([write.go:201-212](/Volumes/mini512/Go%20Projects/customjs/mcp/write.go:201)) —
  `POST/PATCH/DELETE /api/collections/{collection}/records[/{id}]` via `httptest.NewRecorder`,
  forwarding caller `Authorization` + `X-Forwarded-*`. **This is the key design decision:**
  PocketBase create/update/delete rules, validation, and `OnRecordCreateRequest` hooks all
  apply exactly as for a real API call. The universal package should copy this, not `App.Save`.
- **PDF:** reads `accepted_contracts.contract_accepted_url`, external HTTPS GET (60s, max
  25MB, `%PDF-` magic check). SSRF guard ([contract_pdf.go:56-73](/Volumes/mini512/Go%20Projects/customjs/mcp/contract_pdf.go:56)):
  https-only, no credentials, no IP hosts, host allowlist `mcp_pdf_hosts` (default `wasabisys.com`).

### 2.6 Config, lifecycle, errors

- Env: `mcp_tool_mode`, `mcp_write_roles`, `mcp_allowed_origins` (empty = allow all),
  `mcp_pdf_hosts`, `base_url`; `shared_secret` + `auth_server_url` (auth); `is_prod`, `base_path`.
- Lifecycle ([registry.go:21-33](/Volumes/mini512/Go%20Projects/customjs/mcp/registry.go:21)):
  `Init` rebuilds the registry (missing collection → Warn + skip, not fatal) and binds
  `OnCollectionAfterCreate/Update/DeleteSuccess` to rebuild. No stdio/shutdown hooks.
- Errors: JSON-RPC `-32700` parse, `-32600` invalid (batch `[]` rejected; unsupported
  `MCP-Protocol-Version` header → 400), `-32601` method not found, `-32602` invalid
  params/unknown tool; notifications/no-id → 202. Tool failures are `isError` text; internals
  logged, generic *"Internal error…"* returned (no SQL leak).
- Tests (read, not run): `mcp_test.go` (482 lines: protocol, tool list, ListRule-before-limit,
  validation, superuser-only collection, expand ViewRule, generic mode, writes) and
  `contract_pdf_test.go` (incl. SSRF cases `169.254.169.254`, evil suffix hosts).
- Dockerfile: `EXPOSE 8080`, MCP shares port 8080 via `/mcp`; no MCP-specific port.

---

## 3. list_pocket — separate stdio MCP bridge (REST proxy)

Source: [cmd/listpocket-mcp/main.go](/Volumes/mini512/list_pocket/cmd/listpocket-mcp/main.go),
[internal/mcpserver/server.go](/Volumes/mini512/list_pocket/internal/mcpserver/server.go),
[internal/mcpserver/writes.go](/Volumes/mini512/list_pocket/internal/mcpserver/writes.go),
[docs/mcp.md](/Volumes/mini512/list_pocket/docs/mcp.md). Prebuilt binary `listpocket-mcp` (12MB) at repo root.

### 3.1 Transport & wiring

- **stdio only**: `server.Run(ctx, &mcp.StdioTransport{})`
  ([main.go:29](/Volumes/mini512/list_pocket/cmd/listpocket-mcp/main.go:29)).
  [docs/mcp.md:5-7](/Volumes/mini512/list_pocket/docs/mcp.md:5) confirms no HTTP/SSE/Streamable
  endpoint is added to PocketBase.
- **Separate binary, not in-process.** `mcpserver.New(Config{URL, Token, AllowWrites})`;
  `New()` *"does not open or modify a database"* — it proxies HTTP to `{base}/mailapi{path}`
  ([server.go:73-74,142](/Volumes/mini512/list_pocket/internal/mcpserver/server.go:73)).
- PocketBase app mount (for context): `pb.OnServe().BindFunc(...)` →
  `registerHandlers(se.Router)`; `api = se.Group('/mailapi').Bind(apis.RequireAuth('users')).BindFunc(hydrateAuthUser)`
  ([cmd/handlers.go:59](/Volumes/mini512/list_pocket/cmd/handlers.go:59)). No `OnBeforeServe`.
- SDK: official `github.com/modelcontextprotocol/go-sdk v1.8.0`; PocketBase `v0.41.1` under the
  renamed module `ganigeorgiev.com/pocketbase/pocketbase` (dbx `v1.13.0`), go 1.27.
  Server identity `listpocket 0.1.0`. **Zero resources/prompts** (no `AddResource`/`AddPrompt`).

### 3.2 Tool surface (static, hardcoded) — 27 read + 5 write

Read tools ([server.go:43-71](/Volumes/mini512/list_pocket/internal/mcpserver/server.go:43),
all `ReadOnlyHint` + `IdempotentHint`, one shared arg shape `readArgs{id?, query?}`,
`additionalProperties:false`; `id` required iff the path has `{id}`, validated
`^[A-Za-z0-9_-]+$`; `query` is a URL-encoded string with no leading `?`; every call is a backend `GET`):

`list_lists`, `get_list`, `list_subscribers`, `get_subscriber`, `get_subscriber_activity`,
`get_subscriber_channels`, `list_contact_notes`, `list_contact_tasks`, `list_campaigns`,
`get_campaign`, `list_templates`, `get_template`, `list_media`, `list_boards`, `get_board`,
`list_tasks`, `list_conversations`, `get_conversation_thread`, `list_inbound_emails`,
`get_inbound_email`, `list_workflows`, `get_workflow`, `get_workflow_catalog`,
`list_workflow_runs`, `get_workflow_run`, `get_dashboard_counts`, `get_dashboard_charts`.

Write tools ([writes.go:58-107](/Volumes/mini512/list_pocket/internal/mcpserver/writes.go:58),
only exposed when `AllowWrites`):

| Tool | Input (essentials) | Backend |
|---|---|---|
| `create_list` | `{name, type: private\|public, optin: single\|double, description?, tags?}` | `POST /lists` |
| `create_subscriber` | `{email?, phone?, first/last/name?, status: enabled\|disabled\|blocklisted, list_record_ids?, attribs?}` | `POST /subscribers` |
| `create_template` | `{name, type: campaign\|campaign_sms\|tx, body, subject?, channel?}` | `POST /templates` |
| `create_contact_note` | `{id, body}` | `POST /subscribers/{id}/notes` |
| `update_subscriber` | `{id, email?*, phone?*, first_name?*, last_name?*, attribs?*}` (*pointer: omitted = preserved, empty = cleared) | `PATCH /subscribers/{id}` |

Tests assert the default tool count is exactly the read set, and `+5` with writes enabled.

### 3.3 Auth

- **MCP layer: almost none.** Startup check only — token non-empty, no CRLF
  ([server.go:79-82](/Volumes/mini512/list_pocket/internal/mcpserver/server.go:79)) — plus the
  `AllowWrites` exposure gate. Annotations are hints, not enforcement.
- **Backend enforces everything:** `RequireAuth('users')` validates the PocketBase
  `users`-collection token; `hydrateAuthUser` loads the user, rejects disabled/invalid,
  extracts role ([cmd/pbhttp.go:129-155](/Volumes/mini512/list_pocket/cmd/pbhttp.go:129));
  `PermRE` ([internal/auth/auth.go:510-537](/Volumes/mini512/list_pocket/internal/auth/auth.go:510)):
  SuperAdmin role bypasses, otherwise any-of permission strings from `PermissionsMap`, else
  `403 permission denied: %s`.
- Per-tool backend permissions (handlers): subscribers/notes/tasks → `subscribers:get_all|get`
  (writes `subscribers:manage`); campaigns → `campaigns:get_all|get`; templates →
  `templates:get` (create `templates:manage`); media → `media:get`; boards → `boards:get`;
  conversations/inbound → `inbox:get`; workflows/runs → `workflows:get|manage`;
  lists/dashboard GETs are auth-only (no permission string). Strings grouped in
  [permissions.json](/Volumes/mini512/list_pocket/permissions.json).
- **Token issuance:** `data.token` from `POST /mailapi/auth/login` (password login, 2FA first
  if enabled; token minted via `IssueClientAuth` → `rec.NewAuthToken()`). Superuser token,
  BasicAuth, and API secret are explicitly **not** substitutes. No OAuth/API-key path in the
  MCP, and no login/refresh/store logic in the server — the host supplies a long-lived token.

### 3.4 Data access, config, errors

- MCP side is a **pure REST proxy**: `backend.call` builds `{base}/mailapi{path}?query` with
  `Bearer` token, 30s client, no redirects. No Dao/Record API/SQL in `mcpserver`.
  Backend handlers use direct SQL via `pb.DB().NewQuery` wrapped as sqlx
  ([internal/pbdb/db.go](/Volumes/mini512/list_pocket/internal/pbdb/db.go)) over collections:
  `subscribers, lists, campaigns, templates, media, boards, crm tasks, conversations,
  inbound_emails, flows, flow_runs, users, roles, listpocket_settings`.
- Config: `-url` flag (default `http://127.0.0.1:9000`, overridden by `LISTPOCKET_MCP_URL`),
  `LISTPOCKET_MCP_TOKEN` env, `-allow-writes` bool. URL must be http(s) host, no
  user/query/fragment. Lifecycle: `signal.NotifyContext` SIGINT/SIGTERM; stateless.
- Errors/limits: 8MiB max response, 30s timeout, redirects not followed,
  `access_token` in query rejected, token redacted from errors, non-2xx →
  `List Pocket HTTP %d: %s`, non-JSON rejected, transport errors hide URL/token.
  Writes are never auto-retried. `server_test.go` covers 401/403/302/HTML/oversize/redaction.
- **Client config:** the only committed MCP-host guidance in any of the four repos is the
  snippet in [docs/mcp.md](/Volumes/mini512/list_pocket/docs/mcp.md) —
  `mcpServers: { listpocket: { command: /absolute/path/to/listpocket-mcp, args: [],
  env: { LISTPOCKET_MCP_URL, LISTPOCKET_MCP_TOKEN } } }`, optional `args: [--allow-writes]`.
  No `.mcp.json` / `claude_desktop_config` is committed anywhere (see §7).
- Dockerfile builds only `./cmd` (the app); the stdio MCP binary is **not** built or copied,
  no MCP port.

---

## 4. dreia_pocket — no MCP (negative baseline)

Searches for `mcp|MCP|modelcontext|mark3labs|jsonrpc|tools/list` over `main.go`, `go.mod`,
`go.sum`, `authentication/`, `lib/`, `migrations/`, `payments/`, `Dockerfile` returned **0 hits**.
It is a plain PocketBase + Stripe app — useful as proof of what a stock-style app looks like
*before* MCP, and as a future test target for the universal package.

- Wiring ([main.go:34,42-61](/Volumes/mini512/WebstormProjects/dreia_pocket/main.go:34)):
  `pocketbase.New()` + `app.OnServe().BindFunc` mounting `authentication.Routes(se.Router)` and
  `payments.RegisterRoutes(se.Router)`, then `app.Start()`. No `/mcp` route, no stdio/SSE.
- Actual HTTP surface: `GET /student/initial` (`RequireAuth(Students, users)`),
  `GET /user_token`, `GET /student_token`, `/customers` + `/invoices` CRUD/finalize/void,
  public `/pay/{token}*`, `POST /stripe/webhook` (Stripe-Signature verified).
- Auth: PocketBase `e.Auth` via `apis.RequireAuth`; an `adminOnly` collection rule
  (`@request.auth.role ~ "admin"`) on `customers, invoices, invoice_payments,
  invoice_payment_splits`. Custom JWTs are **minted only** (HS256, `shared_secret`, 24h,
  user/student claims) and never verified in this code. No superuser/OAuth/API-key middleware.
- Data access: direct PocketBase app API (`FindRecordsByFilter`, `FindRecordById`,
  `core.NewRecord`, `e.App.Save`). No Dao, no SQL, no REST proxy. No `*_test.go` files exist.
- Versions: declares PocketBase `v0.39.6` but `replace => github.com/compdani/pb-postgress v0.41.4`
  — **it effectively already runs the fork**, which makes it a realistic first adopter:
  adding the universal package to dreia_pocket tests the pb-postgress path, not stock.

---

## 5. pb-postgress — the fork: extension points + built-in OAuth2 server

Source: [pocketbase.go](/Volumes/mini512/Go%20Projects/pb-postgress/pocketbase.go),
[apis/](/Volumes/mini512/Go%20Projects/pb-postgress/apis),
[core/](/Volumes/mini512/Go%20Projects/pb-postgress/core). No MCP code of any kind
(case-insensitive `mcp` search hits only `NumCPU` in `apis/file.go:29` + CHANGELOG; no MCP SDK
in `go.mod`). MCP here is a greenfield addition.

### 5.1 Fork identity & divergence (why "just use stock APIs" is a risk)

- `git describe` = `v0.41.3-8-g3725e783`; merge-base with upstream master is in tags
  `v0.41.0..v0.41.3`; HEAD is 47 ahead / 0 behind upstream master; diff vs upstream =
  **467 files, +51632/−13893**. README claims "fixes up to v0.41.1" with selective backports —
  i.e. it is *not* a clean upstream release.
- Not Postgres-only: `core/database_config.go` validates driver `postgres|mysql`
  (pgx `QueryExecModeDescribeExec`, 10s ping; MySQL also supported). SQLite is removed as a
  primary DB and kept only (modernc.org/sqlite) for backup restore. Unconfigured DB falls
  back to `DataDir/db.json` + installer UI.
- Postgres layer: `PB_POSTGRES_URL` / `PB_POSTGRES_SCHEMA` (default `public`),
  `_pb_collections` / `_pb_table_schemas` metadata, dual concurrent/nonconcurrent dbx
  builders routing `SELECT/WITH` vs writes, placeholders `$n` (pgx) vs `?` (mysql).
  Config also via `Config.DBDriver/DBDSN` + `--dbDriver/--dbDSN` flags (`PB_DB_DRIVER`/`PB_DB_DSN`).
- Module is `github.com/pocketbase/pocketbase` itself (no self-version; `Version` is
  ldflags-injected). list_pocket instead imports the renamed `ganigeorgiev.com/pocketbase`
  path — **import paths and version strings are not comparable across the four projects**,
  and observable API already differs (e.g. `RequireAuth()` in customjs vs
  `RequireAuth('users')` in list_pocket).

### 5.2 Where a universal package plugs in (evidenced on the fork)

1. `app.OnServe().Bind(...)` in the host's `main` (canonical example in
   `examples/base/main.go:112-121`) — the same hook both existing apps use.
2. `e.Router.GET/POST/Any/Group` + `Bind`/`Route` on `ServeEvent{App, Router, Server, ...}`
   — mount `POST /mcp` or `/api/mcp`.
3. `apis.WrapStdHandler` / `WrapStdMiddleware` ([apis/base.go:70-87](/Volumes/mini512/Go%20Projects/pb-postgress/apis/base.go:70))
   — adapts a stdlib `http.Handler`, which is exactly what an SDK-based streamable-HTTP MCP
   handler is. This is the lowest-risk mount for an official-SDK server.
4. Plugin pattern: `plugins/jsvm` / `migratecmd` show the `MustRegister/Register(app, config)`
   convention the package should follow (`Register(app, Config)`), plus `RootCmd` if a stdio
   cobra subcommand is wanted.
5. Auth reuse: global `loadAuthToken` middleware strips an optional `Bearer ` prefix and
   resolves `FindAuthRecordByToken(token, TokenTypeAuth)` (HS256 JWT verified against
   record `TokenKey` + collection secret) → `e.Auth`; `RequireAuth` /
   `RequireSuperuserAuth` / `RequireSameCollectionContextAuth` are available as route binds.
6. Data reuse: `core.App` Record API (`FindRecord*`, `Save`, `Delete`, `CountRecords`,
   `RunInTransaction`) and `DB()/ConcurrentDB()/NonconcurrentDB()` dbx builders. There is no
   `Dao` (removed upstream). Collection API rules (`List/View/Create/Update/DeleteRule`,
   `nil` = superuser-only) are enforced in `apis/record_crud.go` — **only** when going
   through the record API layer, which is why customjs's subrequest design (§2.5) matters.

### 5.3 Built-in OAuth2/OIDC authorization server (the fork's auth gift — and trap)

Confirmed built-in: [apis/record_auth_oauth2_server.go](/Volumes/mini512/Go%20Projects/pb-postgress/apis/record_auth_oauth2_server.go)
(1258 lines, read in full; header: *"collection acts as authorization server for authorization
code grant (with optional PKCE S256); issued access_token is regular collection auth token"*).

**Endpoints** (per auth collection `{c}`, except `_superusers`):
`GET|POST /api/collections/{c}/oauth2-server/authorize` (hosted login / account chooser;
password, passkey, or the collection's outbound OAuth2 providers; MFA rejected),
`POST .../oauth2-server/token`, `GET .../oauth2-server/userinfo`
(`RequireSameCollectionContextAuth`), `GET .../.well-known/openid-configuration`,
`GET|POST .../logout` (clears SSO cookie, validates `post_logout_redirect_uri`).
Plus `GET|POST /api/oauth2-redirect` completing external-provider/popup flows, and a
no-client "popup/SSO" flow (virtual client `@popup`, exposed as virtual `sso` provider).

| Property | Value (evidenced) |
|---|---|
| Grant types | `authorization_code` **only**. No refresh_token, client_credentials, password, implicit. Token response has **no refresh token**. |
| PKCE | `S256` only; optional for registered clients, **mandatory** for popup. Verifier-without-challenge rejected (no downgrade). |
| Client registration | **None** (no dynamic registration endpoint). Clients are inline config on the auth collection: `OAuth2ServerConfig{Enabled, CodeDuration, SessionDuration, Clients[]{Id, Name, Secret, RedirectURIs}, Popup}`. Secret 30–255 chars, constant-time compare, exact-match redirect URIs (https or loopback-http only), secrets redacted on serialization. |
| Authorization codes | `security.RandomString(48)`, stored **SHA256-hashed only** in system collection `_oauth2Codes`, single-use (atomic consume), default lifetime 120s (valid range 10–600), hourly cleanup cron, invalidated on record tokenKey change. |
| Access token | `record.NewAuthToken()` — an ordinary PocketBase auth JWT (`{type:auth, id, collectionId, refreshable:true, exp}`, HS256 with record TokenKey + collection secret). Validated by the standard PB auth path everywhere. |
| id_token | Only when scope contains `openid`; HS256 signed with the **client Secret**, 1h, claims `iss/sub/aud=client.Id/iat/auth_time/nonce?` + profile claims. |
| Scopes | Free-form space-separated string stored verbatim; `scopes_supported=[openid, email, profile]`. **No per-client allowed scopes, no scope registry, and no scope/audience claim in the access token.** Scope only shapes id_token/userinfo claims (and `userinfo` even hardcodes `email profile`). |
| SSO session | JWT cookie `pb_oauth2_session_{collectionId}` (HttpOnly, SameSite=Lax), `SessionDuration` default 30d. |
| Defaults | `Enabled:false`, `CodeDuration:120`, `SessionDuration:2592000`; authorize/token rate-limited. |

> **Critical caveat for MCP:** because the OAuth2 access token *is* a full PocketBase auth
> token with no scope/audience restriction, completing the fork's OAuth flow gives an MCP
> client **full PB API access for that user** (subject only to collection rules) — not
> MCP-scoped authorization. Any optional MCP tool policy must
> be enforced locally and must not be described as a verified OAuth grant from this token.
> Full PB session reuse is explicitly accepted for this package. This
> also means a leaked token is a leaked PB session, and with no refresh token, remote MCP
> hosts must re-run the authorize flow (or accept long-lived tokens) on expiry.

### 5.4 What the fork does *not* give you

No MCP transport/tools, no dynamic client registration (an MCP host cannot self-register —
clients must be pre-created in collection config), no refresh tokens, no scoped tokens,
no MCP-specific bearer handling (an external MCP server would just reuse `loadAuthToken`).

---

## 6. Cross-cutting surfaces

### 6.1 Shared by both MCP implementations

- Go 1.27, **tools-only** surface (zero resources/prompts in either).
- Read tools = list/search + get/fetch; writes are separately gated (`canWrite` role check /
  `-allow-writes` flag) and never auto-retried.
- Authorization is **delegated to PocketBase** (route `RequireAuth` + collection rules or
  backend permission strings), not reimplemented at the MCP layer.
- Errors are tool/HTTP errors that do not leak SQL, internals, or tokens.
- Configuration is env-driven; neither commits a client config file.

### 6.2 Divergent (the package must abstract these)

| Axis | customjs | list_pocket |
|---|---|---|
| Deployment | In-process, shares PB port 8080 | Sidecar binary, stdio, talks HTTP to PB :9000 |
| Tool generation | Dynamic from live schema + registry presets | Hardcoded per endpoint |
| Filter model | Schema-derived (contains/equals/ranges), invalid rejected | Opaque `query` string passed through |
| Write path | REST subrequest through own router (rules+hooks apply) | REST call to custom handlers (PermRE applies) |
| Token | PB token via app-custom OAuth or `/user_token` exchange | Password-login token supplied by host env |
| Extra surface | `fetch_contract_pdf` (SSRF-guarded external fetch) | Dashboard counts/charts, workflow catalog/runs |

### 6.3 Client / consumer surface

- **No committed MCP client config in any repo** (no `.mcp.json`, `claude_desktop_config*`,
  `.vscode/mcp.json`; customjs `.vscode/` is empty; list_pocket `.claude/launch.json` only
  defines frontend/backend dev servers). The *only* host-integration doc is list_pocket's
  [docs/mcp.md](/Volumes/mini512/list_pocket/docs/mcp.md) `mcpServers` snippet (§3.4).
- Env key names only (no values inspected): customjs — `mcp_allowed_origins, mcp_tool_mode,
  mcp_write_roles, mcp_pdf_hosts` (+ app keys); list_pocket MCP — `LISTPOCKET_MCP_URL,
  LISTPOCKET_MCP_TOKEN` (supplied via host env, **not** in its `.env`); dreia —
  `pgurl, shared_secret, STRIPE_*`, etc.; pb-postgress — no `.env` (DB via flags/db.json).

---

## 7. Proposed universal package shape (evidence-based, pending §8 verification)

**Accepted authentication decision:** reuse the authenticated user's full PocketBase session.
Ordinary PB API access with the same token is intentional. MCP-specific OAuth grants,
scoped tokens, and a separate grant store are not required. Existing PB rules, application
permissions and fork instance visibility remain authoritative. Any optional MCP tool/write
gates are application policy, not token scopes. Remote-client OAuth compatibility must be
tested separately; this choice does not assert compliance with the MCP OAuth profile.

A Go package, e.g. `pocketbase-mcp`, following the fork's own plugin convention:

```go
mcp.Register(app, mcp.Config{ ... }) // binds OnServe, mounts transport(s)
```

**Ports to abstract (each maps to a divergence above):**

1. **Host** — minimal interface over `OnServe` registration, `Router` group mounting,
   `WrapStdHandler` for an SDK `http.Handler`, app accessor, and collection-change hooks
   for registry rebuilds. Must tolerate both module paths (`github.com/pocketbase/...` vs
   `ganigeorgiev.com/pocketbase/...`) and `RequireAuth` arity differences — via build-tagged
   or version-specific adapter files, not assumptions.
2. **Transport** — one tool registry served over: (a) embedded stateless Streamable HTTP
   `POST /mcp` (customjs shape; protocol floor `2025-06-18`), and (b) optional stdio binary
   (list_pocket shape, official go-sdk, `mcpServers` snippet documented). Prefer the official
   SDK + `WrapStdHandler` over customjs's hand-rolled JSON-RPC for the HTTP transport.
   Sessions/server-initiated SSE are explicit opt-ins (customjs deliberately has none).
3. **ToolRegistry** — declarative `CollectionConfig{Name, SummaryField, TitleField, URLField,
   SearchFields, Expand}` generating `describe_collection` + `search_<c>`/`fetch_<c>`
   (and generic `search`/`fetch` mode) with schema-derived filters, rebuilt on
   `OnCollectionAfter{Create,Update,Delete}Success`. Static endpoint tools (list_pocket
   shape) remain available as hand-registered tools on the same registry.
   **Fix the known leak:** metadata tools must apply the same visibility rule
   (`ListRule/ViewRule == nil` ⇒ superuser-only) that `tools/list` applies.
4. **AuthProvider / ToolAuthorizer** — `TokenValidator → Principal{Record, Collection,
   IsSuperuser, Roles}` with three providers: (a) *StockPB* — raw PB auth token via
   `RequireAuth`/`loadAuthToken` (both existing implementations' de-facto model);
   (b) *ForkOAuth2* — authorization_code + PKCE S256 against the fork's
   discovery/token endpoints when the collection's `OAuth2Server.Enabled`, treating the
   result as a full PB token and applying existing PB permissions plus optional tool policy; re-auth on expiry
   (no refresh token exists); (c) *StaticToken* for stdio (host-supplied token).
   Writes additionally gated by a role/permission check (`mcp_write_roles` pattern) and an
   explicit `AllowWrites` switch.
5. **DataAccess** — two modes with explicitly different guarantees: *InProcess* must
   replicate customjs semantics (ListRule compiled into the query **before** limit;
   `CanAccessRecord(ViewRule)` on fetch/expand; writes via REST subrequest through
   `BuildMux` so rules/validation/hooks/file-field handling apply) — direct `App.Save`
   is **not** equivalent and must not be the silent default; *RESTProxy* for the stdio
   sidecar (Bearer forwarding, backend permission layer applies, 8MiB/30s/no-redirect/
   token-redaction limits as in list_pocket).

**Config surface (superset of both existing ones):** mount path (`/mcp`), tool mode
(per-collection | generic), collection registry, `AllowWrites` + write roles, allowed
origins, per-tool external-fetch host allowlist (PDF pattern), auth provider selection,
stdio binary flags (`-url`, token env, `-allow-writes`).

---

## 8. BLOCKING gaps — verify before building

From the completeness critic; all six block picking the package's target API/auth/data layer:

1. **Stock-vs-fork API compatibility unverified.** No clean stock PocketBase tree was
   inspected. Whether the fork's OAuth2 server, `OnServe`/`Router`/`WrapStdHandler`
   signatures, `RequireAuth` arity, and `FindAuthRecordByToken`/`CanAccessRecord`/
   `FindCollection*` APIs exist identically in stock PB (and which version) is unknown.
   *Next step: diff stock v0.41.x vs fork master on exactly these symbols.*
2. **Remote-MCP client compatibility unverified (auth model decided).** Fork OAuth2 tokens are full unscoped PB tokens
   with no refresh; customjs uses a different app-custom OAuth; list_pocket uses a
   long-lived password token. The accepted model reuses full PB sessions, including raw PB tokens on stock PB.
   Verify actual client login/discovery support for that model; narrower credentials
   and a separate grant store are not prerequisites.
3. **Write/data-access semantics unreconciled.** REST subrequest vs direct `App.Save` vs
   REST proxy differ in rules/hooks/file/hidden-field/validation behavior; collection-rule
   enforcement for a direct in-process tool layer on the fork is unverified (incl. the
   `describe_collection` metadata leak in §2.2).
4. **Cross-dialect behavior unverified.** Postgres/MySQL (pgx, `_pb_collections` metadata,
   dual builders) vs SQLite: the "driver-agnostic via dbx" claim for contains/JSON/select/
   relation filters, sorting, and expand is asserted, never demonstrated.
5. **No runtime evidence.** Everything above is static reading. No live
   `initialize`/`tools/list`/`tools/call`, 401/403 rule-enforcement, OAuth code+PKCE
   exchange, or token-validation round-trip has been executed on stock or fork.
6. **Dynamic collection discovery on the fork unproven.** `FindCollectionByNameOrId` /
   `FindCachedCollectionByNameOrId` + registry rebuild on collection hooks is evidenced
   only on stock-derived v0.40.1 (customjs), not against the fork's Postgres-backed
   collection metadata — yet it is the package's core configurability mechanism.

**Suggested first step:** a throwaway spike — mount a one-tool (`tools/list` only) MCP
handler via `WrapStdHandler` on (a) dreia_pocket (fork path, via its `replace`) and
(b) a clean stock PocketBase v0.41.x app — and run a real client `initialize` +
authenticated/unauthenticated call against both. That single spike resolves gaps 1, 5,
and most of 6 before any tool-surface code is written.

---

*Generated from read-only inspection of the four projects above. File paths are absolute so
they open directly in the editor. No secret values were read (env key names only).*
