import Foundation
import Testing
@testable import PBMCPChat

@Suite("SSEParser")
struct SSEParserTests {
    private let sample = [
        ": ping",
        "",
        "id: 1",
        "event: turn.queued",
        "data: {\"v\":\"1\",\"seq\":1,\"text\":\"héllo 🌍\"}",
        "",
        "id: 2",
        "event: text.delta",
        "data: line one",
        "data: line two",
        "",
        "id: 3",
        "event: turn.completed",
        "data: {\"v\":\"1\",\"seq\":3}",
    ].joined(separator: "\n")

    private let expected: [SSEFrame] = [
        SSEFrame(id: "1", event: "turn.queued", data: "{\"v\":\"1\",\"seq\":1,\"text\":\"héllo 🌍\"}"),
        SSEFrame(id: "2", event: "text.delta", data: "line one\nline two"),
        SSEFrame(id: "3", event: "turn.completed", data: "{\"v\":\"1\",\"seq\":3}"),
    ]

    @Test("stream split at every byte offset parses identically")
    func splitAtEveryByteOffset() throws {
        let bytes = Array(sample.utf8)
        for offset in 0...bytes.count {
            var parser = try SSEParser()
            var frames = try parser.feed(bytes[..<offset])
            frames += try parser.feed(bytes[offset...])
            frames += try parser.finish()
            #expect(frames == expected, "split at byte \(offset)")
        }
    }

    @Test("one byte at a time keeps multi-byte UTF-8 intact")
    func oneByteAtATime() throws {
        var parser = try SSEParser()
        var frames: [SSEFrame] = []
        for byte in sample.utf8 {
            frames += try parser.feed([byte])
        }
        frames += try parser.finish()
        #expect(frames == expected)
    }

    @Test("CRLF and lone CR are line endings")
    func crlfAndLoneCR() throws {
        let crlf = sample.replacingOccurrences(of: "\n", with: "\r\n")
        let cr = sample.replacingOccurrences(of: "\n", with: "\r")

        for (text, chunkSize) in [(crlf, 3), (cr, 2)] {
            var parser = try SSEParser()
            var frames: [SSEFrame] = []
            let bytes = Array(text.utf8)
            var i = 0
            while i < bytes.count {
                let end = min(i + chunkSize, bytes.count)
                frames += try parser.feed(bytes[i..<end])
                i = end
            }
            frames += try parser.finish()
            #expect(frames == expected)
        }
    }

    @Test("CR at chunk boundary does not split CRLF into an extra blank line")
    func crAtChunkBoundary() throws {
        let text = "data: a\r\ndata: b\r\n\r\ndata: c\r\n\r\n"
        let bytes = Array(text.utf8)
        for offset in 0...bytes.count {
            var parser = try SSEParser()
            var frames = try parser.feed(bytes[..<offset])
            frames += try parser.feed(bytes[offset...])
            frames += try parser.finish()
            #expect(frames == [
                SSEFrame(id: "", event: "message", data: "a\nb"),
                SSEFrame(id: "", event: "message", data: "c"),
            ])
        }
    }

    @Test("comments report through onComment and never become frames")
    func commentsOnly() throws {
        var parser = try SSEParser()
        var comments: [String] = []
        var frames = try parser.feed(Data(": ping\n\n:ping-two\r\ndata: x\n\n: tail".utf8)) { comments.append($0) }
        frames += try parser.finish { comments.append($0) }
        #expect(comments == ["ping", "ping-two", "tail"])
        #expect(frames == [SSEFrame(id: "", event: "message", data: "x")])
    }

    @Test("large chunk of many small frames does not trip maxEventBytes")
    func manySmallFrames() throws {
        let text = String(repeating: "data: x\n\n", count: 2000)
        var parser = try SSEParser(maxEventBytes: 64)
        var frames = try parser.feed(Data(text.utf8))
        frames += try parser.finish()
        #expect(frames.count == 2000)
        #expect(frames.last == SSEFrame(id: "", event: "message", data: "x"))
    }

    @Test("oversized event throws event_too_large counting UTF-8 bytes")
    func maxEventBytesUTF8() throws {
        var parser = try SSEParser(maxEventBytes: 10)
        #expect(throws: SSEError(message: "SSE event exceeds 10 bytes.", code: "event_too_large")) {
            _ = try parser.feed(Data("data: 🌍🌍🌍🌍\n\n".utf8))
        }

        var okParser = try SSEParser(maxEventBytes: 20)
        let ok = try okParser.feed(Data("data: 🌍\n\n".utf8))
        #expect(ok == [SSEFrame(id: "", event: "message", data: "🌍")])
    }

    @Test("parseSSE async stream yields frames and cancels cleanly")
    func asyncStreamParsing() async throws {
        let stream = AsyncStream<Data> { continuation in
            continuation.yield(Data("id: 1\nevent: text.delta\ndata: hi\n\n".utf8))
            continuation.yield(Data("id: 2\nevent: turn.completed\ndata: ok\n\n".utf8))
            continuation.finish()
        }
        var frames: [SSEFrame] = []
        for try await frame in parseSSE(chunks: stream) {
            frames.append(frame)
        }
        #expect(frames.count == 2)
        #expect(frames[0].data == "hi")
        #expect(frames[1].event == "turn.completed")
    }
}
