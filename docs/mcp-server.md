# MCP server

How the MCP server works, as built. For deployment steps see
[operations.md](operations.md); for open work see [roadmap.md](roadmap.md).

## Contents

1. [Design rules](#1-design-rules)
2. [Modules and hosts](#2-modules-and-hosts)
3. [Sign-in and OAuth](#3-sign-in-and-oauth)
4. [The manifest](#4-the-manifest)
5. [Who can do what](#5-who-can-do-what)
6. [How reads and writes run](#6-how-reads-and-writes-run)
7. [Custom tools](#7-custom-tools)
8. [Protocol and transport](#8-protocol-and-transport)
9. [Audit and logging](#9-audit-and-logging)

## 1. Design rules

- **Full session reuse.** A user signs in as themselves and the MCP client
  gets a normal PocketBase token with that user's permissions. This is
  deliberate. See [operations.md](operations.md#full-session-reuse) for what
  it means for revocation.
- **Explicit exposure.** Only collections and operations listed in the
  manifest exist. An empty manifest exposes nothing. Writes need
  `Config.AllowWrites` (off by default).
- **Never wider than PocketBase.** The module can hide things PocketBase
  would allow, never allow things PocketBase would refuse. Superusers only
  get access when `Auth.AllowSuperusers` is set, and even then only to what
  the manifest exposes.
- **No added libraries.** The root module uses only the Go standard library.
  Adapters use their PocketBase module and what it already pulls in. There
  is no MCP SDK, OAuth library, JSON Schema library or vendor model SDK.
  The design started from the customjs app's hand-written MCP server, which
  already works with ChatGPT.
- **Nothing advertised that isn't implemented.** Spec features the module
  doesn't support are not announced to clients.

## 2. Modules and hosts

The root module (`github.com/compdani/pocketbase-mcp`, package `pbmcp`)
contains the protocol, manifest, policy engine, custom-tool registry and
schema validator. It talks to PocketBase only through a small host interface
(`host.go`): authenticate a token, look up collections, list what an instance
can see, read records, and dispatch writes.

Two adapters implement that interface:

| Adapter | Host | OAuth |
|---|---|---|
| `adapters/pocketbase` | Stock PocketBase v0.40.1 | Installs `auth/stockoauth` (module mode) |
| `adapters/pbpostgress` | The pb-postgress fork | Reuses the fork's own OAuth2 server (native mode) |

The fork declares the same module path as stock PocketBase
(`github.com/pocketbase/pocketbase`), so an app picks the fork with a
`replace` directive in its own `go.mod`. **Never create a `go.work` that
includes both adapters or both examples**: one binary can't contain two
implementations of the same module path.

## 3. Sign-in and OAuth

### Which tokens `/mcp` accepts

`Auth.Collections` lists the auth collections whose tokens are accepted (by
`/mcp` and by chat). It defaults to the OAuth collection (`Auth.Collection`)
and is required when OAuth is off. Tokens from other collections get 401.
`_superusers` tokens are refused unless `Auth.AllowSuperusers` is set.

Every request re-checks the token through PocketBase's normal auth path.

### OAuth modes

`OAuthConfig.Mode` picks who owns sign-in:

| Mode | Stock adapter | Fork adapter |
|---|---|---|
| `auto` | Module OAuth when enabled | Fork's native OAuth; a disabled collection config is a startup error |
| `native` | Startup error | Fork's native OAuth |
| `module` | Installs `auth/stockoauth` | Refused (would duplicate the fork's server) |
| `external` | A configured external issuer | Same |
| `off` | Bearer tokens only | Bearer tokens only |

Exactly one owner is chosen per mount and auth collection. The choice is
validated before any route is added, and a failed validation leaves nothing
half-installed. A startup log line (without credentials) reports the owner,
endpoints, accepted collections and manifest revision.

### What every OAuth mode serves

- `401` from `/mcp` carries
  `WWW-Authenticate: Bearer resource_metadata="<base>/.well-known/oauth-protected-resource<mcp path>"`.
- `/.well-known/oauth-protected-resource` (plus the MCP-path form) names the
  issuer.
- Authorization-server metadata advertises S256 PKCE.

### ChatGPT compatibility

ChatGPT connects the way it does with customjs today: root-level discovery
documents (`/.well-known/oauth-authorization-server` and
`/.well-known/openid-configuration`, plus `/sse/...` copies), a client
created by hand with its ID and secret typed into ChatGPT
(`client_secret_post`), and an authorization-code flow that returns a
PocketBase token. This is kept working on purpose.

### Self-registration

Clients like Claude Code, Cursor and VS Code can't take a hand-entered
client ID, so both hosts support Dynamic Client Registration. Self-registered
clients are public, must use S256 PKCE, must use exact redirect URIs, and are
rate-limited per IP per hour (see
[operations.md](operations.md#self-registration-rate-limits)). Client ID
Metadata Documents are not implemented.

### Stock OAuth (`auth/stockoauth`)

A hardened port of customjs's OAuth flow:

| customjs | stockoauth |
|---|---|
| Redirect URIs checked by base domain | Exact match (loopback port flexibility for native apps only) |
| PKCE ignored | S256 verified when sent; required for public and self-registered clients |
| Login posted to an upstream server | Signs in against the local auth collection, including PocketBase's MFA/OTP |
| Client secret stored as a field | Stored as a hash, shown once |
| HS256 ID token | No ID token; the access token is the PocketBase token |

Codes are short-lived, single-use, stored hashed and bound to the client,
redirect URI, PKCE challenge and auth collection. Token exchange re-checks the
account and its token key. Default paths are `/oauth/login`, `/oauth/token`,
`/oauth/userinfo` and `/oauth/register` (configurable;
`DisableRegistration` turns self-registration off). There are no refresh
tokens; `TokenDuration` sets a separate lifetime for tokens issued here.

### Fork OAuth (pb-postgress, branch `oauth-mcp-clients`)

In native mode the adapter adds only the protected-resource document. Every
other OAuth feature lives in the fork:

- **Self-registration** per auth collection: `registration.enabled` (off by
  default), `allowedRedirectHosts`, `rateLimit` (0 means 10 per hour).
  Registered clients are stored in `_oauth2ServerClients` with a `dcr_` ID
  prefix and can be revoked by superusers. They always see the sign-in form
  with an "unverified app" warning; `prompt=none` returns
  `interaction_required`.
- **Public clients** (`public: true`) have no secret and require PKCE.
- **Hashed secrets.** Confidential client secrets are bcrypt-hashed (`$pb$`
  marker) and never shown again. The migration that hashes existing secrets
  can't be undone.
- **RS256 ID tokens**, signed with a per-collection key published at the
  collection's `/jwks` endpoint. HS256 is gone.
- **Discovery** at the collection issuer; `rootDiscovery` also serves the
  root documents for exactly one collection.
- **Sign-in app URL** (`issuerURL`). Apps sharing one database share the
  collection settings, so this makes all of them report one issuer and send
  sign-in to one main app (one session, one passkey domain).

The issuer the adapter announces is
`<base>/api/collections/<collection>/oauth2-server`, where `<base>` is the
first one set of: `OAuthConfig.IssuerBaseURL`, the collection's sign-in app
URL, the app's Application URL, the request's base URL.

### Choosing the auth collection

`Auth.Collection` names the auth collection that owns sign-in (`users`,
`staff`, …). There is no default and no guessing. It must exist and be an
auth collection, and `_superusers` is refused for module OAuth. One OAuth
collection per mount; use separate mounts for several identity collections.
The auth collection doesn't need to be exposed in the manifest.

## 4. The manifest

A versioned JSON document (`manifest.schema.json`) that lists what is
exposed. Go code can build the same structure.

```json
{
  "schemaVersion": "1",
  "toolMode": "per_collection",
  "collections": [{
    "collection": "properties",
    "description": "Property records available to the current user",
    "operations": ["describe", "search", "fetch", "create", "update"],
    "presentation": {"titleFields": ["address1", "city"], "summaryFields": ["address1", "price", "status"]},
    "fields": {
      "read":   ["id", "address1", "city", "price", "status", "owner"],
      "create": ["address1", "city", "price", "status"],
      "update": ["price", "status"]
    },
    "search": {
      "textFields": ["address1", "city"],
      "filters": {"status": ["eq", "in"], "price": ["gte", "lte"]},
      "sortFields": ["price"], "defaultLimit": 20, "maxLimit": 50
    },
    "relations": {"owner": {"target": "users", "fields": ["id", "name"], "maxDepth": 1}},
    "policy": "authenticated"
  }]
}
```

Rules:

- **Operations** are `describe`, `search`, `fetch`, `create`, `update` and
  `delete`. Unlisted ones don't exist. `delete` must be listed explicitly.
- **Fields** are separate allowlists for reading, creating and updating.
  Unknown input keys are refused. Password, token and hidden fields are never
  exposed automatically. File fields can't be written.
- **Search** only filters and sorts on declared fields and operators. There
  is no free-form PocketBase filter.
- **Relations** are read projections: the target's View rule still applies,
  and only the listed fields come back.
- **Tool mode** is `per_collection` (`search_<alias>`, `create_<alias>`, …)
  or `generic` (one `search`, `create`, … tool taking a `collection`
  argument). Startup fails if per-collection mode would exceed
  `maxGeneratedTools`.
- **Startup validation** checks names, aliases, tool-name collisions,
  fields, operators and relation targets. A missing collection is an error
  unless it is marked `optional: true`.

### Policies

A collection or tool can name a policy:

| Policy | Passes when |
|---|---|
| `authenticated` | Any accepted token |
| `collection:<name>` | The caller belongs to that auth collection |
| `field:<name>:<value>` | The caller's record has that value in a field (for example `field:role:admin`) |
| `superuser` | The caller is a superuser (only if superusers are accepted) |
| anything else | A policy the host registered in `Config.Policies`; unknown names fail startup |

## 5. Who can do what

Every operation is allowed only if all of these allow it:

```text
accepted token collection
AND manifest exposure
AND host instance visibility (the fork's per-instance collection scope)
AND the configured policy
AND the PocketBase record rule for this request and record
```

A search can return nothing even when it is offered, and whether a create is
allowed can depend on its values.

Two built-in read-only tools describe the caller's view:

- `describe_access`: visible collections, operation states (available,
  conditional or unavailable), limits, custom tools and manifest revision.
- `describe_collection`: input and output schemas, search options and fields
  for one collection.

Listing and calling share one gate: a tool hidden from a user is missing from
`tools/list` and refused when called by name. The list is computed per
request and never shared between users. Descriptions never include raw rule
expressions, secrets or hidden collection names.

## 6. How reads and writes run

- **Reads** (search, fetch) query the database directly, with the
  collection's List or View rule compiled into the query, so totals and
  paging only count visible records. Missing and forbidden records give the
  same "not found". Relations are expanded with the target's View rule
  checked. These reads don't fire PocketBase's list/view request hooks.
- **Writes** go through PocketBase's own record API as an in-process
  subrequest carrying the caller's token, so validation, rules, request hooks
  and serialization all apply. Results are projected to the allowed read
  fields.

Counting uses the host's own semantics (customjs's `_rowid_` doesn't exist on
Postgres). Sorting always ends with the record ID so paging is stable. A write
subrequest carries only the caller's `Authorization` and a JSON content type,
and can only reach record endpoints, never `/mcp` or OAuth routes.

## 7. Custom tools

Custom tools are typed Go handlers registered next to the generated tools in
`Config.CustomTools`:

```go
cfg.CustomTools = []pbmcp.ToolSpec{{
    Name:        "invoice_total",
    Title:       "Invoice total",
    Description: "Sum the line items of one invoice the caller may view.",
    Policy:      "authenticated",
    Effects:     pbmcp.Effects{Kind: "read", Idempotent: true},
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1}},
        "required": []string{"id"}, "additionalProperties": false,
    },
    Handler: func(ctx context.Context, call pbmcp.CallContext, args map[string]any) (any, error) {
        // args are already validated against InputSchema.
        return call.Access.Fetch(ctx, "invoices", args["id"].(string))
    },
}}
```

The handler gets a `CallContext` with the verified `Principal` (including the
caller's token, never logged), a collection access facade (`call.Access`), the
request ID, a cancellable context, a logger and the audit emitter. The
facade's search, fetch, create, update and delete go through exactly the same
manifest, policy, field and record-rule checks as the generated tools.

A handler returns a plain value (sent as `structuredContent` plus a JSON text
rendering) or a `protocol.ToolResult` to add embedded resources (base64
`blob` plus `mimeType`) and resource links.

### Gates

- **Effects.** A write tool (`Effects.Kind: "write"`) also needs
  `Config.AllowWrites`. A read tool's facade refuses create, update and delete
  ("This tool is read-only."), whatever the other settings. Annotations such
  as `readOnlyHint` are hints for clients and never grant or deny anything.
- **Channels.** `mcp` (default), `chat` or `both`. A `chat` tool never
  appears on MCP.
- **Execution location.** `server` (default) or `editor`. Editor tools are
  chat-only, have no handler and always need review; see
  [chat.md](chat.md#6-editor-tools).
- **Approval.** `Approval: "auto"` lets a chat write tool run without review,
  but only if the host also sets `ChatOptions.AutoApprove`.
- **Limits.** A tool's `Timeout` and `MaxResponseBytes` may not exceed the
  server maximums. A timeout cancels the handler and returns a tool error; an
  oversized response is refused, never truncated. Writes are never retried.
- **Names** must not collide with generated tools, the built-in tools or each
  other; a collision fails startup.

### Errors

- Bad arguments return JSON-RPC `-32602` with a JSON pointer and reason, for
  example `/rows/0/qty: minimum violated`.
- A handler error becomes a generic tool error; details go to the server log
  only. `pbmcp.ToolError` values are shown as written, since they are
  user-safe by construction.
- Facade refusals are `AccessError` values: `not_found` (missing, hidden or
  out of scope, all with the same message), `denied` or `invalid`. The kind
  only feeds the audit outcome.
- Authentication failures keep their HTTP status and headers.

### Schema subset

Input and output schemas are checked by the in-house validator in `schema/`.
A schema using any other keyword is refused at registration, so validation is
never silently skipped.

| Keyword | Notes |
|---|---|
| `type` | `object`, `array`, `string`, `number`, `integer`, `boolean`, `null`, or a list of these |
| `properties`, `required`, `additionalProperties` | `additionalProperties` is `false` or a schema |
| `enum`, `const` | Compared after JSON normalization |
| `items` | One schema for all items |
| `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum` | Numeric bounds |
| `minLength`, `maxLength` | Counted in characters (runes) |
| `minItems`, `maxItems` | Array size |
| `pattern` | Go RE2 |
| `format` | `date-time`, `email`, `uri` |
| `title`, `description`, `default` | Annotations only |

Outputs are validated against `OutputSchema` at runtime by default
(`Config.DisableOutputValidation` turns that off; keep it on in tests).

### Trust boundary

Handlers are trusted application code, not sandboxed plugins. A handler that
closes over the raw PocketBase `App` or a database handle can bypass the
facade. Keep raw access minimal and prefer the facade for records.

### Reference tool: `fetch_contract_pdf`

`ContractPDFTool(app, collection, field, maxBytes)` (in each adapter) fetches
a record through the facade, so the View rule and field allowlist apply, then
reads its protected file field through PocketBase's filesystem with a size
limit and returns it as an embedded `application/pdf` resource. Both examples
register it on an owner-scoped `contracts` collection; `MCP_SEED_DEMO=true`
creates a demo user and contract.

## 8. Protocol and transport

- **Stateless Streamable HTTP.** POST only; GET and DELETE return 405 with
  `Allow: POST`. No sessions and no server-initiated streams.
- **JSON-RPC 2.0** with the standard error codes. Notifications and client
  responses get 202. Batches are refused.
- **Methods:** `initialize`, `ping`, `tools/list`, `tools/call`.
  `resources/list`, `resources/templates/list` and `prompts/list` answer
  empty without advertising those capabilities.
- **Versions** come from a table in `protocol/`. A supported requested
  version is echoed, otherwise the latest is used. An unsupported
  `MCP-Protocol-Version` header gets 400.
- **Origins** are checked against `Config.AllowedOrigins` before anything
  else. The request body is bounded by `Config.MaxBodyBytes` before parsing.
- **Paths:** `Config.Path` (default `/mcp`) plus `AliasPaths` such as
  customjs's `/sse`.
- **Server info** (name, title, version, instructions) comes from config.

## 9. Audit and logging

`Config.AuditFunc` receives one `AuditEvent` per tool call (generated and
custom tools, MCP and chat): principal, tool, effect, collection and action,
outcome, duration, request ID and channel. Outcomes are `ok`, `denied`,
`invalid`, `not_found`, `error`, `timeout`, and for chat also `proposed`.

No arguments are recorded by default. `Config.AuditIncludePayload` adds a
redacted copy: keys naming tokens, passwords, secrets or authorization are
replaced and long strings are truncated. Tokens and raw record bodies are
never written to routine logs.
