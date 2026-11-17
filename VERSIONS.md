# Pinned versions (milestone 1)

| Item | Pin |
|---|---|
| Root module | `github.com/compdani/pocketbase-mcp`, stdlib only |
| Stock PocketBase | `github.com/pocketbase/pocketbase v0.40.1` (the version customjs builds against) |
| Fork | `/Volumes/mini512/Go Projects/pb-postgress`, git `656b31ad`, clean tree at pin time; declares module `github.com/pocketbase/pocketbase`, `go 1.27.0` |
| Go toolchain | `go1.26.5 darwin/arm64` locally; modules advertising PB v0.40.1 / the fork auto-switch to `go1.27.4` (their `go >= 1.27` requirement) |
| Test Postgres (fork only) | Local Homebrew `postgresql@14`, disposable instance on port 55433 (verification only, not a runtime dependency of the root/stock modules) |

Adapters and fixtures select PocketBase via their own `go.mod` replace directives.
No `go.work` includes both adapters or both fixtures.
