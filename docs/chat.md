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

In a PocketBase adapter, call `EnableChat` before `RegisterRoutes`:

```go
err := srv.EnableChat(pb.ChatConfig{
    Options:  pbmcp.ChatOptions{Namespace: "my-app", Proposals: true, Uploads: true},
    Provider: provider, // openai.New(...) or anthropic.New(...)
    Model:    "gpt-4.1",
    System:   "You help users manage their listings.",
})
```

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
are no vendor SDKs, and API keys stay on the server.

| Package | Speaks |
|---|---|
| `chat/provider/openai` | OpenAI-compatible streaming Chat Completions (also works with gateways via `BaseURL`) |
| `chat/provider/anthropic` | Anthropic Messages API |
| `chat/provider/scripted` | A scripted provider for tests |

Both real adapters support tool calls and attachments (images, PDF, text).
Request bodies are capped at 32 MiB to fit base64 attachments. A feature a
provider doesn't support fails clearly instead of being dropped.

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
| `MCP_CHAT_PROVIDER` | `openai`, `anthropic` or (stock only) `demo`; unset leaves chat off |
| `MCP_CHAT_MODEL`, `MCP_CHAT_API_KEY`, `MCP_CHAT_BASE_URL`, `MCP_CHAT_SYSTEM` | Provider settings |
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
