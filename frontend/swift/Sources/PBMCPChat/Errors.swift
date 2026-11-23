import Foundation

/// An error response from the server, decoded from `{"error":{"code","message"}}`.
///
/// A decision or outcome conflict (409) or refusal (422) may also carry the authoritative
/// stored `proposal`, so the caller can adopt it without a second request.
public struct ChatApiError: Error, LocalizedError, Sendable, Equatable {
    public let status: Int
    public let code: String
    public let message: String
    public let proposal: ChatProposal?

    public init(status: Int, code: String, message: String, proposal: ChatProposal? = nil) {
        self.status = status
        self.code = code.isEmpty ? "internal" : code
        self.message = message.isEmpty ? "The request failed." : message
        self.proposal = proposal
    }

    public var errorDescription: String? { message }
}

/// Alias matching the implementation plan's naming alongside JS `ChatApiError`.
public typealias ChatClientError = ChatApiError

/// The event cursor is no longer available (409 `resync_required`); reload canonical state.
public struct ResyncRequiredError: Error, LocalizedError, Sendable, Equatable {
    public let status: Int
    public let code: String = "resync_required"
    public let message: String

    public init(
        status: Int = 409,
        message: String = "The event cursor is no longer available; reload the conversation."
    ) {
        self.status = status
        self.message = message.isEmpty
            ? "The event cursor is no longer available; reload the conversation."
            : message
    }

    public var errorDescription: String? { message }
}

/// The request never reached a usable HTTP response.
public struct ChatNetworkError: Error, LocalizedError, Sendable {
    public let message: String
    public let underlyingError: (any Error & Sendable)?

    public init(
        message: String = "The server could not be reached.",
        underlyingError: (any Error & Sendable)? = nil
    ) {
        self.message = message.isEmpty ? "The server could not be reached." : message
        self.underlyingError = underlyingError
    }

    public var errorDescription: String? { message }
}

/// The server answered outside the protocol this client implements.
public struct ChatProtocolError: Error, LocalizedError, Sendable, Equatable {
    public let code: String
    public let message: String

    public init(message: String, code: String = "protocol_error") {
        self.code = code.isEmpty ? "protocol_error" : code
        self.message = message
    }

    public var errorDescription: String? { message }
}

/// An SSE framing or size error. `code` is stable; `message` is for humans.
public struct SSEError: Error, LocalizedError, Sendable, Equatable {
    public let code: String
    public let message: String

    public init(message: String, code: String = "sse_error") {
        self.code = code.isEmpty ? "sse_error" : code
        self.message = message
    }

    public var errorDescription: String? { message }
}

/// Reducer errors matching `reducer.mjs` (`GapError`, `ForeignTurnError`,
/// `AfterTerminalError`, `InvalidEventError`).
public enum ReducerError: Error, LocalizedError, Sendable, Equatable {
    case sequenceGap(turnId: String, have: Int, got: Int)
    case foreignTurn(eventTurnId: String, viewTurnId: String)
    case afterTerminal(eventType: String, state: String)
    case invalidEvent(String)

    public var code: String {
        switch self {
        case .sequenceGap: return "sequence_gap"
        case .foreignTurn: return "foreign_turn"
        case .afterTerminal: return "after_terminal"
        case .invalidEvent: return "invalid_event"
        }
    }

    public var message: String {
        switch self {
        case .sequenceGap(let turnId, let have, let got):
            return "chat: event sequence gap for turn \"\(turnId)\": have \(have), got \(got)"
        case .foreignTurn(let eventTurnId, let viewTurnId):
            return "chat: event for turn \"\(eventTurnId)\" applied to turn \"\(viewTurnId)\""
        case .afterTerminal(let eventType, let state):
            return "chat: event \(eventType) after terminal state \(state)"
        case .invalidEvent(let detail):
            return detail
        }
    }

    public var errorDescription: String? { message }
}

/// Invalid caller arguments (Swift equivalent of JS `TypeError` / `RangeError` for validation).
public struct ChatValidationError: Error, LocalizedError, Sendable, Equatable {
    public let message: String

    public init(_ message: String) {
        self.message = message
    }

    public var errorDescription: String? { message }
}

/// Internal error used when an SSE stream hits the controller's idle timeout.
struct IdleTimeoutError: Error, LocalizedError, Sendable {
    let milliseconds: Int
    var message: String { "No event or heartbeat for \(milliseconds)ms." }
    var errorDescription: String? { message }
}
