# Milestone 5 — Custom tools

Date: 2026-11-17. Resumed from a previous agent's partial work; every
pre-existing change was read and verified, not trusted. No commits, no
branches, `.gitignore` untouched, no `go.work` created, `customjs` /
`pb-postgress` / plan documents unmodified. Reference (read-only):
customjs `mcp/contract_pdf.go` (URL-download variant) and `mcp/types.go`
(embedded-resource shape).

## Scope / acceptance status

Status key: **verified-as-done** = present on arrival, confirmed by
inspection + passing tests; **fixed** = present but defective, corrected
here; **newly implemented** = built in this run; **not done** = open,
with reason.

| # | Scope item / acceptance check | Status | Evidence |
|---|---|---|---|
| 1 | ToolSpec (name/title/description/schemas/annotations, policy, effects, channels, execution location, bounded timeout/size, handler) | fixed | Nil `InputSchema` crashed `schema.Compile`; `editor` now rejected only for MCP-targeted tools (chat-only editor tools are carried); name required; unknown policy/effect/channel/location fail startup. `TestValidateCustomTools` (root) |
| 2 | CallContext (principal + token, facade, request ID, cancellation, logger, audit) | verified-as-done | Facade dispatches through `canInvoke` + host Search/Fetch/Write — the generated-tool path incl. field allowlists and caller-token write subrequests |
| 3 | schema/ subset + rejection of unsupported keywords + pointer-path errors + thorough tests | fixed | Added nested-array/object, type-array, exclusive-bound, boundary and format cases (`TestNestedAndBoundaries`). Subset table below |
| 4a | Shared gate: hidden tool absent from tools/list and rejected on call | verified-as-done | `TestCustomRegistry` both adapters; `TestCustomToolGateAndAudit` (root) |
| 4b | Write tools require AllowWrites AND policy; annotations never grant | verified-as-done | `custom_write_admin` policy-denied for plain user; AllowWrites=false rebuild denies admin write. Annotations only synthesized as hints when unset |
| 4c | Name collisions (generated / built-in / duplicate) fail startup | fixed | Removed false positive: custom tool named `search` in per-collection mode was rejected via pre-seeded generic names. Adapter collision test (`fetch_deals`) + root generated/built-in/duplicate cases |
| 4d | describe_access lists usable custom tools only | newly implemented (test) | Behavior existed; now asserted for admin vs plain user in both adapters |
| 5a | Malformed arguments → JSON-RPC -32602 with pointer path | verified-as-done | Both adapters + root (`/id`, `/n`) |
| 5b | Handler failures sanitized; ToolError messages surface | fixed | Previously **every** handler error became generic, hiding facade not-found/PB-rule messages. Now `ToolError` (user-safe by construction) surfaces; other errors return the generic message and are logged without the error value. Token-bearing error in `failure` tool never leaks |
| 5c | Auth failures keep HTTP status/headers | verified-as-done | Untouched adapter auth path; existing OAuth/401 tests green |
| 5d | Timeouts cancel handler context; outcome recorded deterministically | fixed | Race: when handler return and `ctx.Done` raced, the outcome could be recorded as `error` instead of `timeout`. Handler result is now re-checked against `ctx.Err()`. `slow` tool + root `TestCustomToolTimeoutOutcome` |
| 5e | Oversized response → tool error, never truncated | verified-as-done | `large` tool (30-byte limit) both adapters |
| 5f | No automatic retries of writes | verified-as-done | No retry loop exists anywhere in the dispatch path |
| 6 | structuredContent + text; embedded resources; resource links | verified-as-done | Handlers may return `protocol.ToolResult`; text rendering prepended. `EmbeddedResourceContent` / `ResourceLink` types in protocol (present on arrival) |
| 7a | AuditFunc for custom AND generated tools (principal/tool/effect/collection/action/outcome/duration/request ID) | fixed | Denied custom writes previously audited with effect `read`; generated dispatch now threads effect + payload through `emitAudit`. Asserted via `fetch_deals` audit event |
| 7b | No payloads by default; opt-in redacted capture; never secrets | newly implemented | `Config.AuditIncludePayload` + recursive redaction (token/password/secret/authorization keys, 256-char truncation). Root test asserts a secret argument never appears in events. Adapter tests assert caller token never appears |
| 7c | Output validation (runtime, default on) | verified-as-done | `bad_output` tool caught both adapters |
| 8 | fetch_contract_pdf: facade fetch, PB filesystem read, size limit, embedded PDF; registered in both examples with seeded collection; tested on both adapters | fixed | Hardened unchecked type assertions, added OutputSchema + annotations. `TestFetchContractPDF` both adapters. Examples: `ensureContracts` (owner-scoped `contracts`, protected file field) in both mains + `contracts` entry in both `manifest.json`. Where-code-lives rationale: below |
| 9 | README custom-tools section | newly implemented | [README.md](/Volumes/mini512/Components/Pocketbase-MCP/README.md): registration, schema subset, policies/effects, audit, trust boundary, facade, PDF tool |
| A(a) | Read+write tools: policy-hidden absent + rejected | verified | `TestCustomRegistry` ×2 adapters |
| A(b) | Write denied (AllowWrites off / policy fail / PB rule), allowed when all pass | verified | Same test: all four branches |
| A(c) | Facade field allowlists + record rules (other user's record not found) | verified | `secret_note` never in facade fetch; draft deal (ViewRule) rejected; owner-scoped contract rejected for non-owner |
| A(d) | Invalid input → -32602 with path | verified | Both adapters + root |
| A(e) | Handler error sanitized | verified | `failure` tool (error embeds caller token) |
| A(f) | Timeout cancels handler (ctx.Done observed) | verified | `slow` tool closes channel from `ctx.Done` |
| A(g) | Oversize rejected | verified | `large` tool; PDF size-limit case |
| A(h) | Output schema violation caught | verified | `bad_output` tool |
| A(i) | Audit outcomes ok/denied/invalid/error/timeout; no secrets | verified | Outcome sweep asserted both adapters; token-substring sweep; root payload-redaction test |
| A(j) | Collision with generated tool fails startup | verified | `fetch_deals` rename, both adapters |
| A(k) | PDF: allowed record returns PDF; other user not-found; size limit | verified | `TestFetchContractPDF` ×2 adapters |
| A-HTTP | Stock fixture over HTTP: tools/list shows tool; call returns embedded application/pdf | verified | Transcript below |
| — | Chat channel execution | not done (by design) | Plan defers chat to v1.1; field carried, chat-only tools never listed on MCP |

## Files created / changed in this run

Created:

- [custom_test.go](/Volumes/mini512/Components/Pocketbase-MCP/custom_test.go) (root: registration validation, shared gate, audit redaction, timeout outcome)
- [README.md](/Volumes/mini512/Components/Pocketbase-MCP/README.md)

Changed:

- [custom.go](/Volumes/mini512/Components/Pocketbase-MCP/custom.go) — validation fixes (nil schema, editor rule, name/policy), effect normalization, deterministic timeout, ToolError-vs-sanitized split, redacted audit payloads
- [config.go](/Volumes/mini512/Components/Pocketbase-MCP/config.go) — `Config.AuditIncludePayload`
- [server.go](/Volumes/mini512/Components/Pocketbase-MCP/server.go) — denied-call audit effect, generated-call audit payload, synthesized hint-only annotations
- [manifest.go](/Volumes/mini512/Components/Pocketbase-MCP/manifest.go) — generic-name collision false positive removed
- [schema/schema_test.go](/Volumes/mini512/Components/Pocketbase-MCP/schema/schema_test.go) — boundary/nesting coverage
- [adapters/pocketbase/custom_test.go](/Volumes/mini512/Components/Pocketbase-MCP/adapters/pocketbase/custom_test.go) and the pbpostgress twin — mux rebuilt via `apis.NewRouter` + `RegisterRoutes` + `BuildMux` (was `env.mux = srv`, which cannot compile: `*Server` has no `ServeHTTP`); added allowlist/record-rule/describe_access/generated-audit assertions and `TestFetchContractPDF`
- [adapters/pocketbase/contract_pdf.go](/Volumes/mini512/Components/Pocketbase-MCP/adapters/pocketbase/contract_pdf.go) and the pbpostgress twin — safe result assertions, multi-file tolerance, OutputSchema, annotations
- [examples/stock/main.go](/Volumes/mini512/Components/Pocketbase-MCP/examples/stock/main.go), [examples/fork/main.go](/Volumes/mini512/Components/Pocketbase-MCP/examples/fork/main.go) — register `fetch_contract_pdf`; seed `contracts` collection; `MCP_SEED_DEMO=true` demo user + contract
- Both example `manifest.json` files — `contracts` collection entry

Present on arrival and verified without changes: `schema/schema.go`
core validator, `protocol` embedded-resource types, `ToolSpec` /
`Effects` / `CallContext` / facade shape, adapter `ValidateCustomTools`
hooks.

### Where the PDF code lives, and why

`ContractPDFTool` is duplicated in each adapter module, not the root:
reading the file needs the host's PocketBase `core.App.NewFilesystem()`,
and the root module must stay PocketBase-independent (plan section 2).
Each adapter compiles against only its own PocketBase (stock vs fork),
so neither imports the other's host. The tool body is otherwise
identical; the facade call keeps manifest, ViewRule and field-allowlist
enforcement in the shared root code.

## Schema subset as implemented

| Keyword | Behavior |
|---|---|
| `type` | string or array of `object|array|string|number|integer|boolean|null`; `integer` requires an integral float |
| `properties` / `required` / `additionalProperties` | `additionalProperties` is `false` or a schema (`true` rejected) |
| `enum` / `const` | JSON-normalized deep equality; empty/duplicate enums rejected |
| `items` | single schema, recursive |
| `minimum`/`maximum`/`exclusiveMinimum`/`exclusiveMaximum` | numeric (2020-12 style); boolean exclusive form is outside the subset and rejected |
| `minLength`/`maxLength` | rune counts |
| `minItems`/`maxItems` | inclusive bounds |
| `pattern` | Go RE2, compiled at registration |
| `format` | `date-time` (RFC 3339), `email`, `uri` (absolute) |
| `title`/`description`/`default` | annotations, never validated |
| anything else | registration error `…: unsupported keyword` with pointer path |

Validation errors are `<json-pointer>: <reason>`, e.g.
`/rows/0/qty: minimum violated`.

## Commands and results

All modules in isolation (`GOWORK=off`, each its own directory):
`go build ./...`, `go vet ./...`, `go test -count=1 ./...`,
`gofmt -l .` — final results, all exit 0, gofmt empty everywhere:

| Module | Result |
|---|---|
| root (`.`) | ok — root, protocol (no tests), schema |
| auth/stockoauth | ok |
| adapters/pocketbase | ok (7.2s) |
| adapters/pbpostgress | ok (5.1s, against disposable Postgres) |
| examples/stock | build/vet ok, no test files |
| examples/fork | build/vet ok, no test files |

Postgres (task-prescribed short path; stopped and deleted afterwards):

```sh
/opt/homebrew/opt/postgresql@14/bin/initdb -D /tmp/m5pg -U postgres -A trust
/opt/homebrew/opt/postgresql@14/bin/pg_ctl -D /tmp/m5pg -o "-p 55441 -k '' -c listen_addresses=127.0.0.1" -l /tmp/m5pg.log -w start
/opt/homebrew/opt/postgresql@14/bin/createdb -h 127.0.0.1 -p 55441 -U postgres pbmcp_test
PB_DB_DSN='postgres://postgres@127.0.0.1:55441/pbmcp_test?sslmode=disable' GOWORK=off go test -count=1 ./...
/opt/homebrew/opt/postgresql@14/bin/pg_ctl -D /tmp/m5pg -w stop   # then rm -rf /tmp/m5pg
```

No `/tmp/m5pg` or listener on 55441 existed before setup (checked).

Focused custom-tool runs (before the full matrix):

```text
adapters/pocketbase: TestCustomRegistry PASS, TestFetchContractPDF PASS
adapters/pbpostgress: TestCustomRegistry PASS, TestFetchContractPDF PASS
```

Failures encountered and corrected:

- Initial `go vet` on both adapters: `custom_test.go:44: cannot use srv
  (*Server) as http.Handler` (the known partial-work defect) — fixed via
  the router/mux rebuild above.
- Fork adapter vet then failed once more: its `RegisterRoutes` returns
  no error (pre-existing divergence from the stock adapter) — the fork
  test now calls it without an error check.
- First HTTP attempt failed with `manifest: collection "items" not
  found`: a fresh data dir has no `items`/`reports` collections. The
  HTTP check was rerun with a contracts-only manifest (below) against
  the same fixture binary; the shipped example manifest (now including
  `contracts`) works against a data dir that has the fixture
  collections, as in prior milestones.
- `gofmt -l` initially flagged `custom.go`, `server.go` and both
  `contract_pdf.go`; formatted, now empty.

### HTTP check (stock fixture, then stopped)

```sh
cd examples/stock && GOWORK=off go build -o /tmp/m5stock_bin .
MCP_SEED_DEMO=true MCP_MANIFEST=/tmp/m5manifest.json \
  /tmp/m5stock_bin --dir /tmp/m5stock_data serve --http=127.0.0.1:18095
python3 /tmp/m5curl.py   # login demo@example.com -> tools/list -> search_contracts -> fetch_contract_pdf
```

Observed output (server terminated afterwards; port confirmed closed):

```text
LOGIN_OK
TOOLS: describe_access,describe_collection,fetch_contract_pdf,search_contracts,fetch_contracts
CONTRACT_ID: lsq35j8xw0u8z9d
PDF_OK bytes=25 mime=application/pdf uri=pocketbase://pbc_1836797931/lsq35j8xw0u8z9d/contract_g5s6ymm8vb.pdf
STRUCTURED: {"id": "lsq35j8xw0u8z9d", "mimeType": "application/pdf", "sizeBytes": 25}
PASS
```

`/tmp/m5manifest.json` contains only the `contracts` entry (identical to
the one added to both shipped manifests); `/tmp/m5curl.py` and the
tokens it used were discarded with the data dir.

## Deviations and notes

- Exclusive bounds use the numeric form; the legacy boolean
  `exclusiveMinimum: true` form is rejected as an unsupported shape
  rather than silently ignored — consistent with "reject outside the
  subset".
- `format: date-time` is strict RFC 3339 (what PocketBase emits);
  lowercase `t`/`z` separators are rejected.
- Generated collection tools have no JSON-Schema argument stage (the
  adapters validate), so their validation failures audit as outcome
  `error`, not `invalid`. `invalid` is produced by custom-tool schema
  validation. Distinguishing adapter validation errors from rule denials
  would need a typed error split in the host port.
- Facade denials that reach a handler as `ToolError` (record not found,
  PB rule rejection) audit as `error`; `denied` is reserved for the
  tool-level gate. The outcome set is emitted as specified; only this
  classification is a judgment call.
- Stock `core.FileSystem.GetFile` prints a deprecation notice
  (successor: `GetReader`). Kept because it exists in both the stock
  release and the fork build in use; switching is a safe follow-up once
  both are confirmed to expose `GetReader`.
- The fork adapter's `RegisterRoutes` returns nothing while the stock
  adapter's returns `error` (pre-existing). Left unchanged; noted for a
  future consistency pass.
- Plan check: section 7's narrower schema list (no `const`, no type
  arrays, `additionalProperties: false` only) is extended by the task
  scope (const, type arrays, schema-valued additionalProperties,
  exclusive bounds, min/max items, formats). Implemented per the task;
  no plan text was modified.

## Open questions

- Should editor-location tools for the chat channel be registerable now
  (currently yes — carried, never listed), or rejected until chat ships?
  Current behavior follows the task wording ("reject `editor` for the
  mcp channel").
- Should audit `denied` also cover facade-level record denials inside a
  handler? That would require the facade to report denial distinctly
  from handler errors.
