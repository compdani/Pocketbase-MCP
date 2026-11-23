import Foundation

/// A single parsed Server-Sent Events frame.
public struct SSEFrame: Sendable, Equatable, Codable {
    public var id: String
    public var event: String
    public var data: String

    public init(id: String, event: String, data: String) {
        self.id = id
        self.event = event
        self.data = data
    }
}

/// Incremental, UTF-8 byte-safe Server-Sent Events parser matching `sse.mjs`.
///
/// - Chunks may split anywhere, including inside a multi-byte UTF-8 sequence.
/// - LF, CRLF, and lone CR all end a line, including a CR at a chunk boundary.
/// - Several `data:` lines join with `"\n"`.
/// - Comment lines start with `":"` and invoke `onComment` with one leading space stripped.
/// - A final frame without a trailing blank line is still dispatched if it had `data:`.
/// - Each frame is bounded by `maxEventBytes` (UTF-8 bytes, default 1 MiB).
public struct SSEParser: Sendable {
    public static let defaultMaxEventBytes = 1024 * 1024

    public let maxEventBytes: Int

    // Persistent SSE last-event-id across frames.
    private var lastId: String = ""
    private var dataLines: [String] = []
    private var hasData: Bool = false
    private var eventName: String = ""
    private var eventBytes: Int = 0

    // Raw line assembly buffer (bytes), plus pending CR flag for CRLF split across chunks.
    private var lineBytes: [UInt8] = []
    private var pendingCR: Bool = false

    public init(maxEventBytes: Int = SSEParser.defaultMaxEventBytes) throws {
        guard maxEventBytes > 0 else {
            throw ChatValidationError("maxEventBytes must be a positive finite number")
        }
        self.maxEventBytes = maxEventBytes
    }

    /// Number of UTF-8 bytes a string occupies.
    public static func utf8ByteLength(_ value: String) -> Int {
        value.utf8.count
    }

    /// Feed a chunk of bytes. Returns any complete frames produced and invokes `onComment`
    /// for each comment line encountered.
    public mutating func feed(
        _ chunk: some Sequence<UInt8>,
        onComment: ((String) -> Void)? = nil
    ) throws -> [SSEFrame] {
        var frames: [SSEFrame] = []

        for byte in chunk {
            if pendingCR {
                pendingCR = false
                if byte == 0x0A { // \n completing \r\n
                    continue
                }
            }

            if byte == 0x0A { // \n
                if let frame = try takeLine(decodeLineBytes(), onComment: onComment) {
                    frames.append(frame)
                }
            } else if byte == 0x0D { // \r
                pendingCR = true
                if let frame = try takeLine(decodeLineBytes(), onComment: onComment) {
                    frames.append(frame)
                }
            } else {
                lineBytes.append(byte)
                if lineBytes.count > maxEventBytes + 4096 {
                    throw tooLarge()
                }
            }
        }

        return frames
    }

    /// Flush the end of the stream. Dispatches any unterminated line and any trailing frame
    /// that ended without a blank line.
    public mutating func finish(onComment: ((String) -> Void)? = nil) throws -> [SSEFrame] {
        var frames: [SSEFrame] = []
        pendingCR = false

        if !lineBytes.isEmpty {
            if let frame = try takeLine(decodeLineBytes(), onComment: onComment) {
                frames.append(frame)
            }
        }
        if let tail = try takeLine("", onComment: onComment) {
            frames.append(tail)
        }
        return frames
    }

    private mutating func decodeLineBytes() -> String {
        if lineBytes.isEmpty { return "" }
        let line = String(decoding: lineBytes, as: UTF8.self)
        lineBytes.removeAll(keepingCapacity: true)
        return line
    }

    private mutating func resetFrame() {
        dataLines.removeAll(keepingCapacity: true)
        hasData = false
        eventName = ""
        eventBytes = 0
    }

    private func tooLarge() -> SSEError {
        SSEError(message: "SSE event exceeds \(maxEventBytes) bytes.", code: "event_too_large")
    }

    private mutating func takeLine(_ raw: String, onComment: ((String) -> Void)?) throws -> SSEFrame? {
        if raw.isEmpty {
            guard hasData else {
                resetFrame()
                return nil
            }
            let value = dataLines.joined(separator: "\n")
            let frame = SSEFrame(
                id: lastId,
                event: eventName.isEmpty ? "message" : eventName,
                data: value
            )
            resetFrame()
            return frame
        }

        if raw.utf8.first == 0x3A { // ":"
            var comment = String(raw.dropFirst())
            if comment.utf8.first == 0x20 { // single leading space
                comment.removeFirst()
            }
            onComment?(comment)
            return nil
        }

        let field: String
        var value: String
        if let colonIndex = raw.firstIndex(of: ":") {
            field = String(raw[..<colonIndex])
            value = String(raw[raw.index(after: colonIndex)...])
            if value.utf8.first == 0x20 {
                value.removeFirst()
            }
        } else {
            field = raw
            value = ""
        }

        switch field {
        case "data":
            dataLines.append(value)
            hasData = true
            eventBytes += value.utf8.count + 1
        case "event":
            eventName = value
            eventBytes += value.utf8.count
        case "id":
            if !value.contains("\0") {
                lastId = value
                eventBytes += value.utf8.count
            }
        default:
            // "retry" and unknown fields are ignored, matching sse.mjs.
            break
        }

        if eventBytes > maxEventBytes {
            throw tooLarge()
        }
        return nil
    }
}

/// Parse an `AsyncSequence` of byte chunks (`Data` or `[UInt8]`) into an `AsyncThrowingStream` of `SSEFrame`.
public func parseSSE<S: AsyncSequence & Sendable>(
    chunks: S,
    maxEventBytes: Int = SSEParser.defaultMaxEventBytes,
    onComment: (@Sendable (String) -> Void)? = nil
) -> AsyncThrowingStream<SSEFrame, Error> where S.Element == Data {
    AsyncThrowingStream { continuation in
        let task = Task {
            do {
                try Task.checkCancellation()
                var parser = try SSEParser(maxEventBytes: maxEventBytes)
                for try await chunk in chunks {
                    try Task.checkCancellation()
                    let frames = try parser.feed(chunk, onComment: onComment)
                    for frame in frames {
                        try Task.checkCancellation()
                        continuation.yield(frame)
                    }
                }
                try Task.checkCancellation()
                let tail = try parser.finish(onComment: onComment)
                for frame in tail {
                    continuation.yield(frame)
                }
                continuation.finish()
            } catch {
                continuation.finish(throwing: error)
            }
        }
        continuation.onTermination = { _ in
            task.cancel()
        }
    }
}

/// Parse a byte-by-byte `AsyncSequence` (such as `URLSession.AsyncBytes`) into `SSEFrame`s,
/// batching bytes per line or 8 KiB chunk to avoid per-byte parser overhead while preserving
/// immediate streaming delivery.
public func parseSSE<S: AsyncSequence & Sendable>(
    bytes: S,
    maxEventBytes: Int = SSEParser.defaultMaxEventBytes,
    onComment: (@Sendable (String) -> Void)? = nil
) -> AsyncThrowingStream<SSEFrame, Error> where S.Element == UInt8 {
    AsyncThrowingStream { continuation in
        let task = Task {
            do {
                try Task.checkCancellation()
                var parser = try SSEParser(maxEventBytes: maxEventBytes)
                var buffer: [UInt8] = []
                buffer.reserveCapacity(4096)
                for try await byte in bytes {
                    try Task.checkCancellation()
                    buffer.append(byte)
                    if byte == 0x0A || byte == 0x0D || buffer.count >= 8192 {
                        let frames = try parser.feed(buffer, onComment: onComment)
                        buffer.removeAll(keepingCapacity: true)
                        for frame in frames {
                            try Task.checkCancellation()
                            continuation.yield(frame)
                        }
                    }
                }
                if !buffer.isEmpty {
                    let frames = try parser.feed(buffer, onComment: onComment)
                    for frame in frames {
                        try Task.checkCancellation()
                        continuation.yield(frame)
                    }
                }
                try Task.checkCancellation()
                let tail = try parser.finish(onComment: onComment)
                for frame in tail {
                    continuation.yield(frame)
                }
                continuation.finish()
            } catch {
                continuation.finish(throwing: error)
            }
        }
        continuation.onTermination = { _ in
            task.cancel()
        }
    }
}
