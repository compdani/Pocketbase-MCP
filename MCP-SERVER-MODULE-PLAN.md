# PocketBase MCP server module plan

Status: proposed implementation contract, revised 2026-11-17. This is a plan, not implemented or runtime-verified functionality. It supersedes the provisional architecture in section 7 of the integration inventory.

Revision notes (2026-11-17):

- Implementation basis is now customjs's hand-written MCP server and OAuth flow, generalized into a package. No third-party MCP SDK, OAuth library, JSON Schema library, LLM SDK or frontend dependency.
- The OAuth flow that already works with ChatGPT in customjs is the compatibility baseline. Spec discovery and client self-registration are additions for other clients, not replacements.
- Folded in review findings: which tokens `/mcp` accepts, release tiers, manifest gaps, the build-workspace hazard, and revocation and token-lifetime caveats.

## 1. Decisions and scope

Build a reusable Go module with a PocketBase-independent MCP core and explicit embedded adapters for stock PocketBase and pb-postgress. Reuse the authenticated user's full PocketBase token and permissions, as accepted by the user. MCP exposure configuration selects what the module exposes; it never grants rights beyond the host's authorization.

### Implementation basis: customjs, no added libraries

Start from the customjs `mcp/` and `authentication/` packages and generalize them. Do not adopt the official Go MCP SDK or any other MCP, OAuth, JWT, JSON Schema or LLM client library.

- Allowed dependencies: Go standard library, the host's PocketBase module (stock or fork), and libraries PocketBase itself already pulls in (for example dbx). Nothing else in the root module or adapters.
- Frontend packages (chat client) are dependency-free TypeScript.
- Test-only tools (for example the MCP Inspector, run as an external process) are allowed for conformance checks; they are never imported.
- Any spec behavior the module claims to support is implemented and tested in-house. Unsupported spec features are not advertised.

Rationale: customjs already runs in production against ChatGPT with a small, readable implementation. The protocol surface needed (stateless JSON-RPC over HTTP POST, tools only) is small enough to own, and owning it avoids SDK session/registry behavior that could leak per-user tool lists.

### OAuth interpretation

Install the module's OAuth implementation on stock PB when requested; reuse the fork's existing implementation without duplicating its routes, persistence or hooks. Confirmed fork routes are collection-scoped and already mounted by its API router, not a plugin the MCP module must register. Gaps in the fork's MCP-client support are fixed in the fork itself (it is user-owned), not patched over by the module.

### Release tiers

- **v1 (MCP):** embedded Streamable HTTP (stateless POST), stock and fork adapters, manifest-generated collection tools, discovery tools, custom tools, bearer auth, stock OAuth (customjs-compatible flow, hardened), native fork OAuth reuse.
- **v1.1 (chat, read):** chat service and browser client with server-persisted conversations, resumable turns, cancellation and read-only tools. Single instance only.
- **v1.2 (chat, write):** proposals and approval, editor adapters, uploads.
- **Later:** stdio/REST sidecar, resources/prompts, file tools, idempotency keys, multi-instance chat workers.

No automatic exposure of every collection, hardcoded users/admin roles, raw SQL tool, or unconditional superuser bypass of the configured MCP manifest. A superuser retains PB powers but sees only deliberately exposed MCP operations, and only when superuser tokens are explicitly accepted (section 3).

## 2. Package structure and host compatibility

Proposed layout (names and API sketches below are provisional):

```text
pocketbase-mcp/                 # root Go module: stdlib only, no PB dependency
  protocol/                    # JSON-RPC types, lifecycle, version negotiation (from customjs routes.go/types.go)
  server.go                    # registry, request pipeline, HTTP handler
  config.go / manifest.go      # versioned declarative contracts
  policy.go / principal.go     # host-neutral authorization contracts
  schema/                      # in-house JSON Schema subset validator
  tools/                       # generated collection and discovery tools (from customjs search/tools/write)
  oauth/                       # discovery documents, PKCE, client registration, shared by stock and fork helpers
  chat/                        # optional chat API, provider loop and storage ports (v1.1+)
  frontend/client/             # dependency-free TypeScript client; reuses agent-panel UI
  adapters/pocketbase/          # nested Go module: stock adapter
  adapters/pbpostgress/         # nested Go module: fork adapter
  auth/stockoauth/              # optional stock provider (from customjs authentication/oauth.go)
  examples/stock/               # independently buildable integration fixture
  examples/fork/                # independently buildable integration fixture
  integration/                 # shared behavior scenarios run against both fixtures
```

Use separate adapter modules to keep fork-only symbols out of stock compilation. Both adapters translate PB types to neutral core types; PB Record/App must not leak into the root API. Reuse internal neutral helpers instead of maintaining two MCP implementations.

The fork declares the canonical github.com/pocketbase/pocketbase module path. Its consumer fixture must select the fork with its own go.mod replace directive; replace directives inside dependencies do not configure the consuming application. Never import stock and fork as competing implementations of the same module path in one binary.

**Workspace hazard:** do not create a single `go.work` that includes both adapter modules or both fixtures; they resolve the same module path to different code. CI builds and tests each fixture in isolation.

Pin an actual stock release, fork commit and Go toolchain in milestone 1. The renamed ganigeorgiev.com module path seen in list_pocket is a separate compatibility target, not an automatic first-release promise. Do not infer compatibility from a version string or RequireAuth call arity alone.

### Host port

Expose neutral operations for authentication, collection/schema resolution, instance visibility, record reads, record API dispatch, optional metadata invalidation and shutdown. Return a structured capability report:

```text
host kind and adapter version
native OAuth: supported / enabled for configured auth collection / issuer endpoints
native OAuth MCP support: protected-resource metadata, client registration, PKCE
instance collection visibility: supported
record request hooks preserved: writes yes; reads per section 6
schema invalidation: hooks / revision polling / static
storage dialect: sqlite / postgres / mysql
```

Host identity is selected by the application importing the adapter. Capability checks validate that selection; absence of a fork symbol must not be treated as proof that stock OAuth installation is safe.

## 3. Authentication and OAuth

### Accepted tokens on /mcp

`Auth.Collections` lists which auth collections' tokens `/mcp` (and chat) accept, in every OAuth mode including off. It defaults to the single OAuth `Auth.Collection` when OAuth is on and is required when OAuth is off. Tokens from other collections are rejected with 401. `_superusers` tokens are rejected unless `Auth.AllowSuperusers` is set explicitly. This replaces customjs's `apis.RequireAuth()` (any auth collection) and its hardcoded `users.role` write gate.

### Modes

Config separates token validation from login/authorization-server ownership:

- Token authentication: PB bearer token, validated through the adapter's normal PB auth path on every request.
- OAuth mode: auto, native, module, external, or off.
- OAuth auth collection: explicitly configured; separate from exposed data collections.
- Public base URL and allowed origins: explicit deployment configuration (customjs's `base_url` and `mcp_allowed_origins`, as config not env-only).

| Mode | Stock adapter | Fork adapter |
|---|---|---|
| auto | Install module OAuth if OAuth is enabled in config | Reuse native OAuth; disabled native collection config is a startup error |
| native | Startup error unless adapter proves native support | Reuse enabled collection OAuth |
| module | Install module-owned OAuth | Reject by default to prevent duplicate ownership |
| external | Use explicitly configured provider integration and PB identity contract | Same; never install native replacements |
| off | PB bearer tokens only; no OAuth installation | PB bearer tokens only; no OAuth installation |

OAuth enablement is explicit. A fork operator can choose off for bearer-only use; auto must not silently switch an enabled OAuth deployment into bearer-only mode.

Installation sequence:

1. Validate manifest and host capabilities, then compute an immutable registration plan before adding hooks or routes.
2. Resolve exactly one OAuth owner per server mount and auth collection. Check provider configuration and path conflicts.
3. Bind MCP lifecycle hooks once, using module-owned stable hook IDs scoped by mount. Reject duplicate registration on the same app/mount; independent apps must not share globals (customjs's package-level `registry` and `apiRouter` become per-server fields).
4. Native fork mode adds only MCP routes and MCP-facing discovery (below). It does not add authorize/token/userinfo/login/logout handlers, code collections, cleanup jobs or OAuth hooks.
5. Stock module mode installs its own routes, storage and cleanup lifecycle. It must not replace PB's outbound OAuth login routes or record auth hooks.
6. Log a credential-free startup report describing the chosen owner, endpoints, accepted collections and manifest revision.

### Client compatibility baseline (what works with ChatGPT today)

customjs connects to ChatGPT with:

- Authorization-server metadata at the site root, `/.well-known/oauth-authorization-server` and `/.well-known/openid-configuration`, plus `/sse/...` copies for older connector paths.
- A client created by hand as an `oauth_apps` record; the client ID and secret were entered manually in ChatGPT's connector settings (`client_secret_post`). Confirmed by the user, 2026-11-17; ChatGPT does not self-register here.
- Authorization code flow returning a PB auth token from `/oauth/token`.

The package keeps this working as a first-class configuration: root-level discovery, compatibility aliases for the configured MCP paths, and hand-created clients. A regression test with these exact settings is a release gate.

### Additions for spec-following clients

Some clients (Claude Code, Cursor, VS Code and others) cannot take a hand-entered client ID and expect the MCP authorization discovery chain. Add, in every OAuth-enabled mode:

- `401` from `/mcp` with `WWW-Authenticate: Bearer resource_metadata="<base>/.well-known/oauth-protected-resource<mcp path>"`. customjs currently returns PB's plain JSON 401.
- `/.well-known/oauth-protected-resource` (and the path-suffixed form for the MCP path) naming the mount's issuer in `authorization_servers`.
- Authorization-server metadata listing `code_challenge_methods_supported: ["S256"]`.
- Client self-registration: Dynamic Client Registration (`registration_endpoint`) and/or Client ID Metadata Documents (client ID is an HTTPS URL to the client's metadata). Pick in milestone 3 after testing current clients; implement at least one in v1. Self-registered clients get a restricted default (public client, PKCE required, exact redirect URIs, rate-limited registration, admin-visible and revocable).

These additions must not change the baseline flow above. When both root-level and issuer-path discovery exist, they describe the same issuer.

### Selecting the user/auth collection

OAuth explicitly configures which existing PocketBase auth collection owns the login identity, using Auth.Collection (for example users, staff or customers). There is no implicit users default and no first-auth-collection discovery. Resolve the configured name or ID to a stable collection ID after bootstrap.

This is separate from the collection exposure manifest: authenticating against staff can permit access to properties, tasks or other collections according to PB rules. The auth collection need not itself be exposed as an MCP data collection.

Startup validation requires that the collection exists, is an auth collection, is visible to the host instance where applicable, and supports the configured login method. Reject _superusers for the module OAuth flow in v1. Reject missing, ambiguous or invalid configuration before installing OAuth routes. Do not automatically create or convert a user collection.

Bind each client registration, authorization transaction and code to the resolved auth collection ID. Login, consent and token exchange preserve that binding: an authenticated record from another collection cannot satisfy the transaction, even if its record ID or email matches.

v1: one OAuth auth collection per MCP server mount. Multiple identity collections use separate mounts with separate provider namespaces, client registrations and transaction storage. Never choose the collection from an unvalidated caller-supplied name or guess it from an email.

Fork native mode uses the same Auth.Collection setting to select and validate that collection's existing OAuth2Server configuration.

### Stock OAuth: port customjs, then harden

Port `authentication/oauth.go` (oauth_apps, oauth_sessions, login page, code exchange, userinfo, discovery) into `auth/stockoauth`, keeping the external behavior ChatGPT relies on. Required changes:

| customjs today | Package behavior |
|---|---|
| Redirect URI validated by base domain only (`extractBaseDomain`) | Exact match against registered redirect URIs; loopback port flexibility only for native clients per OAuth for native apps |
| PKCE parameters ignored | Verify S256 when sent; require it for public and self-registered clients; hand-created confidential clients may omit it for compatibility |
| Login posts credentials to an upstream `auth_server_url` | Authenticate against the local configured auth collection through PB's auth API, including MFA/OTP flows it supports; upstream login stays an optional adapter for customjs migration |
| Client secret stored as a record field | Store confidential-client secrets as verifiable hashes; show once on creation |
| HS256 ID token signed with a shared secret | ID tokens optional; access token is the PB auth token. If ID tokens remain, sign with a dedicated key, never the PB token secret |
| Fixed collection names | Namespaced, migration-provisioned private collections, never MCP-exposed |

Code records bind client, redirect URI, PKCE challenge, authenticated record/collection and expiry. Codes are short-lived, single-use and stored hashed; consumption is atomic on the supported database. Token exchange revalidates account eligibility and token-key revocation before minting a PB token. Protect cookie-based authorization submissions against CSRF, keep PB tokens out of redirect/query URLs, and never log credentials, codes or tokens. No user creation as a side effect of login.

Refresh tokens are not in v1. Document reauthentication on expiry and allow a separate, configurable token lifetime for MCP-issued tokens so remote clients do not silently stop weekly.

### Full-session reuse caveats

Full PB session reuse is intentional, with consequences that must be documented for operators:

- Tokens issued to an MCP client work against the entire PB REST API, not just /mcp.
- The only revocation is rotating the user's token key, which also logs out that user's browser and every other client. Per-client revocation is not possible in v1.
- The fork's identity scopes are not MCP grants. Do not claim MCP OAuth-profile conformance where audience binding (resource indicators) is unmet.

External mode requires an explicit supported mapping/verification integration, never arbitrary third-party bearer-token acceptance.

## 4. Standard collection exposure manifest

A versioned JSON contract is canonical; Go configuration builds the same normalized structure. A schema file enables validation and editor completion. Keep sensitive deployment settings and executable policy callbacks outside the portable manifest. This replaces customjs's hardcoded `collectionOrder` and `collectionPresets`; preset concepts (title fields, summary fields, URL field, query fields, expand) become manifest fields.

Example:

```json
{
  "schemaVersion": "1",
  "toolMode": "per_collection",
  "maxGeneratedTools": 40,
  "collections": [
    {
      "collection": "properties",
      "alias": "properties",
      "description": "Property records available to the current user",
      "operations": ["describe", "search", "fetch", "create", "update"],
      "presentation": {
        "titleFields": ["address1", "city", "state", "zip"],
        "urlField": "url",
        "summaryFields": ["address1", "city", "state", "price", "status"]
      },
      "fields": {
        "read": ["id", "address1", "city", "state", "zip", "price", "status", "owner"],
        "create": ["address1", "city", "state", "zip", "price", "status"],
        "update": ["price", "status"]
      },
      "search": {
        "textFields": ["address1", "city"],
        "filters": {"status": ["eq", "in"], "price": ["eq", "gte", "lte"]},
        "sortFields": ["address1", "price"],
        "defaultSort": ["id"],
        "defaultLimit": 20,
        "maxLimit": 50
      },
      "relations": {
        "owner": {"target": "users", "fields": ["id", "name"], "maxDepth": 1}
      },
      "policy": "authenticated"
    }
  ]
}
```

Relations are explicit read projections, not implicit permission to search the target collection. Relation targets require host visibility and target ViewRule checks (as customjs's `accessCheckedExpandFetch` does), and expose only configured fields.

Manifest rules:

- Unlisted collections and operations are unavailable. Empty manifest exposes no data. Auth/system/internal collections require deliberate opt-in; module OAuth and chat storage stay inaccessible.
- Operations are `describe`, `search`, `fetch`, `create`, `update`, `delete`. `delete` must be listed explicitly and is a write (needs AllowWrites, policy and PB approval like create/update).
- Validate aliases, tool-name collisions, collection existence, field names/types, operators and relation targets at startup. Resolve stable collection IDs internally. On rename, revalidate; never silently redirect a name to a different collection. customjs's skip-and-log for missing collections remains available as an explicit `optional: true` per collection.
- Read, create and update fields are independent allowlists. Reject disallowed input keys; project outputs after PB serialization. Password/token/secret/hidden fields are never automatically exposed.
- Filters and sorts may use only declared fields/operators. No arbitrary PB filter expression by default.
- Bound result sizes, filter complexity, expand depth and response bytes. File writes are rejected in v1; protected file reads are custom tools (as customjs's `fetch_contract_pdf`).
- `toolMode` is `per_collection` (search_<alias>, fetch_<alias>, …) or `generic` (single search/fetch/create/update/delete tools taking a collection argument), both as in customjs. If per-collection mode would exceed `maxGeneratedTools`, startup fails with a message suggesting generic mode.
- Global AllowWrites defaults false. A write requires that flag, a declared collection operation, any configured policy, and PB approval.

### Built-in policies

Policy names reference either built-ins or host-registered callbacks:

- `authenticated`: any accepted token (section 3).
- `collection:<name>`: caller's auth record belongs to that auth collection.
- `field:<name>:<value>`: caller's auth record has that value in a select/text field, for apps like customjs that use `role` (replaces the `mcp_write_roles` env and hardcoded `users` collection).
- `superuser`: only when superusers are accepted.
- Any other name must be registered by the host; unknown names fail startup.

Publish only caller-visible descriptions, field schemas and operation summaries. Never return raw PB rule expressions, internal secrets, hidden collection names or policy implementation details.

## 5. Effective access and discovery

For each operation, effective access is the intersection of:

```text
accepted token collection (section 3)
AND manifest exposure
AND host instance visibility
AND authenticated principal / optional application policy
AND PocketBase operation rule for the actual request and record
```

Nil PB rules follow PB semantics, including superuser behavior; the manifest still applies. Search may return zero records even when the operation is advertised. A create/update permission may depend on input values.

Built-in, read-only discovery tools:

- `describe_access`: caller-visible collections, aliases, operation states, limits, custom tools and manifest revision.
- `describe_collection`: safe input/output schema, supported search/filter/sort/expand arguments and per-operation fields for one exposed collection (extends customjs's describe_collection).

Operation state is unavailable, available or conditional, with non-sensitive reasons. Discovery and invocation share one policy engine; direct calls to hidden tool names are rejected again. customjs's tools/list rule-based filtering (hide collections where both List and View rules are nil) is kept as one input to this engine.

## 6. Record execution and request semantics

Keep customjs's split, generalized behind the adapter:

- **Reads (search/fetch):** direct queries with the PB rule compiled into the query, as customjs `handleSearch`/`handleFetch` do (ListRule as a WHERE clause before paging so totals reflect visible records; `CanAccessRecord` with ViewRule for fetch; same not-found message for missing and forbidden; access-checked expand). This matches PB's list API semantics but does not run `OnRecordsListRequest`/`OnRecordViewRequest` hooks; document that, and offer per-collection `readVia: "api"` that dispatches through the REST handler for hosts that depend on read hooks.
- **Writes:** in-process subrequest through the PB router (customjs `apiSubrequest`), preserving validation, API rules, serialization and record request hooks. Never `App.Save` as a substitute.

Required fixes when porting:

- Replace `CountCol("_rowid_")` with the host's own count semantics (`_rowid_` does not exist on PostgreSQL).
- Add fork instance visibility to reads; rule compilation alone does not apply it.
- Build the router handler per server, after route registration completes, not in a package global.
- Forward only an allowlisted set of headers; trust forwarded headers only under the host's proxy trust configuration (customjs copies X-Forwarded-* unconditionally).
- Never dispatch to /mcp or OAuth routes; restrict subrequests to generated record endpoints.
- Apply manifest output projection to write responses too.

Stable sorting includes an allowed ID tie-breaker. Do not silently fall back to an unguarded RecordQuery.

## 7. Custom tool registry

Typed Go registration plus a portable descriptor. Descriptors can be part of the manifest; handler code is registered by the host, not loaded from config.

```text
ToolSpec
  Name, Title, Description, InputSchema, OutputSchema, Annotations
  Policy reference
  Effects: read or write; external access; idempotent or not
  Channels: mcp, chat, or both; execution location: server or editor (chat plan)
  Timeout and response limit (bounded by server maximum)
  Handler(context, CallContext, validated input) -> structured result

CallContext
  verified Principal
  policy-enforcing collection access facade
  request ID, cancellation, logger and audit emitter
```

### In-house schema validation

No JSON Schema library. `schema/` implements and documents a strict subset used by generated and custom tools: `type` (object, array, string, number, integer, boolean, null), `properties`, `required`, `additionalProperties: false`, `enum`, `const`, `items`, `minimum`/`maximum`, `minLength`/`maxLength`, `minItems`/`maxItems`, `pattern` (Go RE2), `format` for date-time/email/uri, and `description`. Registration rejects schemas using keywords outside the subset, so validation is never silently skipped. Outputs are validated against the declared output schema in tests and optionally at runtime.

Malformed arguments produce JSON-RPC invalid-params errors; execution failures produce sanitized tool errors (customjs's `writeToolError`/`writeInternalToolError` split); authentication failures keep correct HTTP status and headers. Results carry both `structuredContent` and a text rendering, and may include embedded resources (customjs's PDF pattern).

Write effects require AllowWrites plus tool policy; annotations alone never enforce permission. Go handlers are trusted application code, not sandboxed; raw App/database access through host closures can bypass the facade, and that boundary is documented. External fetch tools opt into destination policies, time/size limits and redirect checks. Writes are never automatically retried.

## 8. Protocol, transport and operations

### Protocol implementation

Generalize customjs `mcp/routes.go` into `protocol/` and `server.go`:

Keep:

- Stateless Streamable HTTP: POST only; GET and DELETE return 405 with `Allow: POST` (no server-initiated stream, no sessions).
- JSON-RPC 2.0 with the standard error codes; notifications and client responses get 202.
- Methods: `initialize`, `ping`, `tools/list`, `tools/call`; empty answers for `resources/list`, `resources/templates/list` and `prompts/list` without advertising those capabilities.
- Version negotiation: echo a supported requested version, otherwise answer with the latest supported; reject an unsupported `MCP-Protocol-Version` header with 400.
- Origin allowlist check before processing.
- Path aliases (customjs's `/sse`) as configurable compatibility paths.

Change:

- Supported protocol versions are a table in `protocol/` updated deliberately with tests for each version's differences, not a hardcoded list buried in config. Add newer spec versions only after implementing what they require.
- Batch requests: keep rejecting unless a supported version requires them.
- Server name, title, version and instructions come from config, not customjs constants.
- Request body size limit and JSON depth limit before parsing (customjs uses unbounded `io.ReadAll`).
- tools/list is computed per request for the caller and never cached across principals.
- 401 responses carry the `WWW-Authenticate` header from section 3 when OAuth is enabled.

### Operations

Configure allowed origins, request/response limits, bounded concurrency, per-principal rate limits and timeouts. Keep tokens and raw record bodies out of routine logs.

Register during application setup, validate DB-dependent configuration after bootstrap, and finalize routes during OnServe (customjs's MCP init already runs on serve). Bind only owned collection-change and shutdown hooks. A failed validation must not leave a partially usable server.

Use immutable, versioned registry snapshots, rebuilt and swapped on collection changes as customjs `rebuildRegistry` does, but per server instead of a package global. The fork adapter must additionally handle shared-database and instance-scope changes via a proven revision signal or bounded polling. Recheck visibility and schema validity on invocation. Fail closed on stale/missing fields. A bad reload keeps the previous valid config.

Structured audit callbacks for principal ID, tool, collection/action, outcome, duration and request ID. Payload capture is opt-in and redacted. Permission/account revocation affects subsequent calls; no unbounded positive authorization cache.

Spec references (behavior targets, not dependencies): [MCP specification](https://modelcontextprotocol.io/specification), its transports and authorization sections.

## 9. Implementation milestones and acceptance gates

1. **Port and fixtures.** Pin stock/fork/toolchain versions. Move customjs protocol, registry, search/fetch and write subrequest into the package with per-server state. Build each adapter in isolation (no shared go.work). Gate: both fixtures compile; a real client calls one protected collection tool; customjs's existing `mcp_test.go` scenarios pass against the stock fixture.
2. **Manifest and collection tools.** JSON manifest, built-in policies, accepted-collection rules, discovery tools, field constraints, generated CRUD in both tool modes, `_rowid_` fix, fork instance visibility. Gate: read-only default; denied users, conditional rules, field limits, relation checks, paginated totals and write-hook parity pass on both hosts; superuser and other-collection tokens rejected unless configured.
3. **OAuth ownership and stock OAuth port.** Mode resolution, startup diagnostics, duplicate guards; port and harden customjs OAuth; protected-resource metadata, `WWW-Authenticate`, PKCE, and the chosen self-registration method. Gate: (a) ChatGPT connects to the stock fixture with a hand-created client exactly as customjs does today; (b) a client that needs self-registration (for example Claude Code) connects with no hand setup; (c) redirect mismatch, replayed/expired code, bad verifier, revoked token key and incomplete MFA are rejected.
4. **Fork OAuth.** In pb-postgress itself: add protected-resource metadata support, root-level discovery for the configured collection, and the same self-registration method. In the module: native mode reuses it and adds zero OAuth routes/storage/jobs/hooks. Gate: same (a)–(c) against the fork fixture.
5. **Custom tools.** Typed tool API, schema subset validator, policy/write/effect gates, audit callbacks and runtime limits. Port `fetch_contract_pdf` as the reference custom tool. Gate: a read tool and a write tool demonstrate permission enforcement and cancellation.
6. **Release hardening.** Schema/scope invalidation, concurrency isolation, two apps in one process, clean shutdown, rate limits. Publish stock/fork examples, a customjs migration guide (section 11) and a tested support matrix. Add PostgreSQL/MySQL runs if both fork dialects are advertised.
7. **Chat v1.1 and v1.2** per the chat plan's delivery sequence; chat step 2 depends on milestone 2 here, chat proposals depend on milestone 5.
8. **Later:** stdio/REST bridge, resources/prompts, file tooling, idempotency support, multi-instance chat.

Keep checks behavioral and focused on actual risk: no tool-list leakage across principals, ChatGPT baseline not regressed, no duplicate native OAuth, actual record rules/hooks, dialect behavior and token reuse.

## 10. Suggested public usage

Illustrative API, not compilable code yet. Collection names are examples; there is no default.

```go
host := pbpostgress.New(app) // stock apps use pocketbase.New(app)
server, err := pbmcp.Register(host, pbmcp.Config{
    Path:         "/mcp",
    AliasPaths:   []string{"/sse"},
    PublicURL:    "https://example.com",
    AllowedOrigins: []string{"https://chatgpt.com"},
    Auth: pbmcp.AuthConfig{
        Collection: "users",
        // Collections defaults to []string{Collection}; AllowSuperusers defaults false.
        OAuth: pbmcp.OAuthConfig{Enabled: true, Mode: "auto"},
    },
    Manifest:    manifest,
    AllowWrites: false,
    Tools:       domainTools,
    Policies:    domainPolicies,
})
```

## 11. customjs migration

customjs becomes the first consumer. Migration preserves what ChatGPT is configured against:

- Same public paths (`/mcp`, `/sse`, `/oauth/login`, `/oauth/token`, `/oauth/userinfo`, root discovery) via configuration.
- Existing `oauth_apps` client IDs and secrets keep working (secrets rehashed on migration).
- `collectionPresets` and `collectionOrder` become a manifest; `mcp_tool_mode` becomes `toolMode`; `mcp_write_roles` becomes a `field:role:<value>` policy.
- Upstream `auth_server_url` login remains available as an adapter until moved to local auth.
- Verify with the live ChatGPT connector on a staging copy before switching production.

## 12. Frontend chat integration

Optional application chat API and dependency-free browser client, reusing the existing agent-panel/agent-page Web Components used by List Pocket, Event Pocket and Static Dealmakers. Do not put PB/provider network logic into that rendering package.

Chat authenticates with the frontend's existing PB session, under the same accepted-collection rules, and invokes the same collection/custom-tool policy pipeline as MCP. Provider credentials are separate from stockoauth and stay server-side. Provider adapters are hand-written over net/http, no vendor SDKs.

Details are in [MCP-CHAT-INTERFACE-PLAN.md](/Volumes/mini512/Components/Pocketbase-MCP/MCP-CHAT-INTERFACE-PLAN.md). Chat ships as v1.1 (read) and v1.2 (write), after the core MCP release. No existing frontend is migrated automatically.

## 13. Source anchors

- customjs protocol handler: [routes.go:27](/Volumes/mini512/Go%20Projects/customjs/mcp/routes.go:27); JSON-RPC and tool types: [types.go](/Volumes/mini512/Go%20Projects/customjs/mcp/types.go:1); protocol versions and presets: [config.go](/Volumes/mini512/Go%20Projects/customjs/mcp/config.go:1).
- customjs schema registry and rebuild hooks: [registry.go:19](/Volumes/mini512/Go%20Projects/customjs/mcp/registry.go:19).
- customjs rule-compiled reads: [search.go:329](/Volumes/mini512/Go%20Projects/customjs/mcp/search.go:329), fetch at line 419, access-checked expand at line 459; portability hazard `CountCol("_rowid_")` at line 377.
- customjs in-process write subrequest: [write.go:273](/Volumes/mini512/Go%20Projects/customjs/mcp/write.go:273); hardcoded role gate at line 80.
- customjs OAuth flow and discovery: [oauth.go:84](/Volumes/mini512/Go%20Projects/customjs/authentication/oauth.go:84) (domain-only redirect check), discovery at line 450, routes at line 492.
- Fork-owned OAuth route registration: [record_auth_oauth2_server.go:41](/Volumes/mini512/Go%20Projects/pb-postgress/apis/record_auth_oauth2_server.go:41); S256 enforcement at line 201; collection-scoped OIDC discovery at line 56.
- Fork instance visibility: [instance.go:113](/Volumes/mini512/Go%20Projects/pb-postgress/core/instance.go:113), plus auth middleware visibility at [middlewares.go:203](/Volumes/mini512/Go%20Projects/pb-postgress/apis/middlewares.go:203).
- Prior inventory: [MCP-POCKETBASE-INTEGRATION-REVIEW.md](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md).
- Prior assessment, with full-session reuse accepted: [MCP-POCKETBASE-INTEGRATION-DOCUMENT-REVIEW.md](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-DOCUMENT-REVIEW.md).
