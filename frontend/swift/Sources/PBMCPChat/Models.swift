import Foundation

/// Protocol version this client implements.
public let PROTOCOL_VERSION = "1"

/// Turn lifecycle state; empty string (`none`) means no event seen yet in a client view.
public enum TurnState: String, Codable, Sendable, Hashable {
    case none = ""
    case queued = "queued"
    case running = "running"
    case completed = "completed"
    case failed = "failed"
    case cancelled = "cancelled"
    case interrupted = "interrupted"

    public var isTerminal: Bool {
        switch self {
        case .completed, .failed, .cancelled, .interrupted:
            return true
        case .none, .queued, .running:
            return false
        }
    }
}

/// Tool statuses in a `TurnView`.
public enum ToolStatus: String, Codable, Sendable, Hashable {
    case running = "running"
    case completed = "completed"
    case failed = "failed"
}

/// Proposal lifecycle states, as stored by the server.
public enum ChatProposalState: String, Codable, Sendable, Hashable, CaseIterable {
    case pending = "pending"
    case rejected = "rejected"
    case expired = "expired"
    case executing = "executing"
    case succeeded = "succeeded"
    case failed = "failed"
    case unknown = "unknown"
    case approved = "approved"
    case applied = "applied"
    case stale = "stale"

    public var isTerminal: Bool {
        switch self {
        case .rejected, .expired, .succeeded, .failed, .unknown, .applied, .stale:
            return true
        case .pending, .executing, .approved:
            return false
        }
    }
}

/// Where a proposal's effect happens.
public enum ChatProposalLocation: String, Codable, Sendable, Hashable {
    case server = "server"
    case editor = "editor"
}

/// Decision sent to `POST /proposals/{id}/decision`.
public enum ProposalDecision: String, Codable, Sendable, Hashable {
    case approve = "approve"
    case reject = "reject"
}

/// Status sent to `POST /proposals/{id}/outcome`.
public enum ProposalOutcomeStatus: String, Codable, Sendable, Hashable {
    case applied = "applied"
    case stale = "stale"
    case failed = "failed"
}

/// Per-hunk status in editor proposal outcomes.
public enum ChatHunkStatus: String, Codable, Sendable, Hashable, CaseIterable {
    case applied = "applied"
    case stale = "stale"
    case failed = "failed"
    case skipped = "skipped"
}

/// One hunk's reported local editor result.
public struct ChatHunkOutcome: Codable, Sendable, Hashable {
    public var id: String
    public var status: ChatHunkStatus

    public init(id: String, status: ChatHunkStatus) {
        self.id = id
        self.status = status
    }
}

/// The recorded failure of a turn or controller operation.
public struct ChatTurnFailure: Codable, Sendable, Hashable {
    public var code: String
    public var message: String

    public init(code: String, message: String) {
        self.code = code
        self.message = message
    }
}

/// One piece of message content.
public struct ChatPart: Codable, Sendable, Hashable {
    public var type: String
    public var text: String?
    public var callId: String?
    public var tool: String?
    public var arguments: JSONValue?
    public var result: JSONValue?
    public var isError: Bool?
    public var proposalId: String?
    public var state: String?
    public var uploadId: String?
    public var name: String?
    public var mime: String?

    public init(
        type: String,
        text: String? = nil,
        callId: String? = nil,
        tool: String? = nil,
        arguments: JSONValue? = nil,
        result: JSONValue? = nil,
        isError: Bool? = nil,
        proposalId: String? = nil,
        state: String? = nil,
        uploadId: String? = nil,
        name: String? = nil,
        mime: String? = nil
    ) {
        self.type = type
        self.text = text
        self.callId = callId
        self.tool = tool
        self.arguments = arguments
        self.result = result
        self.isError = isError
        self.proposalId = proposalId
        self.state = state
        self.uploadId = uploadId
        self.name = name
        self.mime = mime
    }
}

/// One canonical transcript message.
public struct ChatMessage: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var conversationId: String
    public var turnId: String?
    public var role: String
    public var parts: [ChatPart]
    public var createdAt: String

    public init(
        id: String,
        conversationId: String = "",
        turnId: String? = nil,
        role: String,
        parts: [ChatPart],
        createdAt: String = ""
    ) {
        self.id = id
        self.conversationId = conversationId
        self.turnId = turnId
        self.role = role
        self.parts = parts
        self.createdAt = createdAt
    }

    enum CodingKeys: String, CodingKey {
        case id, conversationId, turnId, role, parts, createdAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decodeIfPresent(String.self, forKey: .id) ?? ""
        conversationId = try c.decodeIfPresent(String.self, forKey: .conversationId) ?? ""
        turnId = try c.decodeIfPresent(String.self, forKey: .turnId)
        role = try c.decodeIfPresent(String.self, forKey: .role) ?? ""
        parts = try c.decodeIfPresent([ChatPart].self, forKey: .parts) ?? []
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt) ?? ""
    }
}

/// One caller-owned conversation.
public struct ChatConversation: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var title: String
    public var target: String?
    public var createdAt: String
    public var updatedAt: String

    public init(
        id: String,
        title: String = "",
        target: String? = nil,
        createdAt: String = "",
        updatedAt: String = ""
    ) {
        self.id = id
        self.title = title
        self.target = target
        self.createdAt = createdAt
        self.updatedAt = updatedAt
    }

    enum CodingKeys: String, CodingKey {
        case id, title, target, createdAt, updatedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        target = try c.decodeIfPresent(String.self, forKey: .target)
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt) ?? ""
        updatedAt = try c.decodeIfPresent(String.self, forKey: .updatedAt) ?? ""
    }
}

/// One unit of work in a conversation.
public struct ChatTurn: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var conversationId: String
    public var state: TurnState
    public var idempotencyKey: String?
    public var provider: String?
    public var model: String?
    public var effort: String?
    public var createdAt: String
    public var updatedAt: String
    public var error: ChatTurnFailure?

    public init(
        id: String,
        conversationId: String = "",
        state: TurnState = .none,
        idempotencyKey: String? = nil,
        provider: String? = nil,
        model: String? = nil,
        effort: String? = nil,
        createdAt: String = "",
        updatedAt: String = "",
        error: ChatTurnFailure? = nil
    ) {
        self.id = id
        self.conversationId = conversationId
        self.state = state
        self.idempotencyKey = idempotencyKey
        self.provider = provider
        self.model = model
        self.effort = effort
        self.createdAt = createdAt
        self.updatedAt = updatedAt
        self.error = error
    }

    enum CodingKeys: String, CodingKey {
        case id, conversationId, state, idempotencyKey, provider, model, effort, createdAt, updatedAt, error
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decodeIfPresent(String.self, forKey: .id) ?? ""
        conversationId = try c.decodeIfPresent(String.self, forKey: .conversationId) ?? ""
        let rawState = try c.decodeIfPresent(String.self, forKey: .state) ?? ""
        state = TurnState(rawValue: rawState) ?? .none
        idempotencyKey = try c.decodeIfPresent(String.self, forKey: .idempotencyKey)
        provider = try c.decodeIfPresent(String.self, forKey: .provider)
        model = try c.decodeIfPresent(String.self, forKey: .model)
        effort = try c.decodeIfPresent(String.self, forKey: .effort)
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt) ?? ""
        updatedAt = try c.decodeIfPresent(String.self, forKey: .updatedAt) ?? ""
        error = try c.decodeIfPresent(ChatTurnFailure.self, forKey: .error)
    }
}

/// The envelope carried by every event and every SSE frame's data.
public struct ChatEvent: Codable, Sendable, Hashable {
    public var v: String
    public var seq: Int
    public var conversationId: String
    public var turnId: String
    public var type: String
    public var data: JSONValue?
    public var at: String

    public init(
        v: String = PROTOCOL_VERSION,
        seq: Int,
        conversationId: String = "",
        turnId: String = "",
        type: String,
        data: JSONValue? = nil,
        at: String = ""
    ) {
        self.v = v
        self.seq = seq
        self.conversationId = conversationId
        self.turnId = turnId
        self.type = type
        self.data = data
        self.at = at
    }

    enum CodingKeys: String, CodingKey {
        case v, seq, conversationId, turnId, type, data, at
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        v = try c.decodeIfPresent(String.self, forKey: .v) ?? ""
        seq = try c.decode(Int.self, forKey: .seq)
        conversationId = try c.decodeIfPresent(String.self, forKey: .conversationId) ?? ""
        turnId = try c.decodeIfPresent(String.self, forKey: .turnId) ?? ""
        type = try c.decode(String.self, forKey: .type)
        data = try c.decodeIfPresent(JSONValue.self, forKey: .data)
        at = try c.decodeIfPresent(String.self, forKey: .at) ?? ""
    }
}

/// `/status` feature flags.
public struct ChatFeatures: Codable, Sendable, Hashable {
    public var ready: Bool?
    public var proposals: Bool
    public var uploads: Bool
    public var context: Bool?
    public var contextManagement: Bool?
    public var compaction: Bool?

    public init(
        ready: Bool? = nil,
        proposals: Bool = false,
        uploads: Bool = false,
        context: Bool? = nil,
        contextManagement: Bool? = nil,
        compaction: Bool? = nil
    ) {
        self.ready = ready
        self.proposals = proposals
        self.uploads = uploads
        self.context = context
        self.contextManagement = contextManagement
        self.compaction = compaction
    }

    enum CodingKeys: String, CodingKey {
        case ready, proposals, uploads, context, contextManagement, compaction
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        ready = try c.decodeIfPresent(Bool.self, forKey: .ready)
        proposals = try c.decodeIfPresent(Bool.self, forKey: .proposals) ?? false
        uploads = try c.decodeIfPresent(Bool.self, forKey: .uploads) ?? false
        context = try c.decodeIfPresent(Bool.self, forKey: .context)
        contextManagement = try c.decodeIfPresent(Bool.self, forKey: .contextManagement)
        compaction = try c.decodeIfPresent(Bool.self, forKey: .compaction)
    }
}

/// How full a conversation's context is.
public struct ChatContextStatus: Codable, Sendable, Hashable {
    public var tokens: Int
    public var window: Int
    public var estimated: Bool
    public var summarizedThrough: String
    public var summarizedAt: String
    public var compacting: Bool
    public var canCompact: Bool

    public init(
        tokens: Int = 0,
        window: Int = 0,
        estimated: Bool = false,
        summarizedThrough: String = "",
        summarizedAt: String = "",
        compacting: Bool = false,
        canCompact: Bool = false
    ) {
        self.tokens = max(0, tokens)
        self.window = max(0, window)
        self.estimated = estimated
        self.summarizedThrough = summarizedThrough
        self.summarizedAt = summarizedAt
        self.compacting = compacting
        self.canCompact = canCompact
    }

    enum CodingKeys: String, CodingKey {
        case tokens, window, estimated, summarizedThrough, summarizedAt, compacting, canCompact
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let rawTokens = try c.decodeIfPresent(Int.self, forKey: .tokens) ?? 0
        let rawWindow = try c.decodeIfPresent(Int.self, forKey: .window) ?? 0
        tokens = max(0, rawTokens)
        window = max(0, rawWindow)
        estimated = try c.decodeIfPresent(Bool.self, forKey: .estimated) ?? false
        summarizedThrough = try c.decodeIfPresent(String.self, forKey: .summarizedThrough) ?? ""
        summarizedAt = try c.decodeIfPresent(String.self, forKey: .summarizedAt) ?? ""
        compacting = try c.decodeIfPresent(Bool.self, forKey: .compacting) ?? false
        canCompact = try c.decodeIfPresent(Bool.self, forKey: .canCompact) ?? false
    }
}

public typealias ChatControllerContext = ChatContextStatus

/// Response of `POST /conversations/{id}/compact`.
public struct ChatCompactResult: Codable, Sendable, Hashable {
    public var compacted: Bool
    public var reason: String?
    public var throughMessageId: String?
    public var context: ChatContextStatus?

    public init(
        compacted: Bool,
        reason: String? = nil,
        throughMessageId: String? = nil,
        context: ChatContextStatus? = nil
    ) {
        self.compacted = compacted
        self.reason = reason
        self.throughMessageId = throughMessageId
        self.context = context
    }
}

/// `/status` limits.
public struct ChatLimits: Codable, Sendable, Hashable {
    public var maxMessageChars: Int
    public var maxUploadBytes: Int?
    public var maxAttachmentsPerTurn: Int?
    public var attachmentTypes: [String]?

    public init(
        maxMessageChars: Int = 0,
        maxUploadBytes: Int? = nil,
        maxAttachmentsPerTurn: Int? = nil,
        attachmentTypes: [String]? = nil
    ) {
        self.maxMessageChars = maxMessageChars
        self.maxUploadBytes = maxUploadBytes
        self.maxAttachmentsPerTurn = maxAttachmentsPerTurn
        self.attachmentTypes = attachmentTypes
    }

    enum CodingKeys: String, CodingKey {
        case maxMessageChars, maxUploadBytes, maxAttachmentsPerTurn, attachmentTypes
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        maxMessageChars = try c.decodeIfPresent(Int.self, forKey: .maxMessageChars) ?? 0
        maxUploadBytes = try c.decodeIfPresent(Int.self, forKey: .maxUploadBytes)
        maxAttachmentsPerTurn = try c.decodeIfPresent(Int.self, forKey: .maxAttachmentsPerTurn)
        attachmentTypes = try c.decodeIfPresent([String].self, forKey: .attachmentTypes)
    }
}

/// One reviewed model mutation. `revision` is the optimistic-concurrency token.
public struct ChatProposal: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var conversationId: String
    public var turnId: String?
    public var callId: String?
    public var tool: String
    public var toolFingerprint: String?
    public var manifestRevision: String?
    public var location: ChatProposalLocation
    public var target: String?
    public var arguments: JSONValue
    public var argumentsHash: String
    public var summary: String?
    public var state: ChatProposalState
    public var revision: Int
    public var createdAt: String
    public var updatedAt: String
    public var expiresAt: String?
    public var decidedAt: String?
    public var result: JSONValue?
    public var failure: String?
    public var warnings: [String]?
    public var editorRevision: String?
    public var hunks: [ChatHunkOutcome]?

    public init(
        id: String,
        conversationId: String,
        turnId: String? = nil,
        callId: String? = nil,
        tool: String,
        toolFingerprint: String? = nil,
        manifestRevision: String? = nil,
        location: ChatProposalLocation = .server,
        target: String? = nil,
        arguments: JSONValue = .object([:]),
        argumentsHash: String = "",
        summary: String? = nil,
        state: ChatProposalState = .pending,
        revision: Int = 1,
        createdAt: String = "",
        updatedAt: String = "",
        expiresAt: String? = nil,
        decidedAt: String? = nil,
        result: JSONValue? = nil,
        failure: String? = nil,
        warnings: [String]? = nil,
        editorRevision: String? = nil,
        hunks: [ChatHunkOutcome]? = nil
    ) {
        self.id = id
        self.conversationId = conversationId
        self.turnId = turnId
        self.callId = callId
        self.tool = tool
        self.toolFingerprint = toolFingerprint
        self.manifestRevision = manifestRevision
        self.location = location
        self.target = target
        self.arguments = arguments
        self.argumentsHash = argumentsHash
        self.summary = summary
        self.state = state
        self.revision = revision
        self.createdAt = createdAt
        self.updatedAt = updatedAt
        self.expiresAt = expiresAt
        self.decidedAt = decidedAt
        self.result = result
        self.failure = failure
        self.warnings = warnings
        self.editorRevision = editorRevision
        self.hunks = hunks
    }

    enum CodingKeys: String, CodingKey {
        case id, conversationId, turnId, callId, tool, toolFingerprint, manifestRevision
        case location, target, arguments, argumentsHash, summary, state, revision
        case createdAt, updatedAt, expiresAt, decidedAt, result, failure, warnings
        case editorRevision, hunks
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        conversationId = try c.decodeIfPresent(String.self, forKey: .conversationId) ?? ""
        turnId = try c.decodeIfPresent(String.self, forKey: .turnId)
        callId = try c.decodeIfPresent(String.self, forKey: .callId)
        tool = try c.decodeIfPresent(String.self, forKey: .tool) ?? ""
        toolFingerprint = try c.decodeIfPresent(String.self, forKey: .toolFingerprint)
        manifestRevision = try c.decodeIfPresent(String.self, forKey: .manifestRevision)
        location = try c.decodeIfPresent(ChatProposalLocation.self, forKey: .location) ?? .server
        target = try c.decodeIfPresent(String.self, forKey: .target)
        arguments = try c.decodeIfPresent(JSONValue.self, forKey: .arguments) ?? .object([:])
        argumentsHash = try c.decodeIfPresent(String.self, forKey: .argumentsHash) ?? ""
        summary = try c.decodeIfPresent(String.self, forKey: .summary)
        state = try c.decodeIfPresent(ChatProposalState.self, forKey: .state) ?? .pending
        revision = try c.decodeIfPresent(Int.self, forKey: .revision) ?? 1
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt) ?? ""
        updatedAt = try c.decodeIfPresent(String.self, forKey: .updatedAt) ?? ""
        expiresAt = try c.decodeIfPresent(String.self, forKey: .expiresAt)
        decidedAt = try c.decodeIfPresent(String.self, forKey: .decidedAt)
        result = try c.decodeIfPresent(JSONValue.self, forKey: .result)
        failure = try c.decodeIfPresent(String.self, forKey: .failure)
        warnings = try c.decodeIfPresent([String].self, forKey: .warnings)
        editorRevision = try c.decodeIfPresent(String.self, forKey: .editorRevision)
        hunks = try c.decodeIfPresent([ChatHunkOutcome].self, forKey: .hunks)
    }
}

/// One proposal in controller state, plus a non-fatal client-side error.
public struct ChatProposalEntry: Sendable, Hashable, Identifiable {
    public var proposal: ChatProposal
    public var error: ChatTurnFailure?

    public var id: String { proposal.id }
    public var conversationId: String { proposal.conversationId }
    public var turnId: String? { proposal.turnId }
    public var callId: String? { proposal.callId }
    public var tool: String { proposal.tool }
    public var location: ChatProposalLocation { proposal.location }
    public var target: String? { proposal.target }
    public var arguments: JSONValue { proposal.arguments }
    public var argumentsHash: String { proposal.argumentsHash }
    public var summary: String? { proposal.summary }
    public var state: ChatProposalState { proposal.state }
    public var revision: Int { proposal.revision }
    public var createdAt: String { proposal.createdAt }
    public var updatedAt: String { proposal.updatedAt }
    public var result: JSONValue? { proposal.result }
    public var failure: String? { proposal.failure }
    public var warnings: [String]? { proposal.warnings }
    public var editorRevision: String? { proposal.editorRevision }
    public var hunks: [ChatHunkOutcome]? { proposal.hunks }

    public init(proposal: ChatProposal, error: ChatTurnFailure? = nil) {
        self.proposal = proposal
        self.error = error
    }
}

/// One stored chat attachment.
public struct ChatUpload: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var conversationId: String
    public var name: String
    public var mime: String
    public var kind: String
    public var size: Int
    public var sha256: String
    public var createdAt: String

    public init(
        id: String,
        conversationId: String,
        name: String,
        mime: String,
        kind: String,
        size: Int,
        sha256: String,
        createdAt: String
    ) {
        self.id = id
        self.conversationId = conversationId
        self.name = name
        self.mime = mime
        self.kind = kind
        self.size = size
        self.sha256 = sha256
        self.createdAt = createdAt
    }
}

/// One selectable provider/model pair. `id` is opaque to clients.
public struct ChatModelChoice: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var provider: String
    public var model: String
    public var label: String?
    public var group: String?
    public var isDefault: Bool?
    public var attachmentTypes: [String]?
    public var efforts: [String]?
    public var disabled: Bool?

    public init(
        id: String,
        provider: String,
        model: String,
        label: String? = nil,
        group: String? = nil,
        isDefault: Bool? = nil,
        attachmentTypes: [String]? = nil,
        efforts: [String]? = nil,
        disabled: Bool? = nil
    ) {
        self.id = id
        self.provider = provider
        self.model = model
        self.label = label
        self.group = group
        self.isDefault = isDefault
        self.attachmentTypes = attachmentTypes
        self.efforts = efforts
        self.disabled = disabled
    }

    enum CodingKeys: String, CodingKey {
        case id, provider, model, label, group
        case isDefault = "default"
        case attachmentTypes, efforts, disabled
    }
}

/// Response of `GET /status`.
public struct ChatStatus: Codable, Sendable, Hashable {
    public var version: String
    public var models: [String]
    public var choices: [ChatModelChoice]?
    public var defaultChoice: String?
    public var features: ChatFeatures
    public var limits: ChatLimits

    public init(
        version: String = PROTOCOL_VERSION,
        models: [String] = [],
        choices: [ChatModelChoice]? = nil,
        defaultChoice: String? = nil,
        features: ChatFeatures = ChatFeatures(),
        limits: ChatLimits = ChatLimits()
    ) {
        self.version = version
        self.models = models
        self.choices = choices
        self.defaultChoice = defaultChoice
        self.features = features
        self.limits = limits
    }

    enum CodingKeys: String, CodingKey {
        case version, models, choices, defaultChoice, features, limits
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(String.self, forKey: .version) ?? PROTOCOL_VERSION
        models = try c.decodeIfPresent([String].self, forKey: .models) ?? []
        choices = try c.decodeIfPresent([ChatModelChoice].self, forKey: .choices)
        defaultChoice = try c.decodeIfPresent(String.self, forKey: .defaultChoice)
        features = try c.decodeIfPresent(ChatFeatures.self, forKey: .features) ?? ChatFeatures()
        limits = try c.decodeIfPresent(ChatLimits.self, forKey: .limits) ?? ChatLimits()
    }
}

/// Response of `GET /conversations`.
public struct ChatConversationList: Codable, Sendable, Hashable {
    public var items: [ChatConversation]
    public var nextCursor: String

    public init(items: [ChatConversation] = [], nextCursor: String = "") {
        self.items = items
        self.nextCursor = nextCursor
    }

    enum CodingKeys: String, CodingKey {
        case items, nextCursor
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        items = try c.decodeIfPresent([ChatConversation].self, forKey: .items) ?? []
        nextCursor = try c.decodeIfPresent(String.self, forKey: .nextCursor) ?? ""
    }
}

/// Response of `GET /conversations/{id}`.
public struct ChatConversationDetail: Codable, Sendable, Hashable {
    public var conversation: ChatConversation?
    public var messages: [ChatMessage]
    public var nextCursor: String
    public var activeTurn: ChatTurn?
    public var proposals: [ChatProposal]?
    public var decidedProposals: [ChatProposal]?
    public var context: ChatContextStatus?
    /// True when the response JSON explicitly included the `"context"` key (even if null).
    public var hasContextField: Bool

    public init(
        conversation: ChatConversation? = nil,
        messages: [ChatMessage] = [],
        nextCursor: String = "",
        activeTurn: ChatTurn? = nil,
        proposals: [ChatProposal]? = nil,
        decidedProposals: [ChatProposal]? = nil,
        context: ChatContextStatus? = nil,
        hasContextField: Bool? = nil
    ) {
        self.conversation = conversation
        self.messages = messages
        self.nextCursor = nextCursor
        self.activeTurn = activeTurn
        self.proposals = proposals
        self.decidedProposals = decidedProposals
        self.context = context
        self.hasContextField = hasContextField ?? (context != nil)
    }

    enum CodingKeys: String, CodingKey {
        case conversation, messages, nextCursor, activeTurn, proposals, decidedProposals, context
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        conversation = try c.decodeIfPresent(ChatConversation.self, forKey: .conversation)
        messages = try c.decodeIfPresent([ChatMessage].self, forKey: .messages) ?? []
        nextCursor = try c.decodeIfPresent(String.self, forKey: .nextCursor) ?? ""
        activeTurn = try c.decodeIfPresent(ChatTurn.self, forKey: .activeTurn)
        proposals = try c.decodeIfPresent([ChatProposal].self, forKey: .proposals)
        decidedProposals = try c.decodeIfPresent([ChatProposal].self, forKey: .decidedProposals)
        hasContextField = c.contains(.context)
        context = try c.decodeIfPresent(ChatContextStatus.self, forKey: .context)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encodeIfPresent(conversation, forKey: .conversation)
        try c.encode(messages, forKey: .messages)
        try c.encode(nextCursor, forKey: .nextCursor)
        try c.encodeIfPresent(activeTurn, forKey: .activeTurn)
        try c.encodeIfPresent(proposals, forKey: .proposals)
        try c.encodeIfPresent(decidedProposals, forKey: .decidedProposals)
        if hasContextField {
            try c.encode(context, forKey: .context)
        }
    }
}

/// Response of `POST /conversations/{id}/turns`.
public struct ChatSubmitResult: Codable, Sendable, Hashable {
    public var turn: ChatTurn
    public var created: Bool

    public init(turn: ChatTurn, created: Bool) {
        self.turn = turn
        self.created = created
    }
}

/// Response of `POST /proposals/{id}/decision` and `/outcome`.
public struct ProposalDecisionResult: Codable, Sendable, Hashable {
    public var proposal: ChatProposal
    public var replayed: Bool

    public init(proposal: ChatProposal, replayed: Bool = false) {
        self.proposal = proposal
        self.replayed = replayed
    }
}

/// Result of `ChatController.decide` / `reportOutcome`.
public struct ChatDecisionResult: Sendable, Hashable {
    public var proposal: ChatProposalEntry?
    public var replayed: Bool
    public var error: ChatTurnFailure?

    public init(proposal: ChatProposalEntry?, replayed: Bool, error: ChatTurnFailure? = nil) {
        self.proposal = proposal
        self.replayed = replayed
        self.error = error
    }
}

/// Result of `ChatClient.streamTurn`.
public struct StreamTurnResult: Sendable, Hashable {
    public var lastSeq: Int
    public var terminal: Bool

    public init(lastSeq: Int, terminal: Bool) {
        self.lastSeq = lastSeq
        self.terminal = terminal
    }
}

/// One message in `importConversation`.
public struct ChatImportMessage: Codable, Sendable, Hashable {
    public var role: String
    public var text: String
    public var createdAt: String?

    public init(role: String, text: String, createdAt: String? = nil) {
        self.role = role
        self.text = text
        self.createdAt = createdAt
    }
}

/// Result of `importConversation`.
public struct ChatImportResult: Codable, Sendable, Hashable {
    public var conversation: ChatConversation
    public var created: Bool

    public init(conversation: ChatConversation, created: Bool) {
        self.conversation = conversation
        self.created = created
    }
}

/// Token usage for a turn.
public struct TurnUsage: Codable, Sendable, Hashable {
    public var inputTokens: Int
    public var outputTokens: Int

    public init(inputTokens: Int = 0, outputTokens: Int = 0) {
        self.inputTokens = inputTokens
        self.outputTokens = outputTokens
    }
}

/// One tool invocation inside a `TurnView`.
public struct TurnToolView: Codable, Sendable, Hashable {
    public var callId: String
    public var tool: String
    public var arguments: JSONValue?
    public var status: ToolStatus
    public var result: JSONValue?
    public var message: String

    public init(
        callId: String,
        tool: String,
        arguments: JSONValue? = nil,
        status: ToolStatus = .running,
        result: JSONValue? = nil,
        message: String = ""
    ) {
        self.callId = callId
        self.tool = tool
        self.arguments = arguments
        self.status = status
        self.result = result
        self.message = message
    }
}

/// Immutable reduced view of a single turn, matching `reducer.mjs` / `chat/reducer.go`.
public struct TurnView: Codable, Sendable, Hashable {
    public var turnId: String
    public var state: TurnState
    public var text: String
    public var tools: [TurnToolView]
    public var usage: TurnUsage?
    public var error: ChatTurnFailure?
    public var proposals: [ChatProposal]
    public var lastSeq: Int

    public init(
        turnId: String = "",
        state: TurnState = .none,
        text: String = "",
        tools: [TurnToolView] = [],
        usage: TurnUsage? = nil,
        error: ChatTurnFailure? = nil,
        proposals: [ChatProposal] = [],
        lastSeq: Int = 0
    ) {
        self.turnId = turnId
        self.state = state
        self.text = text
        self.tools = tools
        self.usage = usage
        self.error = error
        self.proposals = proposals
        self.lastSeq = lastSeq
    }
}

/// One transcript entry: a single part of one canonical message.
public struct ChatTranscriptEntry: Sendable, Hashable, Identifiable {
    public var id: String
    public var messageId: String
    public var role: String
    public var turnId: String
    public var createdAt: String
    public var part: ChatPart

    public var type: String { part.type }
    public var text: String? { part.text }
    public var callId: String? { part.callId }
    public var tool: String? { part.tool }
    public var arguments: JSONValue? { part.arguments }
    public var result: JSONValue? { part.result }
    public var isError: Bool? { part.isError }
    public var proposalId: String? { part.proposalId }
    public var state: String? { part.state }
    public var uploadId: String? { part.uploadId }
    public var name: String? { part.name }
    public var mime: String? { part.mime }

    public init(
        id: String,
        messageId: String,
        role: String,
        turnId: String,
        createdAt: String,
        part: ChatPart
    ) {
        self.id = id
        self.messageId = messageId
        self.role = role
        self.turnId = turnId
        self.createdAt = createdAt
        self.part = part
    }
}

/// A local file payload to upload as a chat attachment.
public struct ChatUploadFile: Sendable, Hashable {
    public var data: Data
    public var name: String
    public var mimeType: String

    public var size: Int { data.count }

    public init(data: Data, name: String, mimeType: String = "") {
        self.data = data
        self.name = name
        self.mimeType = mimeType
    }
}

/// Either an already-uploaded attachment ID or a local file to upload first.
public enum ChatAttachmentInput: Sendable, Hashable {
    case uploadId(String)
    case file(ChatUploadFile)
}

/// Account namespace key matching JS `ChatAccountKey`.
public struct ChatAccountKey: Sendable, Hashable, Codable {
    public var appUrl: String
    public var mount: String
    public var authCollectionId: String
    public var userId: String

    public init(appUrl: String, mount: String, authCollectionId: String, userId: String) {
        self.appUrl = appUrl
        self.mount = mount
        self.authCollectionId = authCollectionId
        self.userId = userId
    }
}

/// Reconnect configuration for `ChatController`.
public struct ChatReconnectOptions: Sendable, Hashable {
    public var initialDelayMs: Int
    public var maxDelayMs: Int
    /// `nil` means unlimited (matching JS `Infinity`).
    public var maxAttempts: Int?

    public init(initialDelayMs: Int = 500, maxDelayMs: Int = 10_000, maxAttempts: Int? = nil) {
        self.initialDelayMs = initialDelayMs
        self.maxDelayMs = maxDelayMs
        self.maxAttempts = maxAttempts
    }
}

/// Conversation list sync options for `ChatController`.
public struct ChatSyncOptions: Sendable, Hashable {
    public var channel: Bool
    public var focus: Bool
    public var intervalMs: Int

    public init(channel: Bool = true, focus: Bool = true, intervalMs: Int = 60_000) {
        self.channel = channel
        self.focus = focus
        self.intervalMs = intervalMs
    }

    public static let disabled = ChatSyncOptions(channel: false, focus: false, intervalMs: 0)
}

/// Result of `mergeConversationPage`.
public struct ChatConversationMerge: Sendable {
    public var conversations: [ChatConversation]
    /// `nil` means keep the current cursor (rows from `loadMore` were kept).
    public var nextCursor: String?
    public var complete: Bool
    private let coveredCheck: @Sendable (String) -> Bool

    public init(
        conversations: [ChatConversation],
        nextCursor: String?,
        complete: Bool,
        covers: @escaping @Sendable (String) -> Bool
    ) {
        self.conversations = conversations
        self.nextCursor = nextCursor
        self.complete = complete
        self.coveredCheck = covers
    }

    public func covers(_ id: String) -> Bool {
        coveredCheck(id)
    }
}

/// Snapshot returned by a host `ChatEditorAdapter`.
public struct ChatEditorSnapshot: Sendable, Hashable {
    public var editorId: String?
    public var target: String?
    public var revision: String?
    public var content: String?

    public init(editorId: String? = nil, target: String? = nil, revision: String? = nil, content: String? = nil) {
        self.editorId = editorId
        self.target = target
        self.revision = revision
        self.content = content
    }
}

/// Result returned by `ChatEditorAdapter.applyPatch`.
public struct ChatEditorApplyResult: Sendable, Hashable {
    public var revision: String
    public var hunks: [ChatHunkOutcome]?
    public var undoToken: String?

    public init(revision: String, hunks: [ChatHunkOutcome]? = nil, undoToken: String? = nil) {
        self.revision = revision
        self.hunks = hunks
        self.undoToken = undoToken
    }
}

/// Host editor adapter for editor-located proposals.
public protocol ChatEditorAdapter: Sendable {
    func snapshot() async throws -> ChatEditorSnapshot
    func validatePatch(_ proposal: ChatProposal) async throws -> Bool
    func applyPatch(_ proposal: ChatProposal) async throws -> ChatEditorApplyResult
    func undo(_ token: String) async throws
}

public extension ChatEditorAdapter {
    func validatePatch(_ proposal: ChatProposal) async throws -> Bool { true }
    func undo(_ token: String) async throws {}
}

/// Controller lifecycle status.
public enum ChatControllerStatus: String, Sendable, Hashable, Codable {
    case idle
    case loading
    case ready
    case error
}

/// Immutable snapshot published by `ChatController`.
public struct ChatControllerState: Sendable, Hashable {
    public var status: ChatControllerStatus
    public var conversations: [ChatConversation]
    public var nextCursor: String
    public var activeConversationId: String?
    public var messages: [ChatMessage]
    public var transcript: [ChatTranscriptEntry]
    public var proposals: [ChatProposalEntry]
    public var decidedProposals: [ChatProposal]
    public var pendingUploads: [ChatUpload]
    public var turn: TurnView?
    public var running: Bool
    public var sending: Bool
    public var error: ChatTurnFailure?
    public var features: ChatFeatures?
    public var limits: ChatLimits?
    public var models: [String]
    public var choices: [ChatModelChoice]
    public var defaultChoice: String
    public var choice: String
    public var context: ChatControllerContext?

    public init(
        status: ChatControllerStatus = .idle,
        conversations: [ChatConversation] = [],
        nextCursor: String = "",
        activeConversationId: String? = nil,
        messages: [ChatMessage] = [],
        transcript: [ChatTranscriptEntry]? = nil,
        proposals: [ChatProposalEntry] = [],
        decidedProposals: [ChatProposal] = [],
        pendingUploads: [ChatUpload] = [],
        turn: TurnView? = nil,
        running: Bool = false,
        sending: Bool = false,
        error: ChatTurnFailure? = nil,
        features: ChatFeatures? = nil,
        limits: ChatLimits? = nil,
        models: [String] = [],
        choices: [ChatModelChoice] = [],
        defaultChoice: String = "",
        choice: String = "",
        context: ChatControllerContext? = nil
    ) {
        self.status = status
        self.conversations = conversations
        self.nextCursor = nextCursor
        self.activeConversationId = activeConversationId
        self.messages = messages
        self.transcript = transcript ?? transcriptFromMessages(messages)
        self.proposals = proposals
        self.decidedProposals = decidedProposals
        self.pendingUploads = pendingUploads
        self.turn = turn
        self.running = running
        self.sending = sending
        self.error = error
        self.features = features
        self.limits = limits
        self.models = models
        self.choices = choices
        self.defaultChoice = defaultChoice
        self.choice = choice
        self.context = context
    }
}
