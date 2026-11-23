# PocketBaseMCPChat

Native Swift 6 client and SwiftUI chat interface for the PocketBase MCP chat HTTP API (protocol v1), mirroring [`@pocketbase-mcp/chat-client`](../client/README.md) and `agent-panel`'s `<agent-page>`.

## Products

- **`PBMCPChat`** (Foundation only, no SwiftUI dependency):
  - `ChatClient` — `URLSession` async/await transport with per-request `getToken` (`Authorization` header only), `Idempotency-Key` support, multipart attachment upload, and `URLSession.bytes` SSE streaming.
  - `SSEParser` — incremental, UTF-8 byte-safe Server-Sent Events parser handling LF/CRLF/CR line endings, heartbeats, persistent `id`, and UTF-8 `maxEventBytes` limits.
  - `ChatReducer` — pure value-type port of `chat/reducer.go` and `reducer.mjs` with replay deduplication, gap detection, and strict proposal validation.
  - `ChatController` — `@MainActor @Observable` controller with list sync, reconnect + idle backoff, `resync_required` reload & polling, proposals + editor adapter, attachment validation & uploads, catalog choices & reasoning effort, context window & compaction, per-turn page context, and account switching.
  - Pure presentation helpers (`entriesFromState`, `proposalEntryOf`, `errorCode`, `errorText`, `proposalErrorText`, `modelsFromState`, `effortOptionFromState`, `contextUsageFromState`).
- **`PBMCPChatUI`** (SwiftUI + `MarkdownUI`):
  - `ChatPage` — `NavigationSplitView`-friendly full-page view binding a `ChatController` with a conversation sidebar, inline title rename, streaming transcript, collapsible tool rows with JSON details, proposal cards (Approve/Reject, state badges, warnings, conflict/refusal notices), usage readout, error banner, `transcriptEnd` slot, and empty state.
  - `ChatPanel` — compact dockable or `floating` side-panel view (equivalent of `<agent-panel>` v0.4.1 / v0.5.1) with a header conversation-switcher popover (`conversationsMenu`), New chat & Close controls, `headerStart` / `headerActions` / `switcherEnd` / `transcriptEnd` slots, and optional empty-state `suggestions`.
  - `ComposerView` — multi-line composer with `⌘↩` send, file picker, drag-and-drop & paste attachments, mention trigger, grouped model menu & reasoning effort picker, context ring + Compact popover, cancel while streaming, and `disabled` state.
  - `SafeMarkdownView` / `SafeMarkdownSanitizer` — renders GFM tables, code blocks, and line breaks via `MarkdownUI`; raw HTML in model output is shown as literal text and links only open for `http`, `https`, `mailto`, and `#`.

## Usage

```swift
import PBMCPChat
import PBMCPChatUI
import SwiftUI

let client = ChatClient(
    baseURL: URL(string: "https://example.com")!,
    getToken: { try await auth.validToken() }
)

let controller = try ChatController(
    client: client,
    storageKey: ChatAccountKey(
        appUrl: "https://example.com",
        mount: "/",
        authCollectionId: "users",
        userId: currentUser.id
    ),
    context: { ["app": .string("my-app"), "screen": .string("assistant")] }
)

struct AssistantView: View {
    let controller: ChatController

    var body: some View {
        ChatPage(
            controller: controller,
            placeholder: "Ask anything…",
            emptyTitle: "How can I help?",
            emptyText: "Every change the assistant suggests waits for your approval.",
            renamable: true,
            onLinkTap: { url in NSWorkspace.shared.open(url) }
        ) {
            // Optional transcriptEnd slot
        }
        .task {
            await controller.initialize()
        }
    }
}

// Compact / floating side-panel (equivalent of <agent-panel conversations-menu floating closable>):
struct SidePanelView: View {
    let controller: ChatController
    let onClose: () -> Void
    let onOpenFullPage: () -> Void

    var body: some View {
        ChatPanel(
            controller: controller,
            heading: "Assistant",
            suggestions: ["Summarize today's tasks", "Draft a status update"],
            conversationsMenu: true,
            floating: true,
            onClose: onClose,
            headerStart: { Image(systemName: "sparkles").foregroundStyle(.secondary) },
            headerActions: { EmptyView() },
            switcherEnd: { Button("Open in full page", action: onOpenFullPage) },
            transcriptEnd: { EmptyView() }
        )
    }
}
```

## Guarantees

Matches `@pocketbase-mcp/chat-client`:

- **No token in URLs.** `getToken()` runs before every request and is sent only in the `Authorization` header.
- **Idempotent send.** Network failures before a response retry up to three times with the same `Idempotency-Key`.
- **Reconnect without replaying turns.** Streams reconnect from `lastSeq` with idle timeout and exponential backoff.
- **Resync is explicit.** A 409 `resync_required` or reducer gap reloads canonical state and polls until terminal.
- **Account switch clears state.** Account keys are percent-encoded per field; switching aborts in-flight work, clears state synchronously, and ignores late responses.
- **Nothing is persisted.** Server history is canonical.
- **No implied saves.** A proposal, approval, or editor outcome is never presented as a saved write.

## Tests

```sh
swift test --package-path frontend/swift
```

Conformance fixtures are shared with the JS client (`frontend/client/test/fixtures/` and `frontend/swift/Tests/PBMCPChatTests/Fixtures/`).
