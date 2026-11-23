import Foundation
import Observation
import PBMCPChat

/// State of an inline composer attachment chip before a turn is accepted.
public struct ComposerAttachment: Sendable, Hashable, Identifiable {
    public var id: String
    public var file: ChatUploadFile
    public var isBusy: Bool
    public var error: String?

    public init(id: String = UUID().uuidString, file: ChatUploadFile, isBusy: Bool = false, error: String? = nil) {
        self.id = id
        self.file = file
        self.isBusy = isBusy
        self.error = error
    }
}

/// Active mention-trigger state parsed from the composer draft.
public struct ChatMentionState: Sendable, Hashable {
    public var isOpen: Bool
    public var query: String

    public init(isOpen: Bool = false, query: String = "") {
        self.isOpen = isOpen
        self.query = query
    }

    public static let closed = ChatMentionState(isOpen: false, query: "")
}

/// How a submit finished (Swift twin of `AgentSendResult`).
public enum ChatSendOutcome: Sendable {
    case accepted(text: String, turn: ChatTurn?, created: Bool)
    case refused(text: String, code: String, message: String)
    case failed(text: String, code: String, message: String)
    case cancelled(text: String)
}

/// `@MainActor @Observable` presentation model binding `ChatController` to `ChatPage`.
///
/// Mirrors the behavior of `frontend/client/agent-panel.mjs` (optimistic user echo,
/// error mapping and dismissal, attachment validation & chip states, draft restoration
/// on failed send, model + reasoning effort selection, context ring & compaction,
/// mention trigger detection, and VoiceOver completion announcements).
@MainActor
@Observable
public final class ChatPageViewModel {
    public let controller: ChatController

    public var draft: String = "" {
        didSet {
            updateMentionState()
        }
    }
    public var pendingAttachments: [ComposerAttachment] = []
    public var selectedModel: String = ""
    public var selectedEffort: String = ""
    public var isHostDisabled: Bool = false
    public var autoCreate: Bool = true
    public var disableWhenNotReady: Bool = true
    public var restoreDraftOnFailure: Bool = true
    public var mentionTrigger: String = "@"
    public var contextThreshold: Double = CONTEXT_USAGE_THRESHOLD

    public private(set) var mentionState: ChatMentionState = .closed
    public private(set) var isSubmitting: Bool = false
    public private(set) var lastCompletedAssistantMessage: String?

    public var contextReferences: (([String: JSONValue], ChatMessage) -> [ChatAgentReference])?
    public var errorTextOverride: ((String, String, Error?) -> String?)?
    public var onSend: ((ChatSendOutcome) -> Void)?
    public var onMentionChange: ((ChatMentionState) -> Void)?
    public var onAttachmentDownload: ((String, ChatAgentReference) async throws -> Void)?

    private var pendingText: String = ""
    private var pendingConversationId: String? = nil
    private var localError: String? = nil
    private var dismissedError: String? = nil
    private var staleTurnId: String? = nil
    private var staleStateError: ChatTurnFailure? = nil
    private var lastAnnouncedTurnId: String? = nil
    private var wasRunning: Bool = false

    public init(
        controller: ChatController,
        autoCreate: Bool = true,
        disableWhenNotReady: Bool = true,
        restoreDraftOnFailure: Bool = true,
        mentionTrigger: String = "@"
    ) {
        self.controller = controller
        self.autoCreate = autoCreate
        self.disableWhenNotReady = disableWhenNotReady
        self.restoreDraftOnFailure = restoreDraftOnFailure
        self.mentionTrigger = mentionTrigger
        self.selectedModel = controller.state.choice
    }

    // MARK: - Derived state

    public var state: ChatControllerState {
        controller.state
    }

    public var isRunning: Bool {
        state.running || state.sending || isSubmitting
    }

    public var isProviderMissing: Bool {
        state.features?.ready == false
    }

    public var isComposerDisabled: Bool {
        isHostDisabled || (disableWhenNotReady && isProviderMissing)
    }

    public var attachmentsEnabled: Bool {
        guard let features = state.features else { return true }
        if !features.uploads { return false }
        if let types = state.limits?.attachmentTypes, types.isEmpty { return false }
        return true
    }

    public var activeConversation: ChatConversation? {
        guard let activeId = state.activeConversationId else { return nil }
        return state.conversations.first { $0.id == activeId }
    }

    public var activeConversationTitle: String {
        activeConversation?.title ?? ""
    }

    public var modelOptions: [ChatAgentModelOption] {
        modelsFromState(state)
    }

    public var effectiveModelId: String {
        if !selectedModel.isEmpty { return selectedModel }
        if !state.choice.isEmpty { return state.choice }
        return state.defaultChoice
    }

    public var effortOption: ChatEffortOption? {
        effortOptionFromState(state, model: effectiveModelId)
    }

    public var contextUsage: ChatContextUsageView? {
        contextUsageFromState(state)
    }

    public var shouldShowContextRing: Bool {
        contextUsage?.shouldShow(threshold: contextThreshold) ?? false
    }

    public var canSend: Bool {
        guard !isComposerDisabled, !isRunning else { return false }
        return !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !pendingAttachments.isEmpty
    }

    /// The mapped transcript entries (messages, live turn, proposals, notices, context marker).
    /// Pure projection — never mutates `@Observable` state during `View.body`.
    public var entries: [ChatAgentEntry] {
        entriesFromState(
            state,
            pendingText: effectivePendingText,
            contextReferences: contextReferences,
            proposalErrorFormatter: { [weak self] failure in
                self?.formatProposalError(failure) ?? proposalErrorText(failure)
            }
        )
    }

    /// Plain-language error banner text, or `""` when none should be displayed.
    /// Pure projection — never mutates `@Observable` state during `View.body`.
    public var bannerError: String {
        let turn = state.turn
        let isTurnStale = staleTurnId != nil && turn?.turnId == staleTurnId
        let turnError: ChatTurnFailure? = (turn != nil && !isTurnStale) ? turn?.error : nil
        let isStateErrorStale = staleStateError != nil && state.error == staleStateError
        let stateError: ChatTurnFailure? = isStateErrorStale ? nil : state.error
        let notReady = disableWhenNotReady && isProviderMissing

        let mapped: String
        if let stateError {
            mapped = formatError(stateError)
        } else if let turnError {
            mapped = formatError(turnError)
        } else if notReady {
            mapped = formatError(ChatTurnFailure(code: "no_provider", message: ""))
        } else {
            mapped = ""
        }

        let text = localError ?? (mapped.isEmpty ? nil : mapped) ?? ""
        if text.isEmpty {
            return ""
        }
        if text == dismissedError {
            return ""
        }
        return text
    }

    private var effectivePendingText: String {
        guard !pendingText.isEmpty else { return "" }
        if let pid = pendingConversationId,
           state.activeConversationId == nil || state.activeConversationId != pid {
            return ""
        }
        let trimmed = pendingText.trimmingCharacters(in: .whitespacesAndNewlines)
        let present = state.messages.contains { msg in
            guard msg.role == "user" else { return false }
            let text = msg.parts.compactMap { $0.type == "text" ? $0.text : nil }.joined()
            return text.trimmingCharacters(in: .whitespacesAndNewlines) == trimmed
        }
        return present ? "" : pendingText
    }

    /// Synchronize transient view-model flags with `controller.state` (called on reads & transitions).
    public func syncWithControllerState() {
        reconcileEphemeralState()
        if selectedModel.isEmpty && !state.choice.isEmpty {
            selectedModel = state.choice
        }
        // Track VoiceOver completion announcement: announce only when streaming transitions running -> stopped.
        let currentlyRunning = state.running
        if wasRunning && !currentlyRunning,
           let turn = state.turn,
           turn.state == .completed,
           !turn.turnId.isEmpty,
           turn.turnId != lastAnnouncedTurnId {
            let finalText = completedAssistantText(for: turn)
            if !finalText.isEmpty {
                lastAnnouncedTurnId = turn.turnId
                lastCompletedAssistantMessage = finalText
            }
        }
        wasRunning = currentlyRunning
    }

    private func completedAssistantText(for turn: TurnView) -> String {
        let trimmed = turn.text.trimmingCharacters(in: .whitespacesAndNewlines)
        if !trimmed.isEmpty { return trimmed }
        let fromMessages = state.messages
            .filter { $0.turnId == turn.turnId && $0.role == "assistant" }
            .flatMap(\.parts)
            .compactMap { $0.type == "text" ? $0.text : nil }
            .joined()
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return fromMessages
    }

    private func reconcileEphemeralState() {
        if !pendingText.isEmpty {
            if let pid = pendingConversationId,
               state.activeConversationId == nil || state.activeConversationId != pid {
                pendingText = ""
                pendingConversationId = nil
            } else {
                let trimmed = pendingText.trimmingCharacters(in: .whitespacesAndNewlines)
                let present = state.messages.contains { msg in
                    guard msg.role == "user" else { return false }
                    let text = msg.parts.compactMap { $0.type == "text" ? $0.text : nil }.joined()
                    return text.trimmingCharacters(in: .whitespacesAndNewlines) == trimmed
                }
                if present {
                    pendingText = ""
                    pendingConversationId = nil
                }
            }
        }
        if let staleId = staleTurnId, state.turn?.turnId != staleId {
            staleTurnId = nil
        }
        if let staleErr = staleStateError, state.error != staleErr {
            staleStateError = nil
        }
    }

    // MARK: - Error formatting

    private func formatError(_ failure: ChatTurnFailure?, underlying: Error? = nil) -> String {
        let text = errorText(failure)
        guard !text.isEmpty else { return "" }
        let code = errorCode(failure)
        if let custom = errorTextOverride?(code, text, underlying), !custom.isEmpty {
            return custom
        }
        return text
    }

    private func formatLocalError(_ code: String) -> String {
        let text = LOCAL_ERROR_TEXT[code] ?? errorText(code: code)
        if let custom = errorTextOverride?(code, text, nil), !custom.isEmpty {
            return custom
        }
        return text
    }

    private func formatProposalError(_ failure: ChatTurnFailure?) -> String {
        let text = proposalErrorText(failure)
        guard !text.isEmpty else { return "" }
        let code = proposalErrorCode(failure)
        if let custom = errorTextOverride?(code, text, nil), !custom.isEmpty {
            return custom
        }
        return text
    }

    public func dismissError() {
        let current = bannerError
        dismissedError = current.isEmpty ? nil : current
        localError = nil
    }

    // MARK: - Mention trigger

    private func updateMentionState() {
        guard !mentionTrigger.isEmpty else {
            setMention(.closed)
            return
        }
        // Detect `<trigger><query>` at the end of the draft, preceded by start-of-string or whitespace.
        guard let triggerRange = draft.range(of: mentionTrigger, options: .backwards) else {
            setMention(.closed)
            return
        }
        if triggerRange.lowerBound > draft.startIndex {
            let before = draft[draft.index(before: triggerRange.lowerBound)]
            if !before.isWhitespace {
                setMention(.closed)
                return
            }
        }
        let query = String(draft[triggerRange.upperBound...])
        if query.contains(where: { $0.isWhitespace }) {
            setMention(.closed)
            return
        }
        setMention(ChatMentionState(isOpen: true, query: query))
    }

    private func setMention(_ next: ChatMentionState) {
        guard next != mentionState else { return }
        mentionState = next
        onMentionChange?(next)
    }

    /// Replace the active `@query` token at the end of `draft` with `insertion`.
    public func insertMention(_ insertion: String) {
        guard mentionState.isOpen,
              let triggerRange = draft.range(of: mentionTrigger, options: .backwards) else {
            draft += insertion
            return
        }
        draft.replaceSubrange(triggerRange.lowerBound..<draft.endIndex, with: insertion + " ")
    }

    // MARK: - Attachments

    public func addAttachmentFiles(_ files: [ChatUploadFile]) {
        guard !files.isEmpty else { return }
        if !attachmentsEnabled {
            localError = formatLocalError("attachments_unsupported")
            return
        }
        let limits = state.limits
        let maxFiles = limits?.maxAttachmentsPerTurn ?? 0
        let maxBytes = limits?.maxUploadBytes ?? 0
        let choice = resolveChoice(state.choices, requested: effectiveModelId)
        let allowed = attachmentTypesFor(limits, choice: choice)?.map { $0.lowercased() }

        for file in files {
            if maxFiles > 0 && pendingAttachments.count >= maxFiles {
                localError = formatError(ChatTurnFailure(code: "invalid_attachment", message: ""))
                break
            }
            if maxBytes > 0 && file.size > maxBytes {
                localError = formatError(ChatTurnFailure(code: "payload_too_large", message: ""))
                continue
            }
            let mime = (file.mimeType.isEmpty ? inferAttachmentMime(file.name) : file.mimeType).lowercased()
            if let allowed, !mime.isEmpty, !allowed.contains(mime) {
                localError = formatError(ChatTurnFailure(code: "unsupported_media", message: ""))
                continue
            }
            pendingAttachments.append(ComposerAttachment(file: file))
        }
    }

    public func removeAttachment(id: String) {
        pendingAttachments.removeAll { $0.id == id }
    }

    public func clearComposerAttachments() {
        pendingAttachments.removeAll()
    }

    // MARK: - Model & effort selection

    public func selectModel(_ modelId: String) {
        selectedModel = modelId
        controller.selectChoice(modelId)
        // Clear effort if the newly selected model doesn't support it.
        if let option = effortOption {
            if !option.choices.contains(where: { $0.value == selectedEffort }) {
                selectedEffort = ""
            }
        } else {
            selectedEffort = ""
        }
    }

    public func selectEffort(_ effort: String) {
        selectedEffort = effort
    }

    // MARK: - Actions

    /// Submit the current draft (or an explicit `textOverride`).
    @discardableResult
    public func submit(textOverride: String? = nil) async -> ChatSendOutcome? {
        let fromDraft = textOverride == nil
        let rawText = textOverride ?? draft
        let text = rawText.trimmingCharacters(in: .whitespacesAndNewlines)
        let attachmentsSnapshot = pendingAttachments

        if isComposerDisabled {
            return nil
        }

        if text.isEmpty {
            if !attachmentsSnapshot.isEmpty {
                let message = formatLocalError("attachment_text_required")
                localError = message
                let outcome = ChatSendOutcome.failed(text: "", code: "attachment_text_required", message: message)
                onSend?(outcome)
                return outcome
            }
            return nil
        }

        if isSubmitting {
            let outcome = ChatSendOutcome.failed(text: text, code: "busy", message: "")
            onSend?(outcome)
            return outcome
        }

        if !attachmentsSnapshot.isEmpty && state.features?.uploads == false {
            let message = formatLocalError("attachments_unsupported")
            localError = message
            let outcome = ChatSendOutcome.failed(text: text, code: "attachments_unsupported", message: message)
            onSend?(outcome)
            return outcome
        }

        // Start clean: clear banner and hide stale turn/state error.
        localError = nil
        dismissedError = nil
        staleTurnId = state.turn?.turnId.isEmpty == false ? state.turn?.turnId : nil
        staleStateError = state.error
        pendingText = text
        pendingConversationId = state.activeConversationId
        isSubmitting = true
        if fromDraft {
            draft = ""
        }
        if !attachmentsSnapshot.isEmpty {
            setPendingAttachmentsState(ids: Set(attachmentsSnapshot.map(\.id)), isBusy: true, error: nil)
        }

        defer {
            isSubmitting = false
            syncWithControllerState()
        }

        // Read page context early, before creating a conversation or uploading files.
        let wantContext = state.features == nil || state.features?.context == true
        let contextTask = wantContext ? controller.readContext() : nil

        let outcome: ChatSendOutcome
        do {
            var conversationId = state.activeConversationId
            if conversationId == nil {
                if !autoCreate {
                    throw ChatApiError(status: 400, code: "invalid_request", message: "No conversation is open.")
                }
                let created = await controller.newConversation()
                conversationId = created?.id ?? state.activeConversationId
                if conversationId == nil {
                    pendingText = ""
                    pendingConversationId = nil
                    if let err = state.error {
                        let msg = formatError(err)
                        outcome = err.code == "turn_refused"
                            ? .refused(text: text, code: err.code, message: msg)
                            : .failed(text: text, code: err.code, message: msg)
                    } else {
                        outcome = .cancelled(text: text)
                    }
                    finalizeOutcome(outcome, fromDraft: fromDraft, originalText: text, attachments: attachmentsSnapshot)
                    return outcome
                }
            }

            pendingConversationId = conversationId
            let uploadInputs = attachmentsSnapshot.map { ChatAttachmentInput.file($0.file) }
            let modelToSend = effectiveModelId.isEmpty ? nil : effectiveModelId
            let effortToSend = selectedEffort.isEmpty ? nil : selectedEffort

            let result = try await controller.send(
                text,
                model: modelToSend,
                effort: effortToSend,
                attachments: uploadInputs,
                contextTask: contextTask
            )

            if let result {
                outcome = .accepted(text: text, turn: result.turn, created: result.created)
            } else if let err = state.error {
                let code = err.code.isEmpty || err.code == "error" ? errorCode(err) : err.code
                let msg = formatError(err)
                outcome = code == "turn_refused"
                    ? .refused(text: text, code: code, message: msg)
                    : .failed(text: text, code: code, message: msg)
            } else {
                outcome = .cancelled(text: text)
            }
        } catch {
            let failure = ChatTurnFailure(code: errorCode(error), message: error.localizedDescription)
            let msg = formatProposalError(failure)
            localError = msg.isEmpty ? formatError(failure, underlying: error) : msg
            let code = (error as? ChatApiError)?.code ?? errorCode(error)
            outcome = code == "turn_refused"
                ? .refused(text: text, code: code, message: localError ?? "")
                : .failed(text: text, code: code, message: localError ?? "")
        }

        finalizeOutcome(outcome, fromDraft: fromDraft, originalText: text, attachments: attachmentsSnapshot)
        return outcome
    }

    private func finalizeOutcome(
        _ outcome: ChatSendOutcome,
        fromDraft: Bool,
        originalText: String,
        attachments: [ComposerAttachment]
    ) {
        let ids = Set(attachments.map(\.id))
        switch outcome {
        case .accepted:
            pendingAttachments.removeAll { ids.contains($0.id) }
        case .cancelled:
            pendingText = ""
            pendingConversationId = nil
            setPendingAttachmentsState(ids: ids, isBusy: false, error: nil)
        case .failed(_, _, let message), .refused(_, _, let message):
            pendingText = ""
            pendingConversationId = nil
            let chipError = localError ?? (message.isEmpty ? formatLocalError("upload_failed") : message)
            setPendingAttachmentsState(ids: ids, isBusy: false, error: chipError)
            if restoreDraftOnFailure && fromDraft && draft.isEmpty {
                draft = originalText
            }
        }
        onSend?(outcome)
    }

    private func setPendingAttachmentsState(ids: Set<String>, isBusy: Bool, error: String?) {
        pendingAttachments = pendingAttachments.map { item in
            guard ids.contains(item.id) else { return item }
            var copy = item
            copy.isBusy = isBusy
            copy.error = error
            return copy
        }
    }

    public func stop() async {
        guard state.running else { return }
        _ = await controller.stop()
        syncWithControllerState()
    }

    public func decide(proposalId: String, decision: ProposalDecision) async {
        localError = nil
        do {
            _ = try await controller.decide(proposalId, decision)
        } catch {
            let failure = (error as? ChatApiError).map { ChatTurnFailure(code: $0.code, message: $0.message) }
                ?? ChatTurnFailure(code: errorCode(error), message: error.localizedDescription)
            let formatted = formatProposalError(failure)
            localError = formatted.isEmpty ? formatError(failure, underlying: error) : formatted
        }
        syncWithControllerState()
    }

    public func compact() async {
        guard let usage = contextUsage, usage.canCompact, !usage.compacting else { return }
        _ = await controller.compact()
        syncWithControllerState()
    }

    public func openConversation(_ id: String) async {
        localError = nil
        _ = await controller.open(id)
        syncWithControllerState()
    }

    public func startNewConversation() async {
        localError = nil
        if autoCreate {
            controller.startDraft()
        } else {
            _ = await controller.newConversation()
        }
        syncWithControllerState()
    }

    public func renameActiveConversation(_ newTitle: String) async {
        let trimmed = newTitle.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let id = state.activeConversationId, !trimmed.isEmpty else { return }
        _ = await controller.rename(id, title: trimmed)
        syncWithControllerState()
    }

    public func renameConversation(id: String, title: String) async {
        let trimmed = title.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        _ = await controller.rename(id, title: trimmed)
        syncWithControllerState()
    }

    public func deleteConversation(_ id: String) async {
        _ = await controller.remove(id)
        syncWithControllerState()
    }

    public func loadMoreConversations() async {
        _ = await controller.loadMore()
        syncWithControllerState()
    }

    public func downloadAttachment(_ reference: ChatAgentReference) async {
        do {
            if let custom = onAttachmentDownload {
                try await custom(reference.id, reference)
            } else {
                _ = try await controller.client.downloadUpload(reference.id)
            }
        } catch {
            localError = formatError(
                ChatTurnFailure(code: errorCode(error), message: error.localizedDescription),
                underlying: error
            )
        }
    }
}
