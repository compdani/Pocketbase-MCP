# Milestone 3 verification report

> Raw command logs named here (`checks-*.txt`, `m4-*.txt`) are kept locally in `verification/` and are not tracked in git (2026-11-17).

Verification date: 2026-11-17. No live ChatGPT connection attempted. Reference customjs, pb-postgress, and plan documents were not modified. No go.work or runtime dependencies were added.

## Scope status

“Already done” means present on arrival and verified through source inspection and passing tests. “Fixed” includes retained implementation plus corrections in this run.

| Scope item | Status | Evidence / limits |
|---|---|---|
| Stock auto/module; stock native error; fork auto/native; fork module error; off | Already done | Root mode table tests; adapter startup tests |
| Enabled fork native collection configuration required | Already done | Postgres disabled-native and auto-disabled tests; fixture config can be enabled successfully |
| Native mode adds zero OAuth routes/storage/jobs/hooks | Fixed | Removed module-owned protected-resource routes from native mode; route inventory compares all declarations before/after, allowing only MCP mounts; collection set, cron total and password-auth hook count unchanged |
| Immutable ownership plan and duplicate app/mount registration | Fixed | Plan resolved before route registration; manifest schema validation precedes OAuth provisioning; failed adapter validation can be retried; enabled OAuth duplicate guard tested |
| Stable mount-scoped MCP schema hook IDs | Fixed | Collection-change hook IDs now include mount and event |
| Credential-free startup report | Fixed | Owner, collection ID, endpoints, accepted collections, manifest revision; seeded stock secret moved from log to mode-0600 setup file |
| Explicit auth collection; auth/password/superuser validation | Already done | Stock provider validation; fork native uses its enabled native configuration |
| Stable collection binding for clients, transactions and codes | Fixed | Reject empty/mismatched client collection binding; accepted token default resolved to stable ID; reject malformed client types |
| Preserve paths, root discovery and /sse aliases | Already done | Baseline test and curl transcript; configurable provider endpoints |
| Exact redirect; loopback public-client port exception; legacy diagnostic | Already done | Redirect unit and integration rejection tests; migration helper rejects legacy domain-only values |
| S256; public/DCR required; confidential optional | Already done | Both baseline tests, DCR flow and wrong/missing PKCE rejection tests |
| Local PB password auth, MFA refusal, no credential forwarding | Fixed | Dispatch through PB password API, preserving auth hooks/rules/rate limits; host-denial hook test; MFA test |
| Optional upstream Go hook | Already done | Provider interface exists; no upstream implementation included |
| Hashed secrets/codes; constant-time comparisons; atomic single-use consumption | Already done | Provider code, helper tests, replay/expiry tests; SQLite transaction serialization |
| CSRF; no PB token in redirect; no credential/code/token logging in OAuth handlers | Fixed | CSRF rejection test; redirect checks; fixture secret logging removed |
| PB token response and configured/default duration | Already done | Successful authenticated tools/list proves normal PB token acceptance; duration follows provider setting or collection |
| Eligibility/token-key recheck at exchange | Already done | Rotation rejection test, collection/MFA/AuthRule checks |
| No shared-secret ID token | Already done | No ID token issued; signing-key compatibility field unused |
| Private namespaced schema, conflict rejection, never MCP-exposed | Fixed | Transactional startup schema provisioning; reject public API rules/non-base schemas/unhidden hashes; reserved namespace blocked in manifest even by collection ID; exposure rejection test |
| Small overridable login page; no user creation | Already done | Provider html/template configuration; local auth only |
| 401 WWW-Authenticate in stdlib and PB routes | Already done | Root handler and adapter tests |
| Root/path protected-resource metadata; matching issuer | Already done | Stock discovery-chain integration test; native fork additions deliberately deferred to milestone 4 |
| AS S256, registration endpoint, none/client_secret_post metadata | Already done | Discovery-chain test and curl root document |
| Public-only DCR, exact HTTPS/loopback redirects, per-IP limit, admin-visible/revocable | Already done | Integration success/rejection plus provider storage and limiter inspection |
| Client ID Metadata Documents | Not done (optional) | DCR selected instead |
| Stock hand-created client fixture | Fixed | Credentials saved once to data-dir/mcp-client.json; setup data and trimmed transcript manifest provided separately |
| Fork native fixture | Already done | Builds; native config Save succeeds in Postgres tests; no fork source edits |
| Preserve customjs client IDs/secrets during migration | Newly implemented | ImportHandClient hashes existing secret and retains ID; migration/exchange test |

## Acceptance checks

| Gate | Status | Result |
|---|---|---|
| Isolated build/vet/test in all six modules | Verified | All commands exit 0; logs alongside this report |
| gofmt -l | Verified | Empty for all module source files |
| Automated ChatGPT baseline without PKCE | Verified | TestOAuthChatGPTBaselineNoPKCE |
| Automated confidential S256 | Verified | TestOAuthChatGPTBaselineWithPKCE |
| Running stock fixture curl: both sequences | Verified | stock-curl-transcript.txt; root discovery → login GET/POST → code → client_secret_post → authenticated tools/list |
| Self-registration discovery chain → PKCE → token → tools/list | Verified | TestOAuthSelfRegistrationFlow |
| Inspector CLI | Verified | Connected using HTTP and obtained tools/list; inspector.txt includes initial syntax failure and successful retry |
| Redirect mismatch | Verified | TestOAuthRejections |
| Legacy domain-only redirect | Verified | TestOAuthRejections |
| Replayed code | Verified | TestOAuthRejections |
| Expired code | Verified | TestOAuthRejections |
| Wrong PKCE verifier | Verified | TestOAuthRejections |
| Public client missing PKCE | Verified | TestOAuthRejections |
| Token-key rotation before exchange | Verified | TestOAuthRejections |
| Account from another auth collection | Verified | TestOAuthRejections |
| MFA-required account | Verified | TestOAuthMFARequiredAccountRejected |
| Superuser login | Verified | TestOAuthRejections |
| Wrong client secret | Verified | TestOAuthRejections |
| DCR non-HTTPS non-loopback | Verified | TestOAuthRejections |
| Fork route/collection inventory, disabled native, module rejection | Verified | TestNativeOwnershipInventory and TestForkNativeMode against disposable Postgres |
| Live ChatGPT | Not done (requested simulation only) | Manual staging steps below |

## Commands and results

Each module was run independently with GOWORK=off: root, auth/stockoauth, adapters/pocketbase, adapters/pbpostgress, examples/stock, examples/fork. Exact command output is in checks-*.txt:

```
go build ./...
go vet ./...
go test -count=1 ./...
gofmt -l <all Go source files in that module>
```

Fork test environment:

```
/opt/homebrew/opt/postgresql@14/bin/initdb -D /tmp/mcp3pg -U postgres -A trust
/opt/homebrew/opt/postgresql@14/bin/pg_ctl -D /tmp/mcp3pg -o "-p 55434 -k '' -c listen_addresses=127.0.0.1" -l /tmp/mcp3-pg.log -w start
/opt/homebrew/opt/postgresql@14/bin/createdb -h 127.0.0.1 -p 55434 -U postgres pbmcp_test
PB_DB_DSN='postgres://postgres@127.0.0.1:55434/pbmcp_test?sslmode=disable' go test -count=1 ./...
```

Initial pg_ctl failed because port 55434 was occupied by a leftover disposable Postgres at /tmp/pbmcp-pg.8ThkrE from the earlier work. That instance was stopped, then the new disposable database started successfully. An intermediate attempt to enforce duplicate guards on bearer-only constructors broke existing tests that intentionally construct multiple configurations on one app; corrected to guard enabled OAuth mounts. Several commands initially used root-relative paths from nested working directories and failed without making edits; rerun from the root. Inspector's first invocation with --server-url failed with “missing required argument 'target'”; passing the URL positionally succeeded (exit 0).

Stock fixture reproduction (use a fresh temporary data directory if rerunning the seed; paths in the seed and curl script are explicit):

```
cp verification/stock-seed.go.txt /tmp/mcp3seed.go
# from examples/stock:
go run /tmp/mcp3seed.go
go build -o /tmp/mcp3stock-bin .
MCP_MANIFEST=/tmp/mcp3manifest.json MCP_PUBLIC_URL=http://127.0.0.1:18093 /tmp/mcp3stock-bin serve --http=127.0.0.1:18093 --dir=/tmp/mcp3stock
# from repository root; copy verification/stock-manifest.json to /tmp/mcp3manifest.json first:
python3 verification/stock-curl.py
npx -y @modelcontextprotocol/inspector --cli http://127.0.0.1:18093/mcp --transport http --method tools/list --header 'Authorization: Bearer <token>'
```

The curl harness executes actual curl processes, checks statuses and tools/list contents, and saves only redacted output. Inspector used a token obtained by this flow. It did not automate an interactive browser login.

## Migration and deployment notes

1. Back up oauth_apps. Choose the existing local auth collection explicitly; no automatic users fallback or user creation.
2. Replace domain-only redirect_uri with the **complete callback URL shown by ChatGPT**, including its path and any query. Do not guess or use a sample callback.
3. For each confidential oauth_apps record, call Provider.ImportHandClient(oldRecord.Id, oldRecord.GetString("client_secret"), oldRecord.GetString("name"), []string{fullCallbackURL}). This is an explicit host migration operation, not an HTTP endpoint. Existing ID and secret entered in ChatGPT continue to work; only the salted hash is persisted. No plaintext secret or ID-token signing secret needs to be migrated into provider storage.
4. Module storage uses reserved mcp_oauth_ names. Existing incompatible schemas fail startup; migrate deliberately rather than making the collections public. Provisioning is an idempotent startup schema migration in a DB transaction, rather than registered timestamped PB migration files. This is a plan implementation detail/deviation.
5. Hand-created confidential clients may omit PKCE; public/self-registered clients must use S256. No refresh tokens or ID tokens are issued.
6. PB access tokens also authorize PB REST API calls. Rotating a user's token key revokes all of that user's clients/browser sessions. Revoking a client prevents new OAuth grants/exchanges, not already issued PB tokens. Token expiry requires reauthentication.
7. PublicBaseURL should be explicit in deployed instances. Configure trusted proxies and allowed origins in the host; issuer fallback to request host is for local fixtures.
8. Provider LoginTemplate and UpstreamAuthenticator are Go host customization points; the upstream adapter is intentionally not implemented.

Native fork discovery remains a milestone-4 concern; native mode intentionally installs zero OAuth routes despite the plan's earlier section-3 language suggesting MCP-facing discovery. The later milestone-4 instruction and the task's zero-route constraint take precedence. No MCP OAuth audience/resource isolation conformance claim is made for full PB session tokens.

## Manual ChatGPT staging verification

1. Deploy stock fixture/integration on a public HTTPS staging URL, with the intended local users and manifest. Set MCP_PUBLIC_URL and MCP_OAUTH_REDIRECT_URI to the actual full ChatGPT callback URL before seeding/importing.
2. Read the seeded client ID/secret from the protected data-dir/mcp-client.json setup file, enter them manually in ChatGPT using client_secret_post, then remove the setup file after securely recording the secret.
3. Configure the connector URL as https://your-host/mcp (or the configured /sse alias).
4. Confirm ChatGPT can read root authorization-server metadata. Sign in as an eligible local user; confirm the callback succeeds and tools/list exposes only that user's intended tools.
5. Invoke one permitted read tool and confirm a denied collection remains unavailable. Rotate the user's token key and verify the connection requires reauthentication.
6. Repeat with S256 if the client supplies it; do not require PKCE for the hand-created confidential compatibility client.

Open questions: live ChatGPT staging confirmation remains operator work; upstream identity mapping is host-specific; fork root discovery/DCR support is milestone 4. No upstream implementation or live ChatGPT connection was attempted.

## Files

Retained/verified partial implementation: root oauth.go/oauth_test.go/config.go/server.go/host.go/policy.go/build.go/manifest.go; auth/stockoauth module; OAuth adapter files/tests and adapter constructors; both example modules and main.go files.

Changed in this continuation: build.go; auth/stockoauth/{stockoauth.go,flow.go}; adapters/{pocketbase,pbpostgress}/{adapter.go,oauth.go,oauth_test.go}; examples/stock/main.go. Newly added: verification report, six command logs, redacted curl transcript/harness, standalone seed source (text), trimmed manifest, Inspector output. Example executables were rebuilt by the requested go build commands. Existing module replacements were preserved; no reference project was edited.
