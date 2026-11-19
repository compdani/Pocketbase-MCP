# Chat

An optional chat API that lets a model use the same tools as MCP from inside
your own app, plus a browser client to drive it. It is off unless a host turns
it on, and an MCP-only setup needs none of it.

Chat is an HTTP API for your front end, not another MCP transport. A model's
tool call goes through the same tool list, schema checks, policies and
PocketBase rules as an MCP call. Chat can narrow what MCP offers, never widen
it.

## Contents

1. [Turning it on](#1-turning-it-on)
2. [HTTP API](#2-http-api)
3. [Turns and streaming](#3-turns-and-streaming)
4. [Ownership and storage](#4-ownership-and-storage)
5. [Proposals](#5-proposals)
6. [Editor tools](#6-editor-tools)
7. [Uploads](#7-uploads)
8. [Model providers](#8-model-providers)
9. [Browser client and examples](#9-browser-client-and-examples)
10. [Limits](#10-limits)

## 1. Turning it on

In a PocketBase adapter, call `EnableChat` before `RegisterRoutes`. The simple
form uses one provider:

```go
err := srv.EnableChat(pb.ChatConfig{
    Options:  pbmcp.ChatOptions{Namespace: "my-app", Proposals: true, Uploads: true},
    Provider: provider, // openai.New(...) or anthropic.New(...)
    Model:    "gpt-4.1",
    System:   "You help users manage their listings.",
})
```

A catalog gives every turn a provider and model choice, including several
OpenAI-compatible gateways and a signed-in ChatGPT/Codex plan:

```go
err := srv.EnableChat(pb.ChatConfig{
    Options: pbmcp.ChatOptions{
        Namespace: "my-app", Proposals: true, Uploads: true,
    },
    // Managed mode provisions three private provider collections and mounts
    // the superuser settings API under <Prefix>/providers.
    ManageProviders: true,
    // Optional first-boot seed; later changes go through the settings API.
    Providers: []catalog.Provider{{
        ID: "openai", Kind: catalog.KindOpenAI, Enabled: true,
        BaseURL: "https://api.openai.com/v1",
        Models: []catalog.Model{{ID: "gpt-5"}}, DefaultModel: "gpt-5",
    }},
    System: "You help users manage their listings.",
})
```

Static mode (`Providers` without `ManageProviders`) builds the catalog at
startup and mounts no settings routes. The legacy `Provider` + `Model` form
remains supported and works exactly as before.

`EnableChat` creates the private chat collections, builds the turn runner,
recovers from a previous crash (section 4) and mounts the API.

`ChatOptions`:

| Option | Meaning |
|---|---|
| `Namespace` | Required. Identifies your app in every ownership key. |
| `Prefix` | API root, default `/api/mcp/chat/v1`. Must not overlap any MCP path. |
| `Policy` | Optional policy a caller must pass to use chat at all. |
| `Models` | Model allowlist reported by `/status`. |
| `Proposals` | Offer write tools as proposals a person must approve (section 5). Needs `Config.AllowWrites`, unless the only chat writes are editor tools. |
| `AutoApprove` | Let tools marked `Approval: "auto"` run without review. Never relaxes `AllowWrites`, policies or PocketBase rules. |
| `AllowArgumentEdits` | Let the reviewer edit a proposal's arguments before approving. |
| `DecisionTimeout` | Time limit for running one approved proposal (default 30s). |
| `Uploads` | Turn on file attachments (section 7). |
| `MaxUploadBytes`, `MaxAttachmentsPerTurn`, `MaxUploadsPerConversation` | Upload limits (defaults 10 MiB, 5, 50). |
| `MaxBodyBytes`, `MaxMessageChars`, `HeartbeatInterval` | Request size (64 KiB), message length (16,000 characters), SSE heartbeat (15s). |
| `TargetCheck` | Optional check that a caller may use a conversation's target record. |

Startup refuses inconsistent settings: proposals without `AllowWrites` (when
server writes are possible), proposals or uploads with a store or runner that
can't support them, and a runner that uses a different store from the
handler.

## 2. HTTP API

All routes sit under the prefix and need `Authorization: Bearer <PocketBase
token>`, from the same auth collections `/mcp` accepts. The token is only
ever read from that header, never from a URL.

| Route | Does |
|---|---|
| `GET /status` | Models, limits and which features are on |
| `GET /conversations` | The caller's conversations, newest first, optionally by target |
| `POST /conversations` | Create one, optionally tied to a target record |
| `GET /conversations/{id}` | Transcript plus open proposals |
| `PATCH /conversations/{id}` | Rename |
| `DELETE /conversations/{id}` | Cancel active work, then delete it with its proposals and uploads |
| `POST /conversations/{id}/turns` | Send a message (with an idempotency key and optional attachments); returns the turn |
| `GET /turns/{id}` | The turn's current state |
| `GET /turns/{id}/events?after=N` | Server-sent events: replay after `N`, then live |
| `POST /turns/{id}/cancel` | Stop a queued or running turn |
| `GET /proposals/{id}` | One proposal (proposals on) |
| `POST /proposals/{id}/decision` | Approve or reject (proposals on) |
| `POST /proposals/{id}/outcome` | Browser reports an editor result (proposals on) |
| `POST /uploads` | Upload a file to a conversation (uploads on) |
| `GET /uploads/{id}`, `GET /uploads/{id}/meta` | Download a file, or read its details (uploads on) |

When managed providers are on, the same prefix also serves a superuser-only
settings surface: `GET/POST /providers`, `GET/PATCH/DELETE /providers/{id}`,
`POST /providers/actions/test`, `POST /providers/actions/default`,
`POST /providers/{id}/test`, `POST /providers/{id}/models/refresh`, and the Codex
`signin`, `signin/complete`, `signin/cancel`, `signout` and `import` routes. It
uses a separate superuser authenticator and never widens the normal chat
surface. A wrong method on one of these paths is `405` with an `Allow` header,
as on the chat routes. See [Managing providers](#managing-providers) for the
update rules.

Routes for features that are off return 404. Another user's records also
return 404, the same as missing ones, so nothing leaks. Errors are
`{"error": {"code", "message"}}`; proposal conflicts add the stored
`"proposal"` so the client can show its real state.

## 3. Turns and streaming

A turn is one user message and the model's answer, including any tool calls.

```text
queued → running → completed | failed | cancelled
queued or running → interrupted (server restarted)
```

Each conversation has at most one active turn. Every turn ends with exactly
one terminal event.

**Events.** Every event has the same envelope: protocol version `"1"`, a
per-turn sequence number (also the SSE `id`), conversation ID, turn ID, type
and data. Types:

| Type | Meaning |
|---|---|
| `turn.queued`, `turn.started` | Lifecycle |
| `text.delta` | A piece of the model's reply |
| `tool.started`, `tool.completed`, `tool.failed` | A tool call and its result |
| `proposal.created`, `proposal.updated` | A proposal appeared or changed |
| `usage.updated` | Token usage, when the provider reports it |
| `turn.completed`, `turn.failed`, `turn.cancelled` | The one terminal event (a restart shows up as `turn.failed` with code `interrupted`) |

**Reconnecting** replays from `after=N` and never re-runs the model or a tool.
A cursor older than the stored events returns a "resync required" error, and
the client reloads the conversation. Heartbeats don't use sequence numbers.

**Sending twice.** The idempotency key on `POST …/turns` means a retried
request returns the same turn instead of starting another.

**Stopping.** Closing the page or losing the connection only detaches; the
turn carries on. Stop calls `cancel`, which reaches the provider and any
cancellable tool. It can't undo a change that already happened.

**Token lifetime.** A running turn outlives the request that started it, so
its worker holds the caller's token in memory, never on disk. A turn never
runs past the token's expiry, and the token is checked again
before every tool call, so a revoked user or a removed role stops the next
step.

**History.** The model gets the last 50 stored messages, never splitting a
tool call from its result.

**Failures.** A failed turn records one of `provider_error` (the provider
refused the request or its stream broke), `max_steps`, `timeout`,
`auth_expired`, `interrupted` or `internal_error`, with a generic message.
Provider text never reaches the transcript or the browser. Instead the runner
logs one warning per failed turn (`chat: turn failed`) with the code, turn,
conversation, provider id, model, effort and cause, through
`RunnerConfig.Logger` (the adapters pass the app's logger, so it lands in the
PocketBase log). Provider errors are already sanitized: credentials removed and
length bounded.

## 4. Ownership and storage

Every record is keyed by namespace, mount, auth collection ID, user ID and
an optional tenant. A record ID alone never finds anything. A conversation's
optional target is a reference, not a sharing rule: conversations are always
private to their owner. Every request re-checks ownership and the target.

The PocketBase adapters store chat in six private collections (prefix
`_mcpChat` by default): conversations, messages, turns, events, proposals and
uploads. They have no API rules, so only superusers can see them in the
dashboard, and they are never exposed through the manifest. A conflicting
existing collection with the same name stops startup instead of being taken
over. New collections are added automatically on upgrade.

**On startup** after a crash, queued or running turns become `interrupted`,
and proposals that were mid-execution become `unknown` (section 5). Nothing is
re-run. A background job expires overdue proposals every minute. On the fork,
recovery and expiry only touch the app's own namespace and mount, since
several apps can share one database.

**Single instance.** Chat runs in one process per namespace and mount.
Running several instances needs durable leases and cross-instance event
delivery, which aren't built.

`chat.MemStore` is an in-memory store for tests. A custom store implements
`chat.Store`, plus `ProposalStore` and `UploadStore` for those features, and
can add `ActiveTurnStore` and `RecentMessageStore` to avoid reading whole
transcripts. `chat/storetest` is the shared test suite.

## 5. Proposals

With `Proposals` on, chat offers the caller's write tools (custom write tools
and the generated create, update and delete). When the model calls one,
nothing runs. Instead:

1. The call is validated and stored as a **proposal**: its owner, tool, the
   tool definition's fingerprint, the manifest revision, the arguments and a
   summary written by the server (never by the model). Malformed calls (a
   missing `id`, `data` that isn't an object, unknown keys) are refused
   straight away.
2. A `proposal.created` event goes out, and the model is told the change is
   waiting for approval and isn't done.
3. The user approves or rejects it with `POST /proposals/{id}/decision`:
   `{"decision": "approve" | "reject", "revision": N}`.
4. **Approving runs the change inside that request**, after checking the
   user's sign-in again and confirming the tool, the manifest and the user's
   permissions haven't changed. If any of them changed, the answer is 409
   "out of date" and the proposal stays pending, so the user can still reject
   it.
5. The result is stored on the proposal, and a short note is added to the
   conversation, so the next turn's model knows what happened.

Proposals outlive the turn that made them. Because approval runs in the
decision request, no worker has to keep a token alive while waiting for a
person.

**Exactly once.** The move to `executing` is an atomic compare-and-swap on
state and revision. Of several simultaneous approvals, one runs the change;
the others get back the stored result (`"replayed": true`). Closing the tab
mid-run doesn't stop the result from being recorded.

**States:**

```text
server proposals: pending → executing → succeeded | failed | unknown
                  pending → rejected | expired
editor proposals: pending → approved → applied | stale | failed | expired
                  pending → rejected | expired
```

- `failed` means it definitely didn't apply (a refusal or a PocketBase
  validation error).
- `unknown` means it may or may not have applied: a timeout after the change
  was sent, or a crash before the result was saved. It is never retried
  automatically.
- Proposals expire after 24 hours by default (`RunnerConfig.ProposalTTL`).

**Editing arguments.** With `AllowArgumentEdits`, an approval may include new
`arguments`. They are validated like a fresh call, may not change which
collection, record or editor they target, and are stored as a new revision.

**Auto-approval.** A tool marked `Approval: "auto"` runs immediately when the
host sets `AutoApprove`, still under every other check.

## 6. Editor tools

An editor tool proposes a change to text in the page's editor (a draft), not
to the database. The rules, checked at startup:

- `Channels: "chat"` only, and `ExecutionLocation: "editor"`;
- no `Handler` (nothing runs on the server);
- no `Approval: "auto"` (every change is reviewed);
- a write effect;
- an input schema that requires string `editorId` and `baseRevision`.

Editor tools are offered only while proposals are on, and they don't need
`AllowWrites`, because they write nothing on the server.

Approving an editor proposal only marks it `approved`; nothing runs on the
server, and the approval gets a fresh expiry so the browser has time to
respond. The browser then applies the patch and reports back with
`POST /proposals/{id}/outcome`:

```json
{"revision": 2, "status": "applied", "editorRevision": "7",
 "hunks": [{"id": "h1", "status": "applied"}, {"id": "h2", "status": "stale"}],
 "message": "optional note"}
```

`status` is `applied`, `stale` (the editor content changed) or `failed`. Hunks
record a partial result. This report is an acknowledgement, not proof that
anything was saved: saving still goes through the app's normal API, and the
note to the model always says "not saved yet".

## 7. Uploads

With `Uploads` on, a user uploads a file to a conversation and attaches it to
a message by its upload ID (up to 5 per message).

- **Types:** text (plain, Markdown, CSV, JSON), images (PNG, JPEG, GIF, WebP)
  and PDF. The server decides the type from the file's bytes, not from what
  the browser says. Images and PDFs must start with the right signature; text
  must be valid UTF-8 with no zero bytes. Anything else gets 415.
- **Limits:** 10 MiB per file and 50 per conversation by default. The
  per-conversation count can overshoot slightly when uploads arrive at the
  same instant.
- **Downloads** require the owner's token and are sent as an attachment with
  `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox` and
  `Cache-Control: private, no-store`. There are no public links and no
  PocketBase file tokens.
- **Storage:** files are stored through PocketBase's filesystem (local or
  S3) in a protected file field. Deleting a conversation deletes its uploads
  and their files, and nothing else.
- **To the model:** a turn sends the newest attachments first, up to 20 MiB
  per request. Text is labelled as data, not instructions. A file that
  doesn't fit, was deleted or isn't supported by the provider becomes a short
  "omitted" note. `/status` lists only the types the configured provider
  supports.

## 8. Model providers

Providers are hand-written over `net/http` with an in-house SSE parser; there
are no vendor SDKs, and credentials stay on the server.

| Package | Speaks |
|---|---|
| `chat/provider/openai` | OpenAI-compatible streaming Chat Completions, or the Responses shape (`WireAPI: "responses"`) |
| `chat/provider/anthropic` | Anthropic Messages API |
| `chat/provider/codex` | ChatGPT/Codex plan sign-in: OAuth, refresh tokens, ID-token verification, model list |
| `chat/provider/catalog` | Named provider config, per-turn routing, superuser settings service |
| `chat/provider/scripted` | A scripted provider for tests |

All real adapters support tool calls and attachments (images, PDF, text).
Request bodies are capped at 32 MiB to fit base64 attachments. A feature a
provider doesn't support fails clearly instead of being dropped.

### Catalog

A catalog entry has a stable `id`, a display label, a kind, a base URL, an
optional wire API, a model allowlist and a default model. Kinds:

- `openai` — Chat Completions by default, or Responses with
  `wireAPI: "responses"`. Each entry has its own base URL, key, headers, models
  and optional `extraBody`.
- `anthropic` — Messages API.
- `codex` — OpenAI Responses against `https://api.openai.com/v1`, authorized by
  a ChatGPT plan sign-in rather than an API key.

`GET /status` keeps the old flat `models` list and adds `choices` and
`defaultChoice`:

```json
{
  "models": ["gpt-5", "gpt-5-codex"],
  "choices": [
    {"id": "openai::gpt-5", "provider": "openai", "model": "gpt-5",
     "label": "GPT-5", "group": "OpenAI", "default": true,
     "attachmentTypes": ["text/plain", "image/png", "application/pdf"]}
  ],
  "defaultChoice": "openai::gpt-5"
}
```

A turn names `provider` and `model`; both are optional. Omitting both uses the
default; naming only a model uses the default provider when it offers that
model, otherwise the unique provider that does. The resolved pair is stored on
the turn (`provider` is absent on older turns). A turn is resolved once, so a
catalog change never switches a running turn to another provider.

**Reasoning effort.** A choice lists the efforts its model accepts in
`efforts` (lowest first, from `none`, `minimal`, `low`, `medium`, `high`,
`xhigh`), and a turn may name one with `"effort": "high"`. It is stored on the
turn and sent as `reasoning_effort` (Chat Completions) or `reasoning.effort`
(Responses and the Codex plan route). An effort the resolved choice does not
list is `400 invalid_request`, never a failed turn; omitting it uses the
model's default. Anthropic models take none (extended thinking is not wired
up). The legacy single-provider path advertises no efforts.

Attachment types are listed at two levels. `limits.attachmentTypes` is what
`POST /uploads` accepts: the union over every ready provider. Each choice's
`attachmentTypes` is what that model can actually read, and a turn whose
attachment its resolved model can't read is refused with
`unsupported_attachment`. The browser client's `attach()` checks the union, or
one model with `{model}`, and `send()` checks the model it sends to.

### Managing providers

The settings API follows these rules:

- **Create never replaces.** `POST /providers` with an id that already exists
  is `409 conflict`; use `PATCH /providers/{id}` to change it. The id and the
  kind are fixed once created.
- **Secrets are write-only.** In a `PATCH`, an omitted `apiKey` or `headers`
  keeps the stored value, an empty one clears it, and any other value replaces
  it. Views only show `hasApiKey`, a last-four `apiKeyHint` and `headerNames`.
- **Unreadable secrets must be re-entered.** If a stored key or header blob
  can't be decrypted (usually a changed `PB_ENCRYPTION_KEY`), that provider
  drops out of `/status` and its view shows `lastError`. The other providers
  keep working. The next update must re-enter the secret that failed. Sending
  only the other one is rejected, so the unreadable one is never saved as empty.
- **Changing a base URL keeps the key.** The stored key and headers are sent
  to whatever `baseURL` the entry names, including in `POST
  /providers/actions/test` with a saved id. When you point an entry at a
  different host, re-enter its key rather than reusing it.
- **Changing a Codex `oauthBaseURL`** rebuilds that provider's sign-in state
  and drops any pending sign-in. The stored credentials stay.
- **Deleting** removes the config, any Codex credentials, and the default
  choice if it pointed there. A refresh still running for that provider can't
  write its credentials back.

### Model discovery

The settings API fetches a provider's `/models` endpoint on demand; an explicit
refresh updates the allowlist after creation. OpenAI-compatible entries use
`GET {baseURL}/models` with the bearer key; Codex uses a fresh plan token and
skips entries whose `visibility` is set and is not `list`; Anthropic uses
`GET {baseURL}/v1/models` where available. The fetched list is stored as the
explicit allowlist (auto-populated when a provider is created with no models,
or after Codex sign-in), so a chat page load never fans out to provider APIs
and a gateway cannot silently widen the picker.

Each model may carry `efforts`. Codex fills them from the plan's
`supported_reasoning_levels`, as does any `/models` list that includes that
field. Otherwise OpenAI-compatible entries get `low`/`medium`/`high` for the
`gpt-5` family (except chat-tuned ids) and the `o1`/`o3`/`o4` series, ignoring a
gateway prefix such as `openai/`, and nothing for other ids. An administrator
can set `efforts` per model in `models` on create or `PATCH`; a refresh that
discovers none for a model keeps the configured ones.

### Codex plan sign-in

A superuser starts sign-in from the settings API. The server creates a
10-minute pending attempt and returns an authorization URL. On a browser on the
server's machine a loopback listener completes it, and its page says "Signed
in" only after the code exchange succeeded and the credentials were stored; on a remote browser the
superuser pastes the redirect URL back into the settings API. Only one attempt
per provider runs at a time. It belongs to the superuser who started it, and
cancelling it or starting a new one closes the old loopback listener. Credentials and
the rotating refresh token are encrypted at rest, refreshed ten minutes before
expiry, and refreshed once more on a 401. Signing out revokes the refresh token
and removes the choice from `/status`.

OpenAI offers ChatGPT plan usage for open-source and locally hosted apps. A
paid or remotely hosted deployment is expected to apply through OpenAI's
interest form; do not assume plan tokens may be shared by an arbitrary
production deployment.

### Settings storage

Managed mode provisions three private collections with the chat prefix:
`_mcpChatProviders`, `_mcpChatProviderAuth` and `_mcpChatSettings`. They are
scoped by namespace and mount, have nil API rules (superuser-only), and are
never exposed through the manifest. API keys, header values and OAuth
credentials are encrypted with PocketBase's `security.Encrypt` and the app's
encryption key (normally `PB_ENCRYPTION_KEY`). If that key is absent, the settings API refuses
to store a secret instead of writing plaintext. The admin API never returns a
key, header value or token; only a last-four hint and header names.

## 9. Browser client and examples

`frontend/client` (`@pocketbase-mcp/chat-client`) is a dependency-free
browser package:

- `ChatClient`: the HTTP API, with a fetch-based SSE reader. It takes a
  `getToken` callback and only sends the token in a header.
- `ChatController`: framework-neutral state (conversations, transcript,
  proposals, uploads, limits, errors) with `send`, `stop`, `decide`, `attach`
  and an **editor adapter** (`snapshot`, `validatePatch`, `applyPatch`) that
  applies approved editor proposals and reports the outcome.
- `bindAgentPanel`: connects the controller to the `<agent-panel>` /
  `<agent-page>` web components: approve and reject buttons, outcome notes,
  attachment chips and composer limits taken from `/status`.

See [frontend/client/README.md](../frontend/client/README.md) for the API.

`examples/chat/` has thin front ends in plain JS, Vue and Svelte. The plain
one also has a small note editor that shows editor proposals end to end.

**Demo without an API key.** The stock example's `demo` provider is offline
and canned: "search …" calls a search tool, "create Buy milk" proposes
`create_items`, "edit rev:0 Hello" proposes the `edit_note` editor tool, and
attached files are acknowledged. Environment variables for both examples:

| Variable | Effect |
|---|---|
| `MCP_CHAT_PROVIDER` | Legacy single provider: `openai`, `anthropic` or (stock only) `demo`; unset leaves chat off |
| `MCP_CHAT_MODEL`, `MCP_CHAT_API_KEY`, `MCP_CHAT_BASE_URL`, `MCP_CHAT_SYSTEM` | Legacy provider settings |
| `MCP_CHAT_PROVIDERS_FILE` | JSON file of catalog providers for first-boot seeding; mutually exclusive with `MCP_CHAT_PROVIDER` |
| `MCP_CHAT_MANAGE` | `true` starts managed mode with no seed; the settings API adds providers |
| `MCP_CHAT_PROPOSALS`, `MCP_CHAT_AUTO_APPROVE`, `MCP_CHAT_ARG_EDITS` | Proposal settings (proposals need `MCP_ALLOW_WRITES=true`) |
| `MCP_CHAT_UPLOADS`, `MCP_CHAT_MAX_UPLOAD_BYTES` | Upload settings |

## 10. Limits

Runner defaults (`chat.RunnerConfig`):

| Setting | Default |
|---|---|
| `MaxSteps` (model steps per turn, including tool round trips) | 8 |
| `TurnTimeout` | 2 minutes |
| `MaxConcurrentPerUser` / `MaxConcurrentGlobal` | 2 / 16 |
| `MaxHistoryMessages` | 50 |
| `ProposalTTL` | 24 hours |
| `MaxAttachmentBytesPerRequest` | 20 MiB |

Known gaps are listed in [roadmap.md](roadmap.md#known-gaps).
