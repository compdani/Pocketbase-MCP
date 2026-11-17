# PocketBase MCP module

A reusable Go module that exposes selected PocketBase collections as MCP
tools (stateless Streamable HTTP, JSON-RPC 2.0 over POST), plus a typed
registry for custom Go tools. The root module
(`github.com/compdani/pocketbase-mcp`) is PocketBase-independent and uses
only the Go standard library; host adapters live in
[adapters/pocketbase](/Volumes/mini512/Components/Pocketbase-MCP/adapters/pocketbase)
(stock PocketBase) and
[adapters/pbpostgress](/Volumes/mini512/Components/Pocketbase-MCP/adapters/pbpostgress)
(pb-postgress fork). Collection exposure is declarative (manifest),
read-only by default, and every operation is the intersection of the
accepted auth collection, the manifest, host instance visibility, the
configured policy, and the PocketBase record rules. See
[MCP-SERVER-MODULE-PLAN.md](/Volumes/mini512/Components/Pocketbase-MCP/MCP-SERVER-MODULE-PLAN.md)
for the full contract.

Runnable fixtures: [examples/stock](/Volumes/mini512/Components/Pocketbase-MCP/examples/stock)
and [examples/fork](/Volumes/mini512/Components/Pocketbase-MCP/examples/fork)
(each is its own Go module; build and run them in isolation — never via a
shared `go.work`).

## Custom tools

Custom tools are typed Go handlers registered alongside the generated
collection tools. Register them in `Config.CustomTools`:

```go
cfg.CustomTools = []pbmcp.ToolSpec{{
    Name:        "invoice_total",
    Title:       "Invoice total",
    Description: "Sum the line items of one invoice the caller may view.",
    Policy:      "authenticated", // built-in or host-registered policy
    Effects:     pbmcp.Effects{Kind: "read", Idempotent: true},
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "id": map[string]any{"type": "string", "minLength": 1},
        },
        "required": []string{"id"}, "additionalProperties": false,
    },
    OutputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{"total": map[string]any{"type": "number"}},
        "required": []string{"total"},
    },
    Handler: func(ctx context.Context, call pbmcp.CallContext, args map[string]any) (any, error) {
        // args are already validated against InputSchema.
        return call.Access.Fetch(ctx, "invoices", args["id"].(string))
    },
}}
```

Handlers receive a `CallContext`: the verified `Principal` (including the
original caller token — never logged), a policy-enforcing collection
access facade (`call.Access`), the request ID, a context that is
cancelled on timeout, a logger, and the audit emitter. The facade's
search/fetch/create/update/delete go through exactly the same manifest,
policy, field-allowlist and PocketBase-rule path as the generated tools,
and writes dispatch as subrequests carrying the caller's token, so
record rules, validation and request hooks apply unchanged.

A handler returns either a plain value (sent as `structuredContent` with
a JSON text rendering), or a `protocol.ToolResult` to add content items
such as embedded resources (`protocol.EmbeddedResourceContent`, base64
`blob` + `mimeType`) and resource links (`protocol.ResourceLink`).

### Schema subset

Input and output schemas use the in-house validator in
[schema/](/Volumes/mini512/Components/Pocketbase-MCP/schema) (standard
library only; no JSON Schema library). Supported keywords:

| Keyword | Notes |
|---|---|
| `type` | `object`, `array`, `string`, `number`, `integer`, `boolean`, `null`, or an array of these |
| `properties`, `required`, `additionalProperties` | `additionalProperties` is `false` or a schema |
| `enum`, `const` | compared after JSON normalization |
| `items` | a single schema for all items |
| `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum` | numeric bounds |
| `minLength`, `maxLength` | counted in runes |
| `minItems`, `maxItems` | array size bounds |
| `pattern` | Go RE2 |
| `format` | `date-time`, `email`, `uri` |
| `title`, `description`, `default` | annotations only, never validated |

Registration **rejects** any schema using a keyword outside this subset,
so validation is never silently skipped. Validation errors carry a JSON
pointer path and a reason (for example `/rows/0/qty: minimum violated`)
and are returned as JSON-RPC `-32602`. Inputs are validated before the
handler runs; outputs are validated against `OutputSchema` at runtime by
default (`Config.DisableOutputValidation` turns that off; tests should
keep it on).

### Policies, effects and gates

- `tools/list` and `tools/call` share one gate: a tool hidden by policy
  is absent from `tools/list` **and** rejected on a direct call.
  `describe_access` lists only the custom tools the caller may use.
- Write-effect tools (`Effects.Kind: "write"`, or the deprecated
  `Write: true`) additionally require `Config.AllowWrites`. Annotations
  (`readOnlyHint` and friends) are hints for clients only — they never
  grant or deny permission.
- `Channels` is `mcp` (default), `chat` or `both`. Chat is not
  implemented yet; the field is carried and chat-only tools never appear
  on MCP. `ExecutionLocation` is `server`; `editor` is rejected for any
  tool that targets the MCP channel.
- Per-tool `Timeout` and `MaxResponseBytes` must not exceed the server
  maximums (`Config.MaxToolTimeout`, `Config.MaxResponseBytes`). A
  timeout cancels the handler context and returns a tool error; an
  oversized response is rejected as a tool error, never truncated.
  Writes are never automatically retried.
- Tool names must not collide with generated tools, built-ins
  (`describe_access`, `describe_collection`) or each other — collisions
  fail startup.
- Malformed arguments return JSON-RPC `-32602` with the validation
  detail. Handler failures return a sanitized `isError` tool result
  (generic message; details only in the server log), except
  `pbmcp.ToolError` values, which are user-safe by construction (facade
  not-found and PocketBase rule rejections). Authentication failures keep
  their HTTP status and headers.

### Audit

Set `Config.AuditFunc` to receive one `AuditEvent` per tool call —
generated collection tools and custom tools alike — with the principal
ID and collection, tool name, effect, collection/action where known,
outcome (`ok`, `denied`, `invalid`, `error`, `timeout`), duration and
request ID. No argument payloads are captured by default; setting
`Config.AuditIncludePayload` adds a redacted copy (keys naming tokens,
passwords or secrets are replaced, long strings truncated). Tokens,
passwords and secrets are never audited.

### Trust boundary

Custom-tool handlers are trusted application Go code, not sandboxed
plugins. The facade enforces manifest, policy and PocketBase rules, but
a handler that closes over the raw PocketBase `App` or a database handle
can bypass the facade entirely (the reference PDF tool below does this
only to read an already-authorized file). Review handlers accordingly,
keep raw access minimal, and prefer the facade for record access.

### Reference tool: `fetch_contract_pdf`

`ContractPDFTool(app, collection, field, maxBytes)` implements the
read-only `fetch_contract_pdf` custom tool: it fetches the record
through the facade (manifest, ViewRule and field allowlist apply, so
another user's record is simply not found), then reads the protected
file field through PocketBase's filesystem with a size limit and
returns it as an embedded `application/pdf` resource. Collection and
field names and the byte limit are configurable.

The code lives in the adapters —
[adapters/pocketbase/contract_pdf.go](/Volumes/mini512/Components/Pocketbase-MCP/adapters/pocketbase/contract_pdf.go)
and
[adapters/pbpostgress/contract_pdf.go](/Volumes/mini512/Components/Pocketbase-MCP/adapters/pbpostgress/contract_pdf.go)
— not in the root module, because reading the file needs the host's
PocketBase `core.App` filesystem, and the root module must stay
PocketBase-independent. Both
[examples/stock](/Volumes/mini512/Components/Pocketbase-MCP/examples/stock)
and [examples/fork](/Volumes/mini512/Components/Pocketbase-MCP/examples/fork)
register it against a seeded, owner-scoped `contracts` collection
(set `MCP_SEED_DEMO=true` for a demo user and contract record).
