import Foundation
import Testing
@testable import PBMCPChat

@Suite("ChatSync & Presentation")
@MainActor
struct ChatSyncAndPresentationTests {
    private func row(_ id: String, _ createdAt: String, _ title: String? = nil) -> ChatConversation {
        ChatConversation(id: id, title: title ?? id, createdAt: createdAt, updatedAt: createdAt)
    }

    @Test("mergeConversationPage replaces covered rows and keeps older loaded ones")
    func mergePageBehavior() {
        let loaded = [
            row("c5", "2026-01-05T00:00:00Z", "old title"),
            row("c4", "2026-01-04T00:00:00Z"),
            row("c3", "2026-01-03T00:00:00Z"),
            row("c2", "2026-01-02T00:00:00Z"),
            row("c1", "2026-01-01T00:00:00Z"),
        ]
        let merged = mergeConversationPage(loaded, page: ChatConversationList(
            items: [
                row("c6", "2026-01-06T00:00:00Z"),
                row("c5", "2026-01-05T00:00:00Z", "new title"),
                row("c3", "2026-01-03T00:00:00Z"),
            ],
            nextCursor: "p2"
        ))
        #expect(merged.conversations.map(\.id) == ["c6", "c5", "c3", "c2", "c1"])
        #expect(merged.conversations[1].title == "new title")
        #expect(merged.nextCursor == nil)
        #expect(merged.covers("c4") == true)
        #expect(merged.covers("c1") == false)

        let whole = mergeConversationPage(loaded, page: ChatConversationList(
            items: [row("c1", "2026-01-01T00:00:00Z")],
            nextCursor: ""
        ))
        #expect(whole.conversations.map(\.id) == ["c1"])
        #expect(whole.nextCursor == "")
    }

    @Test("refreshConversations clears active view only when server returns 404")
    func refreshClearsOnlyOn404() async throws {
        let client = MockTransport()
        client.pageResult = ChatConversationList(items: [row("c1", "2026-01-01T00:00:00Z")])
        client.detailResult = ChatConversationDetail(conversation: row("c1", "2026-01-01T00:00:00Z"))
        let controller = try ChatController(client: client, sync: nil)
        await controller.initialize()
        await controller.open("c1")

        client.pageResult = ChatConversationList(items: [])
        await controller.refreshConversations()
        #expect(controller.state.activeConversationId == "c1")
        #expect(controller.state.conversations.map(\.id) == ["c1"])

        client.onGetConversation = { _, _, _ in
            throw ChatApiError(status: 404, code: "not_found", message: "gone")
        }
        await controller.refreshConversations()
        #expect(controller.state.activeConversationId == nil)
        #expect(controller.state.conversations.isEmpty)
        controller.dispose()
    }

    @Test("controllers for same account sync over bus; other accounts do not")
    func crossControllerSameAccountSync() async throws {
        let acct1 = ChatAccountKey(appUrl: "https://sync.test", mount: "/", authCollectionId: "users", userId: "u1")
        let acct2 = ChatAccountKey(appUrl: "https://sync.test", mount: "/", authCollectionId: "users", userId: "u2")
        let clientA = MockTransport()
        let clientB = MockTransport()
        let clientOther = MockTransport()

        let sync = ChatSyncOptions(channel: true, focus: false, intervalMs: 0)
        let a = try ChatController(client: clientA, storageKey: acct1, sync: sync)
        let b = try ChatController(client: clientB, storageKey: acct1, sync: sync)
        let other = try ChatController(client: clientOther, storageKey: acct2, sync: sync)

        await a.initialize()
        await b.initialize()
        await other.initialize()

        clientA.createResult = row("c1", "2026-01-01T00:00:00Z")
        clientB.pageResult = ChatConversationList(items: [row("c1", "2026-01-01T00:00:00Z")])
        clientOther.pageResult = ChatConversationList(items: [row("x1", "2026-01-01T00:00:00Z")])

        _ = await a.newConversation()
        try await Task.sleep(nanoseconds: 400_000_000)

        #expect(b.state.conversations.map(\.id) == ["c1"])
        #expect(other.state.conversations.isEmpty)

        a.dispose()
        b.dispose()
        other.dispose()
    }

    @Test("entriesFromState maps messages, tools, live turn remainder, context marker, notices, and proposals")
    func entriesFromStateMapping() {
        let messages: [ChatMessage] = [
            testMessage("m1", role: "user", text: "Find tasks", turnId: "t0", parts: [
                ChatPart(type: "text", text: "Find tasks"),
                ChatPart(type: "attachment", uploadId: "u1", name: "spec.txt", mime: "text/plain"),
            ]),
            testMessage("m2", role: "assistant", text: "", turnId: "t0", parts: [
                ChatPart(type: "text", text: "Searching…"),
                ChatPart(type: "tool_call", callId: "call-1", tool: "search_tasks", arguments: .object(["q": .string("bug")])),
            ]),
            testMessage("m3", role: "tool", text: "", turnId: "t0", parts: [
                ChatPart(type: "tool_result", callId: "call-1", tool: "search_tasks", result: .object(["count": .number(2)])),
            ]),
            testMessage("m4", role: "notice", text: "", parts: [
                ChatPart(type: "proposal_outcome", text: "Approved.", tool: "create_task", proposalId: "p1", state: "succeeded"),
            ]),
        ]

        let state = ChatControllerState(
            status: .ready,
            activeConversationId: "c1",
            messages: messages,
            proposals: [ChatProposalEntry(proposal: testProposal(id: "p2", state: .pending))],
            turn: TurnView(turnId: "t1", state: .running, text: "Working on it", tools: [
                TurnToolView(callId: "live-1", tool: "list_projects", status: .running),
            ]),
            running: true,
            context: ChatContextStatus(tokens: 70_000, window: 100_000, summarizedThrough: "m2", canCompact: true)
        )

        let entries = entriesFromState(state)
        // Expected order: m1 text (with attachment chip), m2 text, m2 tools, context marker (after m3 tool msg!), m4 notice, live tools, live text, p2 proposal
        #expect(entries.count == 8)

        if case .text(let userEntry) = entries[0] {
            #expect(userEntry.content == "Find tasks")
            #expect(userEntry.references.count == 1)
            #expect(userEntry.references[0].isAttachment == true)
            #expect(userEntry.references[0].label == "spec.txt")
        } else {
            Issue.record("Expected user text entry at index 0")
        }

        if case .tools(let t) = entries[2] {
            #expect(t.tools[0].status == .completed)
            #expect(t.tools[0].detail?.arguments != nil)
            #expect(t.tools[0].detail?.result != nil)
        } else {
            Issue.record("Expected tools entry at index 2")
        }

        if case .contextMarker(let marker) = entries[3] {
            #expect(marker.content == CONTEXT_MARKER_TEXT)
        } else {
            Issue.record("Expected context marker at index 3")
        }

        if case .notice(let n) = entries[4] {
            #expect(n.content == "Approved.")
        } else {
            Issue.record("Expected notice at index 4")
        }

        if case .tools(let liveTools) = entries[5] {
            #expect(liveTools.tools[0].status == .running)
        } else {
            Issue.record("Expected live tools before live text at index 5")
        }

        if case .text(let liveText) = entries[6] {
            #expect(liveText.content == "Working on it")
            #expect(liveText.isStreaming == true)
        } else {
            Issue.record("Expected live text at index 6")
        }

        if case .proposal(let prop) = entries[7] {
            #expect(prop.proposalId == "p2")
            #expect(prop.canDecide == true)
        } else {
            Issue.record("Expected proposal at index 7")
        }
    }
}
