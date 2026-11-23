import Foundation
import Testing
@testable import PBMCPChat

@Suite("ChatController")
@MainActor
struct ChatControllerTests {
    private let accountA = ChatAccountKey(appUrl: "https://a.test", mount: "/", authCollectionId: "users", userId: "u1")
    private let accountB = ChatAccountKey(appUrl: "https://a.test", mount: "/", authCollectionId: "users", userId: "u2")

    @Test("init loads status, catalog choices, limits, and first conversation page")
    func initLoadsCatalog() async throws {
        let client = MockTransport()
        client.statusResult = ChatStatus(
            version: "1",
            models: ["m1"],
            choices: [
                ChatModelChoice(id: "openai::m1", provider: "openai", model: "m1", label: "Model One", group: "OpenAI", isDefault: true, efforts: ["low", "high"]),
            ],
            defaultChoice: "openai::m1",
            features: ChatFeatures(ready: true, proposals: true, uploads: true, context: true),
            limits: ChatLimits(maxMessageChars: 16000, maxUploadBytes: 1024, maxAttachmentsPerTurn: 2, attachmentTypes: ["text/plain"])
        )
        client.pageResult = ChatConversationList(items: [testConversation("c1", title: "One")], nextCursor: "page-2")

        let controller = try ChatController(client: client, sync: nil)
        #expect(controller.state.status == .idle)

        await controller.initialize()
        #expect(controller.state.status == .ready)
        #expect(controller.state.choices.map(\.id) == ["openai::m1"])
        #expect(controller.state.defaultChoice == "openai::m1")
        #expect(controller.state.choice == "openai::m1")
        #expect(controller.state.conversations.map(\.id) == ["c1"])
        #expect(controller.state.nextCursor == "page-2")
        controller.dispose()
    }

    @Test("lost POST response retries with the same Idempotency-Key")
    func idempotentRetry() async throws {
        let client = MockTransport()
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"))
        final class RetryState: @unchecked Sendable {
            let lock = NSLock()
            var keys: [String] = []
        }
        let rs = RetryState()
        client.onSubmitTurn = { _, _, _, _, _, _, _, key in
            let count: Int = rs.lock.withLock {
                rs.keys.append(key ?? "")
                return rs.keys.count
            }
            if count == 1 {
                throw ChatNetworkError(message: "lost response")
            }
            return ChatSubmitResult(turn: testTurn(.queued), created: true)
        }

        let controller = try ChatController(client: client, reconnect: ChatReconnectOptions(initialDelayMs: 5), sync: nil)
        await controller.initialize()
        await controller.open("c1")
        let res = try await controller.send("hello")
        await controller.whenSettled()

        #expect(res?.created == true)
        #expect(rs.keys.count == 2)
        #expect(rs.keys[0] == rs.keys[1])
        #expect(!rs.keys[0].isEmpty)
        controller.dispose()
    }

    @Test("disconnect mid-stream reconnects from lastSeq without duplicating text")
    func midStreamReconnect() async throws {
        let client = MockTransport()
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"))
        final class StreamBox: @unchecked Sendable {
            let lock = NSLock()
            var afters: [Int] = []
        }
        let box = StreamBox()
        client.onStreamTurn = { _, after, onEvent, _ in
            let n: Int = box.lock.withLock {
                box.afters.append(after)
                return box.afters.count
            }
            if n == 1 {
                try await onEvent?(testEvent(1, "turn.started"))
                try await onEvent?(testEvent(2, "text.delta", data: .object(["text": .string("ab")])))
                return StreamTurnResult(lastSeq: 2, terminal: false)
            }
            try await onEvent?(testEvent(3, "text.delta", data: .object(["text": .string("cd")])))
            try await onEvent?(testEvent(4, "turn.completed"))
            return StreamTurnResult(lastSeq: 4, terminal: true)
        }

        let controller = try ChatController(client: client, reconnect: ChatReconnectOptions(initialDelayMs: 5), sync: nil)
        await controller.initialize()
        await controller.open("c1")
        _ = try await controller.send("hello")
        await controller.whenSettled()

        #expect(box.afters == [0, 2])
        #expect(controller.state.turn?.text == "abcd")
        #expect(controller.state.turn?.lastSeq == 4)
        #expect(controller.state.running == false)
        controller.dispose()
    }

    @Test("idle stream reconnects with backoff and heartbeats reset the idle timer")
    func idleReconnectAndHeartbeat() async throws {
        let client = MockTransport()
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"))
        final class Box: @unchecked Sendable {
            let lock = NSLock()
            var afters: [Int] = []
        }
        let box = Box()
        client.onStreamTurn = { _, after, onEvent, _ in
            let n: Int = box.lock.withLock {
                box.afters.append(after)
                return box.afters.count
            }
            if n == 1 {
                try await onEvent?(testEvent(1, "turn.started"))
                try await Task.sleep(nanoseconds: 5_000_000_000)
                return StreamTurnResult(lastSeq: 1, terminal: false)
            }
            try await onEvent?(testEvent(2, "turn.completed"))
            return StreamTurnResult(lastSeq: 2, terminal: true)
        }

        let controller = try ChatController(
            client: client,
            reconnect: ChatReconnectOptions(initialDelayMs: 5),
            idleTimeoutMs: 40,
            sync: nil
        )
        await controller.initialize()
        await controller.open("c1")
        _ = try await controller.send("hello")
        await controller.whenSettled()

        #expect(box.afters == [0, 1])
        #expect(controller.state.turn?.state == .completed)
        controller.dispose()
    }

    @Test("stop cancels on server before aborting local stream")
    func stopCancelsServerFirst() async throws {
        let client = MockTransport()
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"), activeTurn: testTurn(.running))
        final class Log: @unchecked Sendable {
            let lock = NSLock()
            var entries: [String] = []
            func add(_ s: String) { lock.withLock { entries.append(s) } }
        }
        let log = Log()
        client.onStreamTurn = { _, _, _, _ in
            log.add("stream")
            do {
                try await Task.sleep(nanoseconds: 5_000_000_000)
            } catch {
                log.add("abort")
                throw error
            }
            return StreamTurnResult(lastSeq: 0, terminal: false)
        }
        client.onCancelTurn = { _ in
            try await Task.sleep(nanoseconds: 15_000_000)
            log.add("cancel")
            return testTurn(.cancelled)
        }

        let controller = try ChatController(client: client, idleTimeoutMs: 5_000, sync: nil)
        await controller.initialize()
        await controller.open("c1")
        try await Task.sleep(nanoseconds: 10_000_000)
        await controller.stop()
        await controller.whenSettled()

        #expect(log.entries.prefix(2) == ["stream", "cancel"])
        #expect(controller.state.running == false)
        #expect(controller.state.turn?.state == .cancelled)
        controller.dispose()
    }

    @Test("account switch clears state synchronously and ignores late responses")
    func accountSwitchClearsState() async throws {
        let client = MockTransport()
        client.pageResult = ChatConversationList(items: [testConversation("new-conv")])
        let gate = AsyncGate()
        client.onGetConversation = { _, _, _ in
            await gate.wait()
            return ChatConversationDetail(
                conversation: testConversation("old-conv"),
                messages: [testMessage("old", role: "assistant", text: "OLD ACCOUNT")]
            )
        }

        let controller = try ChatController(client: client, storageKey: accountA, sync: nil)
        #expect(controller.accountKey == (try accountKeyOf(accountA)))

        let opening = Task { await controller.open("old-conv") }
        try await Task.sleep(nanoseconds: 10_000_000)
        #expect(controller.state.activeConversationId == "old-conv")

        _ = try await controller.setAccount(accountB)
        #expect(controller.state.activeConversationId == nil)
        #expect(controller.state.messages.isEmpty)
        #expect(controller.state.conversations.map(\.id) == ["new-conv"])

        await gate.open()
        _ = await opening.value
        await controller.whenSettled()

        #expect(controller.state.activeConversationId == nil)
        #expect(controller.state.messages.isEmpty)
        controller.dispose()
    }

    @Test("account keys cannot collide across field boundaries")
    func accountKeyCollisionSafety() throws {
        let one = ChatAccountKey(appUrl: "a|b", mount: "c", authCollectionId: "d", userId: "e")
        let two = ChatAccountKey(appUrl: "a", mount: "b|c", authCollectionId: "d", userId: "e")
        #expect(try accountKeyOf(one) != accountKeyOf(two))
        #expect(try accountKeyOf(nil) == nil)
        #expect(throws: ChatValidationError.self) {
            _ = try accountKeyOf(ChatAccountKey(appUrl: "a", mount: "/", authCollectionId: "users", userId: ""))
        }
    }

    @Test("editor adapter validates, applies, and reports outcome")
    func editorProposalLifecycle() async throws {
        final class TestEditor: ChatEditorAdapter, @unchecked Sendable {
            let lock = NSLock()
            var applicable = true
            var calls: [String] = []

            func snapshot() async throws -> ChatEditorSnapshot {
                ChatEditorSnapshot(editorId: "demo-note", revision: "5")
            }

            func validatePatch(_ proposal: ChatProposal) async throws -> Bool {
                lock.withLock { calls.append("validate:\(proposal.id)") }
                return applicable
            }

            func applyPatch(_ proposal: ChatProposal) async throws -> ChatEditorApplyResult {
                lock.withLock { calls.append("apply:\(proposal.id)") }
                return ChatEditorApplyResult(revision: "9", hunks: [ChatHunkOutcome(id: "h1", status: .applied)])
            }
        }

        let editor = TestEditor()
        let editorProp = testProposal(id: "e1", tool: "edit_note", location: .editor, target: "demo-note")
        let approvedProp = testProposal(id: "e1", tool: "edit_note", location: .editor, target: "demo-note", state: .approved, revision: 2)

        let client = MockTransport()
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"), proposals: [editorProp])
        client.decideResult = ProposalDecisionResult(proposal: approvedProp, replayed: false)
        client.onReportOutcome = { _, _, status, rev, hunks, _ in
            let final = testProposal(
                id: "e1",
                tool: "edit_note",
                location: .editor,
                target: "demo-note",
                state: status == .applied ? .applied : (status == .stale ? .stale : .failed),
                revision: 3,
                editorRevision: rev,
                hunks: hunks
            )
            return ProposalDecisionResult(proposal: final, replayed: false)
        }

        let controller = try ChatController(client: client, editor: editor, sync: nil)
        await controller.initialize()
        await controller.open("c1")

        let result = try await controller.decide("e1", .approve)
        #expect(editor.calls == ["validate:e1", "apply:e1"])
        #expect(result?.proposal?.state == .applied)
        #expect(result?.proposal?.editorRevision == "9")
        controller.dispose()
    }

    @Test("stale editor patch reports stale with snapshot revision and never calls applyPatch")
    func editorProposalStaleOutcome() async throws {
        final class StaleEditor: ChatEditorAdapter, @unchecked Sendable {
            let lock = NSLock()
            var applied = false
            func snapshot() async throws -> ChatEditorSnapshot { ChatEditorSnapshot(revision: "7") }
            func validatePatch(_ proposal: ChatProposal) async throws -> Bool { false }
            func applyPatch(_ proposal: ChatProposal) async throws -> ChatEditorApplyResult {
                lock.withLock { applied = true }
                return ChatEditorApplyResult(revision: "9")
            }
        }
        let editor = StaleEditor()
        let editorProp = testProposal(id: "e1", tool: "edit_note", location: .editor, target: "demo-note")
        let approvedProp = testProposal(id: "e1", tool: "edit_note", location: .editor, target: "demo-note", state: .approved, revision: 2)
        let client = MockTransport()
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"), proposals: [editorProp])
        client.decideResult = ProposalDecisionResult(proposal: approvedProp, replayed: false)
        final class Capture: @unchecked Sendable {
            let lock = NSLock()
            var reportedStatus: ProposalOutcomeStatus?
            var reportedRevision: String?
        }
        let cap = Capture()
        client.onReportOutcome = { _, _, status, rev, _, _ in
            cap.lock.withLock {
                cap.reportedStatus = status
                cap.reportedRevision = rev
            }
            return ProposalDecisionResult(
                proposal: testProposal(id: "e1", tool: "edit_note", location: .editor, state: .stale, revision: 3, editorRevision: rev),
                replayed: false
            )
        }

        let controller = try ChatController(client: client, editor: editor, sync: nil)
        await controller.initialize()
        await controller.open("c1")
        let result = try await controller.decide("e1", .approve)
        #expect(editor.applied == false)
        #expect(result?.proposal?.state == .stale)
        #expect(cap.reportedStatus == .stale)
        #expect(cap.reportedRevision == "7")
        controller.dispose()
    }

    @Test("dispose resumes pending whenSettled waiters")
    func disposeResumesWhenSettled() async throws {
        let client = MockTransport()
        let gate = AsyncGate()
        client.onStatus = {
            await gate.wait()
            return ChatStatus()
        }
        let controller = try ChatController(client: client, sync: nil)
        let initTask = Task { await controller.initialize() }
        try await Task.sleep(nanoseconds: 10_000_000)

        let settledTask = Task { await controller.whenSettled() }
        try await Task.sleep(nanoseconds: 10_000_000)
        controller.dispose()
        await settledTask.value
        await gate.open()
        _ = await initTask.value
    }

    @Test("attach validates limits and send uploads files and clears pendingUploads")
    func attachmentsValidationAndSend() async throws {
        let client = MockTransport()
        client.statusResult = ChatStatus(
            version: "1",
            models: ["model-a"],
            features: ChatFeatures(ready: true, proposals: true, uploads: true),
            limits: ChatLimits(maxMessageChars: 16000, maxUploadBytes: 10, maxAttachmentsPerTurn: 2, attachmentTypes: ["text/plain", "image/png"])
        )
        client.detailResult = ChatConversationDetail(conversation: testConversation("c1"))
        let controller = try ChatController(client: client, sync: nil)
        await controller.initialize()
        await controller.open("c1")

        await #expect(throws: ChatApiError.self) {
            _ = try await controller.attach([ChatUploadFile(data: Data("x".utf8), name: "a.zip", mimeType: "application/zip")])
        }
        await #expect(throws: ChatApiError.self) {
            _ = try await controller.attach([ChatUploadFile(data: Data(repeating: 65, count: 20), name: "big.txt", mimeType: "text/plain")])
        }

        let uploaded = try await controller.attach([ChatUploadFile(data: Data("hi".utf8), name: "one.txt", mimeType: "text/plain")])
        #expect(uploaded.count == 1)
        #expect(controller.state.pendingUploads.map(\.id) == ["u1"])

        _ = try await controller.send("see", attachments: [.uploadId("u1"), .file(ChatUploadFile(data: Data("ok".utf8), name: "two.txt", mimeType: "text/plain"))])
        await controller.whenSettled()
        #expect(controller.state.pendingUploads.isEmpty)
        controller.dispose()
    }

    @Test("send sends page context only when features.context is true and filters efforts by choice")
    func pageContextAndEfforts() async throws {
        for accepts in [true, false] {
            let client = MockTransport()
            client.statusResult = ChatStatus(
                version: "1",
                models: ["reasoner", "plain"],
                choices: [
                    ChatModelChoice(id: "a::reasoner", provider: "a", model: "reasoner", isDefault: true, efforts: ["low", "high"]),
                    ChatModelChoice(id: "a::plain", provider: "a", model: "plain"),
                ],
                defaultChoice: "a::reasoner",
                features: ChatFeatures(ready: true, proposals: false, uploads: false, context: accepts),
                limits: ChatLimits(maxMessageChars: 16000)
            )
            client.detailResult = ChatConversationDetail(conversation: testConversation("c1"))

            final class Captured: @unchecked Sendable {
                let lock = NSLock()
                var contexts: [[String: JSONValue]?] = []
                var efforts: [String?] = []
            }
            let cap = Captured()
            client.onSubmitTurn = { _, _, _, _, effort, _, context, _ in
                cap.lock.withLock {
                    cap.contexts.append(context)
                    cap.efforts.append(effort)
                }
                return ChatSubmitResult(turn: testTurn(.queued), created: true)
            }

            let controller = try ChatController(
                client: client,
                sync: nil,
                context: { ["screen": .string("assistant")] }
            )
            await controller.initialize()
            await controller.open("c1")

            _ = try await controller.send("one", effort: "high")
            await controller.whenSettled()
            _ = try await controller.send("two", model: "a::plain", effort: "high")
            await controller.whenSettled()

            if accepts {
                #expect(cap.contexts == [["screen": .string("assistant")], ["screen": .string("assistant")]])
            } else {
                #expect(cap.contexts == [nil, nil])
            }
            #expect(cap.efforts == ["high", nil])
            controller.dispose()
        }
    }

    @Test("compact updates context and preserves previous numbers on refusal")
    func contextAndCompaction() async throws {
        let client = MockTransport()
        client.detailResult = ChatConversationDetail(
            conversation: testConversation("c1"),
            context: ChatContextStatus(tokens: 80_000, window: 100_000, canCompact: true)
        )
        let controller = try ChatController(client: client, sync: nil)
        await controller.initialize()
        await controller.open("c1")
        #expect(controller.state.context?.tokens == 80_000)

        client.compactResult = .success(ChatCompactResult(
            compacted: true,
            throughMessageId: "m5",
            context: ChatContextStatus(tokens: 20_000, window: 100_000, summarizedThrough: "m5", summarizedAt: "2026-11-19T10:00:00Z", compacting: false, canCompact: true)
        ))
        let res = await controller.compact()
        #expect(res?.compacted == true)
        #expect(controller.state.context?.tokens == 20_000)
        #expect(controller.state.context?.summarizedThrough == "m5")

        client.compactResult = .failure(ChatApiError(status: 429, code: "too_soon", message: "Too soon"))
        let refused = await controller.compact()
        #expect(refused == nil)
        #expect(controller.state.context?.tokens == 20_000)
        #expect(controller.state.context?.compacting == false)
        #expect(controller.state.error?.code == "too_soon")
        controller.dispose()
    }
}

private actor AsyncGate {
    private var opened = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func wait() async {
        if opened { return }
        await withCheckedContinuation { waiters.append($0) }
    }

    func open() {
        opened = true
        let all = waiters
        waiters.removeAll()
        all.forEach { $0.resume() }
    }
}
