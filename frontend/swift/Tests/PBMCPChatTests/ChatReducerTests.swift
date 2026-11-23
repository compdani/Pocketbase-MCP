import Foundation
import Testing
@testable import PBMCPChat

@Suite("ChatReducer")
struct ChatReducerTests {
    private func applyAll(_ view: TurnView, _ events: [ChatEvent]) throws -> TurnView {
        var out = view
        for e in events {
            out = try ChatReducer.applyEvent(out, e)
        }
        return out
    }

    @Test("happy path reduces text, usage, and terminal state")
    func happyPath() throws {
        let view = try applyAll(ChatReducer.createTurnView(), [
            testEvent(1, "turn.queued"),
            testEvent(2, "turn.started"),
            testEvent(3, "text.delta", data: .object(["text": .string("héllo ")])),
            testEvent(4, "text.delta", data: .object(["text": .string("🌍 wörld")])),
            testEvent(5, "usage.updated", data: .object(["inputTokens": .number(10), "outputTokens": .number(7)])),
            testEvent(6, "turn.completed"),
        ])
        #expect(view.turnId == "t1")
        #expect(view.text == "héllo 🌍 wörld")
        #expect(view.state == .completed)
        #expect(ChatReducer.isTerminalState(view.state))
        #expect(view.usage == TurnUsage(inputTokens: 10, outputTokens: 7))
        #expect(view.lastSeq == 6)
    }

    @Test("duplicates are ignored without mutating view")
    func duplicatesIgnored() throws {
        let events = [
            testEvent(1, "turn.started"),
            testEvent(2, "text.delta", data: .object(["text": .string("ab")])),
            testEvent(3, "text.delta", data: .object(["text": .string("cd")])),
        ]
        let first = try applyAll(ChatReducer.createTurnView(), events)
        var replayed = first
        for e in events {
            let next = try ChatReducer.applyEvent(replayed, e)
            #expect(next == replayed)
            replayed = next
        }
        let done = try ChatReducer.applyEvent(replayed, testEvent(4, "turn.completed"))
        #expect(try ChatReducer.applyEvent(done, testEvent(4, "turn.completed")) == done)
    }

    @Test("gap, foreign turn, and after-terminal throw typed ReducerErrors")
    func errorGuards() throws {
        let running = try applyAll(ChatReducer.createTurnView(), [
            testEvent(1, "turn.started"),
            testEvent(2, "text.delta", data: .object(["text": .string("a")])),
        ])
        #expect(throws: ReducerError.sequenceGap(turnId: "t1", have: 2, got: 4)) {
            _ = try ChatReducer.applyEvent(running, testEvent(4, "text.delta", data: .object(["text": .string("b")])))
        }
        #expect(throws: ReducerError.foreignTurn(eventTurnId: "other", viewTurnId: "t1")) {
            _ = try ChatReducer.applyEvent(running, testEvent(3, "text.delta", data: .object(["text": .string("x")]), turn: "other"))
        }

        let completed = try ChatReducer.applyEvent(running, testEvent(3, "turn.completed"))
        #expect(throws: ReducerError.afterTerminal(eventType: "text.delta", state: "completed")) {
            _ = try ChatReducer.applyEvent(completed, testEvent(4, "text.delta", data: .object(["text": .string("late")])))
        }
    }

    @Test("tools move through running, completed, failed, and duplicate start replaces")
    func toolsLifecycle() throws {
        let view = try applyAll(ChatReducer.createTurnView(), [
            testEvent(1, "turn.started"),
            testEvent(2, "tool.started", data: .object(["callId": .string("a"), "tool": .string("search"), "arguments": .object(["q": .string("x")])])),
            testEvent(3, "tool.completed", data: .object(["callId": .string("a"), "result": .object(["n": .number(1)])])),
            testEvent(4, "tool.started", data: .object(["callId": .string("a"), "tool": .string("search")])),
        ])
        #expect(view.tools == [
            TurnToolView(callId: "a", tool: "search", arguments: nil, status: .running, result: nil, message: ""),
        ])
    }

    @Test("invalid proposal payloads throw invalidEvent")
    func invalidProposals() throws {
        let started = try ChatReducer.applyEvent(ChatReducer.createTurnView(), testEvent(1, "turn.started"))
        let badPayloads: [JSONValue] = [
            .object(["proposal": .null]),
            .object(["proposal": .object(["id": .string(""), "conversationId": .string("c1"), "location": .string("server"), "tool": .string("t"), "state": .string("pending"), "revision": .number(1), "arguments": .object([:])])]),
            .object(["proposal": .object(["id": .string("p1"), "conversationId": .string("c1"), "location": .string("browser"), "tool": .string("t"), "state": .string("pending"), "revision": .number(1), "arguments": .object([:])])]),
            .object(["proposal": .object(["id": .string("p1"), "conversationId": .string("c1"), "location": .string("server"), "tool": .string("t"), "state": .string("done"), "revision": .number(1), "arguments": .object([:])])]),
            .object(["proposal": .object(["id": .string("p1"), "conversationId": .string("c1"), "location": .string("server"), "tool": .string("t"), "state": .string("pending"), "revision": .number(0), "arguments": .object([:])])]),
            .object(["proposal": .object(["id": .string("p1"), "conversationId": .string("c1"), "location": .string("server"), "tool": .string("t"), "state": .string("pending"), "revision": .number(1), "arguments": .array([])])]),
            .object(["proposal": .object(["id": .string("p1"), "conversationId": .string("c1"), "location": .string("server"), "tool": .string("t"), "state": .string("pending"), "revision": .number(1), "arguments": .object([:]), "hunks": .array([.object(["id": .string("h"), "status": .string("maybe")])])])]),
        ]
        for payload in badPayloads {
            var threwInvalid = false
            do {
                _ = try ChatReducer.applyEvent(started, testEvent(2, "proposal.created", data: payload))
            } catch ReducerError.invalidEvent {
                threwInvalid = true
            }
            #expect(threwInvalid)
        }
    }

    @Test("two proposals keep first-seen order and editor proposals keep hunks")
    func proposalOrderAndEditor() throws {
        let pA = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(testProposal(id: "a")))
        let pB = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(testProposal(id: "b")))
        let pA2 = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(testProposal(id: "a", state: .rejected, revision: 2)))

        let view = try applyAll(ChatReducer.createTurnView(), [
            testEvent(1, "proposal.created", data: .object(["proposal": pA])),
            testEvent(2, "proposal.created", data: .object(["proposal": pB])),
            testEvent(3, "proposal.updated", data: .object(["proposal": pA2])),
        ])
        #expect(view.proposals.map(\.id) == ["a", "b"])
        #expect(view.proposals[0].state == .rejected)
    }
}
