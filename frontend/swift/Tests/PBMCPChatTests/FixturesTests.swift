import Foundation
import Testing
@testable import PBMCPChat

/// Conformance tests running the exact shared JSON fixtures verified by `node --test`
/// in `frontend/client/test/fixtures.test.mjs`.
@Suite("Shared Conformance Fixtures")
struct FixturesTests {
    private func loadFixtureData(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: "json", subdirectory: "Fixtures") else {
            throw ChatValidationError("Missing bundled fixture: \(name).json")
        }
        return try Data(contentsOf: url)
    }

    // MARK: - SSE Fixtures

    private struct SSEFixtureFile: Decodable {
        struct Case: Decodable {
            let name: String
            let chunks: [String]
            let splitByteByByte: Bool?
            let maxEventBytes: Int?
            let expectedComments: [String]?
            let expectedFrames: [SSEFrame]?
            let expectedErrorCode: String?
        }
        let cases: [Case]
    }

    @Test("SSE fixtures match JS parseSSE")
    func sseFixtures() throws {
        let fixture = try JSONDecoder().decode(SSEFixtureFile.self, from: loadFixtureData("sse"))
        for tc in fixture.cases {
            var parser = try SSEParser(maxEventBytes: tc.maxEventBytes ?? SSEParser.defaultMaxEventBytes)
            var comments: [String] = []
            var frames: [SSEFrame] = []

            if let expectedErr = tc.expectedErrorCode {
                var caughtCode: String? = nil
                do {
                    for chunk in tc.chunks {
                        frames += try parser.feed(Data(chunk.utf8), onComment: { comments.append($0) })
                    }
                    frames += try parser.finish(onComment: { comments.append($0) })
                } catch let err as SSEError {
                    caughtCode = err.code
                }
                #expect(caughtCode == expectedErr, "Case \(tc.name)")
            } else {
                if tc.splitByteByByte == true {
                    let bytes = Data(tc.chunks.joined().utf8)
                    for b in bytes {
                        frames += try parser.feed([b], onComment: { comments.append($0) })
                    }
                } else {
                    for chunk in tc.chunks {
                        frames += try parser.feed(Data(chunk.utf8), onComment: { comments.append($0) })
                    }
                }
                frames += try parser.finish(onComment: { comments.append($0) })
                #expect(frames == (tc.expectedFrames ?? []), "Case \(tc.name) frames")
                #expect(comments == (tc.expectedComments ?? []), "Case \(tc.name) comments")
            }
        }
    }

    // MARK: - Reducer Fixtures

    private struct ReducerFixtureFile: Decodable {
        struct Case: Decodable {
            let name: String
            let initialTurnId: String?
            let events: [ChatEvent]
            let expected: TurnView?
            let expectedErrorCode: String?
        }
        struct TranscriptCase: Decodable {
            let name: String
            let turn: ChatTurn
            let messages: [ChatMessage]
            let expected: TurnView
        }
        let cases: [Case]
        let transcriptCases: [TranscriptCase]
    }

    @Test("Reducer fixtures match JS applyEvent and viewFromTranscript")
    func reducerFixtures() throws {
        let fixture = try JSONDecoder().decode(ReducerFixtureFile.self, from: loadFixtureData("reducer"))
        for tc in fixture.cases {
            if let expectedErr = tc.expectedErrorCode {
                var caughtCode: String? = nil
                do {
                    var view = ChatReducer.createTurnView(tc.initialTurnId ?? "")
                    for ev in tc.events {
                        view = try ChatReducer.applyEvent(view, ev)
                    }
                } catch let err as ReducerError {
                    caughtCode = err.code
                }
                #expect(caughtCode == expectedErr, "Case \(tc.name)")
            } else {
                var view = ChatReducer.createTurnView(tc.initialTurnId ?? "")
                for ev in tc.events {
                    view = try ChatReducer.applyEvent(view, ev)
                }
                #expect(view == tc.expected, "Case \(tc.name)")
            }
        }

        for tc in fixture.transcriptCases {
            let view = ChatReducer.viewFromTranscript(tc.turn, messages: tc.messages)
            #expect(view == tc.expected, "Transcript case \(tc.name)")
        }
    }

    // MARK: - Error Fixtures

    private struct ErrorFixtureFile: Decodable {
        struct HTTPErrorCase: Decodable {
            let name: String
            let status: Int
            let statusText: String?
            let body: String
            let expectedKind: String
            let expectedCode: String
            let expectedMessage: String
            let hasProposal: Bool
            let expectedProposalState: String?
            let expectedProposalRevision: Int?
        }
        struct ErrorTextCase: Decodable {
            let code: String
            let message: String
            let expectedCode: String
            let expectedText: String
        }
        struct ProposalErrorCase: Decodable {
            let code: String
            let message: String
            let expectedText: String
        }
        let httpErrors: [HTTPErrorCase]
        let errorTextCases: [ErrorTextCase]
        let proposalErrorCases: [ProposalErrorCase]
    }

    @Test("Error fixtures match JS HTTP decoding and plain-language mapping")
    func errorFixtures() throws {
        let fixture = try JSONDecoder().decode(ErrorFixtureFile.self, from: loadFixtureData("errors"))
        for tc in fixture.httpErrors {
            let err = ChatClient.decodeError(
                status: tc.status,
                body: Data(tc.body.utf8),
                statusText: tc.statusText ?? ""
            )
            if tc.expectedKind == "ResyncRequiredError" {
                let resync = try #require(err as? ResyncRequiredError, "Case \(tc.name)")
                #expect(resync.status == tc.status)
                #expect(resync.code == tc.expectedCode)
                #expect(resync.message == tc.expectedMessage)
            } else {
                let api = try #require(err as? ChatApiError, "Case \(tc.name)")
                #expect(api.status == tc.status)
                #expect(api.code == tc.expectedCode)
                #expect(api.message == tc.expectedMessage)
                if tc.hasProposal {
                    let proposal = try #require(api.proposal, "Case \(tc.name) proposal")
                    #expect(proposal.state.rawValue == tc.expectedProposalState)
                    #expect(proposal.revision == tc.expectedProposalRevision)
                } else {
                    #expect(api.proposal == nil)
                }
            }
        }

        for tc in fixture.errorTextCases {
            #expect(errorCode(code: tc.code, message: tc.message) == tc.expectedCode)
            #expect(errorText(code: tc.code, message: tc.message) == tc.expectedText)
        }

        for tc in fixture.proposalErrorCases {
            let failure = ChatTurnFailure(code: tc.code, message: tc.message)
            #expect(proposalErrorText(failure) == tc.expectedText)
        }
    }

    // MARK: - Controller Scenarios

    private struct ControllerFixtureFile: Decodable {
        struct ScriptedResponse: Decodable {
            let method: String
            let path: String
            let status: Int
            let body: JSONValue?
            let sse: [ChatEvent]?
        }
        struct Action: Decodable {
            let type: String
            let conversationId: String?
            let text: String?
            let model: String?
            let effort: String?
            let proposalId: String?
            let decision: ProposalDecision?
        }
        struct ExpectedProposal: Decodable, Equatable {
            let id: String
            let state: String
            let revision: Int
            let errorCode: String?
        }
        struct Expected: Decodable {
            let status: String?
            let activeConversationId: String?
            let choice: String?
            let running: Bool?
            let conversationTitles: [String]?
            let messageIds: [String]?
            let turnState: String?
            let turnText: String?
            let turnLastSeq: Int?
            let contextTokens: Int?
            let errorCode: String?
            let proposals: [ExpectedProposal]?
        }
        struct Scenario: Decodable {
            let name: String
            let responses: [ScriptedResponse]
            let actions: [Action]
            let expected: Expected
        }
        let scenarios: [Scenario]
    }

    @Test("Controller scenario fixtures match JS final state via URLProtocol")
    @MainActor
    func controllerScenarioFixtures() async throws {
        let fixture = try JSONDecoder().decode(ControllerFixtureFile.self, from: loadFixtureData("controller"))

        for scenario in fixture.scenarios {
            final class ResponseQueue: @unchecked Sendable {
                let lock = NSLock()
                var remaining: [ControllerFixtureFile.ScriptedResponse]
                init(_ steps: [ControllerFixtureFile.ScriptedResponse]) { self.remaining = steps }
                func take(method: String, path: String) -> ControllerFixtureFile.ScriptedResponse? {
                    lock.withLock {
                        guard let idx = remaining.firstIndex(where: { $0.method == method && $0.path == path }) else {
                            return nil
                        }
                        return remaining.remove(at: idx)
                    }
                }
            }

            let queue = ResponseQueue(scenario.responses)
            let (session, stubId) = StubURLProtocol.makeSession { request, _ in
                let url = request.url!
                let method = request.httpMethod ?? "GET"
                guard let step = queue.take(method: method, path: url.path) else {
                    Issue.record("\(scenario.name): unexpected request \(method) \(url.path)")
                    throw URLError(.badServerResponse)
                }
                if let sseEvents = step.sse {
                    return makeSSEResponse(url: url, status: step.status, body: sseFrames(sseEvents))
                }
                let data = try JSONEncoder().encode(step.body ?? .object([:]))
                let resp = HTTPURLResponse(
                    url: url,
                    statusCode: step.status,
                    httpVersion: "HTTP/1.1",
                    headerFields: ["Content-Type": "application/json"]
                )!
                return (resp, data)
            }
            defer { StubURLProtocol.unregister(id: stubId) }

            let client = ChatClient(
                baseURL: URL(string: "https://api.test")!,
                getToken: { "tok" },
                session: session
            )
            let controller = try ChatController(
                client: client,
                reconnect: ChatReconnectOptions(initialDelayMs: 5, maxDelayMs: 20),
                sync: nil
            )

            for action in scenario.actions {
                switch action.type {
                case "init":
                    _ = await controller.initialize()
                case "open":
                    _ = await controller.open(action.conversationId ?? "")
                case "send":
                    _ = try await controller.send(
                        action.text ?? "",
                        model: action.model,
                        effort: action.effort
                    )
                case "decide":
                    _ = try await controller.decide(action.proposalId ?? "", action.decision ?? .approve)
                default:
                    Issue.record("Unknown action \(action.type)")
                }
            }

            await controller.whenSettled()
            let st = controller.state
            let exp = scenario.expected

            if let status = exp.status { #expect(st.status.rawValue == status, "\(scenario.name)") }
            if let activeId = exp.activeConversationId { #expect(st.activeConversationId == activeId, "\(scenario.name)") }
            if let choice = exp.choice { #expect(st.choice == choice, "\(scenario.name)") }
            if let running = exp.running { #expect(st.running == running, "\(scenario.name)") }
            if let titles = exp.conversationTitles { #expect(st.conversations.map(\.title) == titles, "\(scenario.name)") }
            if let msgIds = exp.messageIds { #expect(st.messages.map(\.id) == msgIds, "\(scenario.name)") }
            if let turnState = exp.turnState { #expect(st.turn?.state.rawValue == turnState, "\(scenario.name)") }
            if let turnText = exp.turnText { #expect(st.turn?.text == turnText, "\(scenario.name)") }
            if let lastSeq = exp.turnLastSeq { #expect(st.turn?.lastSeq == lastSeq, "\(scenario.name)") }
            if let tokens = exp.contextTokens { #expect(st.context?.tokens == tokens, "\(scenario.name)") }
            if let expectedProposals = exp.proposals {
                let actual = st.proposals.map {
                    ControllerFixtureFile.ExpectedProposal(
                        id: $0.id,
                        state: $0.state.rawValue,
                        revision: $0.revision,
                        errorCode: $0.error?.code
                    )
                }
                #expect(actual == expectedProposals, "\(scenario.name)")
            }

            controller.dispose()
        }
    }
}
