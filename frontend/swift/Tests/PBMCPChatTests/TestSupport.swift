import Foundation
@testable import PBMCPChat

/// Thread-safe `URLProtocol` stub for testing `ChatClient` and `ChatController` end-to-end
/// without hitting the network. Each test creates its own isolated session keyed by a unique header.
final class StubURLProtocol: URLProtocol, @unchecked Sendable {
    typealias Handler = @Sendable (URLRequest, Data?) throws -> (HTTPURLResponse, Data)

    private static let lock = NSLock()
    private nonisolated(unsafe) static var handlers: [String: Handler] = [:]

    static func register(id: String, handler: @escaping Handler) {
        lock.lock()
        handlers[id] = handler
        lock.unlock()
    }

    static func unregister(id: String) {
        lock.lock()
        handlers.removeValue(forKey: id)
        lock.unlock()
    }

    static func makeSession(handler: @escaping Handler) -> (URLSession, String) {
        let id = UUID().uuidString
        register(id: id, handler: handler)
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        config.httpAdditionalHeaders = ["X-Stub-Test-ID": id]
        return (URLSession(configuration: config), id)
    }

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest {
        request
    }

    override func startLoading() {
        guard let id = request.value(forHTTPHeaderField: "X-Stub-Test-ID") else {
            client?.urlProtocol(self, didFailWithError: URLError(.unknown))
            return
        }
        StubURLProtocol.lock.lock()
        let handler = StubURLProtocol.handlers[id]
        StubURLProtocol.lock.unlock()

        guard let handler else {
            client?.urlProtocol(self, didFailWithError: URLError(.cancelled))
            return
        }

        let bodyData = readBody(from: request)
        do {
            let (response, data) = try handler(request, bodyData)
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            if !data.isEmpty {
                client?.urlProtocol(self, didLoad: data)
            }
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}

    private func readBody(from request: URLRequest) -> Data? {
        if let body = request.httpBody {
            return body
        }
        guard let stream = request.httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var data = Data()
        let bufferSize = 4096
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: bufferSize)
        defer { buffer.deallocate() }
        while stream.hasBytesAvailable {
            let read = stream.read(buffer, maxLength: bufferSize)
            if read > 0 {
                data.append(buffer, count: read)
            } else {
                break
            }
        }
        return data
    }
}

func makeJSONResponse(
    url: URL,
    status: Int = 200,
    json: Any,
    headers: [String: String] = [:]
) -> (HTTPURLResponse, Data) {
    let data = try! JSONSerialization.data(withJSONObject: json)
    var allHeaders = ["Content-Type": "application/json; charset=utf-8"]
    for (k, v) in headers { allHeaders[k] = v }
    let resp = HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1", headerFields: allHeaders)!
    return (resp, data)
}

func makeEncodableResponse<T: Encodable>(
    url: URL,
    status: Int = 200,
    value: T,
    headers: [String: String] = [:]
) -> (HTTPURLResponse, Data) {
    let data = try! JSONEncoder().encode(value)
    var allHeaders = ["Content-Type": "application/json; charset=utf-8"]
    for (k, v) in headers { allHeaders[k] = v }
    let resp = HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1", headerFields: allHeaders)!
    return (resp, data)
}

func makeSSEResponse(url: URL, status: Int = 200, body: String) -> (HTTPURLResponse, Data) {
    let resp = HTTPURLResponse(
        url: url,
        statusCode: status,
        httpVersion: "HTTP/1.1",
        headerFields: ["Content-Type": "text/event-stream"]
    )!
    return (resp, Data(body.utf8))
}

func sseFrame(_ event: ChatEvent) -> String {
    let data = try! JSONEncoder().encode(event)
    let json = String(data: data, encoding: .utf8)!
    return "id: \(event.seq)\nevent: \(event.type)\ndata: \(json)\n\n"
}

func sseFrames(_ events: [ChatEvent]) -> String {
    events.map(sseFrame).joined()
}

func testEvent(
    _ seq: Int,
    _ type: String,
    data: JSONValue? = nil,
    turn: String = "t1",
    conv: String = "c1",
    v: String = "1"
) -> ChatEvent {
    ChatEvent(v: v, seq: seq, conversationId: conv, turnId: turn, type: type, data: data, at: "2026-01-02T03:04:05.00\(seq)Z")
}

func testConversation(_ id: String, title: String? = nil, createdAt: String = "2026-01-02T03:04:05Z") -> ChatConversation {
    ChatConversation(id: id, title: title ?? id, createdAt: createdAt, updatedAt: createdAt)
}

func testTurn(_ state: TurnState, id: String = "t1", conversationId: String = "c1", error: ChatTurnFailure? = nil) -> ChatTurn {
    ChatTurn(id: id, conversationId: conversationId, state: state, createdAt: "x", updatedAt: "x", error: error)
}

func testMessage(_ id: String, role: String, text: String, conversationId: String = "c1", turnId: String? = nil, parts: [ChatPart]? = nil) -> ChatMessage {
    ChatMessage(
        id: id,
        conversationId: conversationId,
        turnId: turnId,
        role: role,
        parts: parts ?? [ChatPart(type: "text", text: text)],
        createdAt: "2026-01-02T03:04:05Z"
    )
}

func testProposal(
    id: String = "p1",
    conversationId: String = "c1",
    turnId: String? = "t1",
    tool: String = "create_items",
    location: ChatProposalLocation = .server,
    target: String? = nil,
    arguments: JSONValue = .object(["data": .object(["title": .string("Buy milk")])]),
    summary: String? = "Create an item",
    state: ChatProposalState = .pending,
    revision: Int = 1,
    result: JSONValue? = nil,
    failure: String? = nil,
    warnings: [String]? = nil,
    editorRevision: String? = nil,
    hunks: [ChatHunkOutcome]? = nil
) -> ChatProposal {
    ChatProposal(
        id: id,
        conversationId: conversationId,
        turnId: turnId,
        tool: tool,
        location: location,
        target: target,
        arguments: arguments,
        argumentsHash: "sha256:abc",
        summary: summary,
        state: state,
        revision: revision,
        createdAt: "2026-01-02T03:04:05Z",
        updatedAt: "2026-01-02T03:04:05Z",
        result: result,
        failure: failure,
        warnings: warnings,
        editorRevision: editorRevision,
        hunks: hunks
    )
}

/// Mutable in-memory mock transport for deterministic `ChatController` and sync tests.
final class MockTransport: ChatTransport, @unchecked Sendable {
    private let lock = NSLock()
    private func synced<T>(_ body: () -> T) -> T {
        lock.lock()
        defer { lock.unlock() }
        return body()
    }

    var calls: [String] = []
    var statusResult = ChatStatus(
        version: "1",
        models: ["model-a"],
        features: ChatFeatures(ready: true, proposals: false, uploads: false),
        limits: ChatLimits(maxMessageChars: 16000)
    )
    var pageResult = ChatConversationList()
    var createResult = testConversation("c-new", title: "New")
    var detailResult = ChatConversationDetail(conversation: testConversation("c1"))
    var renameResult = testConversation("c1", title: "Renamed")
    var contextResult = ChatContextStatus()
    var compactResult: Result<ChatCompactResult, Error> = .success(ChatCompactResult(compacted: true))
    var submitResult = ChatSubmitResult(turn: testTurn(.queued), created: true)
    var turnResult = testTurn(.completed)
    var cancelResult = testTurn(.cancelled)
    var proposalResult = testProposal()
    var decideResult = ProposalDecisionResult(proposal: testProposal(state: .succeeded, revision: 2), replayed: false)
    var outcomeResult = ProposalDecisionResult(proposal: testProposal(location: .editor, state: .applied, revision: 3), replayed: false)
    var uploadCounter = 0

    var onStatus: (@Sendable () async throws -> ChatStatus)?
    var onListConversations: (@Sendable (Int?, String?, String?) async throws -> ChatConversationList)?
    var onGetConversation: (@Sendable (String, Int?, String?) async throws -> ChatConversationDetail)?
    var onSubmitTurn: (@Sendable (String, String, String?, String?, String?, [String]?, [String: JSONValue]?, String?) async throws -> ChatSubmitResult)?
    var onGetTurn: (@Sendable (String) async throws -> ChatTurn)?
    var onCancelTurn: (@Sendable (String) async throws -> ChatTurn)?
    var onDecideProposal: (@Sendable (String, ProposalDecision, Int, JSONValue?) async throws -> ProposalDecisionResult)?
    var onReportOutcome: (@Sendable (String, Int, ProposalOutcomeStatus, String?, [ChatHunkOutcome]?, String?) async throws -> ProposalDecisionResult)?
    var onUploadFile: (@Sendable (String, ChatUploadFile, String?) async throws -> ChatUpload)?
    var onGetContext: (@Sendable (String) async throws -> ChatContextStatus)?
    var onStreamTurn: (@Sendable (String, Int, (@Sendable (ChatEvent) async throws -> Void)?, (@Sendable (String) -> Void)?) async throws -> StreamTurnResult)?

    func record(_ name: String) {
        synced { calls.append(name) }
    }

    func status() async throws -> ChatStatus {
        record("status")
        if let onStatus { return try await onStatus() }
        return synced { statusResult }
    }

    func listConversations(limit: Int?, cursor: String?, target: String?) async throws -> ChatConversationList {
        record("listConversations")
        if let onListConversations { return try await onListConversations(limit, cursor, target) }
        return synced { pageResult }
    }

    func createConversation(title: String?, target: String?) async throws -> ChatConversation {
        record("createConversation")
        return synced { createResult }
    }

    func getConversation(_ id: String, limit: Int?, cursor: String?) async throws -> ChatConversationDetail {
        record("getConversation:\(id)")
        if let onGetConversation { return try await onGetConversation(id, limit, cursor) }
        return synced { detailResult }
    }

    func renameConversation(_ id: String, title: String) async throws -> ChatConversation {
        record("renameConversation:\(id):\(title)")
        return synced { renameResult }
    }

    func deleteConversation(_ id: String) async throws {
        record("deleteConversation:\(id)")
    }

    func getContext(_ id: String) async throws -> ChatContextStatus {
        record("getContext:\(id)")
        if let onGetContext { return try await onGetContext(id) }
        return synced { contextResult }
    }

    func compactConversation(_ id: String) async throws -> ChatCompactResult {
        record("compactConversation:\(id)")
        return try synced { compactResult }.get()
    }

    func importConversation(sourceId: String, title: String?, createdAt: String?, messages: [ChatImportMessage]) async throws -> ChatImportResult {
        record("importConversation")
        return ChatImportResult(conversation: synced { createResult }, created: true)
    }

    func submitTurn(
        _ conversationId: String,
        text: String,
        provider: String?,
        model: String?,
        effort: String?,
        attachments: [String]?,
        context: [String: JSONValue]?,
        idempotencyKey: String?
    ) async throws -> ChatSubmitResult {
        record("submitTurn")
        if let onSubmitTurn {
            return try await onSubmitTurn(conversationId, text, provider, model, effort, attachments, context, idempotencyKey)
        }
        return synced { submitResult }
    }

    func getTurn(_ id: String) async throws -> ChatTurn {
        record("getTurn:\(id)")
        if let onGetTurn { return try await onGetTurn(id) }
        return synced { turnResult }
    }

    func cancelTurn(_ id: String) async throws -> ChatTurn {
        record("cancelTurn:\(id)")
        if let onCancelTurn { return try await onCancelTurn(id) }
        return synced { cancelResult }
    }

    func getProposal(_ id: String) async throws -> ChatProposal {
        record("getProposal:\(id)")
        return synced { proposalResult }
    }

    func decideProposal(_ id: String, decision: ProposalDecision, revision: Int, arguments: JSONValue?) async throws -> ProposalDecisionResult {
        record("decideProposal:\(id):\(decision.rawValue):\(revision)")
        if let onDecideProposal { return try await onDecideProposal(id, decision, revision, arguments) }
        return synced { decideResult }
    }

    func reportOutcome(
        _ id: String,
        revision: Int,
        status: ProposalOutcomeStatus,
        editorRevision: String?,
        hunks: [ChatHunkOutcome]?,
        message: String?
    ) async throws -> ProposalDecisionResult {
        record("reportOutcome:\(id):\(status.rawValue)")
        if let onReportOutcome {
            return try await onReportOutcome(id, revision, status, editorRevision, hunks, message)
        }
        return synced { outcomeResult }
    }

    func uploadFile(conversationId: String, file: ChatUploadFile, name: String?) async throws -> ChatUpload {
        record("uploadFile:\(conversationId):\(name ?? file.name)")
        if let onUploadFile { return try await onUploadFile(conversationId, file, name) }
        let idx = synced { () -> Int in
            uploadCounter += 1
            return uploadCounter
        }
        return ChatUpload(
            id: "u\(idx)",
            conversationId: conversationId,
            name: name ?? file.name,
            mime: file.mimeType.isEmpty ? "text/plain" : file.mimeType,
            kind: "text",
            size: file.size,
            sha256: "sha256:x",
            createdAt: "x"
        )
    }

    func getUploadMeta(_ id: String) async throws -> ChatUpload {
        record("getUploadMeta:\(id)")
        return ChatUpload(id: id, conversationId: "c1", name: "a.txt", mime: "text/plain", kind: "text", size: 1, sha256: "x", createdAt: "x")
    }

    func downloadUpload(_ id: String) async throws -> Data {
        record("downloadUpload:\(id)")
        return Data("hello".utf8)
    }

    func streamTurn(
        _ turnId: String,
        after: Int,
        onEvent: (@Sendable (ChatEvent) async throws -> Void)?,
        onHeartbeat: (@Sendable (String) -> Void)?
    ) async throws -> StreamTurnResult {
        record("streamTurn:\(turnId):\(after)")
        if let onStreamTurn {
            return try await onStreamTurn(turnId, after, onEvent, onHeartbeat)
        }
        return StreamTurnResult(lastSeq: after, terminal: true)
    }
}
