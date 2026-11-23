import Foundation

// Pure presentation and mapping helpers ported from `agent-panel.mjs`.
// Foundation-only so both headless tests and `PBMCPChatUI` share one source of truth.

public let ATTACHMENTS_UNSUPPORTED_TEXT =
    "Attachments are not enabled on this server. Remove the files and send the message as text."

public let CONTEXT_MARKER_TYPE = "context-marker"
public let CONTEXT_MARKER_TEXT = "Earlier messages are summarized for the model"
public let CONTEXT_USAGE_THRESHOLD = 0.6

public let PROPOSAL_STATE_TEXT: [ChatProposalState: String] = [
    .pending: "Waiting for your approval.",
    .rejected: "Rejected.",
    .expired: "Expired before it was decided.",
    .executing: "Applying this change on the server…",
    .succeeded: "The server applied this change.",
    .failed: "This change could not be applied.",
    .unknown: "The server may or may not have applied this change — check before retrying.",
    .approved: "Approved — applying in the editor…",
    .applied: "Applied in the editor — not saved yet.",
    .stale: "Not applied: the editor changed since this change was proposed.",
]

public let PROPOSAL_STATE_LABEL: [ChatProposalState: String] = [
    .pending: "Pending",
    .rejected: "Rejected",
    .expired: "Expired",
    .executing: "Applying",
    .succeeded: "Applied",
    .failed: "Failed",
    .unknown: "Unknown",
    .approved: "Approved",
    .applied: "Applied in editor",
    .stale: "Out of date",
]

public enum ChatBadgeTone: String, Sendable, Hashable {
    case info
    case muted
    case success
    case error
    case warning
}

public let PROPOSAL_STATE_TONE: [ChatProposalState: ChatBadgeTone] = [
    .pending: .info,
    .rejected: .muted,
    .expired: .muted,
    .executing: .info,
    .succeeded: .success,
    .failed: .error,
    .unknown: .warning,
    .approved: .info,
    .applied: .success,
    .stale: .warning,
]

private let errorTextMap: [String: String] = [
    "active_turn": "A reply is already in progress. Stop it or wait for it to finish.",
    "auth_expired": "Your session has expired. Sign in again.",
    "unauthorized": "Your session has expired. Sign in again.",
    "forbidden": "You do not have access to this conversation.",
    "resync_required": "This conversation changed somewhere else. Reload it to continue.",
    "not_found": "That conversation is no longer available.",
    "rate_limited": "Too many requests. Wait a moment and try again.",
    "payload_too_large": "That message or attachment is too large to send.",
    "invalid_attachment": "That file was not accepted. Check its type and size.",
    "unsupported_media": "That file type is not supported.",
    "editor_unavailable": "Approving this change needs an editor adapter in the page.",
    "invalid_request": "The message was not accepted. Check it and try again.",
    "method_not_allowed": "The message was not accepted. Check it and try again.",
    "conflict": "This conversation is busy. Try again in a moment.",
    "provider": "The assistant could not answer this time. Try again.",
    "internal": "Something went wrong on the server. Try again.",
    "no_provider": "AI isn't set up yet. Ask an administrator to add a model provider.",
    "turn_refused": "The message was not accepted here.",
    "provider_error": "The AI provider refused the request. Try again, or ask an administrator to check the provider settings.",
    "max_steps": "This question needed too many steps. Try asking something narrower.",
    "context_too_long": "This conversation is too long for the selected model. Compact it, start a new chat, or pick a model with a larger context.",
    "too_soon": "The conversation was summarized moments ago.",
    "output_limit": "The reply was cut off at the model's output limit. Ask for a shorter answer, or raise the limit.",
    "content_filtered": "The AI provider withheld this reply. Try rephrasing the request.",
    "empty_reply": "The assistant returned an empty reply. Try again.",
    "provider_auth": "The AI provider rejected its credentials. Ask an administrator to check the key or sign in again in the provider settings.",
    "usage_limit": "The AI provider's usage limit was reached. Wait a while or switch to another model.",
    "internal_error": "Something went wrong on the server. Try again.",
    "timeout": "The server took too long to answer. Try again.",
    "interrupted": "The reply was interrupted. Try again.",
    "cancelled": "",
]

private let networkErrorText = "Can't reach the server. Check your connection and try again."
private let genericErrorText = "Something went wrong. Try again."

public let LOCAL_ERROR_TEXT: [String: String] = [
    "attachments_unsupported": ATTACHMENTS_UNSUPPORTED_TEXT,
    "attachment_text_required": "A message with attachments still needs some text.",
    "upload_failed": "The files could not be uploaded.",
    "download_unavailable": "This attachment cannot be downloaded here.",
]

private let effortLabels: [String: String] = [
    "none": "None",
    "minimal": "Minimal",
    "low": "Low",
    "medium": "Medium",
    "high": "High",
    "xhigh": "Extra high",
    "max": "Max",
    "ultra": "Ultra",
]

private let proposalErrorCodes: Set<String> = [
    "stale", "expired", "conflict", "refused", "not_found", "editor_unavailable",
]

private func matchHintCode(_ message: String) -> (String, String)? {
    let lower = message.lowercased()
    if lower.contains("could not be reached")
        || lower.contains("could not be reconnected")
        || lower.contains("network")
        || lower.contains("fetch") {
        return ("network", networkErrorText)
    }
    if lower.contains("no event or heartbeat")
        || lower.contains("timed out")
        || lower.contains("timeout") {
        return ("timeout", errorTextMap["timeout"]!)
    }
    if lower.contains("resync") || lower.contains("reload the conversation") {
        return ("resync_required", errorTextMap["resync_required"]!)
    }
    return nil
}

/// The normalized error code `errorText` branches on.
public func errorCode(code: String?, message: String? = nil) -> String {
    let rawCode = (code ?? "").lowercased()
    if errorTextMap[rawCode] != nil {
        return rawCode
    }
    if let message, let hint = matchHintCode(message) {
        return hint.0
    }
    return "unknown"
}

public func errorCode(_ failure: ChatTurnFailure?) -> String {
    guard let failure else { return "" }
    return errorCode(code: failure.code, message: failure.message)
}

public func errorCode(_ error: Error?) -> String {
    guard let error else { return "" }
    if let api = error as? ChatApiError {
        return errorCode(code: api.code, message: api.message)
    }
    if let resync = error as? ResyncRequiredError {
        return errorCode(code: resync.code, message: resync.message)
    }
    if let net = error as? ChatNetworkError {
        return errorCode(code: nil, message: net.message)
    }
    if let proto = error as? ChatProtocolError {
        return errorCode(code: proto.code, message: proto.message)
    }
    return errorCode(code: nil, message: error.localizedDescription)
}

/// A user-facing plain-language line for an error; never leaks raw server internals.
public func errorText(code: String?, message: String? = nil) -> String {
    let rawCode = (code ?? "").lowercased()
    if let mapped = errorTextMap[rawCode] {
        return mapped
    }
    if let message, let hint = matchHintCode(message) {
        return hint.1
    }
    return genericErrorText
}

public func errorText(_ failure: ChatTurnFailure?) -> String {
    guard let failure else { return "" }
    return errorText(code: failure.code, message: failure.message)
}

public func errorText(_ error: Error?) -> String {
    guard let error else { return "" }
    if let api = error as? ChatApiError {
        return errorText(code: api.code, message: api.message)
    }
    if let resync = error as? ResyncRequiredError {
        return errorText(code: resync.code, message: resync.message)
    }
    if let net = error as? ChatNetworkError {
        return errorText(code: nil, message: net.message)
    }
    return errorText(code: nil, message: error.localizedDescription)
}

/// A plain-language line for a proposal's non-fatal conflict or refusal.
public func proposalErrorText(_ failure: ChatTurnFailure?) -> String {
    guard let failure else { return "" }
    switch failure.code.lowercased() {
    case "stale":
        return "This change is out of date. Ask again for a fresh one."
    case "expired":
        return "This change expired before it was decided."
    case "conflict":
        return "This change was decided somewhere else. Review the latest version."
    case "refused":
        let trimmed = failure.message.trimmingCharacters(in: .whitespacesAndNewlines)
        if !trimmed.isEmpty {
            return "\(trimmed) Nothing was saved; the change is waiting for your decision again."
        }
        return "The change was refused before anything was saved. It is waiting for your decision again."
    case "not_found":
        return "This change is no longer available."
    case "editor_unavailable":
        return errorTextMap["editor_unavailable"]!
    default:
        return errorText(failure)
    }
}

public func proposalErrorCode(_ failure: ChatTurnFailure?) -> String {
    guard let failure else { return "" }
    let own = failure.code.lowercased()
    return proposalErrorCodes.contains(own) ? own : errorCode(failure)
}

public func humanizeToolName(_ name: String) -> String {
    let text = name.replacingOccurrences(of: "_", with: " ").trimmingCharacters(in: .whitespacesAndNewlines)
    guard let first = text.first else { return "Change" }
    return String(first).uppercased() + text.dropFirst()
}

// MARK: - Mapped UI entry types (Swift equivalent of agent-panel Entry shapes)

public struct ChatAgentReference: Sendable, Hashable, Identifiable {
    public var type: String
    public var id: String
    public var label: String
    public var isAttachment: Bool
    public var mime: String?

    public init(type: String, id: String, label: String, isAttachment: Bool = false, mime: String? = nil) {
        self.type = type
        self.id = id
        self.label = label
        self.isAttachment = isAttachment
        self.mime = mime
    }
}

public struct ChatAgentToolDetail: Sendable, Hashable {
    public var arguments: JSONValue?
    public var result: JSONValue?
    public var error: String?

    public init(arguments: JSONValue? = nil, result: JSONValue? = nil, error: String? = nil) {
        self.arguments = arguments
        self.result = result
        self.error = error
    }

    public var isEmpty: Bool {
        arguments == nil && result == nil && (error == nil || error?.isEmpty == true)
    }
}

public struct ChatAgentToolCall: Sendable, Hashable, Identifiable {
    public var id: String
    public var name: String
    public var status: ToolStatus
    public var detail: ChatAgentToolDetail?

    public init(id: String, name: String, status: ToolStatus, detail: ChatAgentToolDetail? = nil) {
        self.id = id
        self.name = name
        self.status = status
        self.detail = detail
    }
}

public struct ChatAgentProposalEntry: Sendable, Hashable, Identifiable {
    public var id: String
    public var proposalId: String
    public var tool: String
    public var summary: String
    public var state: ChatProposalState
    public var location: ChatProposalLocation
    public var revision: Int
    public var error: String
    public var status: String
    public var detail: String
    public var warnings: [String]
    public var canDecide: Bool
    public var proposal: ChatProposal

    public var stateLabel: String {
        PROPOSAL_STATE_LABEL[state] ?? state.rawValue
    }

    public var stateTone: ChatBadgeTone {
        PROPOSAL_STATE_TONE[state] ?? .muted
    }

    public var locationLabel: String {
        location == .editor ? "editor change" : "server change"
    }

    public var metaText: String {
        "\(humanizeToolName(tool)) · \(locationLabel) · revision \(revision)"
    }
}

public struct ChatAgentNoticeEntry: Sendable, Hashable, Identifiable {
    public var id: String
    public var content: String
    public var proposalId: String
    public var state: String
    public var tool: String
    public var isError: Bool
    public var result: JSONValue?
    public var proposal: ChatProposal?
}

public struct ChatAgentTextEntry: Sendable, Hashable, Identifiable {
    public var id: String
    public var role: String
    public var content: String
    public var references: [ChatAgentReference]
    public var isStreaming: Bool

    public init(
        id: String,
        role: String,
        content: String,
        references: [ChatAgentReference] = [],
        isStreaming: Bool = false
    ) {
        self.id = id
        self.role = role
        self.content = content
        self.references = references
        self.isStreaming = isStreaming
    }
}

public struct ChatAgentToolsEntry: Sendable, Hashable, Identifiable {
    public var id: String
    public var tools: [ChatAgentToolCall]

    public init(id: String, tools: [ChatAgentToolCall]) {
        self.id = id
        self.tools = tools
    }
}

public struct ChatContextMarkerEntry: Sendable, Hashable, Identifiable {
    public var id: String
    public var content: String

    public init(id: String, content: String = CONTEXT_MARKER_TEXT) {
        self.id = id
        self.content = content
    }
}

public enum ChatAgentEntry: Sendable, Hashable, Identifiable {
    case text(ChatAgentTextEntry)
    case tools(ChatAgentToolsEntry)
    case notice(ChatAgentNoticeEntry)
    case proposal(ChatAgentProposalEntry)
    case contextMarker(ChatContextMarkerEntry)

    public var id: String {
        switch self {
        case .text(let e): return e.id
        case .tools(let e): return e.id
        case .notice(let e): return e.id
        case .proposal(let e): return e.id
        case .contextMarker(let e): return e.id
        }
    }
}

public struct ChatAgentModelOption: Sendable, Hashable, Identifiable {
    public var id: String
    public var label: String
    public var group: String?
    public var disabled: Bool

    public init(id: String, label: String, group: String? = nil, disabled: Bool = false) {
        self.id = id
        self.label = label
        self.group = group
        self.disabled = disabled
    }
}

public struct ChatEffortChoice: Sendable, Hashable, Identifiable {
    public var value: String
    public var label: String
    public var id: String { value }

    public init(value: String, label: String) {
        self.value = value
        self.label = label
    }
}

public struct ChatEffortOption: Sendable, Hashable {
    public var id: String
    public var label: String
    public var choices: [ChatEffortChoice]

    public init(id: String = "effort", label: String = "Reasoning effort", choices: [ChatEffortChoice]) {
        self.id = id
        self.label = label
        self.choices = choices
    }
}

public struct ChatContextUsageView: Sendable, Hashable {
    public var used: Int
    public var window: Int
    public var estimated: Bool
    public var summarizedAt: String?
    public var compacting: Bool
    public var canCompact: Bool

    public var share: Double {
        guard window > 0 else { return 0 }
        return min(1.0, max(0.0, Double(used) / Double(window)))
    }

    public var percent: Int {
        Int((share * 100.0).rounded())
    }

    public func shouldShow(threshold: Double = CONTEXT_USAGE_THRESHOLD) -> Bool {
        share >= threshold || compacting
    }
}

/// Format token counts compactly (e.g. `1234 -> "1.2k"`, `200000 -> "200k"`, `1000000 -> "1M"`).
public func formatTokenCount(_ count: Int) -> String {
    let n = max(0, count)
    if n >= 1_000_000 {
        let m = Double(n) / 1_000_000.0
        return m.truncatingRemainder(dividingBy: 1) == 0 ? "\(Int(m))M" : String(format: "%.1fM", m)
    }
    if n >= 1_000 {
        let k = Double(n) / 1_000.0
        return k >= 100 || k.truncatingRemainder(dividingBy: 1) == 0
            ? "\(Int(k.rounded()))k"
            : String(format: "%.1fk", k)
    }
    return "\(n)"
}

/// Map `ChatControllerState` to menu model choices.
public func modelsFromState(_ state: ChatControllerState) -> [ChatAgentModelOption] {
    if !state.choices.isEmpty {
        return state.choices
            .filter { !$0.id.isEmpty }
            .map { choice in
                let label = (choice.label?.isEmpty == false) ? choice.label! : (choice.model.isEmpty ? choice.id : choice.model)
                let group = (choice.group?.isEmpty == false) ? choice.group : nil
                return ChatAgentModelOption(id: choice.id, label: label, group: group, disabled: choice.disabled == true)
            }
    }
    return state.models
        .filter { !$0.isEmpty }
        .map { ChatAgentModelOption(id: $0, label: $0) }
}

/// The "Reasoning effort" menu options for the selected model, or `nil` when none apply.
public func effortOptionFromState(_ state: ChatControllerState, model: String = "") -> ChatEffortOption? {
    let choice: ChatModelChoice?
    if !model.isEmpty {
        choice = resolveChoice(state.choices, requested: model)
    } else if !state.choice.isEmpty, let current = state.choices.first(where: { $0.id == state.choice }) {
        choice = current
    } else {
        choice = resolveChoice(state.choices, requested: "")
    }
    let efforts = (choice?.efforts ?? []).filter { !$0.isEmpty }
    guard !efforts.isEmpty else { return nil }
    var options = [ChatEffortChoice(value: "", label: "Model default")]
    for e in efforts {
        options.append(ChatEffortChoice(value: e, label: effortLabels[e] ?? e))
    }
    return ChatEffortOption(choices: options)
}

/// Map controller context status to ring view data, or `nil` when window is unknown.
public func contextUsageFromState(_ state: ChatControllerState) -> ChatContextUsageView? {
    guard let ctx = state.context, ctx.window > 0 else { return nil }
    return ChatContextUsageView(
        used: max(0, ctx.tokens),
        window: ctx.window,
        estimated: ctx.estimated,
        summarizedAt: ctx.summarizedAt.isEmpty ? nil : ctx.summarizedAt,
        compacting: ctx.compacting,
        canCompact: ctx.canCompact
    )
}

/// Map one `ChatProposalEntry` to a `ChatAgentProposalEntry`.
public func proposalEntryOf(
    _ entry: ChatProposalEntry,
    errorFormatter: ((ChatTurnFailure?) -> String)? = nil
) -> ChatAgentProposalEntry? {
    guard !entry.id.isEmpty else { return nil }
    let tool = entry.tool.isEmpty ? "tool" : entry.tool
    let summary = (entry.summary ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    let failure = (entry.failure ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    var status = PROPOSAL_STATE_TEXT[entry.state] ?? "State: \(entry.state.rawValue)."
    if entry.state == .failed && !failure.isEmpty {
        status = "\(PROPOSAL_STATE_TEXT[.failed]!) \(failure)"
    }
    let warnings = (entry.warnings ?? [])
        .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
        .filter { !$0.isEmpty }
    let errLine = errorFormatter?(entry.error) ?? proposalErrorText(entry.error)

    return ChatAgentProposalEntry(
        id: "proposal:\(entry.id)",
        proposalId: entry.id,
        tool: tool,
        summary: summary.isEmpty ? humanizeToolName(tool) : summary,
        state: entry.state,
        location: entry.location,
        revision: entry.revision,
        error: errLine,
        status: status,
        detail: failure,
        warnings: warnings,
        canDecide: entry.state == .pending,
        proposal: entry.proposal
    )
}

public func proposalEntriesFromState(
    _ state: ChatControllerState,
    errorFormatter: ((ChatTurnFailure?) -> String)? = nil
) -> [ChatAgentProposalEntry] {
    state.proposals.compactMap { proposalEntryOf($0, errorFormatter: errorFormatter) }
}

private func textOfParts(_ parts: [ChatPart]) -> String {
    parts.reduce(into: "") { acc, part in
        if part.type == "text", let text = part.text {
            acc += text
        }
    }
}

private func hasUserText(_ messages: [ChatMessage], _ text: String) -> Bool {
    messages.contains { msg in
        msg.role == "user" && textOfParts(msg.parts).trimmingCharacters(in: .whitespacesAndNewlines) == text
    }
}

private func buildToolDetail(arguments: JSONValue?, result: JSONValue?, errorMessage: String?) -> ChatAgentToolDetail? {
    let detail = ChatAgentToolDetail(
        arguments: arguments,
        result: result,
        error: (errorMessage?.isEmpty == false) ? errorMessage : nil
    )
    return detail.isEmpty ? nil : detail
}

/// Translate a `ChatControllerState` snapshot into display entries (port of `entriesFromState`).
public func entriesFromState(
    _ state: ChatControllerState,
    pendingText: String = "",
    contextReferences: (([String: JSONValue], ChatMessage) -> [ChatAgentReference])? = nil,
    proposalErrorFormatter: ((ChatTurnFailure?) -> String)? = nil
) -> [ChatAgentEntry] {
    let messages = state.messages

    // Collect tool_result parts keyed by callId.
    var toolResults: [String: (failed: Bool, result: JSONValue?)] = [:]
    for message in messages {
        for part in message.parts where part.type == "tool_result" {
            if let callId = part.callId {
                toolResults[callId] = (failed: part.isError == true, result: part.result)
            }
        }
    }

    // Index all known proposals (decided + active) for notice enrichment.
    var proposalsById: [String: ChatProposal] = [:]
    for p in state.decidedProposals where !p.id.isEmpty {
        proposalsById[p.id] = p
    }
    for entry in state.proposals where !entry.id.isEmpty {
        proposalsById[entry.id] = entry.proposal
    }

    let turn = state.turn
    let activeTurnId = turn?.turnId ?? ""
    let running = state.running
    var liveCalls: [String: TurnToolView] = [:]
    for t in turn?.tools ?? [] {
        liveCalls[t.callId] = t
    }

    let summarizedThrough = state.context?.summarizedThrough ?? ""
    var markerAt = -1
    if !summarizedThrough.isEmpty {
        if let idx = messages.firstIndex(where: { $0.id == summarizedThrough }) {
            markerAt = idx
            while markerAt + 1 < messages.count && messages[markerAt + 1].role == "tool" {
                markerAt += 1
            }
        }
    }

    var entries: [ChatAgentEntry] = []
    func pushMarkerIfNeeded(_ index: Int) {
        if index == markerAt {
            entries.append(.contextMarker(ChatContextMarkerEntry(id: "context-marker:\(summarizedThrough)")))
        }
    }

    for (index, message) in messages.enumerated() {
        if index > 0 { pushMarkerIfNeeded(index - 1) }
        let role = message.role.isEmpty ? "assistant" : message.role
        if role == "tool" { continue }

        let msgId = message.id.isEmpty ? "#\(index)" : message.id
        let turnId = message.turnId ?? ""

        if role == "notice" {
            for (partIndex, part) in message.parts.enumerated() where part.type == "proposal_outcome" {
                let text = (part.text ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
                guard !text.isEmpty else { continue }
                let pid = part.proposalId ?? ""
                entries.append(.notice(ChatAgentNoticeEntry(
                    id: "notice:\(msgId):\(partIndex)",
                    content: text,
                    proposalId: pid,
                    state: part.state ?? "",
                    tool: part.tool ?? "",
                    isError: part.isError == true,
                    result: part.result.flatMap { $0.isNull ? nil : $0 },
                    proposal: pid.isEmpty ? nil : proposalsById[pid]
                )))
            }
            continue
        }

        var buffer = ""
        var textIndex = 0
        var tools: [ChatAgentToolCall] = []
        var references: [ChatAgentReference] = []
        var firstTextEntryIndex: Int? = nil

        func flushText() {
            guard !buffer.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                buffer = ""
                return
            }
            let entryId = textIndex == 0 ? "msg:\(msgId)" : "msg:\(msgId):\(textIndex)"
            let entry = ChatAgentTextEntry(id: entryId, role: role, content: buffer)
            if firstTextEntryIndex == nil {
                firstTextEntryIndex = entries.count
            }
            entries.append(.text(entry))
            buffer = ""
            textIndex += 1
        }

        func flushTools() {
            guard !tools.isEmpty else { return }
            entries.append(.tools(ChatAgentToolsEntry(id: "tools:\(msgId)", tools: tools)))
            tools = []
        }

        for part in message.parts {
            switch part.type {
            case "text":
                buffer += part.text ?? ""

            case "attachment":
                if let uploadId = part.uploadId, !uploadId.isEmpty {
                    let label = (part.name?.isEmpty == false) ? part.name! : uploadId
                    let mime = (part.mime?.isEmpty == false) ? part.mime : nil
                    references.append(ChatAgentReference(
                        type: "attachment",
                        id: uploadId,
                        label: label,
                        isAttachment: true,
                        mime: mime
                    ))
                }

            case "context":
                if let hook = contextReferences, case .object(let dict) = part.result {
                    let chips = hook(dict, message).filter { !$0.label.isEmpty }
                    references.append(contentsOf: chips)
                }

            case "tool_call":
                flushText()
                let callId = part.callId ?? ""
                let live = (!activeTurnId.isEmpty && turnId == activeTurnId) ? liveCalls[callId] : nil
                let outcome = toolResults[callId]
                let resolvedStatus: ToolStatus
                let resolvedResult: JSONValue?
                let resolvedError: String?
                if let outcome {
                    resolvedStatus = outcome.failed ? .failed : .completed
                    resolvedResult = outcome.result
                    resolvedError = nil
                } else if let live, live.status != .running {
                    resolvedStatus = live.status
                    resolvedResult = live.result
                    resolvedError = live.message
                } else {
                    resolvedStatus = (running && !turnId.isEmpty && turnId == activeTurnId) ? .running : .failed
                    resolvedResult = nil
                    resolvedError = nil
                }
                let args = part.arguments ?? live?.arguments
                let detail = buildToolDetail(arguments: args, result: resolvedResult, errorMessage: resolvedError)
                tools.append(ChatAgentToolCall(
                    id: callId,
                    name: (part.tool?.isEmpty == false) ? part.tool! : "tool",
                    status: resolvedStatus,
                    detail: detail
                ))

            default:
                break
            }
        }
        flushText()
        flushTools()

        if !references.isEmpty {
            if let idx = firstTextEntryIndex, case .text(var existing) = entries[idx] {
                existing.references = references
                entries[idx] = .text(existing)
            } else {
                entries.append(.text(ChatAgentTextEntry(
                    id: "msg:\(msgId):attachments",
                    role: role,
                    content: "",
                    references: references
                )))
            }
        }
    }

    if !messages.isEmpty {
        pushMarkerIfNeeded(messages.count - 1)
    }

    let trimmedPending = pendingText.trimmingCharacters(in: .whitespacesAndNewlines)
    if !trimmedPending.isEmpty && !hasUserText(messages, trimmedPending) {
        entries.append(.text(ChatAgentTextEntry(id: "pending:user", role: "user", content: trimmedPending)))
    }

    if let turn, !activeTurnId.isEmpty {
        var transcriptText = ""
        var knownCalls = Set<String>()
        for message in messages where message.turnId == activeTurnId {
            if message.role != "user" {
                transcriptText += textOfParts(message.parts)
            }
            for part in message.parts where part.type == "tool_call" {
                if let callId = part.callId { knownCalls.insert(callId) }
            }
        }

        var remainder = turn.text
        if !transcriptText.isEmpty {
            if remainder.hasPrefix(transcriptText) {
                remainder = String(remainder.dropFirst(transcriptText.count))
            } else {
                remainder = ""
            }
        }

        var liveTools: [ChatAgentToolCall] = []
        var liveIndex = 0
        for tool in turn.tools {
            liveIndex += 1
            if !tool.callId.isEmpty && knownCalls.contains(tool.callId) { continue }
            let status: ToolStatus = (!running && tool.status == .running) ? .failed : tool.status
            let detail = buildToolDetail(
                arguments: tool.arguments,
                result: tool.status == .running ? nil : tool.result,
                errorMessage: tool.status == .running ? nil : tool.message
            )
            liveTools.append(ChatAgentToolCall(
                id: tool.callId.isEmpty ? "live-\(liveIndex)" : tool.callId,
                name: tool.tool.isEmpty ? "tool" : tool.tool,
                status: status,
                detail: detail
            ))
        }

        let textEntry: ChatAgentEntry? = remainder.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            ? nil
            : .text(ChatAgentTextEntry(
                id: "turn:\(activeTurnId)",
                role: "assistant",
                content: remainder,
                isStreaming: running
            ))
        let toolsEntry: ChatAgentEntry? = liveTools.isEmpty
            ? nil
            : .tools(ChatAgentToolsEntry(id: "tools:\(activeTurnId)", tools: liveTools))

        if let toolsEntry, let textEntry {
            entries.append(toolsEntry)
            entries.append(textEntry)
        } else if let toolsEntry {
            entries.append(toolsEntry)
        } else if let textEntry {
            entries.append(textEntry)
        }
    }

    for proposal in proposalEntriesFromState(state, errorFormatter: proposalErrorFormatter) {
        entries.append(.proposal(proposal))
    }

    return entries
}
