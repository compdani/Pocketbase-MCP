import Foundation

/// Pure, immutable port of `reducer.mjs` / `chat/reducer.go`.
public enum ChatReducer {
    public static let terminalEventTypes: Set<String> = [
        "turn.completed",
        "turn.failed",
        "turn.cancelled",
    ]

    public static func isTerminalState(_ state: TurnState) -> Bool {
        state.isTerminal
    }

    public static func isTerminalEventType(_ type: String) -> Bool {
        terminalEventTypes.contains(type)
    }

    public static func isTerminalProposalState(_ state: ChatProposalState) -> Bool {
        state.isTerminal
    }

    /// A fresh view for one turn, matching a zero Go `TurnView`.
    public static func createTurnView(_ turnId: String = "") -> TurnView {
        TurnView(turnId: turnId)
    }

    /// Validate one proposal payload exactly like `chat.Proposal.Validate` / `validateProposal`.
    public static func validateProposal(_ raw: JSONValue?, type: String = "proposal") throws -> ChatProposal {
        guard case .object(let dict) = raw else {
            throw ReducerError.invalidEvent("chat: decode \(type) data: proposal is not an object")
        }
        func fail(_ reason: String) throws -> Never {
            throw ReducerError.invalidEvent("chat: \(type) data: \(reason)")
        }

        guard let id = dict["id"]?.stringValue, !id.isEmpty else {
            try fail("proposal has no id")
        }
        guard let conversationId = dict["conversationId"]?.stringValue, !conversationId.isEmpty else {
            try fail("proposal \"\(id)\" has no conversation")
        }
        guard let locStr = dict["location"]?.stringValue,
              let location = ChatProposalLocation(rawValue: locStr) else {
            try fail("proposal \"\(id)\" has invalid location")
        }
        guard let tool = dict["tool"]?.stringValue, !tool.isEmpty else {
            try fail("proposal \"\(id)\" has no tool")
        }
        guard let stateStr = dict["state"]?.stringValue,
              let state = ChatProposalState(rawValue: stateStr) else {
            try fail("proposal \"\(id)\" has invalid state")
        }
        guard let revision = dict["revision"]?.intValue, revision >= 1 else {
            try fail("proposal \"\(id)\" has revision less than 1")
        }

        var argsValue = dict["arguments"]
        if case .string(let argString) = argsValue {
            guard let data = argString.data(using: .utf8),
                  let parsed = try? JSONDecoder().decode(JSONValue.self, from: data) else {
                try fail("proposal \"\(id)\" has invalid arguments")
            }
            argsValue = parsed
        }
        guard case .object = argsValue else {
            try fail("proposal \"\(id)\" arguments are not a JSON object")
        }

        var hunks: [ChatHunkOutcome]? = nil
        if let rawHunks = dict["hunks"], !rawHunks.isNull {
            guard case .array(let arr) = rawHunks else {
                try fail("proposal \"\(id)\" has invalid hunks")
            }
            var parsedHunks: [ChatHunkOutcome] = []
            for item in arr {
                guard case .object(let hObj) = item,
                      let hId = hObj["id"]?.stringValue,
                      let hStatusStr = hObj["status"]?.stringValue,
                      let hStatus = ChatHunkStatus(rawValue: hStatusStr) else {
                    try fail("proposal \"\(id)\" has an invalid hunk")
                }
                parsedHunks.append(ChatHunkOutcome(id: hId, status: hStatus))
            }
            hunks = parsedHunks
        }

        var warnings: [String]? = nil
        if case .array(let wArr) = dict["warnings"] {
            warnings = wArr.compactMap { $0.stringValue }
        }

        return ChatProposal(
            id: id,
            conversationId: conversationId,
            turnId: dict["turnId"]?.stringValue,
            callId: dict["callId"]?.stringValue,
            tool: tool,
            toolFingerprint: dict["toolFingerprint"]?.stringValue,
            manifestRevision: dict["manifestRevision"]?.stringValue,
            location: location,
            target: dict["target"]?.stringValue,
            arguments: argsValue ?? .object([:]),
            argumentsHash: dict["argumentsHash"]?.stringValue ?? "",
            summary: dict["summary"]?.stringValue,
            state: state,
            revision: revision,
            createdAt: dict["createdAt"]?.stringValue ?? "",
            updatedAt: dict["updatedAt"]?.stringValue ?? "",
            expiresAt: dict["expiresAt"]?.stringValue,
            decidedAt: dict["decidedAt"]?.stringValue,
            result: dict["result"].flatMap { $0.isNull ? nil : $0 },
            failure: dict["failure"]?.stringValue,
            warnings: warnings,
            editorRevision: dict["editorRevision"]?.stringValue,
            hunks: hunks
        )
    }

    /// Insert or replace one proposal by id: newer-or-equal revision wins; older loses.
    public static func putProposal(_ proposals: [ChatProposal], _ proposal: ChatProposal) -> [ChatProposal] {
        guard let index = proposals.firstIndex(where: { $0.id == proposal.id }) else {
            return proposals + [proposal]
        }
        if proposal.revision < proposals[index].revision {
            return proposals
        }
        var next = proposals
        next[index] = proposal
        return next
    }

    /// Fold one event envelope into a `TurnView`.
    public static func applyEvent(_ view: TurnView, _ event: ChatEvent) throws -> TurnView {
        guard event.seq >= 1 else {
            throw ReducerError.invalidEvent("chat: event sequence \(event.seq) is not a positive integer")
        }
        if !view.turnId.isEmpty && !event.turnId.isEmpty && event.turnId != view.turnId {
            throw ReducerError.foreignTurn(eventTurnId: event.turnId, viewTurnId: view.turnId)
        }
        if event.seq <= view.lastSeq {
            return view
        }
        if event.seq != view.lastSeq + 1 {
            throw ReducerError.sequenceGap(
                turnId: view.turnId.isEmpty ? event.turnId : view.turnId,
                have: view.lastSeq,
                got: event.seq
            )
        }
        if isTerminalState(view.state) {
            throw ReducerError.afterTerminal(eventType: event.type, state: view.state.rawValue)
        }

        var next = view
        next.turnId = view.turnId.isEmpty ? event.turnId : view.turnId
        next.lastSeq = event.seq

        switch event.type {
        case "turn.queued":
            next.state = .queued

        case "turn.started":
            next.state = .running

        case "text.delta":
            let data = try decodeData(event)
            next.text += try stringField(data, key: "text", type: event.type)

        case "tool.started":
            let data = try decodeData(event)
            let callId = try stringField(data, key: "callId", type: event.type)
            let toolName = try stringField(data, key: "tool", type: event.type)
            let tool = TurnToolView(
                callId: callId,
                tool: toolName,
                arguments: data["arguments"],
                status: .running,
                result: nil,
                message: ""
            )
            next.tools = putTool(view.tools, tool)

        case "tool.completed":
            let data = try decodeData(event)
            let callId = try stringField(data, key: "callId", type: event.type)
            let existing = findTool(view.tools, callId: callId)
                ?? TurnToolView(callId: callId, tool: "", status: .running, result: nil, message: "")
            let name = try stringField(data, key: "tool", type: event.type)
            var updated = existing
            updated.tool = name.isEmpty ? existing.tool : name
            updated.status = .completed
            updated.result = data["result"]
            updated.message = ""
            next.tools = putTool(view.tools, updated)

        case "tool.failed":
            let data = try decodeData(event)
            let callId = try stringField(data, key: "callId", type: event.type)
            let existing = findTool(view.tools, callId: callId)
                ?? TurnToolView(callId: callId, tool: "", status: .running, result: nil, message: "")
            let name = try stringField(data, key: "tool", type: event.type)
            var updated = existing
            updated.tool = name.isEmpty ? existing.tool : name
            updated.status = .failed
            updated.message = try stringField(data, key: "message", type: event.type)
            next.tools = putTool(view.tools, updated)

        case "usage.updated":
            let data = try decodeData(event)
            let inputTokens = try numberField(data, key: "inputTokens", type: event.type)
            let outputTokens = try numberField(data, key: "outputTokens", type: event.type)
            next.usage = TurnUsage(inputTokens: Int(inputTokens), outputTokens: Int(outputTokens))

        case "turn.completed":
            next.state = .completed

        case "turn.failed":
            let data = try decodeData(event)
            let failure = ChatTurnFailure(
                code: try stringField(data, key: "code", type: event.type),
                message: try stringField(data, key: "message", type: event.type)
            )
            next.error = failure
            next.state = failure.code == "interrupted" ? .interrupted : .failed

        case "turn.cancelled":
            next.state = .cancelled

        case "proposal.created", "proposal.updated":
            let data = try decodeData(event)
            let proposal = try validateProposal(data["proposal"], type: event.type)
            next.proposals = putProposal(view.proposals, proposal)

        default:
            throw ReducerError.invalidEvent("chat: unknown event type \"\(event.type)\"")
        }

        return next
    }

    /// Rebuild a `TurnView` from canonical transcript records (after resync or on open).
    public static func viewFromTranscript(_ turn: ChatTurn?, messages: [ChatMessage] = []) -> TurnView {
        let turnId = turn?.id ?? ""
        var view = createTurnView(turnId)
        view.state = turn?.state ?? .none
        view.error = turn?.error

        var text = ""
        var tools: [TurnToolView] = []

        for message in messages {
            guard message.turnId == turnId else { continue }
            for part in message.parts {
                switch part.type {
                case "text":
                    if message.role != "user" {
                        text += part.text ?? ""
                    }
                case "tool_call":
                    tools = putTool(
                        tools,
                        TurnToolView(
                            callId: part.callId ?? "",
                            tool: part.tool ?? "",
                            arguments: part.arguments,
                            status: .running,
                            result: nil,
                            message: ""
                        )
                    )
                case "tool_result":
                    let callId = part.callId ?? ""
                    var existing = findTool(tools, callId: callId)
                        ?? TurnToolView(
                            callId: callId,
                            tool: part.tool ?? "",
                            status: .running,
                            result: nil,
                            message: ""
                        )
                    existing.status = part.isError == true ? .failed : .completed
                    existing.result = part.result
                    existing.message = ""
                    tools = putTool(tools, existing)
                default:
                    break
                }
            }
        }

        view.text = text
        view.tools = tools
        return view
    }

    // MARK: - Private helpers

    private static func decodeData(_ event: ChatEvent) throws -> [String: JSONValue] {
        guard let raw = event.data, !raw.isNull else {
            throw ReducerError.invalidEvent("chat: \(event.type) event without data")
        }
        let value: JSONValue
        if case .string(let str) = raw {
            if str.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                throw ReducerError.invalidEvent("chat: \(event.type) event without data")
            }
            guard let data = str.data(using: .utf8),
                  let parsed = try? JSONDecoder().decode(JSONValue.self, from: data) else {
                throw ReducerError.invalidEvent("chat: decode \(event.type) data: invalid JSON")
            }
            value = parsed
        } else {
            value = raw
        }
        guard case .object(let dict) = value else {
            throw ReducerError.invalidEvent("chat: decode \(event.type) data: not an object")
        }
        return dict
    }

    private static func stringField(_ data: [String: JSONValue], key: String, type: String) throws -> String {
        guard let value = data[key], !value.isNull else { return "" }
        guard case .string(let str) = value else {
            throw ReducerError.invalidEvent("chat: decode \(type) data: \(key) is not a string")
        }
        return str
    }

    private static func numberField(_ data: [String: JSONValue], key: String, type: String) throws -> Double {
        guard let value = data[key], !value.isNull else { return 0 }
        guard case .number(let num) = value, num.isFinite else {
            throw ReducerError.invalidEvent("chat: decode \(type) data: \(key) is not a number")
        }
        return num
    }

    private static func findTool(_ tools: [TurnToolView], callId: String) -> TurnToolView? {
        tools.first { $0.callId == callId }
    }

    private static func putTool(_ tools: [TurnToolView], _ tool: TurnToolView) -> [TurnToolView] {
        guard let index = tools.firstIndex(where: { $0.callId == tool.callId }) else {
            return tools + [tool]
        }
        var next = tools
        next[index] = tool
        return next
    }
}
