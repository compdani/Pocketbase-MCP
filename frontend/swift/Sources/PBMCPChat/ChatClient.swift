import Foundation

/// Generate a random RFC 4122 v4 idempotency key (lowercase).
public func newIdempotencyKey() -> String {
    UUID().uuidString.lowercased()
}

/// Transport protocol implemented by `ChatClient` and test doubles.
public protocol ChatTransport: Sendable {
    func status() async throws -> ChatStatus
    func listConversations(limit: Int?, cursor: String?, target: String?) async throws -> ChatConversationList
    func createConversation(title: String?, target: String?) async throws -> ChatConversation
    func getConversation(_ id: String, limit: Int?, cursor: String?) async throws -> ChatConversationDetail
    func renameConversation(_ id: String, title: String) async throws -> ChatConversation
    func deleteConversation(_ id: String) async throws
    func getContext(_ id: String) async throws -> ChatContextStatus
    func compactConversation(_ id: String) async throws -> ChatCompactResult
    func importConversation(
        sourceId: String,
        title: String?,
        createdAt: String?,
        messages: [ChatImportMessage]
    ) async throws -> ChatImportResult
    func submitTurn(
        _ conversationId: String,
        text: String,
        provider: String?,
        model: String?,
        effort: String?,
        attachments: [String]?,
        context: [String: JSONValue]?,
        idempotencyKey: String?
    ) async throws -> ChatSubmitResult
    func getTurn(_ id: String) async throws -> ChatTurn
    func cancelTurn(_ id: String) async throws -> ChatTurn
    func getProposal(_ id: String) async throws -> ChatProposal
    func decideProposal(
        _ id: String,
        decision: ProposalDecision,
        revision: Int,
        arguments: JSONValue?
    ) async throws -> ProposalDecisionResult
    func reportOutcome(
        _ id: String,
        revision: Int,
        status: ProposalOutcomeStatus,
        editorRevision: String?,
        hunks: [ChatHunkOutcome]?,
        message: String?
    ) async throws -> ProposalDecisionResult
    func uploadFile(conversationId: String, file: ChatUploadFile, name: String?) async throws -> ChatUpload
    func getUploadMeta(_ id: String) async throws -> ChatUpload
    func downloadUpload(_ id: String) async throws -> Data
    func streamTurn(
        _ turnId: String,
        after: Int,
        onEvent: (@Sendable (ChatEvent) async throws -> Void)?,
        onHeartbeat: (@Sendable (String) -> Void)?
    ) async throws -> StreamTurnResult
}

public extension ChatTransport {
    func listConversations(
        limit: Int? = nil,
        cursor: String? = nil,
        target: String? = nil
    ) async throws -> ChatConversationList {
        try await listConversations(limit: limit, cursor: cursor, target: target)
    }

    func createConversation(
        title: String? = nil,
        target: String? = nil
    ) async throws -> ChatConversation {
        try await createConversation(title: title, target: target)
    }

    func getConversation(
        _ id: String,
        limit: Int? = nil,
        cursor: String? = nil
    ) async throws -> ChatConversationDetail {
        try await getConversation(id, limit: limit, cursor: cursor)
    }

    func importConversation(
        sourceId: String,
        title: String? = nil,
        createdAt: String? = nil,
        messages: [ChatImportMessage]
    ) async throws -> ChatImportResult {
        try await importConversation(sourceId: sourceId, title: title, createdAt: createdAt, messages: messages)
    }

    func submitTurn(
        _ conversationId: String,
        text: String,
        provider: String? = nil,
        model: String? = nil,
        effort: String? = nil,
        attachments: [String]? = nil,
        context: [String: JSONValue]? = nil,
        idempotencyKey: String? = nil
    ) async throws -> ChatSubmitResult {
        try await submitTurn(
            conversationId,
            text: text,
            provider: provider,
            model: model,
            effort: effort,
            attachments: attachments,
            context: context,
            idempotencyKey: idempotencyKey
        )
    }

    func decideProposal(
        _ id: String,
        decision: ProposalDecision,
        revision: Int,
        arguments: JSONValue? = nil
    ) async throws -> ProposalDecisionResult {
        try await decideProposal(id, decision: decision, revision: revision, arguments: arguments)
    }

    func reportOutcome(
        _ id: String,
        revision: Int,
        status: ProposalOutcomeStatus,
        editorRevision: String? = nil,
        hunks: [ChatHunkOutcome]? = nil,
        message: String? = nil
    ) async throws -> ProposalDecisionResult {
        try await reportOutcome(
            id,
            revision: revision,
            status: status,
            editorRevision: editorRevision,
            hunks: hunks,
            message: message
        )
    }

    func uploadFile(
        conversationId: String,
        file: ChatUploadFile,
        name: String? = nil
    ) async throws -> ChatUpload {
        try await uploadFile(conversationId: conversationId, file: file, name: name)
    }

    func streamTurn(
        _ turnId: String,
        after: Int = 0,
        onEvent: (@Sendable (ChatEvent) async throws -> Void)? = nil,
        onHeartbeat: (@Sendable (String) -> Void)? = nil
    ) async throws -> StreamTurnResult {
        try await streamTurn(turnId, after: after, onEvent: onEvent, onHeartbeat: onHeartbeat)
    }
}

/// Typed HTTP client for the MCP chat API (protocol v1), ported from `client.mjs`.
///
/// `getToken` runs before every request and its value is placed ONLY in the
/// `Authorization` header (never in URLs).
public final class ChatClient: ChatTransport, Sendable {
    public static let defaultApiRoot = "/api/mcp/chat/v1"

    public let baseURL: URL
    public let apiRoot: String
    public var prefix: String { apiRoot }
    public let getToken: @Sendable () async throws -> String
    public let session: URLSession

    private static let statusCodes: [Int: String] = [
        400: "invalid_request",
        401: "unauthorized",
        403: "forbidden",
        404: "not_found",
        405: "method_not_allowed",
        409: "conflict",
        413: "payload_too_large",
        429: "rate_limited",
        500: "internal",
    ]

    public init(
        baseURL: URL,
        apiRoot: String = ChatClient.defaultApiRoot,
        getToken: @escaping @Sendable () async throws -> String,
        session: URLSession = .shared
    ) {
        self.baseURL = baseURL
        var clean = apiRoot.isEmpty ? Self.defaultApiRoot : apiRoot
        if !clean.hasPrefix("/") { clean = "/" + clean }
        while clean.count > 1 && clean.hasSuffix("/") {
            clean.removeLast()
        }
        self.apiRoot = clean
        self.getToken = getToken
        self.session = session
    }

    // MARK: - URL construction

    private static let pathSegmentAllowed: CharacterSet = {
        // Strict RFC 3986 unreserved characters matching JS encodeURIComponent.
        var set = CharacterSet()
        set.insert(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()")
        return set
    }()

    static func encodeSegment(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: pathSegmentAllowed) ?? value
    }

    func buildURL(path: String, query: [(String, String)] = []) throws -> URL {
        var baseString = baseURL.absoluteString
        while baseString.hasSuffix("/") {
            baseString.removeLast()
        }
        var urlString = "\(baseString)\(apiRoot)\(path)"
        let filtered = query.filter { !$0.1.isEmpty }
        if !filtered.isEmpty {
            var components = URLComponents()
            components.queryItems = filtered.map { URLQueryItem(name: $0.0, value: $0.1) }
            // Encode "/" inside query values to match JS URLSearchParams.
            if let raw = components.percentEncodedQuery {
                let encoded = raw.replacingOccurrences(of: "/", with: "%2F")
                urlString += "?\(encoded)"
            }
        }
        guard let url = URL(string: urlString) else {
            throw ChatValidationError("Invalid request URL: \(urlString)")
        }
        return url
    }

    // MARK: - Core HTTP helpers

    private func makeRequest(
        method: String,
        path: String,
        query: [(String, String)] = [],
        jsonBody: Data? = nil,
        rawBody: Data? = nil,
        contentType: String? = nil,
        accept: String = "application/json",
        extraHeaders: [String: String] = [:],
        idempotencyKey: String? = nil
    ) async throws -> URLRequest {
        try Task.checkCancellation()
        let token = try await getToken()
        try Task.checkCancellation()

        let url = try buildURL(path: path, query: query)
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue(accept, forHTTPHeaderField: "Accept")
        for (k, v) in extraHeaders {
            request.setValue(v, forHTTPHeaderField: k)
        }
        if !token.isEmpty {
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        if let jsonBody {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = jsonBody
        } else if let rawBody {
            if let contentType {
                request.setValue(contentType, forHTTPHeaderField: "Content-Type")
            }
            request.httpBody = rawBody
        }
        if let idempotencyKey, !idempotencyKey.isEmpty {
            request.setValue(idempotencyKey, forHTTPHeaderField: "Idempotency-Key")
        }
        return request
    }

    private func sendData(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch is CancellationError {
            throw CancellationError()
        } catch let urlError as URLError where urlError.code == .cancelled {
            throw CancellationError()
        } catch {
            throw ChatNetworkError(message: "The server could not be reached.", underlyingError: error)
        }

        guard let http = response as? HTTPURLResponse else {
            throw ChatNetworkError(message: "The server did not return an HTTP response.")
        }
        if !(200...299).contains(http.statusCode) {
            throw Self.decodeError(status: http.statusCode, body: data)
        }
        return (data, http)
    }

    /// Decode a non-2xx HTTP error response into `ResyncRequiredError` or `ChatApiError`.
    public static func decodeError(status: Int, body: Data, statusText: String = "") -> any Error {
        var code = ""
        var message = ""
        var proposal: ChatProposal? = nil

        if !body.isEmpty,
           let parsed = try? JSONDecoder().decode(JSONValue.self, from: body),
           case .object(let root) = parsed {
            if case .object(let errObj) = root["error"] {
                if let c = errObj["code"]?.stringValue { code = c }
                if let m = errObj["message"]?.stringValue { message = m }
            }
            if case .object = root["proposal"] {
                if let propData = try? JSONEncoder().encode(root["proposal"]),
                   let decoded = try? JSONDecoder().decode(ChatProposal.self, from: propData) {
                    proposal = decoded
                }
            }
        }

        if code.isEmpty {
            code = statusCodes[status] ?? "internal"
        }
        if message.isEmpty {
            message = statusText.isEmpty ? "The request failed." : statusText
        }
        if code == "resync_required" {
            return ResyncRequiredError(status: status, message: message)
        }
        return ChatApiError(status: status, code: code, message: message, proposal: proposal)
    }

    private func requestJSON<T: Decodable>(
        _ type: T.Type,
        method: String,
        path: String,
        query: [(String, String)] = [],
        body: JSONValue? = nil,
        idempotencyKey: String? = nil
    ) async throws -> T {
        let bodyData: Data? = try body.map { try JSONEncoder().encode($0) }
        let request = try await makeRequest(
            method: method,
            path: path,
            query: query,
            jsonBody: bodyData,
            idempotencyKey: idempotencyKey
        )
        let (data, http) = try await sendData(request)
        if data.isEmpty {
            throw ChatApiError(status: http.statusCode, code: "invalid_response", message: "The server returned an invalid response.")
        }
        do {
            return try JSONDecoder().decode(T.self, from: data)
        } catch {
            throw ChatApiError(status: http.statusCode, code: "invalid_response", message: "The server returned an invalid response.")
        }
    }

    // MARK: - Endpoints

    /// `GET /status`
    public func status() async throws -> ChatStatus {
        try await requestJSON(ChatStatus.self, method: "GET", path: "/status")
    }

    /// `GET /conversations`
    public func listConversations(
        limit: Int? = nil,
        cursor: String? = nil,
        target: String? = nil
    ) async throws -> ChatConversationList {
        var query: [(String, String)] = []
        if let limit { query.append(("limit", String(limit))) }
        if let cursor, !cursor.isEmpty { query.append(("cursor", cursor)) }
        if let target, !target.isEmpty { query.append(("target", target)) }
        return try await requestJSON(ChatConversationList.self, method: "GET", path: "/conversations", query: query)
    }

    /// `POST /conversations`
    public func createConversation(
        title: String? = nil,
        target: String? = nil
    ) async throws -> ChatConversation {
        var dict: [String: JSONValue] = [:]
        if let title { dict["title"] = .string(title) }
        if let target { dict["target"] = .string(target) }
        return try await requestJSON(ChatConversation.self, method: "POST", path: "/conversations", body: .object(dict))
    }

    /// `GET /conversations/{id}`
    public func getConversation(
        _ id: String,
        limit: Int? = nil,
        cursor: String? = nil
    ) async throws -> ChatConversationDetail {
        var query: [(String, String)] = []
        if let limit { query.append(("limit", String(limit))) }
        if let cursor, !cursor.isEmpty { query.append(("cursor", cursor)) }
        return try await requestJSON(
            ChatConversationDetail.self,
            method: "GET",
            path: "/conversations/\(Self.encodeSegment(id))",
            query: query
        )
    }

    /// `PATCH /conversations/{id}`
    public func renameConversation(_ id: String, title: String) async throws -> ChatConversation {
        try await requestJSON(
            ChatConversation.self,
            method: "PATCH",
            path: "/conversations/\(Self.encodeSegment(id))",
            body: .object(["title": .string(title)])
        )
    }

    /// `DELETE /conversations/{id}`
    public func deleteConversation(_ id: String) async throws {
        let request = try await makeRequest(
            method: "DELETE",
            path: "/conversations/\(Self.encodeSegment(id))"
        )
        _ = try await sendData(request)
    }

    /// `GET /conversations/{id}/context`
    public func getContext(_ id: String) async throws -> ChatContextStatus {
        try await requestJSON(
            ChatContextStatus.self,
            method: "GET",
            path: "/conversations/\(Self.encodeSegment(id))/context"
        )
    }

    /// `POST /conversations/{id}/compact`
    public func compactConversation(_ id: String) async throws -> ChatCompactResult {
        try await requestJSON(
            ChatCompactResult.self,
            method: "POST",
            path: "/conversations/\(Self.encodeSegment(id))/compact"
        )
    }

    /// `POST /conversations/actions/import`
    public func importConversation(
        sourceId: String,
        title: String? = nil,
        createdAt: String? = nil,
        messages: [ChatImportMessage]
    ) async throws -> ChatImportResult {
        var dict: [String: JSONValue] = [
            "sourceId": .string(sourceId),
            "messages": .array(messages.map { msg in
                var m: [String: JSONValue] = [
                    "role": .string(msg.role),
                    "text": .string(msg.text),
                ]
                if let createdAt = msg.createdAt { m["createdAt"] = .string(createdAt) }
                return .object(m)
            }),
        ]
        if let title { dict["title"] = .string(title) }
        if let createdAt { dict["createdAt"] = .string(createdAt) }
        return try await requestJSON(
            ChatImportResult.self,
            method: "POST",
            path: "/conversations/actions/import",
            body: .object(dict)
        )
    }

    /// `POST /conversations/{id}/turns`
    public func submitTurn(
        _ conversationId: String,
        text: String,
        provider: String? = nil,
        model: String? = nil,
        effort: String? = nil,
        attachments: [String]? = nil,
        context: [String: JSONValue]? = nil,
        idempotencyKey: String? = nil
    ) async throws -> ChatSubmitResult {
        let key = (idempotencyKey?.isEmpty == false) ? idempotencyKey! : newIdempotencyKey()
        var dict: [String: JSONValue] = ["text": .string(text)]
        if let provider, !provider.isEmpty { dict["provider"] = .string(provider) }
        if let model, !model.isEmpty { dict["model"] = .string(model) }
        if let effort, !effort.isEmpty { dict["effort"] = .string(effort) }
        if let attachments, !attachments.isEmpty {
            dict["attachments"] = .array(attachments.map { .string($0) })
        }
        if let context {
            dict["context"] = .object(context)
        }

        struct SubmitEnvelope: Decodable {
            let turn: ChatTurn?
            let created: Bool?
        }

        let envelope = try await requestJSON(
            SubmitEnvelope.self,
            method: "POST",
            path: "/conversations/\(Self.encodeSegment(conversationId))/turns",
            body: .object(dict),
            idempotencyKey: key
        )
        guard let turn = envelope.turn else {
            throw ChatApiError(status: 202, code: "invalid_response", message: "The server returned an invalid response.")
        }
        return ChatSubmitResult(turn: turn, created: envelope.created == true)
    }

    /// `GET /turns/{id}`
    public func getTurn(_ id: String) async throws -> ChatTurn {
        try await requestJSON(ChatTurn.self, method: "GET", path: "/turns/\(Self.encodeSegment(id))")
    }

    /// `POST /turns/{id}/cancel`
    public func cancelTurn(_ id: String) async throws -> ChatTurn {
        try await requestJSON(ChatTurn.self, method: "POST", path: "/turns/\(Self.encodeSegment(id))/cancel")
    }

    // MARK: - Proposals

    private struct ProposalEnvelope: Decodable {
        let proposal: ChatProposal?
        let replayed: Bool?
    }

    /// `GET /proposals/{id}`
    public func getProposal(_ id: String) async throws -> ChatProposal {
        let env = try await requestJSON(
            ProposalEnvelope.self,
            method: "GET",
            path: "/proposals/\(Self.encodeSegment(id))"
        )
        guard let proposal = env.proposal else {
            throw ChatApiError(status: 200, code: "invalid_response", message: "The server returned an invalid response.")
        }
        return proposal
    }

    /// `POST /proposals/{id}/decision`
    public func decideProposal(
        _ id: String,
        decision: ProposalDecision,
        revision: Int,
        arguments: JSONValue? = nil
    ) async throws -> ProposalDecisionResult {
        guard revision >= 1 else {
            throw ChatValidationError("decideProposal() requires the reviewed revision")
        }
        var dict: [String: JSONValue] = [
            "decision": .string(decision.rawValue),
            "revision": .number(Double(revision)),
        ]
        if let arguments, !arguments.isNull {
            dict["arguments"] = arguments
        }
        let env = try await requestJSON(
            ProposalEnvelope.self,
            method: "POST",
            path: "/proposals/\(Self.encodeSegment(id))/decision",
            body: .object(dict)
        )
        guard let proposal = env.proposal else {
            throw ChatApiError(status: 200, code: "invalid_response", message: "The server returned an invalid response.")
        }
        return ProposalDecisionResult(proposal: proposal, replayed: env.replayed == true)
    }

    /// `POST /proposals/{id}/outcome`
    public func reportOutcome(
        _ id: String,
        revision: Int,
        status: ProposalOutcomeStatus,
        editorRevision: String? = nil,
        hunks: [ChatHunkOutcome]? = nil,
        message: String? = nil
    ) async throws -> ProposalDecisionResult {
        guard revision >= 1 else {
            throw ChatValidationError("reportOutcome() requires the approved revision")
        }
        var dict: [String: JSONValue] = [
            "revision": .number(Double(revision)),
            "status": .string(status.rawValue),
        ]
        if let editorRevision, !editorRevision.isEmpty {
            dict["editorRevision"] = .string(editorRevision)
        }
        if let hunks, !hunks.isEmpty {
            dict["hunks"] = .array(hunks.map {
                .object(["id": .string($0.id), "status": .string($0.status.rawValue)])
            })
        }
        if let message, !message.isEmpty {
            dict["message"] = .string(message)
        }
        let env = try await requestJSON(
            ProposalEnvelope.self,
            method: "POST",
            path: "/proposals/\(Self.encodeSegment(id))/outcome",
            body: .object(dict)
        )
        guard let proposal = env.proposal else {
            throw ChatApiError(status: 200, code: "invalid_response", message: "The server returned an invalid response.")
        }
        return ProposalDecisionResult(proposal: proposal, replayed: env.replayed == true)
    }

    // MARK: - Uploads

    private struct UploadEnvelope: Decodable {
        let upload: ChatUpload?
    }

    /// `POST /uploads` (multipart/form-data with exact fields `conversationId` and `file`).
    public func uploadFile(
        conversationId: String,
        file: ChatUploadFile,
        name: String? = nil
    ) async throws -> ChatUpload {
        guard !conversationId.isEmpty else {
            throw ChatValidationError("uploadFile() requires a conversation id")
        }
        let filename = (name?.isEmpty == false ? name! : (file.name.isEmpty ? "upload" : file.name))
            .replacingOccurrences(of: "\"", with: "_")
            .replacingOccurrences(of: "\r", with: "_")
            .replacingOccurrences(of: "\n", with: "_")
        let mime = file.mimeType.isEmpty ? "application/octet-stream" : file.mimeType
        let boundary = "Boundary-\(UUID().uuidString)"

        var body = Data()
        func append(_ string: String) {
            body.append(Data(string.utf8))
        }
        append("--\(boundary)\r\n")
        append("Content-Disposition: form-data; name=\"conversationId\"\r\n\r\n")
        append("\(conversationId)\r\n")

        append("--\(boundary)\r\n")
        append("Content-Disposition: form-data; name=\"file\"; filename=\"\(filename)\"\r\n")
        append("Content-Type: \(mime)\r\n\r\n")
        body.append(file.data)
        append("\r\n")
        append("--\(boundary)--\r\n")

        let request = try await makeRequest(
            method: "POST",
            path: "/uploads",
            rawBody: body,
            contentType: "multipart/form-data; boundary=\(boundary)"
        )
        let (data, http) = try await sendData(request)
        guard let env = try? JSONDecoder().decode(UploadEnvelope.self, from: data),
              let upload = env.upload else {
            throw ChatApiError(status: http.statusCode, code: "invalid_response", message: "The server returned an invalid response.")
        }
        return upload
    }

    /// `GET /uploads/{id}/meta`
    public func getUploadMeta(_ id: String) async throws -> ChatUpload {
        let env = try await requestJSON(
            UploadEnvelope.self,
            method: "GET",
            path: "/uploads/\(Self.encodeSegment(id))/meta"
        )
        guard let upload = env.upload else {
            throw ChatApiError(status: 200, code: "invalid_response", message: "The server returned an invalid response.")
        }
        return upload
    }

    /// `GET /uploads/{id}` (returns raw bytes; token stays in `Authorization` header only).
    public func downloadUpload(_ id: String) async throws -> Data {
        let request = try await makeRequest(
            method: "GET",
            path: "/uploads/\(Self.encodeSegment(id))",
            accept: "*/*"
        )
        let (data, _) = try await sendData(request)
        return data
    }

    // MARK: - SSE Stream

    /// `GET /turns/{id}/events`
    public func streamTurn(
        _ turnId: String,
        after: Int = 0,
        onEvent: (@Sendable (ChatEvent) async throws -> Void)? = nil,
        onHeartbeat: (@Sendable (String) -> Void)? = nil
    ) async throws -> StreamTurnResult {
        let resume = after > 0 ? after : 0
        var extraHeaders: [String: String] = [:]
        if resume > 0 {
            extraHeaders["Last-Event-ID"] = String(resume)
        }
        let request = try await makeRequest(
            method: "GET",
            path: "/turns/\(Self.encodeSegment(turnId))/events",
            query: resume > 0 ? [("after", String(resume))] : [],
            accept: "text/event-stream",
            extraHeaders: extraHeaders
        )

        let bytes: URLSession.AsyncBytes
        let response: URLResponse
        do {
            (bytes, response) = try await session.bytes(for: request)
        } catch is CancellationError {
            throw CancellationError()
        } catch let urlError as URLError where urlError.code == .cancelled {
            throw CancellationError()
        } catch {
            throw ChatNetworkError(message: "The server could not be reached.", underlyingError: error)
        }

        guard let http = response as? HTTPURLResponse else {
            throw ChatNetworkError(message: "The server did not return an HTTP response.")
        }

        if !(200...299).contains(http.statusCode) {
            var errBytes: [UInt8] = []
            errBytes.reserveCapacity(1024)
            do {
                for try await b in bytes {
                    errBytes.append(b)
                    if errBytes.count >= 1024 * 1024 { break }
                }
            } catch {
                // Use whatever bytes arrived before closing.
            }
            throw Self.decodeError(status: http.statusCode, body: Data(errBytes))
        }

        let contentType = http.value(forHTTPHeaderField: "Content-Type") ?? ""
        if !contentType.isEmpty && !contentType.lowercased().contains("text/event-stream") {
            var bodyBytes: [UInt8] = []
            bodyBytes.reserveCapacity(1024)
            do {
                for try await b in bytes {
                    bodyBytes.append(b)
                    if bodyBytes.count >= 1024 * 1024 { break }
                }
            } catch {
                // ignore read error
            }
            let bodyData = Data(bodyBytes)
            if !bodyData.isEmpty,
               let parsed = try? JSONDecoder().decode(JSONValue.self, from: bodyData),
               case .object(let root) = parsed,
               case .object(let errObj) = root["error"],
               let code = errObj["code"]?.stringValue {
                let message = errObj["message"]?.stringValue ?? ""
                if code == "resync_required" {
                    throw ResyncRequiredError(status: http.statusCode, message: message)
                }
                throw ChatApiError(status: http.statusCode, code: code, message: message)
            }
            throw ChatProtocolError(message: "Unexpected event stream content type \"\(contentType)\".")
        }

        var lastSeq = resume
        var terminal = false
        var parser = try SSEParser()

        func handleFrame(_ frame: SSEFrame) async throws -> Bool {
            guard let frameData = frame.data.data(using: .utf8),
                  let rawJSON = try? JSONDecoder().decode(JSONValue.self, from: frameData) else {
                throw ChatProtocolError(message: "The event stream contained invalid JSON.")
            }
            guard case .object(let obj) = rawJSON else {
                throw ChatProtocolError(message: "The event stream contained an invalid envelope.")
            }
            let version = obj["v"]?.stringValue ?? ""
            if version != PROTOCOL_VERSION {
                throw ChatProtocolError(
                    message: "Unsupported event version \"\(version)\"; this client implements version 1.",
                    code: "unsupported_version"
                )
            }
            guard let seq = obj["seq"]?.intValue, seq >= 1 else {
                throw ChatProtocolError(message: "The event stream contained an invalid sequence.")
            }
            guard let eventType = obj["type"]?.stringValue, !eventType.isEmpty else {
                throw ChatProtocolError(message: "The event stream contained an invalid event type.")
            }
            let eventTurnId = obj["turnId"]?.stringValue ?? ""
            if !eventTurnId.isEmpty && eventTurnId != turnId {
                throw ChatProtocolError(message: "The event stream contained an event for another turn.")
            }
            let event = ChatEvent(
                v: version,
                seq: seq,
                conversationId: obj["conversationId"]?.stringValue ?? "",
                turnId: eventTurnId,
                type: eventType,
                data: obj["data"],
                at: obj["at"]?.stringValue ?? ""
            )
            if event.seq > lastSeq {
                lastSeq = event.seq
            }
            try await onEvent?(event)
            if ChatReducer.isTerminalEventType(event.type) {
                terminal = true
                return true
            }
            return false
        }

        var chunkBuffer: [UInt8] = []
        chunkBuffer.reserveCapacity(4096)
        do {
            for try await byte in bytes {
                try Task.checkCancellation()
                chunkBuffer.append(byte)
                // Flush at line boundaries or 8 KiB chunks so live tokens arrive with zero delay
                // while avoiding per-byte parser calls on large payloads.
                if byte == 0x0A || byte == 0x0D || chunkBuffer.count >= 8192 {
                    let frames = try parser.feed(chunkBuffer, onComment: onHeartbeat)
                    chunkBuffer.removeAll(keepingCapacity: true)
                    for frame in frames {
                        if try await handleFrame(frame) {
                            return StreamTurnResult(lastSeq: lastSeq, terminal: true)
                        }
                    }
                }
            }
            if !chunkBuffer.isEmpty {
                let frames = try parser.feed(chunkBuffer, onComment: onHeartbeat)
                chunkBuffer.removeAll(keepingCapacity: true)
                for frame in frames {
                    if try await handleFrame(frame) {
                        return StreamTurnResult(lastSeq: lastSeq, terminal: true)
                    }
                }
            }
        } catch is CancellationError {
            throw CancellationError()
        } catch let urlError as URLError where urlError.code == .cancelled {
            throw CancellationError()
        } catch let err as ChatProtocolError {
            throw err
        } catch let err as ChatApiError {
            throw err
        } catch let err as ResyncRequiredError {
            throw err
        } catch let err as ReducerError {
            throw err
        } catch let err as SSEError {
            throw err
        } catch {
            throw ChatNetworkError(message: "The event stream ended unexpectedly.", underlyingError: error)
        }

        let tailFrames = try parser.finish(onComment: onHeartbeat)
        for frame in tailFrames {
            if try await handleFrame(frame) {
                return StreamTurnResult(lastSeq: lastSeq, terminal: true)
            }
        }

        return StreamTurnResult(lastSeq: lastSeq, terminal: terminal)
    }
}
