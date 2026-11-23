import Foundation
import Testing
@testable import PBMCPChat

@Suite("ChatClient")
struct ChatClientTests {
    @Test("getToken runs per request and token only travels in Authorization header")
    func tokenPerRequestHeaderOnly() async throws {
        final class Box: @unchecked Sendable {
            let lock = NSLock()
            var issued = 0
            var requests: [URLRequest] = []
        }
        let box = Box()
        let (session, stubId) = StubURLProtocol.makeSession { req, _ in
            box.lock.lock()
            box.requests.append(req)
            box.lock.unlock()
            return makeEncodableResponse(url: req.url!, value: ChatStatus(version: "1", models: ["m1"]))
        }
        defer { StubURLProtocol.unregister(id: stubId) }

        let client = ChatClient(
            baseURL: URL(string: "https://api.test/")!,
            getToken: {
                let n = box.lock.withLock {
                    box.issued += 1
                    return box.issued
                }
                return "tok-\(n)"
            },
            session: session
        )

        let first = try await client.status()
        let second = try await client.status()
        #expect(first.version == "1")
        #expect(second.version == "1")
        #expect(box.issued == 2)
        #expect(box.requests[0].url?.absoluteString == "https://api.test/api/mcp/chat/v1/status")
        #expect(box.requests[0].value(forHTTPHeaderField: "Authorization") == "Bearer tok-1")
        #expect(box.requests[1].value(forHTTPHeaderField: "Authorization") == "Bearer tok-2")
        for req in box.requests {
            let urlStr = req.url?.absoluteString ?? ""
            #expect(!urlStr.contains("tok-"))
            #expect(!urlStr.contains("token="))
            #expect(!urlStr.contains("access_token="))
        }
    }

    @Test("path segments are percent-encoded and query carries only set fields")
    func urlEncodingAndQueries() async throws {
        final class Box: @unchecked Sendable {
            let lock = NSLock()
            var urls: [String] = []
            var methods: [String] = []
        }
        let box = Box()
        let (session, stubId) = StubURLProtocol.makeSession { req, _ in
            box.lock.lock()
            box.urls.append(req.url!.absoluteString)
            box.methods.append(req.httpMethod ?? "")
            box.lock.unlock()
            if req.url!.path.contains("/turns/") {
                return makeEncodableResponse(url: req.url!, value: testTurn(.cancelled))
            }
            if req.url!.path.hasSuffix("/conversations") {
                return makeEncodableResponse(url: req.url!, value: ChatConversationList())
            }
            return makeEncodableResponse(url: req.url!, value: ChatConversationDetail(conversation: testConversation("c1")))
        }
        defer { StubURLProtocol.unregister(id: stubId) }

        let client = ChatClient(baseURL: URL(string: "https://api.test")!, getToken: { "t" }, session: session)
        _ = try await client.getConversation("a/b c")
        _ = try await client.cancelTurn("t 1")
        _ = try await client.listConversations(limit: 10, cursor: "abc", target: "rec/1")
        _ = try await client.listConversations()

        #expect(box.urls[0] == "https://api.test/api/mcp/chat/v1/conversations/a%2Fb%20c")
        #expect(box.urls[1] == "https://api.test/api/mcp/chat/v1/turns/t%201/cancel")
        #expect(box.methods[1] == "POST")
        #expect(box.urls[2] == "https://api.test/api/mcp/chat/v1/conversations?limit=10&cursor=abc&target=rec%2F1")
        #expect(box.urls[3] == "https://api.test/api/mcp/chat/v1/conversations")
    }

    @Test("submitTurn sends Idempotency-Key, effort, and context")
    func submitTurnPayload() async throws {
        final class Box: @unchecked Sendable {
            let lock = NSLock()
            var headers: [[String: String]] = []
            var bodies: [JSONValue] = []
        }
        let box = Box()
        let (session, stubId) = StubURLProtocol.makeSession { req, body in
            box.lock.lock()
            box.headers.append(req.allHTTPHeaderFields ?? [:])
            if let body, let json = try? JSONDecoder().decode(JSONValue.self, from: body) {
                box.bodies.append(json)
            }
            box.lock.unlock()
            return makeEncodableResponse(url: req.url!, status: 202, value: ChatSubmitResult(turn: testTurn(.queued), created: true))
        }
        defer { StubURLProtocol.unregister(id: stubId) }

        let client = ChatClient(baseURL: URL(string: "https://api.test")!, getToken: { "t" }, session: session)
        let res = try await client.submitTurn("c1", text: "hello")
        #expect(res.created == true)
        #expect(box.headers[0]["Idempotency-Key"]?.count == 36)
        #expect(box.bodies[0] == .object(["text": .string("hello")]))

        _ = try await client.submitTurn(
            "c1",
            text: "think",
            provider: "openai",
            model: "gpt-5",
            effort: "high",
            attachments: ["u1"],
            context: ["route": .string("/tasks/1")],
            idempotencyKey: "fixed-key"
        )
        #expect(box.headers[1]["Idempotency-Key"] == "fixed-key")
        #expect(box.bodies[1] == .object([
            "text": .string("think"),
            "provider": .string("openai"),
            "model": .string("gpt-5"),
            "effort": .string("high"),
            "attachments": .array([.string("u1")]),
            "context": .object(["route": .string("/tasks/1")]),
        ]))
    }

    @Test("streamTurn sends Last-Event-ID on resume, parses events, reports heartbeats, and rejects bad version")
    func streamTurnBehavior() async throws {
        final class Box: @unchecked Sendable {
            let lock = NSLock()
            var callCount = 0
            var lastHeaders: [String: String] = [:]
            var lastURL = ""
        }
        let box = Box()
        let events = [
            testEvent(5, "turn.started"),
            testEvent(6, "text.delta", data: .object(["text": .string("hi")])),
            testEvent(7, "turn.completed"),
        ]
        let (session, stubId) = StubURLProtocol.makeSession { req, _ in
            box.lock.lock()
            box.callCount += 1
            let n = box.callCount
            box.lastHeaders = req.allHTTPHeaderFields ?? [:]
            box.lastURL = req.url!.absoluteString
            box.lock.unlock()
            if n == 1 {
                return makeSSEResponse(url: req.url!, body: ": ping\n\n" + sseFrames(events))
            }
            return makeSSEResponse(url: req.url!, body: sseFrames([testEvent(1, "turn.started", v: "2")]))
        }
        defer { StubURLProtocol.unregister(id: stubId) }

        let client = ChatClient(baseURL: URL(string: "https://api.test")!, getToken: { "t" }, session: session)
        let seen = MutexBox<[Int]>([])
        let beats = MutexBox<[String]>([])

        let result = try await client.streamTurn(
            "t1",
            after: 4,
            onEvent: { ev in seen.withLock { $0.append(ev.seq) } },
            onHeartbeat: { c in beats.withLock { $0.append(c) } }
        )
        #expect(box.lastHeaders["Last-Event-ID"] == "4")
        #expect(box.lastURL == "https://api.test/api/mcp/chat/v1/turns/t1/events?after=4")
        #expect(seen.value == [5, 6, 7])
        #expect(beats.value == ["ping"])
        #expect(result == StreamTurnResult(lastSeq: 7, terminal: true))

        await #expect(throws: ChatProtocolError.self) {
            _ = try await client.streamTurn("t1")
        }
    }

    @Test("uploadFile sends multipart/form-data with conversationId and file fields and downloadUpload uses Authorization")
    func uploadAndDownload() async throws {
        final class Box: @unchecked Sendable {
            let lock = NSLock()
            var uploadBody = ""
            var uploadContentType = ""
            var downloadAuth = ""
        }
        let box = Box()
        let (session, stubId) = StubURLProtocol.makeSession { req, body in
            if req.httpMethod == "POST" {
                box.lock.lock()
                box.uploadContentType = req.value(forHTTPHeaderField: "Content-Type") ?? ""
                box.uploadBody = String(data: body ?? Data(), encoding: .utf8) ?? ""
                box.lock.unlock()
                let upload = ChatUpload(id: "u1", conversationId: "c1", name: "notes.txt", mime: "text/plain", kind: "text", size: 5, sha256: "x", createdAt: "x")
                return makeEncodableResponse(url: req.url!, status: 201, value: ["upload": upload])
            } else {
                box.lock.lock()
                box.downloadAuth = req.value(forHTTPHeaderField: "Authorization") ?? ""
                box.lock.unlock()
                let resp = HTTPURLResponse(url: req.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "text/plain"])!
                return (resp, Data("hello".utf8))
            }
        }
        defer { StubURLProtocol.unregister(id: stubId) }

        let client = ChatClient(baseURL: URL(string: "https://api.test")!, getToken: { "secret-tok" }, session: session)
        let file = ChatUploadFile(data: Data("hello".utf8), name: "notes.txt", mimeType: "text/plain")
        let upload = try await client.uploadFile(conversationId: "c1", file: file)
        #expect(upload.id == "u1")
        #expect(box.uploadContentType.hasPrefix("multipart/form-data; boundary="))
        #expect(box.uploadBody.contains("name=\"conversationId\""))
        #expect(box.uploadBody.contains("c1"))
        #expect(box.uploadBody.contains("name=\"file\"; filename=\"notes.txt\""))
        #expect(box.uploadBody.contains("hello"))

        let downloaded = try await client.downloadUpload("u1")
        #expect(String(data: downloaded, encoding: .utf8) == "hello")
        #expect(box.downloadAuth == "Bearer secret-tok")
    }

    @Test("streamTurn handles >64 KiB SSE payload cleanly with chunked buffering")
    func streamLargePayload() async throws {
        let bigChunk = String(repeating: "a", count: 70_000)
        let events = [
            testEvent(1, "turn.started"),
            testEvent(2, "text.delta", data: .object(["text": .string(bigChunk)])),
            testEvent(3, "turn.completed"),
        ]
        let (session, stubId) = StubURLProtocol.makeSession { req, _ in
            makeSSEResponse(url: req.url!, body: sseFrames(events))
        }
        defer { StubURLProtocol.unregister(id: stubId) }

        let client = ChatClient(baseURL: URL(string: "https://api.test")!, getToken: { "t" }, session: session)
        let receivedText = MutexBox<String>("")
        let result = try await client.streamTurn("t1", onEvent: { ev in
            if ev.type == "text.delta", let t = ev.data?["text"]?.stringValue {
                receivedText.withLock { $0 += t }
            }
        })
        #expect(result == StreamTurnResult(lastSeq: 3, terminal: true))
        #expect(receivedText.value.count == 70_000)
    }
}

private final class MutexBox<T>: @unchecked Sendable {
    private let lock = NSLock()
    private var _value: T
    init(_ value: T) { _value = value }
    var value: T {
        lock.lock()
        defer { lock.unlock() }
        return _value
    }
    func withLock(_ body: (inout T) -> Void) {
        lock.lock()
        defer { lock.unlock() }
        body(&_value)
    }
}
