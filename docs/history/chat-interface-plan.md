# Frontend chat interface for PocketBase MCP

Status: proposed addition to the module plan, revised 2026-11-17. Source inspection only; no application behavior or builds were tested, and no existing app was modified.

Revision: aligned with the module plan's no-added-libraries rule (hand-written provider adapters, dependency-free client) and its release tiers (v1.1 read-only chat, v1.2 proposals/editor/uploads).

## 1. Direction

Ship an optional Go chat service and a small framework-independent TypeScript client alongside the MCP server. Reuse the existing agent-panel and agent-page Web Components for rendering. Keep frontend network/state ownership outside those components, consistent with their existing contract.

Chat is an application-facing HTTP API, not a new MCP transport. Both chat model tool calls and external MCP calls enter the same registry, validation, principal checks and execution pipeline. Chat does not need to make an HTTP MCP connection back to itself. An MCP-only installation requires no model provider, chat storage or frontend package.

The frontend uses its existing PB session; it need not perform another OAuth login. Stock/fork OAuth selection remains exactly as in the main plan. LLM provider credentials and optional provider sign-in are a separate concern from PB user authentication.

## 2. Evidence from the three apps

### List Pocket

- [AIAssistant.vue](/Volumes/mini512/list_pocket/frontend/src/components/AIAssistant.vue:1) hosts Vue UI, account-scoped browser history, route/editor context, replay cursors and revision-checked editor proposals.
- [assistant/client.js](/Volumes/mini512/list_pocket/frontend/src/assistant/client.js:1) starts a turn, then reads a separate authenticated SSE endpoint.
- [handlers.go:61](/Volumes/mini512/list_pocket/cmd/handlers.go:61) mounts start/stream/cancel/decision endpoints under /mailapi/ai/assistant.
- [ai_assistant.go](/Volumes/mini512/list_pocket/cmd/ai_assistant.go:1) implements bounded workers, active-turn ownership, replay event IDs, pending proposals and a provider tool loop. Its current turn state is process-local.
- [assistant/AGENTS.md](/Volumes/mini512/list_pocket/frontend/src/assistant/AGENTS.md:1) also documents a full Agent page with server conversations; browser history is a side-panel choice, not a limitation of the whole app.

### Event Pocket

- [AiPanel.vue](/Volumes/mini512/WebstormProjects/Event%20Pocket/events_admin/app/components/ai/AiPanel.vue:1) provides a Vue/Nuxt side panel, thread selection, responsive sizing, active-screen context and editor hooks.
- [AiChat.vue](/Volumes/mini512/WebstormProjects/Event%20Pocket/events_admin/app/components/ai/AiChat.vue:1) hosts transcript/proposal behavior on custom agent elements.
- [aiClient.ts](/Volumes/mini512/WebstormProjects/Event%20Pocket/events_admin/app/lib/aiClient.ts:1) uses authenticated /ai APIs, streamed replies, files, thread management and provider settings.
- [ai/chat.go](/Volumes/mini512/WebstormProjects/Event%20Pocket/ai/chat.go:1) persists user-owned conversations, runs server tools and emits client proposals. Current admin-only policy is app-specific and should become a configurable policy reference.

### Static Dealmakers Event

- [AiCopilotPanel.svelte](/Volumes/mini512/WebstormProjects/Static%20Dealmakers%20Event/admin/src/lib/AiCopilotPanel.svelte:1) hosts agent-panel in Svelte, including context references, edit review and persisted per-hunk decisions.
- [streamChat.js](/Volumes/mini512/WebstormProjects/Static%20Dealmakers%20Event/admin/src/lib/ai/streamChat.js:1) reads POST SSE with a bearer token and AbortSignal.
- [ai_chat.go](/Volumes/mini512/WebstormProjects/Static%20Dealmakers%20Event/internal/hooks/ai_chat.go:267) associates persisted conversations with targets; its target-based lookup differs from Event Pocket's user-and-target lookup. New shared storage must define ownership explicitly.
- [portable spec](/Volumes/mini512/WebstormProjects/Static%20Dealmakers%20Event/docs/ai-assistant-portable-spec.md:1) is a useful UX reference, but the apps' actual protocols have evolved independently; it is not an established common wire contract.

### Existing shared frontend

All three package manifests reference agent-panel: List Pocket and Event Pocket pin v0.3.1; Static Dealmakers pins v0.2.2 at inspection time. Select and test a compatible release before migrations; do not silently update every app.

The component's [contract](/Volumes/mini512/Components/agent-panel/AGENTS.md:1) keeps it dependency-free, with host-owned state/network and properties/events for UI integration. Keep PB authentication, HTTP transport, provider code and persistence in the new client package, not in agent-panel. Reusable visual changes, if needed, belong in that component's own repository and release workflow.

## 3. Deliverables and boundaries

```text
Go root module
  chat/                optional orchestrator and authenticated API
  chat/provider/       streaming provider port and adapters
  chat/store/          conversation/turn/proposal/event storage port
PB adapter modules
  chat storage/schema integration, authentication and ownership enforcement
frontend/client/       separate npm package: typed HTTP client and state controller
examples/chat/         plain JS plus thin Vue/Nuxt and Svelte examples
```

The module's own tool registry (no MCP SDK) remains the shared source of tool descriptions and schemas, validated by the in-house schema subset. Provider adapters translate supported schemas/content and feed calls back into the same executor. Fail unsupported schema/content clearly instead of silently dropping validation.

The browser client owns transport, reconnect, event reduction and account changes. The host owns routing, theme, editor access, context construction and domain proposal rendering. Agent-panel owns drawing and composer behavior. No mandatory Vue, Nuxt, Svelte or state-library dependency.

## 4. Registration and configuration

Chat is disabled by default. Enable it on the registered MCP module with a ChatConfig specifying:

- API prefix, for example /api/mcp/chat/v1, distinct from /mcp and OAuth routes.
- Access policy reference; tokens limited to the module's accepted auth collections (superusers only if explicitly accepted), with any app-specific role gate explicit as a built-in or host policy.
- Provider resolver, model allowlist and default model.
- Persistence adapter and storage namespace; required server persistence by default when chat is enabled.
- System instructions and context resolvers registered by the host.
- Tool subset and execution/approval settings that may narrow the MCP manifest, never widen it.
- Turn/step/token/time/attachment limits and audit hooks.

Provider secrets stay server-side. Provider adapters are written in-house over net/http with a hand-written SSE parser; no vendor SDKs. Begin with the OpenAI-compatible streaming Chat Completions shape already present in these apps, and add the Anthropic Messages API as the second adapter early so the provider port is shaped by both tool-call models. Add other provider protocols as tested adapters. Existing app-specific ChatGPT/provider login integrations remain separate adapters; they are not stockoauth features and are not automatically portable.

## 5. Standard HTTP and event contract

Proposed v1 endpoints under the configurable prefix:

| Method/path | Behavior |
|---|---|
| GET /status | Availability, allowed models, attachment limits and supported features |
| GET /conversations | Paginated caller-owned threads, optionally filtered by authorized target |
| POST /conversations | Create a thread, optionally associated with a target |
| GET /conversations/{id} | Paginated canonical transcript and pending proposals |
| PATCH /conversations/{id} | Rename |
| DELETE /conversations/{id} | Delete after cancelling active work; apply attachment retention rules |
| POST /conversations/{id}/turns | Submit message/context/assets; return turn ID |
| GET /turns/{id} | Authoritative current state and terminal outcome |
| GET /turns/{id}/events?after=N | Authenticated SSE replay plus new events |
| POST /turns/{id}/cancel | Explicitly cancel queued/running work |
| POST /proposals/{id}/decision | Approve/reject a stored proposal; return authoritative state |
| POST /proposals/{id}/outcome | Report editor-local application outcome, distinct from server execution |
| POST /uploads | Upload authorized, bounded chat attachments |
| GET /uploads/{id} | Authenticated file retrieval, with ownership/target checks |

Use fetch-based SSE so authentication is supplied in headers. Never put a PB token in the stream URL. Use one event envelope with protocol version, monotonically increasing sequence, conversationId, turnId, type and typed data. Carry sequence in the SSE id field too.

Event types: turn.queued, turn.started, text.delta, tool.started, tool.completed, tool.failed, proposal.created, proposal.updated, usage.updated, turn.completed, turn.failed and turn.cancelled. Each turn has exactly one terminal state; usage is optional where provider support is absent. Bound event storage and distinguish replay expiry from a network interruption. (An `asset.created` event was planned for tool-produced files; it was dropped in v1.2 because nothing produces assets yet. Add it back with the first tool that does.)

Reconnect replays events; it never restarts a model turn or repeats a tool call. A creation idempotency key prevents duplicate turns when the POST response is lost. A cursor older than retained events returns a resync-required response; the client reloads canonical state. Heartbeats are non-persisted and do not advance sequence.

Closing a view or losing a stream connection merely detaches from the turn. Stop explicitly calls cancel, then aborts the local reader. Cancellation propagates to the provider and cancellable tools; it cannot roll back a completed side effect. Retry creates a new turn and must not imply replay-safe writes.

## 6. Ownership, persistence and workers

Partition records by host/app namespace, mount, auth collection ID and user record ID. Tenant-aware hosts add their configured tenant partition. Record IDs alone are insufficient identity keys. Targets are optional resource references, not a substitute for thread ownership.

Persist conversations, messages/parts, turns, tool calls, proposals/decisions, uploads and a bounded replay log in private namespaced storage. PB adapters provision schemas through explicit versioned migrations, reject conflicting existing schemas, and support a custom store. Do not silently take over an app's existing ai_* collections. Internal chat storage is not automatically an MCP-exposed data collection.

Recheck caller ownership and applicable target access on every history, stream, cancel, proposal and file request. Validate target/context references server-side. Do not copy Static Dealmakers' target-only sharing semantics into a universal default; shared team conversations need a separate explicit policy.

Initially allow one active turn per conversation and configurable per-user/global worker bounds. Persist state transitions and use compare-and-swap/transactions for decisions and active-turn claims. v1.1 and v1.2 are single-instance and declare that limitation; multi-instance mode (durable leases and event delivery to the instance serving SSE) is later work. Do not rely on a package-global map as durable state.

On restart or lease loss, interrupted turns become interrupted/failed; do not automatically rerun uncertain mutations. Persist an execution intent and outcome; if a crash occurs after a side effect but before its result is saved, mark outcome unknown and require reconciliation. Do not claim exactly-once business effects without a tool-specific transactional or idempotent implementation.

Use short-lived request/worker credentials only in memory. A running turn outlives the POST that started it, so its worker holds the caller's token in memory for the turn's duration; cap turn duration below the token's remaining lifetime and fail the turn rather than outlive it. Revalidate the normal PB identity before each tool and resumed approval. Do not persist full PB tokens in conversations, logs or browser history. User reauthentication is required when a token expires while waiting for approval; a replayed transcript cannot authorize new work.

## 7. One registry, two invocation surfaces

Extend ToolSpec with declared channels (mcp, chat, or both), execution location (server or editor), and approval policy. Core collection and server custom tools can serve both surfaces. Browser-local editor actions are chat-only unless a real remote-client handler is separately provided; external MCP clients cannot execute an arbitrary browser editor callback.

**Current state (2026-11-18):** chat v1.1 and v1.2 are built. The module accepts channels `mcp` (the default), `chat` and `both`. A `chat` tool is never listed or callable over MCP, and chat offers read-effect tools directly plus write-effect tools as proposals (when `ChatOptions.Proposals` is on), all through the same policy pipeline as MCP (`listTools`/`callTool` with a channel argument).

Execution location `editor` is accepted under the editor `ToolSpec` contract in [custom.go](/Volumes/mini512/Components/Pocketbase-MCP/custom.go) `validateEditorTool`: the tool must be **chat-only** (`Channels: "chat"`), declare **no handler** (its effect is a draft patch the browser applies), declare **no auto approval** (`Approval` empty, so every change is reviewed), be a write effect, and require string `editorId` and `baseRevision` properties in its input schema. An editor tool is offered only while proposals are enabled; approval authorizes the browser to apply the patch and never performs a server write. A `server` tool with a handler is unchanged.

Read tools can run directly under the shared policy engine. For chat, mutations default to proposals requiring review, matching these apps. The host may configure automatic execution for eligible tools, but client/model requests cannot relax server policy. AllowWrites and PB/application authorization still apply after approval.

Each proposal binds its owner, target, exact validated arguments, tool identity/version, manifest revision and effect summary. Approval sends the proposal ID and decision, not an unchecked replacement tool call. If review allows editing arguments, validate and bind a new revision before execution. Concurrent decisions use atomic state transitions and return the stored result rather than run again.

Recheck current authorization, manifest, schema and record preconditions at application time. Changing permission while a proposal is pending must take effect. Expired/rejected/stale proposals cannot execute.

For server effects, the service executes and records the result. For editor effects, approval authorizes a client-local draft patch; the client reports applied/stale/failed with editor revision. That report is an acknowledgement, not proof of a PB write or permission to perform one. Saving continues through the app's normal protected API. Never display saved/sent/published solely because a model emitted a proposal or the browser reported local application.

Editor patches use explicit target/editor IDs, before revision and exact-match or stable-block operations. Validate again immediately before applying; stale context requests a fresh proposal. Undo belongs to the host editor and must not overwrite intervening edits. Persist per-hunk decisions when partial application is supported; never imply the whole proposal applied.

## 8. Browser client and host integration

Provide a dependency-free typed ChatClient (fetch plus an in-house SSE reader) and framework-neutral controller exposing conversation state, transcript entries, pending proposals, current turn, limits and errors. Methods include list/create/rename/delete, send, attach, reconnect, stop, decide and reportOutcome.

Take a getToken callback rather than capturing a token once. Namespace local state by app URL, mount, auth collection and user ID; clear it on logout/account switch. Server history is canonical by default. Optional browser-only history is a deliberate adapter mode with different recovery semantics, not a silent fallback on storage failure.

Map controller state to agent-panel entries/running/error/models/references, and map agent-submit/agent-stop/agent-action back to the client. Use agent-page for a full chat route. Supply thin framework examples rather than three independent clients.

Host context adapter:

```text
captureContext() -> route, selected target/record refs, bounded filters/selection
editor.snapshot() -> editor ID, target, revision, unsaved content
editor.validatePatch(proposal) -> applicable/stale and review preview
editor.applyPatch(proposal) -> new revision and optional undo token
editor.undo(token) -> result after checking current revision
capturePreview() -> optional explicitly requested assets
```

Context references are resolved through the same access policy as tools. Treat client context, attachments and retrieved records as data, never as authority or system instructions. Do not send page snapshots or upload content to a model merely because the sidebar opened. Send on explicit submission/context action. Cap history by provider budget while preserving tool-call/result groups; never reconstruct authoritative server tool outcomes from client-submitted history.

Reuse component themes, composer, attachment handling, mobile behavior and accessibility. Keep sanitized Markdown and safe links. Support keyboard review, Stop, visible tool progress, stale edit errors and account-safe history. Avoid dumping protocol internals into the normal chat transcript.

## 9. Attachments and model context

Use authenticated uploads with explicit MIME/size/count/model capability limits. Durable references use upload IDs; transient blob URLs are browser-only. Downloads and model-context resolution repeat ownership/target checks. Do not expose protected PB file tokens as public links or fetch arbitrary user-supplied URLs with server credentials.

Start with text, supported images and PDF using tested provider behavior. Broader office-document conversion is optional and advertised only when implemented. A browser accept list is not server validation. Attachments and screen snapshots should have clear retention/deletion policy, and deleting a chat must not delete a referenced application's original asset.

## 10. Delivery sequence and acceptance

Tiering: steps 1–3 and 5 (read-only tools) form v1.1; step 4, uploads (section 9) and the write-capable parts of step 5 form v1.2. Step 2 depends on module milestone 2; step 4 depends on module milestone 5.

1. Specify/version the wire schemas, state machine and ownership keys; implement a provider-free scripted test fixture and headless client reducer.
2. Add the optional chat service to stock/fork fixtures with persisted threads, resumable turns, worker limits and the shared collection tool executor. Gate: same user's MCP and chat calls observe the same data restrictions.
3. Integrate provider streaming and custom server tools; test tool/result pairing, stop, rate/timeout handling and transcript recovery. Provider-specific unsupported features fail explicitly.
4. Add proposals and editor adapters; test double approval, permission revocation, stale drafts, edited arguments, partial application, unknown server outcomes and restart recovery.
5. Demonstrate plain JS, Vue/Nuxt and Svelte hosts using the existing agent-panel package. Test fragmented SSE/UTF-8 frames, missing terminal events, reconnect without duplicate writes, account switching and uploaded-file isolation.
6. Migrate one bounded screen in each existing app under separate authorized implementation work. Preserve app permission policies, prompts, tool coverage, thread ownership and unsaved-editor behavior. Provide an explicit migration/import path for old history; keep original data until verified. Do not replace all three apps' production assistants in the initial module change.

**As built (2026-11-18):** v1.1 (steps 1–3 and 5, read-only) and v1.2 (steps 4, 9 and the write/editor parts of 5) are implemented and uncommitted: `chat/` (including the proposal model, state machine, decision/outcome routes and uploads), `chat/provider/{openai,anthropic,scripted}`, `chat_bridge.go`, PocketBase chat/proposal/upload stores in both adapters, `frontend/client/` (client, controller, editor adapter and agent-panel binding) and `examples/chat/{plain,vue,svelte}` with the stock example's offline demo provider.

The v1.2 design decision: **proposals outlive turns, and approval executes inside the decision request.** A proposal is stored with its owner, tool identity, manifest revision, validated arguments and effect summary. The decided request re-authenticates the caller, re-checks the tool, manifest and arguments, and runs the effect within that request (bounded by `DecisionTimeout`), so no worker holds a caller token while waiting for review and a token expiring mid-approval cannot leave a pending worker armed. Exactly one decision wins the compare-and-swap; a replay returns the stored state; a crash after a side effect but before its result is stored records `unknown` and is never rerun automatically. Editor approval executes nothing on the server: it authorizes a browser-local draft patch whose `applied`/`stale`/`failed` report is an acknowledgement, never proof of a save.

Known gaps: the per-conversation upload count is checked before storage (concurrent uploads can overshoot it by a bounded amount, while the per-turn count is re-checked on the turn); the conversation target is not re-evaluated while the runner reads attachments; generated write arguments are validated by PocketBase at execution, not at proposal time; the `proposed` audit event precedes the proposal store write; the provider request body limit is 32 MiB; no real-browser check yet; Nuxt untried; the fork's new collections are untested on MySQL. v1.1/v1.2 remain single-instance per namespace and mount.

Chat ships as v1.1/v1.2, after the core MCP release, not as a vague resources/prompts follow-up. It depends on the registry and authorization contract. Existing apps remain reference implementations until migration is requested.
