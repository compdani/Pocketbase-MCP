# PocketBase MCP

A Go module that lets AI clients (ChatGPT, Claude, Cursor and others) work
with a PocketBase app through the [Model Context Protocol](https://modelcontextprotocol.io).
It also has an optional chat API and browser client, so the same tools can be
used from a chat panel inside your own app.

- **Explicit exposure.** You list which collections and operations are
  exposed in a manifest. Nothing else is reachable, and writes are off by
  default.
- **Same permissions as the user.** Every call runs as the signed-in
  PocketBase user, under their normal record rules. The module can narrow
  access, never widen it.
- **Two hosts.** Works on stock PocketBase and on the pb-postgress fork.
- **No third-party libraries.** The core uses only the Go standard library.
  The MCP protocol, OAuth flow, JSON Schema checks and model-provider clients
  are all written in-house.

## What's here

| Path | What it is |
|---|---|
| `*.go`, `protocol/`, `schema/` | Root module `github.com/compdani/pocketbase-mcp`: MCP server, manifest, policies, custom tools, schema validator. No PocketBase dependency. |
| `adapters/pocketbase/` | Adapter for stock PocketBase (v0.40.1). |
| `adapters/pbpostgress/` | Adapter for the pb-postgress fork. |
| `auth/stockoauth/` | OAuth sign-in for stock PocketBase (the fork has its own). |
| `chat/` | Optional chat service: turns, streaming, proposals, uploads, provider adapters. |
| `frontend/client/` | Dependency-free browser client for the chat API, with an `<agent-panel>` binding. |
| `examples/stock/`, `examples/fork/` | Runnable example apps, one per host. |
| `examples/chat/` | Plain JS, Vue and Svelte chat front ends. |
| `verification/` | Milestone test reports. |

Each adapter and example is its own Go module. Build them one at a time and
never put both adapters in one `go.work`: they resolve the same PocketBase
module path to different code.

## Quick start (stock PocketBase)

```go
app := pocketbase.New()
app.OnServe().BindFunc(func(se *core.ServeEvent) error {
    srv, err := pb.New(se.App, pbmcp.Config{
        Path:     "/mcp",
        Auth:     pbmcp.AuthConfig{Collection: "users"},
        OAuth:    pbmcp.OAuthConfig{Enabled: true, Mode: pbmcp.OAuthModeModule},
        Manifest: manifest, // which collections and operations to expose
    })
    if err != nil {
        return err
    }
    if err := srv.RegisterRoutes(se.Router); err != nil {
        return err
    }
    return se.Next()
})
```

`examples/stock/main.go` is a complete version with a manifest, a custom tool
and chat. To try everything without an API key:

```sh
MCP_CHAT_PROVIDER=demo MCP_CHAT_PROPOSALS=true MCP_ALLOW_WRITES=true \
  MCP_CHAT_UPLOADS=true MCP_SEED_DEMO=true \
  MCP_MANIFEST="$PWD/examples/stock/manifest.json" \
  go run ./examples/stock serve --http 127.0.0.1:8090 --dir /tmp/pbmcp-demo
node examples/chat/plain/serve.mjs   # open http://127.0.0.1:5174
```

Sign in as `demo@example.com` / `password123`.

## Documentation

| Doc | Read it for |
|---|---|
| [docs/mcp-server.md](docs/mcp-server.md) | How the MCP server works: sign-in and OAuth, the manifest, permissions, custom tools, audit. |
| [docs/chat.md](docs/chat.md) | The chat API: turns and streaming, proposals and approval, editor tools, uploads, browser client. |
| [docs/operations.md](docs/operations.md) | Deploying: pinned versions, rate limits, OAuth setup steps, migrating from customjs. |
| [docs/roadmap.md](docs/roadmap.md) | What's left to do and the known gaps. |
| [frontend/client/README.md](frontend/client/README.md) | The browser client API. |
| [docs/history/](docs/history/) | The original design plans and research, kept for background. |
