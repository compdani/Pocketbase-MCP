import Foundation
import MarkdownUI
import SwiftUI
import Testing
@testable import PBMCPChat
@testable import PBMCPChatUI

@Suite("PBMCPChatUI ViewModel & Markdown Safety")
@MainActor
struct ChatPageUITests {
    private final class StubTransport: ChatTransport, @unchecked Sendable {
        var statusResult = ChatStatus(
            version: "1",
            models: ["model-a"],
            choices: [
                ChatModelChoice(id: "openai::gpt-5", provider: "openai", model: "gpt-5", label: "GPT-5", group: "OpenAI", isDefault: true, efforts: ["low", "high"]),
                ChatModelChoice(id: "anthropic::sonnet", provider: "anthropic", model: "sonnet", label: "Sonnet", group: "Anthropic"),
            ],
            defaultChoice: "openai::gpt-5",
            features: ChatFeatures(ready: true, proposals: true, uploads: true, context: true, contextManagement: true, compaction: true),
            limits: ChatLimits(maxMessageChars: 16000, maxUploadBytes: 1024, maxAttachmentsPerTurn: 2, attachmentTypes: ["text/plain", "image/png"])
        )
        var shouldFailSubmit = false

        func status() async throws -> ChatStatus { statusResult }
        func listConversations(limit: Int?, cursor: String?, target: String?) async throws -> ChatConversationList {
            ChatConversationList(items: [ChatConversation(id: "c1", title: "Demo", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z")])
        }
        func createConversation(title: String?, target: String?) async throws -> ChatConversation {
            ChatConversation(id: "c-new", title: title ?? "New chat", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z")
        }
        func getConversation(_ id: String, limit: Int?, cursor: String?) async throws -> ChatConversationDetail {
            ChatConversationDetail(
                conversation: ChatConversation(id: id, title: "Demo", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z"),
                context: ChatContextStatus(tokens: 72_000, window: 100_000, canCompact: true)
            )
        }
        func renameConversation(_ id: String, title: String) async throws -> ChatConversation {
            ChatConversation(id: id, title: title, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z")
        }
        func deleteConversation(_ id: String) async throws {}
        func getContext(_ id: String) async throws -> ChatContextStatus {
            ChatContextStatus(tokens: 72_000, window: 100_000, canCompact: true)
        }
        func compactConversation(_ id: String) async throws -> ChatCompactResult {
            ChatCompactResult(compacted: true, context: ChatContextStatus(tokens: 20_000, window: 100_000, canCompact: true))
        }
        func importConversation(sourceId: String, title: String?, createdAt: String?, messages: [ChatImportMessage]) async throws -> ChatImportResult {
            ChatImportResult(conversation: ChatConversation(id: "c1", title: "Demo"), created: true)
        }
        func submitTurn(_ conversationId: String, text: String, provider: String?, model: String?, effort: String?, attachments: [String]?, context: [String: JSONValue]?, idempotencyKey: String?) async throws -> ChatSubmitResult {
            if shouldFailSubmit {
                throw ChatApiError(status: 400, code: "turn_refused", message: "Refused")
            }
            return ChatSubmitResult(turn: ChatTurn(id: "t1", conversationId: conversationId, state: .queued), created: true)
        }
        func getTurn(_ id: String) async throws -> ChatTurn { ChatTurn(id: id, conversationId: "c1", state: .completed) }
        func cancelTurn(_ id: String) async throws -> ChatTurn { ChatTurn(id: id, conversationId: "c1", state: .cancelled) }
        func getProposal(_ id: String) async throws -> ChatProposal { ChatProposal(id: id, conversationId: "c1", tool: "create_task") }
        func decideProposal(_ id: String, decision: ProposalDecision, revision: Int, arguments: JSONValue?) async throws -> ProposalDecisionResult {
            ProposalDecisionResult(proposal: ChatProposal(id: id, conversationId: "c1", tool: "create_task", state: decision == .approve ? .succeeded : .rejected, revision: revision + 1))
        }
        func reportOutcome(_ id: String, revision: Int, status: ProposalOutcomeStatus, editorRevision: String?, hunks: [ChatHunkOutcome]?, message: String?) async throws -> ProposalDecisionResult {
            ProposalDecisionResult(proposal: ChatProposal(id: id, conversationId: "c1", tool: "edit_note", location: .editor, state: .applied, revision: revision + 1))
        }
        func uploadFile(conversationId: String, file: ChatUploadFile, name: String?) async throws -> ChatUpload {
            ChatUpload(id: "u1", conversationId: conversationId, name: name ?? file.name, mime: file.mimeType, kind: "text", size: file.size, sha256: "x", createdAt: "x")
        }
        func getUploadMeta(_ id: String) async throws -> ChatUpload {
            ChatUpload(id: id, conversationId: "c1", name: "a.txt", mime: "text/plain", kind: "text", size: 1, sha256: "x", createdAt: "x")
        }
        func downloadUpload(_ id: String) async throws -> Data { Data("ok".utf8) }
        func streamTurn(_ turnId: String, after: Int, onEvent: (@Sendable (ChatEvent) async throws -> Void)?, onHeartbeat: (@Sendable (String) -> Void)?) async throws -> StreamTurnResult {
            try await onEvent?(ChatEvent(seq: 1, conversationId: "c1", turnId: turnId, type: "turn.started"))
            try await onEvent?(ChatEvent(seq: 2, conversationId: "c1", turnId: turnId, type: "text.delta", data: .object(["text": .string("Done.")])))
            try await onEvent?(ChatEvent(seq: 3, conversationId: "c1", turnId: turnId, type: "turn.completed"))
            return StreamTurnResult(lastSeq: 3, terminal: true)
        }
    }

    @Test("proposal card states have distinct honest text, badges, and canDecide flags")
    func proposalCardStates() {
        for state in ChatProposalState.allCases {
            let proposal = ChatProposal(
                id: "p-\(state.rawValue)",
                conversationId: "c1",
                tool: "create_task",
                location: state == .applied || state == .approved || state == .stale ? .editor : .server,
                summary: "Create task",
                state: state,
                revision: 2,
                failure: state == .failed ? "Permission denied." : nil,
                warnings: ["Will notify assignees."]
            )
            let entry = proposalEntryOf(ChatProposalEntry(proposal: proposal))
            #expect(entry != nil)
            #expect(entry?.canDecide == (state == .pending))
            #expect(entry?.status.isEmpty == false)
            #expect(entry?.stateLabel.isEmpty == false)
            #expect(entry?.warnings == ["Will notify assignees."])
            // Never claim something is "saved" on approval or editor apply.
            if state == .approved || state == .applied {
                #expect(entry?.status.contains("not saved") == true || entry?.status.contains("applying") == true)
            }
            if state == .failed {
                #expect(entry?.status.contains("Permission denied.") == true)
            }
        }
    }

    @Test("composer enable/disable respects host disabled, features.ready, and draft state")
    func composerEnableDisable() async throws {
        let transport = StubTransport()
        let controller = try ChatController(client: transport, sync: nil)
        let vm = ChatPageViewModel(controller: controller)

        // Provider missing disables composer and shows no_provider error.
        transport.statusResult.features.ready = false
        await controller.initialize()
        vm.syncWithControllerState()
        #expect(vm.isComposerDisabled == true)
        #expect(vm.canSend == false)
        #expect(vm.bannerError.contains("AI isn't set up yet"))

        // Provider becomes ready -> composer enables.
        transport.statusResult.features.ready = true
        await controller.initialize()
        vm.syncWithControllerState()
        #expect(vm.isComposerDisabled == false)
        #expect(vm.canSend == false) // empty draft

        vm.draft = "Hello"
        #expect(vm.canSend == true)

        // Host offline disables composer even with draft.
        vm.isHostDisabled = true
        #expect(vm.isComposerDisabled == true)
        #expect(vm.canSend == false)

        // Back online, effort menu appears for GPT-5 and disappears for Sonnet.
        vm.isHostDisabled = false
        #expect(vm.effortOption?.choices.map(\.value) == ["", "low", "high"])
        vm.selectModel("anthropic::sonnet")
        #expect(vm.effortOption == nil)

        controller.dispose()
    }

    @Test("composer restores draft on failed/refused submit and enforces attachment text requirement")
    func composerSubmitAndDraftRestore() async throws {
        let transport = StubTransport()
        let controller = try ChatController(client: transport, sync: nil)
        await controller.initialize()
        await controller.open("c1")

        let vm = ChatPageViewModel(controller: controller)
        vm.addAttachmentFiles([ChatUploadFile(data: Data("hi".utf8), name: "note.txt", mimeType: "text/plain")])
        #expect(vm.pendingAttachments.count == 1)

        // Attachments without text fail locally and keep the attachment.
        let noText = await vm.submit()
        if case .failed(_, let code, _) = noText {
            #expect(code == "attachment_text_required")
        } else {
            Issue.record("Expected attachment_text_required failure")
        }
        #expect(vm.pendingAttachments.count == 1)

        // Refused turn restores draft and marks chip error.
        transport.shouldFailSubmit = true
        vm.draft = "Try this"
        let refused = await vm.submit()
        if case .refused(_, let code, _) = refused {
            #expect(code == "turn_refused")
        } else {
            Issue.record("Expected turn_refused outcome")
        }
        #expect(vm.draft == "Try this")
        #expect(vm.pendingAttachments[0].error != nil)

        // Succeeding clears draft and attachments.
        transport.shouldFailSubmit = false
        let accepted = await vm.submit()
        if case .accepted = accepted {
            #expect(vm.draft.isEmpty)
            #expect(vm.pendingAttachments.isEmpty)
        } else {
            Issue.record("Expected accepted outcome")
        }
        await controller.whenSettled()
        controller.dispose()
    }

    @Test("mention trigger opens on @query and closes on whitespace")
    func mentionTriggerDetection() throws {
        let controller = try ChatController(client: StubTransport(), sync: nil)
        let vm = ChatPageViewModel(controller: controller)

        vm.draft = "Check @tas"
        #expect(vm.mentionState == ChatMentionState(isOpen: true, query: "tas"))

        vm.insertMention("@Task-42")
        #expect(vm.draft == "Check @Task-42 ")
        #expect(vm.mentionState == .closed)

        vm.draft = "email@example.com"
        #expect(vm.mentionState == .closed)
        controller.dispose()
    }

    @Test("context ring visibility respects 60% threshold")
    func contextRingThreshold() async throws {
        let transport = StubTransport()
        let controller = try ChatController(client: transport, sync: nil)
        await controller.initialize()
        await controller.open("c1")

        let vm = ChatPageViewModel(controller: controller)
        #expect(vm.contextUsage?.percent == 72)
        #expect(vm.shouldShowContextRing == true)
        controller.dispose()
    }

    @Test("Markdown safety: raw HTML tags (<script>, <img onerror>) render as literal text, not HTML")
    func markdownSafetyRawHTML() {
        let xssScript = "Hello <script>alert(1)</script> world"
        let sanitizedScript = SafeMarkdownSanitizer.sanitize(xssScript)
        // `<script>` and `</script>` are backslash-escaped so CommonMark renders them as literal visible text.
        #expect(sanitizedScript == "Hello \\<script>alert(1)\\</script> world")
        #expect(MarkdownContent(sanitizedScript).renderPlainText().contains("<script>alert(1)</script>"))

        let xssImg = "Look: <img src=x onerror=alert(1)>"
        let sanitizedImg = SafeMarkdownSanitizer.sanitize(xssImg)
        #expect(sanitizedImg == "Look: \\<img src=x onerror=alert(1)>")
        #expect(MarkdownContent(sanitizedImg).renderPlainText().contains("<img src=x onerror=alert(1)>"))

        // Inline code and fenced code blocks stay verbatim (already rendered literally as code).
        let inlineCode = "Use `<script>alert(1)</script>` safely"
        #expect(SafeMarkdownSanitizer.sanitize(inlineCode) == inlineCode)

        let fenced = "```html\n<img src=x onerror=alert(1)>\n```"
        #expect(SafeMarkdownSanitizer.sanitize(fenced) == fenced)

        let indented = "    <script>alert(1)</script>"
        #expect(SafeMarkdownSanitizer.sanitize(indented) == indented)
    }

    @Test("Markdown safety: only http, https, mailto, and # links are allowed (inline and reference-style)")
    func markdownSafetyLinks() {
        #expect(SafeMarkdownSanitizer.isAllowedURLString("https://example.com/a"))
        #expect(SafeMarkdownSanitizer.isAllowedURLString("http://localhost:8090"))
        #expect(SafeMarkdownSanitizer.isAllowedURLString("mailto:user@example.com"))
        #expect(SafeMarkdownSanitizer.isAllowedURLString("#section-1"))

        #expect(!SafeMarkdownSanitizer.isAllowedURLString("javascript:alert(1)"))
        #expect(!SafeMarkdownSanitizer.isAllowedURLString("JAVASCRIPT:alert(1)"))
        #expect(!SafeMarkdownSanitizer.isAllowedURLString("data:text/html,<script>alert(1)</script>"))
        #expect(!SafeMarkdownSanitizer.isAllowedURLString("file:///etc/passwd"))
        #expect(!SafeMarkdownSanitizer.isAllowedURLString("vbscript:msgbox(1)"))

        let badLink = "Click [here](javascript:alert(1)) or [ok](https://example.com)"
        let sanitized = SafeMarkdownSanitizer.sanitize(badLink)
        #expect(sanitized == "Click here or [ok](https://example.com)")

        let badAutolink = "See <javascript:alert(1)> and <https://example.com>"
        let sanitizedAuto = SafeMarkdownSanitizer.sanitize(badAutolink)
        #expect(sanitizedAuto == "See \\<javascript:alert(1)> and <https://example.com>")

        // Reference-style links, images, and shortcut links with disallowed schemes are stripped.
        let refLink = "[a][1]\n\n[1]: javascript:alert(1)"
        #expect(SafeMarkdownSanitizer.sanitize(refLink) == "a")

        let refImg = "![x][1]\n\n[1]: data:text/html;base64,abc"
        #expect(SafeMarkdownSanitizer.sanitize(refImg) == "x")

        let shortcutRef = "[click]\n\n[click]: javascript:alert(1)"
        #expect(SafeMarkdownSanitizer.sanitize(shortcutRef) == "click")

        // Allowed reference links remain intact.
        let okRef = "[docs][1]\n\n[1]: https://example.com"
        #expect(SafeMarkdownSanitizer.sanitize(okRef) == okRef)
    }

    @Test("downloadAttachment failure surfaces a plain-language banner error")
    func downloadAttachmentFailure() async throws {
        let transport = StubTransport()
        let controller = try ChatController(client: transport, sync: nil)
        let vm = ChatPageViewModel(controller: controller)
        vm.onAttachmentDownload = { _, _ in
            throw ChatApiError(status: 403, code: "forbidden", message: "raw server text")
        }
        await vm.downloadAttachment(ChatAgentReference(type: "attachment", id: "u1", label: "secret.pdf", isAttachment: true))
        #expect(vm.bannerError == "You do not have access to this conversation.")
        controller.dispose()
    }

    @Test("ChatPanel resolves heading vs conversation switcher title like <agent-panel>")
    func chatPanelHeaderTitleResolution() {
        let convs = [
            ChatConversation(id: "c1", title: "Q4 Planning"),
            ChatConversation(id: "c2", title: ""),
        ]

        // No conversations yet -> shows fixed heading.
        #expect(
            ChatPanel<EmptyView, EmptyView, EmptyView, EmptyView>.resolvedHeaderTitle(
                heading: "Assistant",
                conversationsMenu: true,
                conversations: [],
                activeConversationId: nil
            ) == "Assistant"
        )

        // Switcher disabled -> always shows fixed heading.
        #expect(
            ChatPanel<EmptyView, EmptyView, EmptyView, EmptyView>.resolvedHeaderTitle(
                heading: "Assistant",
                conversationsMenu: false,
                conversations: convs,
                activeConversationId: "c1"
            ) == "Assistant"
        )

        // Switcher enabled with active conversation -> shows active title (or Untitled).
        #expect(
            ChatPanel<EmptyView, EmptyView, EmptyView, EmptyView>.resolvedHeaderTitle(
                heading: "Assistant",
                conversationsMenu: true,
                conversations: convs,
                activeConversationId: "c1"
            ) == "Q4 Planning"
        )
        #expect(
            ChatPanel<EmptyView, EmptyView, EmptyView, EmptyView>.resolvedHeaderTitle(
                heading: "Assistant",
                conversationsMenu: true,
                conversations: convs,
                activeConversationId: "c2"
            ) == "Untitled"
        )

        // Switcher enabled in draft mode (conversations exist, none open) -> "New chat".
        #expect(
            ChatPanel<EmptyView, EmptyView, EmptyView, EmptyView>.resolvedHeaderTitle(
                heading: "Assistant",
                conversationsMenu: true,
                conversations: convs,
                activeConversationId: nil
            ) == "New chat"
        )
    }
}
