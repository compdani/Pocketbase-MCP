# Milestone 4 — Fork OAuth

Date: 2026-11-17. Implementation and verification report.

## Branch and commits

Fork branch: `oauth-mcp-clients`, based on `master` at `5c8729070cf3ee8ce77367b7f61085d2abe2c370`. Master was not modified. No push or PR.

- `fb7078f3` — Add public OAuth clients, dynamic registration and standard discovery.
- `999523b7` — Expose public clients and registration in the embedded dashboard.
- `aee3c7f2` — Adjust collection paging fixture for OAuth client storage.
- `a1ea8d83` — Handle unset registration redirect hosts in the dashboard.

All commits end with `Co-Authored-By: GPT-6.1 Sol <noreply@openai.com>`.

## Implementation

Public hand-configured clients have `public: true`, no secret, mandatory S256 PKCE, token authentication method `none`, and no ID token. Basic authentication and any submitted client_secret parameter are rejected for public clients. Confidential clients retain optional PKCE and basic/post authentication.

Per-collection `registration` has `enabled` (default false), `allowedRedirectHosts`, and `rateLimit` (0 selects 10 per hour). Registration is limited to public authorization-code clients, exact HTTPS redirects or HTTP loopback, bounded request bodies (16 KiB), and exact case-insensitive hostname allowlisting for non-loopback hosts. Limits use the request's socket IP and a fixed hourly window, in memory per app instance; trusted-proxy deployment implications are below.

Registered clients live in `_oauth2ServerClients`, a system collection with nil API rules, visible under dashboard system collections. Fields: collectionRef, clientId, name, redirectURIs, revoked, created. IDs use reserved `dcr_`; hand-configured clients cannot claim that prefix. Both authorization and token exchange resolve these records afresh. Deletion or revocation blocks future grants/exchanges; issued PB access tokens remain valid.

Self-registered clients always receive the hosted sign-in form, including with an SSO cookie. Their names are marked unverified, the redirect host is displayed, and a warning is shown. prompt=none returns interaction_required. Existing X-Frame-Options DENY and CSP frame-ancestors 'none' were retained. Popup sign-in, passkeys and provider flows are covered by existing OAuth tests.

Both RFC 8414 and OIDC path-insertion metadata routes describe the existing collection issuer. `rootDiscovery` enables both plain root metadata routes for one collection. Save validation rejects competing flags; an internal `_oauth2ServerRoot` singleton claim provides database-transaction enforcement across concurrent saves and releases on disable/deletion. This is an internal SQL table, not an additional API collection. Registration discovery is advertised only when enabled. Metadata includes none and the RS256 JWKS endpoint.

The MCP adapter adds only GET /.well-known/oauth-protected-resource and GET /.well-known/oauth-protected-resource/mcp (for the configured MCP path), alongside its MCP mounts. Native mode still installs no OAuth server endpoints, collections, jobs or auth hooks. Resource metadata uses the fork's configured app URL/collection issuer. Existing native 401 challenges are retained. The fork fixture enables registration on both newly configured and already enabled native OAuth collections. Stock source is unchanged.

## Secret storage and ID tokens

Confidential secrets are SHA-256-prehashed, base64 encoded, then bcrypt-hashed at the existing default cost using a fresh salt. The `$pb$` storage marker identifies this format; bcrypt format is checked before treating a value as migrated. Prehashing supports the existing maximum secret length without bcrypt's 72-byte limit. Verification uses bcrypt's constant-time comparison. Public clients have no secret. Save hooks hash new/regenerated credentials; collection API redaction and secret restoration by client ID are preserved. The dashboard generates a credential locally and tells the operator to copy it before saving; stored credentials are never returned.

ID tokens now use RS256, with a dedicated 2048-bit RSA private key per enabled auth collection. It is stored in server-side collection options, redacted from normal API serialization, retained across settings edits, and included in secret-preserving backups. It is independent of client credentials and PB auth-token signing keys. Discovery advertises RS256 and jwks_uri; the public JWKS exposes only RSA n/e with kid oauth2-server. Confidential flow tests verify an ID token with the published JWKS. Public clients receive only the PB access token.

Migration `1794000006_oauth2_server_clients.go` provisions client storage and the root-discovery singleton table. Migration `1794000007_oauth2_server_secrets.go` hashes plaintext credentials for every auth collection and provisions signing keys for enabled collections. It updates raw options directly to cover collections outside the current instance scope, preserving unrelated options. Already hashed values and existing signing keys are retained; the migration test executes it twice, checks hash stability, and authenticates an existing confidential client afterwards. Both migrations are forward-only.

## Operator migration notes

1. Back up the database and securely copy existing client secrets before upgrading: hashes cannot be recovered. Existing clients keep sending their original secret; no credential reentry is needed at the token endpoint.
2. OIDC consumers must refresh discovery and use RS256/JWKS. Clients explicitly pinned to HS256/client-secret ID-token verification must update that configuration. Old HS256 ID tokens are not re-signed. Existing PB access tokens are unaffected.
3. Backups containing the RSA signing key are sensitive. Preserve that key across replicas and restores to keep ID-token verification stable.
4. Set the fork Application URL to the deployed issuer base and align MCP public URLs. Configure trusted proxies in the host. The DCR limiter currently keys on the socket address: behind a reverse proxy, registrations share the proxy's quota. Limits are per process, not a distributed abuse-control service.
5. Registration defaults off. Enable it explicitly; configure redirect hosts where practical. The dashboard system collection allows superusers to revoke or delete registrations. These operations do not revoke already issued PB session tokens; rotate the user's token key when account-wide revocation is needed.
6. Hand-configured IDs beginning dcr_ must be renamed before editing their config; that prefix is reserved for self-registered clients.

## Commands and results

Postgres setup (all exit 0):

```sh
/opt/homebrew/opt/postgresql@14/bin/initdb -D /tmp/m4pg -U postgres -A trust
/opt/homebrew/opt/postgresql@14/bin/pg_ctl -D /tmp/m4pg -o "-p 55435 -k '' -c listen_addresses=127.0.0.1" -l /tmp/m4pg.log -w start
/opt/homebrew/opt/postgresql@14/bin/createdb -h 127.0.0.1 -p 55435 -U postgres pbmcp_test
```

Fork commands:

```sh
go build ./...
go vet ./apis ./core ./migrations ./plugins/migratecmd
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test -p 1 ./...
env -u PB_TEST_DB_DRIVER -u PB_TEST_DB_DSN go test ./...
# from ui:
npm run build
```

All six MCP modules passed build/vet/tests (exit 0) and gofmt output was empty.

The dashboard command is the repository's own `dprint fmt && vite build`; it succeeds and ui/dist is committed. No Go/npm dependencies were added. The default test run passes but skips database tests: tests/database.go creates disposable databases with Postgres/MySQL syntax, so this harness does not support a SQLite database run. Neither mysqld nor mariadbd is available, so MySQL was not run.

Each of the six MCP modules is checked independently with GOWORK=off, and PB_DB_DSN=postgres://postgres@127.0.0.1:55435/pbmcp_test?sslmode=disable for the fork adapter:

```sh
go build ./...
go vet ./...
go test -count=1 ./...
gofmt -l <all module Go files>
```

Exact module commands, exit codes and output are in m4-final-*.txt. The native end-to-end test covers /mcp 401 challenge → resource metadata → fork path-insertion AS metadata → DCR → hosted unverified warning → PKCE/password authorization → token → authenticated tools/list. Ownership inventory permits exactly the two resource metadata additions and checks no collection/job/auth-hook additions.

New fork behavioral coverage: public PKCE flow, missing PKCE, submitted secret rejection, no public ID token; DCR enabled/disabled, insecure and disallowed hosts, bad grants/responses, rate limit, warning/redirect host, SSO-cookie page rendering, prompt=none, revoked authorize/token, both root metadata paths, competing root flags, both path-insertion routes, plaintext hashing migration/idempotence/existing confidential exchange. Existing confidential and popup/provider/passkey tests remain.

Failures encountered and corrected:

- Initial secret migration used app.Save on out-of-scope auth collections; changed to raw option updates.
- Nested validation.Field pointer produced “field #0 cannot be found”; added registration's own Validate method.
- Existing collection-count and schema-template expectations changed with the new system collection/config defaults; updated those fixtures. One template update initially used spaces where Go snapshots required tabs; corrected. The public-client dashboard controls were also hidden and a nullable registration-host list was handled before the final UI build. The paging fixture also needed its expected third-page system collection adjusted; its focused rerun passes.
- An obsolete security import after changing ID-token verification caused a compile failure; removed it.
- A few source-edit commands used the wrong working directory and failed with FileNotFoundError; rerun from the correct root.
- Several overlapping full-suite attempts overloaded the disposable database and produced connection/login failures. Those runs were terminated and replaced by one serialized-package acceptance run. Earlier logs are retained separately from the final acceptance log.

## Files

Fork changes (including renamed rebuilt assets):

```text
apis/base.go
apis/collection.go
apis/collection_import_test.go
apis/collection_test.go
apis/record_auth_oauth2_server.go
apis/record_auth_oauth2_server_register.go
apis/record_auth_oauth2_server_test.go
core/collection_model.go
core/collection_model_auth_oauth2_server.go
core/collection_model_test.go
core/collection_query_test.go
core/collection_validate.go
migrations/1794000006_oauth2_server_clients.go
migrations/1794000007_oauth2_server_secrets.go
plugins/migratecmd/migratecmd_test.go
ui/dist/assets/index-B4_m6cTh.js
ui/dist/assets/pageConfirmEmailChange-D6euK9ej.js
ui/dist/assets/pageConfirmPasswordReset-0iNKzi4L.js
ui/dist/assets/pageConfirmVerification-DNTvOPge.js
ui/dist/index.html
ui/src/collections/oauth2ServerAccordion.js
```

MCP changes: adapters/pbpostgress/oauth.go; adapters/pbpostgress/oauth_test.go; examples/fork/main.go; verification/MILESTONE-4-REPORT.md and m4 verification logs. The example executables (examples/fork/fork and examples/stock/stock) were rebuilt by go build. No stock source, plan, customjs, go.mod, dependency files or go.work was changed/created.

## Deviations and open questions

No OAuth-server ownership was moved into the MCP module. The required protected-resource routes are module-owned. Additional internal singleton SQL storage enforces the root-discovery claim. Registration rate limits use fixed hourly windows and process-local state; operators needing shared limits should enforce them at the ingress. Live third-party connector staging and MySQL verification remain unperformed; there are no unresolved product decisions.

Final acceptance: PASS. `go build ./...` and changed-package vet exit 0; `PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN=... go test -p 1 ./...` exits 0 (m4-fork-PASS.txt). The complete API package passed in 117.875s and the final aggregate command reused that result; core reran and passed. The default test command exits 0 (database cases skip). All six isolated MCP build/vet/test commands exit 0 and gofmt output is empty. Dashboard build exits 0 and the rebuilt embedded fork passes go build. Fork working tree is clean; master remains 5c872907.

Cleanup completed with exit 0:

```sh
/opt/homebrew/opt/postgresql@14/bin/pg_ctl -D /tmp/m4pg -m fast -w stop
rm -rf /tmp/m4pg
```

The disposable server is stopped and its data directory no longer exists. No application servers were left running.

Additional exact diagnostic commands (from fork root unless stated):

```sh
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test ./... # initial snapshots/count failures; later overlapping reruns terminated (143)
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test ./apis -run OAuth2Server -count=1 # intermediate validator/import/snapshot failures, corrected
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test ./apis -run TestOAuth2Server -count=1 # exit 0, m4-oauth-final.txt
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test ./core ./plugins/migratecmd -run 'Test(CollectionDBExport|FindAllCollections|AutomigrateCollectionCreate|AutomigrateCollectionDelete)$' -count=1 # initial Go template whitespace failure
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test ./plugins/migratecmd -count=1 # corrected, exit 0
PB_TEST_DB_DRIVER=postgres PB_TEST_DB_DSN='postgres://postgres@127.0.0.1:55435/postgres?sslmode=disable' go test ./apis -run TestCollectionsList -count=1 # corrected paging, exit 0
command -v mysqld # exit 1
command -v mariadbd # exit 1
```
