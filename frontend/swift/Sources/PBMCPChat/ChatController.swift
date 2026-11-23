import Foundation
import Observation

// MARK: - Pure controller helpers (ported from controller.mjs)

private let mimeByExtension: [String: String] = [
    "txt": "text/plain",
    "text": "text/plain",
    "log": "text/plain",
    "md": "text/markdown",
    "markdown": "text/markdown",
    "csv": "text/csv",
    "json": "application/json",
    "png": "image/png",
    "jpg": "image/jpeg",
    "jpeg": "image/jpeg",
    "gif": "image/gif",
    "webp": "image/webp",
    "pdf": "application/pdf",
]

/// Resolve a picker value or model id to one catalog choice.
public func resolveChoice(_ choices: [ChatModelChoice], requested: String? = nil) -> ChatModelChoice? {
    guard !choices.isEmpty else { return nil }
    let value = requested ?? ""
    if !value.isEmpty {
        if let exact = choices.first(where: { $0.id == value }) {
            return exact
        }
        let byModel = choices.filter { $0.model == value }
        if byModel.count == 1 {
            return byModel[0]
        }
        if let fallback = choices.first(where: { $0.isDefault == true }), fallback.model == value {
            return fallback
        }
        return nil
    }
    return choices.first(where: { $0.isDefault == true }) ?? choices.first
}

/// The attachment types a message to `choice` can carry, or `nil` when unknown.
public func attachmentTypesFor(_ limits: ChatLimits?, choice: ChatModelChoice? = nil) -> [String]? {
    let source: [String]?
    if let choiceTypes = choice?.attachmentTypes {
        source = choiceTypes
    } else {
        source = limits?.attachmentTypes
    }
    return source?.filter { !$0.isEmpty }
}

/// Flatten canonical transcript messages into one entry per part.
public func transcriptFromMessages(_ messages: [ChatMessage]) -> [ChatTranscriptEntry] {
    var out: [ChatTranscriptEntry] = []
    for message in messages {
        for (index, part) in message.parts.enumerated() {
            out.append(
                ChatTranscriptEntry(
                    id: "\(message.id):\(index)",
                    messageId: message.id,
                    role: message.role,
                    turnId: message.turnId ?? "",
                    createdAt: message.createdAt,
                    part: part
                )
            )
        }
    }
    return out
}

/// Merge proposals into a list: newest-or-equal revision wins, no duplicates.
public func mergeProposals(_ list: [ChatProposalEntry], incoming: [ChatProposal]) -> [ChatProposalEntry] {
    mergeProposalEntries(list, incoming: incoming.map { ChatProposalEntry(proposal: $0, error: nil) })
}

/// Merge proposal entries (preserving incoming client error when provided).
public func mergeProposalEntries(_ list: [ChatProposalEntry], incoming: [ChatProposalEntry]) -> [ChatProposalEntry] {
    var next = list
    for entry in incoming where !entry.id.isEmpty {
        if let idx = next.firstIndex(where: { $0.id == entry.id }) {
            if entry.revision < next[idx].revision {
                continue
            }
            next[idx] = entry
        } else {
            next.append(entry)
        }
    }
    return next
}

/// The best MIME guess for an untyped file name, or `""` when unknown.
public func inferAttachmentMime(_ name: String?) -> String {
    guard let name, !name.isEmpty else { return "" }
    let clean = name.lowercased().split(whereSeparator: { $0 == "/" || $0 == "\\" }).last.map(String.init) ?? ""
    guard let dot = clean.lastIndex(of: ".") else { return "" }
    let ext = String(clean[clean.index(after: dot)...])
    return mimeByExtension[ext] ?? ""
}

private let accountKeyAllowed: CharacterSet = {
    var set = CharacterSet()
    set.insert(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()")
    return set
}()

/// Percent-encoded account key matching `accountKeyOf` in `controller.mjs`.
/// Returns `nil` when `storageKey` is `nil`, throws `ChatValidationError` if any field is empty.
public func accountKeyOf(_ storageKey: ChatAccountKey?) throws -> String? {
    guard let key = storageKey else { return nil }
    let fields: [(String, String)] = [
        ("appUrl", key.appUrl),
        ("mount", key.mount),
        ("authCollectionId", key.authCollectionId),
        ("userId", key.userId),
    ]
    for (name, value) in fields where value.isEmpty {
        throw ChatValidationError("storageKey.\(name) is required")
    }
    return fields
        .map { $0.1.addingPercentEncoding(withAllowedCharacters: accountKeyAllowed) ?? $0.1 }
        .joined(separator: "|")
}

private func parseISO8601Date(_ value: String) -> Date? {
    if value.isEmpty { return nil }
    let withFrac = ISO8601DateFormatter()
    withFrac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let d = withFrac.date(from: value) { return d }
    let plain = ISO8601DateFormatter()
    plain.formatOptions = [.withInternetDateTime]
    return plain.date(from: value)
}

private func isOlderConversation(_ a: ChatConversation, _ b: ChatConversation) -> Bool? {
    guard let ta = parseISO8601Date(a.createdAt), let tb = parseISO8601Date(b.createdAt) else {
        return nil
    }
    if ta != tb { return ta < tb }
    return a.id < b.id
}

/// Merge a fresh first conversation page into the loaded list (ported from `mergeConversationPage`).
public func mergeConversationPage(_ loaded: [ChatConversation], page: ChatConversationList) -> ChatConversationMerge {
    let items = page.items.filter { !$0.id.isEmpty }
    let pageCursor = page.nextCursor
    let fresh = Set(items.map(\.id))

    if pageCursor.isEmpty {
        return ChatConversationMerge(
            conversations: items,
            nextCursor: "",
            complete: true,
            covers: { _ in true }
        )
    }

    let last = items.last
    let lastIndex = last.flatMap { lastItem in loaded.firstIndex(where: { $0.id == lastItem.id }) } ?? -1

    func beyond(_ row: ChatConversation, index: Int) -> Bool {
        guard let last else { return false }
        if let older = isOlderConversation(row, last) {
            return older
        }
        return lastIndex >= 0 && index > lastIndex
    }

    var kept: [ChatConversation] = []
    for (index, row) in loaded.enumerated() {
        if !fresh.contains(row.id) && beyond(row, index: index) {
            kept.append(row)
        }
    }
    let keptIds = Set(kept.map(\.id))
    return ChatConversationMerge(
        conversations: items + kept,
        nextCursor: kept.isEmpty ? pageCursor : nil,
        complete: false,
        covers: { id in fresh.contains(id) || !keptIds.contains(id) }
    )
}

// MARK: - In-process same-account sync bus (Swift equivalent of BroadcastChannel)

@MainActor
final class ChatSyncBus {
    static let shared = ChatSyncBus()

    private struct Subscriber {
        let senderId: String
        let handler: () -> Void
    }

    private var channels: [String: [UUID: Subscriber]] = [:]

    func subscribe(accountKey: String, senderId: String, handler: @escaping () -> Void) -> UUID {
        let id = UUID()
        channels[accountKey, default: [:]][id] = Subscriber(senderId: senderId, handler: handler)
        return id
    }

    func unsubscribe(accountKey: String, token: UUID) {
        channels[accountKey]?[token] = nil
        if channels[accountKey]?.isEmpty == true {
            channels[accountKey] = nil
        }
    }

    func announce(accountKey: String, from senderId: String) {
        guard let subs = channels[accountKey] else { return }
        for sub in subs.values where sub.senderId != senderId {
            sub.handler()
        }
    }
}

// MARK: - ChatController

/// Framework-neutral `@MainActor @Observable` chat controller, ported from `controller.mjs`.
///
/// Guarantees:
/// - No token in URLs (delegated to `ChatClient`).
/// - Idempotent send (retries network failures up to 3 times with linear backoff and the same `Idempotency-Key`).
/// - Reconnect without replaying turns (resumes from `lastSeq` with idle timeout and exponential backoff).
/// - Explicit resync on `ResyncRequiredError` or reducer error (reloads turn + conversation, then polls).
/// - Account switch clears state synchronously and ignores late responses from the previous account.
/// - Nothing is persisted; server history is canonical.
/// - Same-account `ChatSyncBus` syncs controllers in the same process; `sync.focus` listens to app activation notifications.
@MainActor
@Observable
public final class ChatController {
    public static let defaultIdleTimeoutMs = 45_000
    public static let defaultSyncIntervalMs = 60_000
    private static let syncDebounceMs = 250
    private static let contextPollMs = 3_000
    private static let contextPollAttempts = 20

    /// The current immutable state snapshot.
    public private(set) var state: ChatControllerState

    /// The percent-encoded account key, or `nil` when no storage key was given.
    public private(set) var accountKey: String?

    /// The host editor adapter, or `nil`.
    public let editor: (any ChatEditorAdapter)?

    /// The underlying transport (accessible to `PBMCPChatUI` for attachment downloads).
    public let client: any ChatTransport

    private let reconnect: ChatReconnectOptions
    private let idleTimeoutMs: Int
    private let syncOptions: ChatSyncOptions?
    private let contextProvider: (@Sendable () async throws -> [String: JSONValue]?)?

    private var storageKey: ChatAccountKey?
    private var epoch: Int = 0
    private var openToken: Int = 0
    private var listToken: Int = 0
    private var streamToken: Int = 0
    private var refreshToken: Int = 0
    private var listVersion: Int = 0
    private var sendsInFlight: Int = 0
    private var disposed: Bool = false

    private var activeCount: Int = 0
    private var idleWaiters: [CheckedContinuation<Void, Never>] = []
    private var initPendingTask: Task<ChatControllerState, Never>?

    private var streamTask: Task<Void, Never>?
    private var compactionTask: Task<Void, Never>?
    private var compactionWatchConversationId: String?

    private let syncSenderId: String = newIdempotencyKey()
    private var syncBusAccountKey: String?
    private var syncBusToken: UUID?
    private var syncDebounceTask: Task<Void, Never>?
    private var syncIntervalTask: Task<Void, Never>?
    private var focusObserverTokens: [NSObjectProtocol] = []

    /// Optional callback invoked on every state change (in addition to `@Observable` tracking).
    public var onChange: ((ChatControllerState) -> Void)?

    public init(
        client: any ChatTransport,
        storageKey: ChatAccountKey? = nil,
        reconnect: ChatReconnectOptions = ChatReconnectOptions(),
        idleTimeoutMs: Int = ChatController.defaultIdleTimeoutMs,
        editor: (any ChatEditorAdapter)? = nil,
        sync: ChatSyncOptions? = ChatSyncOptions(),
        context: (@Sendable () async throws -> [String: JSONValue]?)? = nil
    ) throws {
        self.client = client
        self.reconnect = reconnect
        self.idleTimeoutMs = idleTimeoutMs
        self.editor = editor
        self.syncOptions = sync
        self.contextProvider = context
        self.storageKey = storageKey
        self.accountKey = try accountKeyOf(storageKey)
        self.state = ChatControllerState()

        startSync()
    }

    // MARK: - Internal plumbing

    private func updateState(_ mutate: (inout ChatControllerState) -> Void) {
        guard !disposed else { return }
        var next = state
        let oldMessages = next.messages
        mutate(&next)
        if next.messages != oldMessages {
            next.transcript = transcriptFromMessages(next.messages)
        }
        state = next
        onChange?(next)
    }

    private func isAlive(epoch: Int, token: Int? = nil) -> Bool {
        guard !disposed, epoch == self.epoch else { return false }
        if let token { return token == self.streamToken }
        return true
    }

    private func isCurrent(epoch: Int) -> Bool {
        !disposed && epoch == self.epoch
    }

    private func backoffMs(attempt: Int) -> Int {
        let exponent = max(0, attempt - 1)
        let multiplier = exponent >= 20 ? 1_048_576 : (1 << exponent)
        return min(reconnect.maxDelayMs, reconnect.initialDelayMs * multiplier)
    }

    private func errorOf(_ error: Error) -> ChatTurnFailure {
        if let api = error as? ChatApiError {
            return ChatTurnFailure(code: api.code, message: api.message)
        }
        if let resync = error as? ResyncRequiredError {
            return ChatTurnFailure(code: resync.code, message: resync.message)
        }
        if let net = error as? ChatNetworkError {
            return ChatTurnFailure(code: "error", message: net.message)
        }
        if let proto = error as? ChatProtocolError {
            return ChatTurnFailure(code: proto.code, message: proto.message)
        }
        if let sse = error as? SSEError {
            return ChatTurnFailure(code: sse.code, message: sse.message)
        }
        if let reducer = error as? ReducerError {
            return ChatTurnFailure(code: reducer.code, message: reducer.message)
        }
        let desc = error.localizedDescription
        return ChatTurnFailure(code: "error", message: desc.isEmpty ? "Something went wrong." : desc)
    }

    private func isResyncError(_ error: Error) -> Bool {
        error is ResyncRequiredError
            || error is ChatProtocolError
            || error is ReducerError
    }

    private func fail(_ error: Error) {
        updateState {
            $0.error = errorOf(error)
            $0.running = false
        }
    }

    private func beginActivity() {
        activeCount += 1
    }

    private func endActivity() {
        activeCount = max(0, activeCount - 1)
        if activeCount == 0 && !idleWaiters.isEmpty {
            let waiters = idleWaiters
            idleWaiters.removeAll()
            for waiter in waiters {
                waiter.resume()
            }
        }
    }

    private func track<T>(_ body: () async throws -> T) async rethrows -> T {
        beginActivity()
        defer { endActivity() }
        return try await body()
    }

    /// Resolves when no request, stream, reconnect, or poll is in flight.
    public func whenSettled() async {
        if activeCount == 0 { return }
        await withCheckedContinuation { continuation in
            if activeCount == 0 {
                continuation.resume()
            } else {
                idleWaiters.append(continuation)
            }
        }
    }

    // MARK: - List sync

    private func startSync() {
        stopSync()
        guard let sync = syncOptions, !disposed else { return }

        if sync.channel, let key = accountKey {
            let token = ChatSyncBus.shared.subscribe(accountKey: key, senderId: syncSenderId) { [weak self] in
                Task { @MainActor [weak self] in
                    self?.scheduleRefresh()
                }
            }
            syncBusAccountKey = key
            syncBusToken = token
        }

        if sync.focus {
            let names: [Notification.Name] = [
                Notification.Name("NSApplicationDidBecomeActiveNotification"),
                Notification.Name("UIApplicationDidBecomeActiveNotification"),
            ]
            for name in names {
                let observer = NotificationCenter.default.addObserver(
                    forName: name,
                    object: nil,
                    queue: .main
                ) { [weak self] _ in
                    Task { @MainActor [weak self] in
                        self?.scheduleRefresh()
                    }
                }
                focusObserverTokens.append(observer)
            }
        }

        if sync.intervalMs > 0 {
            let intervalNs = UInt64(sync.intervalMs) * 1_000_000
            syncIntervalTask = Task { [weak self] in
                while !Task.isCancelled {
                    try? await Task.sleep(nanoseconds: intervalNs)
                    guard !Task.isCancelled else { break }
                    self?.scheduleRefresh()
                }
            }
        }
    }

    private func stopSync() {
        if let key = syncBusAccountKey, let token = syncBusToken {
            ChatSyncBus.shared.unsubscribe(accountKey: key, token: token)
        }
        syncBusAccountKey = nil
        syncBusToken = nil
        for observer in focusObserverTokens {
            NotificationCenter.default.removeObserver(observer)
        }
        focusObserverTokens.removeAll()
        syncDebounceTask?.cancel()
        syncDebounceTask = nil
        syncIntervalTask?.cancel()
        syncIntervalTask = nil
    }

    private func scheduleRefresh() {
        guard !disposed, syncDebounceTask == nil else { return }
        let debounceNs = UInt64(Self.syncDebounceMs) * 1_000_000
        syncDebounceTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: debounceNs)
            guard !Task.isCancelled, let self else { return }
            self.syncDebounceTask = nil
            _ = await self.refreshConversations()
        }
    }

    private func announce() {
        guard let key = syncBusAccountKey else { return }
        ChatSyncBus.shared.announce(accountKey: key, from: syncSenderId)
    }

    private func listChanged() {
        listVersion += 1
        announce()
    }

    // MARK: - Stream lifecycle

    private func stopStream() {
        streamToken += 1
        streamTask?.cancel()
        streamTask = nil
    }

    private func stopCompactionWatch() {
        compactionTask?.cancel()
        compactionTask = nil
        compactionWatchConversationId = nil
    }

    private func startTurnStream(turnId: String, after: Int, openToken: Int) {
        stopStream()
        let token = streamToken
        let currentEpoch = epoch
        beginActivity()
        streamTask = Task { [weak self] in
            guard let self else { return }
            await self.runTurnStream(turnId: turnId, after: after, epoch: currentEpoch, token: token, openToken: openToken)
            self.endActivity()
        }
    }

    private func runTurnStream(turnId: String, after: Int, epoch: Int, token: Int, openToken: Int) async {
        var lastSeq = after
        var attempt = 0

        while isAlive(epoch: epoch, token: token) && !Task.isCancelled {
            var received = false
            var result: StreamTurnResult?
            var failure: Error?
            var idleFired = false

            let idleFlag = LockedFlag()
            let seqBox = LockedInt(lastSeq)
            let receivedFlag = LockedFlag()

            let timerBox = IdleTimerBox(timeoutMs: idleTimeoutMs)
            let onEventHandler: @Sendable (ChatEvent) async throws -> Void = { [weak self] event in
                receivedFlag.set(true)
                seqBox.set(event.seq)
                timerBox.reset()
                guard let self else { return }
                try await MainActor.run {
                    guard self.isAlive(epoch: epoch, token: token) else { return }
                    try self.applyStreamEvent(turnId: turnId, event: event)
                }
            }

            let streamAttemptTask = Task<StreamTurnResult, Error> { [client] in
                let inner = Task<StreamTurnResult, Error> {
                    try await client.streamTurn(
                        turnId,
                        after: seqBox.value,
                        onEvent: onEventHandler,
                        onHeartbeat: { _ in
                            timerBox.reset()
                        }
                    )
                }
                timerBox.arm {
                    idleFlag.set(true)
                    inner.cancel()
                }
                return try await withTaskCancellationHandler {
                    defer { timerBox.cancel() }
                    return try await inner.value
                } onCancel: {
                    timerBox.cancel()
                    inner.cancel()
                }
            }

            do {
                result = try await withTaskCancellationHandler {
                    try await streamAttemptTask.value
                } onCancel: {
                    streamAttemptTask.cancel()
                }
            } catch {
                failure = error
            }

            received = receivedFlag.value
            lastSeq = max(lastSeq, seqBox.value)
            idleFired = idleFlag.value

            guard isAlive(epoch: epoch, token: token) else { return }

            if let failure {
                if idleFired {
                    // Fall through to reconnect below.
                } else if isResyncError(failure) {
                    await resync(turnId: turnId, epoch: epoch, token: token, openToken: openToken)
                    return
                } else if failure is CancellationError {
                    return
                } else if failure is ChatApiError {
                    fail(failure)
                    return
                }
                // Network error or idle timeout -> reconnect.
            } else if let result {
                lastSeq = max(lastSeq, result.lastSeq)
                if result.terminal {
                    updateState { $0.running = false }
                    await reloadTranscript(conversationId: state.activeConversationId, epoch: epoch, openToken: openToken)
                    return
                }
            }

            if received { attempt = 0 }
            attempt += 1
            if let maxAttempts = reconnect.maxAttempts, attempt > maxAttempts {
                fail(ChatNetworkError(message: "The event stream could not be reconnected."))
                return
            }

            let delayNs = UInt64(max(0, backoffMs(attempt: attempt))) * 1_000_000
            do {
                try await Task.sleep(nanoseconds: delayNs)
            } catch {
                return
            }
        }
    }

    private func applyStreamEvent(turnId: String, event: ChatEvent) throws {
        let targetTurnId = event.turnId.isEmpty ? turnId : event.turnId
        let view: TurnView
        if let current = state.turn, current.turnId == targetTurnId {
            view = current
        } else {
            view = ChatReducer.createTurnView(targetTurnId)
        }
        let next = try ChatReducer.applyEvent(view, event)
        if next == view { return }

        let proposalsChanged = next.proposals != view.proposals
        updateState {
            $0.turn = next
            $0.running = !ChatReducer.isTerminalState(next.state)
            $0.error = nil
            if proposalsChanged {
                $0.proposals = mergeProposals($0.proposals, incoming: next.proposals)
            }
        }
    }

    private func reloadTranscript(conversationId: String?, epoch: Int, openToken: Int) async {
        guard let conversationId, !conversationId.isEmpty else { return }
        guard isAlive(epoch: epoch, token: streamToken),
              openToken == self.openToken,
              state.activeConversationId == conversationId else { return }

        do {
            let detail = try await client.getConversation(conversationId)
            guard isAlive(epoch: epoch, token: streamToken),
                  openToken == self.openToken,
                  state.activeConversationId == conversationId else { return }

            var shouldWatchCompaction = false
            updateState {
                $0.messages = detail.messages
                if let serverProposals = detail.proposals {
                    $0.proposals = mergeServerProposals(existing: $0.proposals, incoming: serverProposals)
                }
                if let decided = detail.decidedProposals {
                    $0.decidedProposals = decided.filter { !$0.id.isEmpty }
                }
                if detail.hasContextField {
                    $0.context = detail.context
                }
                shouldWatchCompaction = $0.context?.compacting == true
            }
            if shouldWatchCompaction {
                watchCompaction(conversationId: conversationId)
            }
            announce()
            // Track synchronously before returning so `whenSettled()` waits for the post-turn list refresh.
            beginActivity()
            Task { @MainActor [weak self] in
                defer { self?.endActivity() }
                _ = await self?.refreshConversations()
            }
        } catch is CancellationError {
            return
        } catch {
            guard isCurrent(epoch: epoch) else { return }
            updateState { $0.error = errorOf(error) }
        }
    }

    private func mergeServerProposals(existing: [ChatProposalEntry], incoming: [ChatProposal]) -> [ChatProposalEntry] {
        let ids = Set(incoming.map(\.id))
        let base = existing.filter { ids.contains($0.id) || ChatReducer.isTerminalProposalState($0.state) }
        return mergeProposals(base, incoming: incoming)
    }

    private func resync(turnId: String, epoch: Int, token: Int, openToken: Int) async {
        do {
            let turn = try await client.getTurn(turnId)
            guard isAlive(epoch: epoch, token: token) else { return }

            let conversationId = turn.conversationId.isEmpty ? (state.activeConversationId ?? "") : turn.conversationId
            guard !conversationId.isEmpty else { return }
            if let active = state.activeConversationId, active != conversationId { return }

            let detail = try await client.getConversation(conversationId)
            guard isAlive(epoch: epoch, token: token) else { return }

            let terminal = ChatReducer.isTerminalState(turn.state)
            updateState {
                $0.activeConversationId = conversationId
                $0.messages = detail.messages
                $0.turn = ChatReducer.viewFromTranscript(turn, messages: detail.messages)
                $0.running = !terminal
                $0.error = nil
                if let serverProposals = detail.proposals {
                    $0.proposals = mergeServerProposals(existing: $0.proposals, incoming: serverProposals)
                }
                if let decided = detail.decidedProposals {
                    $0.decidedProposals = decided.filter { !$0.id.isEmpty }
                }
            }
            if terminal { return }

            var attempt = 0
            while isAlive(epoch: epoch, token: token) && !Task.isCancelled {
                attempt += 1
                if let maxAttempts = reconnect.maxAttempts, attempt > maxAttempts {
                    fail(ChatNetworkError(message: "The turn could not be refreshed."))
                    return
                }
                let delayNs = UInt64(max(0, backoffMs(attempt: attempt))) * 1_000_000
                do {
                    try await Task.sleep(nanoseconds: delayNs)
                } catch {
                    return
                }
                guard isAlive(epoch: epoch, token: token) else { return }

                let latest: ChatTurn
                do {
                    latest = try await client.getTurn(turnId)
                } catch is CancellationError {
                    return
                } catch let api as ChatApiError where api.code != "internal" {
                    guard isAlive(epoch: epoch, token: token) else { return }
                    fail(api)
                    return
                } catch {
                    guard isAlive(epoch: epoch, token: token) else { return }
                    continue
                }
                guard isAlive(epoch: epoch, token: token) else { return }

                let done = ChatReducer.isTerminalState(latest.state)
                updateState {
                    var view = $0.turn ?? ChatReducer.createTurnView(turnId)
                    view.state = latest.state
                    view.error = latest.error
                    $0.turn = view
                    $0.running = !done
                    $0.error = nil
                }
                if done {
                    await reloadTranscript(conversationId: conversationId, epoch: epoch, openToken: openToken)
                    return
                }
            }
        } catch is CancellationError {
            return
        } catch {
            guard isAlive(epoch: epoch, token: token) else { return }
            fail(error)
        }
    }

    // MARK: - Lifecycle

    /// Load `/status` and the first conversation page (port of `controller.init()`).
    @discardableResult
    public func initialize() async -> ChatControllerState {
        let task = Task<ChatControllerState, Never> { @MainActor in
            await self.track {
                await self.performInit()
            }
        }
        initPendingTask = task
        let result = await task.value
        if initPendingTask == task {
            initPendingTask = nil
        }
        return result
    }

    private func performInit() async -> ChatControllerState {
        guard !disposed else { return state }
        let currentEpoch = epoch
        updateState {
            $0.status = .loading
            $0.error = nil
        }
        do {
            async let statusTask = client.status()
            async let pageTask = client.listConversations()
            let (statusResp, pageResp) = try await (statusTask, pageTask)
            guard isCurrent(epoch: currentEpoch) else { return state }

            let choices = (statusResp.choices ?? []).filter { !$0.id.isEmpty }
            let serverDefault = statusResp.defaultChoice ?? ""
            let defaultChoice: String
            if choices.contains(where: { $0.id == serverDefault }) {
                defaultChoice = serverDefault
            } else {
                defaultChoice = choices.first(where: { $0.isDefault == true })?.id ?? ""
            }
            let currentChoice = choices.contains(where: { $0.id == state.choice })
                ? state.choice
                : defaultChoice

            updateState {
                $0.status = .ready
                $0.models = statusResp.models
                $0.choices = choices
                $0.defaultChoice = defaultChoice
                $0.choice = currentChoice
                $0.features = statusResp.features
                $0.limits = statusResp.limits
                $0.conversations = pageResp.items
                $0.nextCursor = pageResp.nextCursor
                $0.error = nil
            }
        } catch is CancellationError {
            return state
        } catch {
            guard isCurrent(epoch: currentEpoch) else { return state }
            updateState {
                $0.status = .error
                $0.error = errorOf(error)
            }
        }
        return state
    }

    /// Change the signed-in account. Different key: abort, clear state synchronously, and re-init.
    @discardableResult
    public func setAccount(_ storageKey: ChatAccountKey?) async throws -> ChatControllerState {
        let nextAccountKey = try accountKeyOf(storageKey)
        if nextAccountKey == accountKey {
            self.storageKey = storageKey
            return state
        }
        guard !disposed else { return state }

        self.storageKey = storageKey
        self.accountKey = nextAccountKey
        epoch += 1
        openToken += 1
        listToken += 1
        stopStream()
        stopCompactionWatch()
        initPendingTask?.cancel()
        initPendingTask = nil
        sendsInFlight = 0

        // Clear synchronously before any new request.
        state = ChatControllerState()
        onChange?(state)
        startSync()

        guard nextAccountKey != nil else { return state }
        return await initialize()
    }

    /// Abort every stream and timer; the controller stops updating afterwards.
    public func dispose() {
        guard !disposed else { return }
        stopSync()
        disposed = true
        epoch += 1
        openToken += 1
        listToken += 1
        stopStream()
        stopCompactionWatch()
        initPendingTask?.cancel()
        initPendingTask = nil
        activeCount = 0
        if !idleWaiters.isEmpty {
            let waiters = idleWaiters
            idleWaiters.removeAll()
            for waiter in waiters {
                waiter.resume()
            }
        }
    }

    // MARK: - Conversations

    /// Append the next conversation page.
    @discardableResult
    public func loadMore() async -> ChatControllerState {
        await track {
            await performLoadMore()
        }
    }

    private func performLoadMore() async -> ChatControllerState {
        let cursor = state.nextCursor
        guard !cursor.isEmpty, !disposed else { return state }
        let currentEpoch = epoch
        listToken += 1
        let token = listToken
        do {
            let page = try await client.listConversations(cursor: cursor)
            guard isCurrent(epoch: currentEpoch), token == listToken else { return state }
            let known = Set(state.conversations.map(\.id))
            updateState {
                $0.conversations = $0.conversations + page.items.filter { !known.contains($0.id) }
                $0.nextCursor = page.nextCursor
            }
        } catch is CancellationError {
            return state
        } catch {
            guard isCurrent(epoch: currentEpoch), token == listToken else { return state }
            updateState { $0.error = errorOf(error) }
        }
        return state
    }

    /// Re-read the first conversation page and merge it into the list.
    @discardableResult
    public func refreshConversations() async -> ChatControllerState {
        await track {
            await performRefreshConversations(retry: 0)
        }
    }

    private func performRefreshConversations(retry: Int) async -> ChatControllerState {
        guard !disposed, state.status == .ready else { return state }
        let currentEpoch = epoch
        refreshToken += 1
        let token = refreshToken
        let version = listVersion

        let page: ChatConversationList
        do {
            page = try await client.listConversations()
        } catch {
            // Background refresh never surfaces an error.
            return state
        }
        guard isCurrent(epoch: currentEpoch), token == refreshToken else { return state }
        if version != listVersion {
            return retry < 2 ? await performRefreshConversations(retry: retry + 1) : state
        }

        let merged = mergeConversationPage(state.conversations, page: page)
        var nextConversations = merged.conversations
        let nextCursor = merged.nextCursor ?? state.nextCursor

        let activeId = state.activeConversationId
        let activeRow = activeId.flatMap { id in state.conversations.first(where: { $0.id == id }) }
        var gone = false
        if let activeId, let activeRow,
           merged.covers(activeId),
           !nextConversations.contains(where: { $0.id == activeId }) {
            do {
                _ = try await client.getConversation(activeId, limit: 1)
            } catch let api as ChatApiError where api.status == 404 {
                gone = true
            } catch {
                gone = false
            }
            guard isCurrent(epoch: currentEpoch), token == refreshToken else { return state }
            guard state.activeConversationId == activeId else { return state }
            if !gone {
                nextConversations = [activeRow] + nextConversations
            }
        }

        if gone {
            stopStream()
            stopCompactionWatch()
            openToken += 1
            updateState {
                $0.conversations = nextConversations
                $0.nextCursor = nextCursor
                $0.activeConversationId = nil
                $0.messages = []
                $0.proposals = []
                $0.decidedProposals = []
                $0.pendingUploads = []
                $0.turn = nil
                $0.running = false
            }
        } else if nextCursor != state.nextCursor || nextConversations != state.conversations {
            updateState {
                $0.conversations = nextConversations
                $0.nextCursor = nextCursor
            }
        }
        return state
    }

    /// Leave the open conversation without creating one; the next `send()` creates it.
    @discardableResult
    public func startDraft() -> ChatControllerState {
        guard !disposed else { return state }
        stopStream()
        stopCompactionWatch()
        openToken += 1
        updateState {
            $0.activeConversationId = nil
            $0.messages = []
            $0.proposals = []
            $0.decidedProposals = []
            $0.pendingUploads = []
            $0.turn = nil
            $0.running = false
            $0.error = nil
            $0.context = nil
        }
        return state
    }

    /// Open a conversation and attach to its active turn if any.
    @discardableResult
    public func open(_ conversationId: String) async -> ChatControllerState {
        await track {
            await performOpen(conversationId)
        }
    }

    private func performOpen(_ conversationId: String) async -> ChatControllerState {
        guard !disposed else { return state }
        let currentEpoch = epoch
        openToken += 1
        let token = openToken
        stopStream()
        stopCompactionWatch()
        updateState {
            $0.activeConversationId = conversationId
            $0.messages = []
            $0.proposals = []
            $0.decidedProposals = []
            $0.pendingUploads = []
            $0.turn = nil
            $0.running = false
            $0.error = nil
            $0.context = nil
        }

        do {
            let detail = try await client.getConversation(conversationId)
            guard isCurrent(epoch: currentEpoch), token == openToken else { return state }

            var conversations = state.conversations
            if let conv = detail.conversation, !conversations.contains(where: { $0.id == conv.id }) {
                conversations.insert(conv, at: 0)
            }
            let proposals = mergeProposals([], incoming: detail.proposals ?? [])
            let decided = (detail.decidedProposals ?? []).filter { !$0.id.isEmpty }

            if let active = detail.activeTurn, !active.id.isEmpty {
                var view = ChatReducer.createTurnView(active.id)
                view.state = active.state
                view.error = active.error
                updateState {
                    $0.conversations = conversations
                    $0.messages = detail.messages
                    $0.proposals = proposals
                    $0.decidedProposals = decided
                    $0.turn = view
                    $0.running = !ChatReducer.isTerminalState(active.state)
                    $0.context = detail.context
                }
                startTurnStream(turnId: active.id, after: 0, openToken: token)
            } else {
                updateState {
                    $0.conversations = conversations
                    $0.messages = detail.messages
                    $0.proposals = proposals
                    $0.decidedProposals = decided
                    $0.turn = nil
                    $0.running = false
                    $0.context = detail.context
                }
            }
        } catch is CancellationError {
            return state
        } catch {
            guard isCurrent(epoch: currentEpoch), token == openToken else { return state }
            updateState {
                $0.error = errorOf(error)
                $0.running = false
            }
        }
        return state
    }

    /// Create a conversation and make it active.
    @discardableResult
    public func newConversation(title: String? = nil) async -> ChatConversation? {
        await track {
            await performNewConversation(title: title)
        }
    }

    private func performNewConversation(title: String?) async -> ChatConversation? {
        guard !disposed else { return nil }
        let currentEpoch = epoch
        do {
            let conversation = try await client.createConversation(title: title)
            guard isCurrent(epoch: currentEpoch) else { return nil }
            stopStream()
            stopCompactionWatch()
            openToken += 1
            updateState {
                $0.conversations = [conversation] + $0.conversations.filter { $0.id != conversation.id }
                $0.activeConversationId = conversation.id
                $0.messages = []
                $0.proposals = []
                $0.decidedProposals = []
                $0.pendingUploads = []
                $0.turn = nil
                $0.running = false
                $0.error = nil
                $0.context = nil
            }
            listChanged()
            return conversation
        } catch is CancellationError {
            return nil
        } catch {
            guard isCurrent(epoch: currentEpoch) else { return nil }
            updateState { $0.error = errorOf(error) }
            return nil
        }
    }

    /// Re-read how full the open conversation's context is.
    @discardableResult
    public func refreshContext() async -> ChatControllerContext? {
        await track {
            await performRefreshContext(conversationId: state.activeConversationId)
        }
    }

    private func performRefreshContext(conversationId: String?) async -> ChatControllerContext? {
        guard !disposed, let conversationId, !conversationId.isEmpty else { return state.context }
        let currentEpoch = epoch
        let currentOpenToken = openToken
        do {
            let context = try await client.getContext(conversationId)
            guard isCurrent(epoch: currentEpoch),
                  currentOpenToken == openToken,
                  state.activeConversationId == conversationId else {
                return state.context
            }
            updateState { $0.context = context }
            return context
        } catch {
            // Advisory: leave the last value.
            return state.context
        }
    }

    private func watchCompaction(conversationId: String) {
        if compactionWatchConversationId == conversationId { return }
        stopCompactionWatch()
        compactionWatchConversationId = conversationId
        let currentOpenToken = openToken
        beginActivity()
        compactionTask = Task { [weak self] in
            defer {
                self?.endActivity()
            }
            guard let self else { return }
            defer {
                if self.compactionWatchConversationId == conversationId {
                    self.compactionWatchConversationId = nil
                }
            }
            for _ in 0..<Self.contextPollAttempts {
                do {
                    try await Task.sleep(nanoseconds: UInt64(Self.contextPollMs) * 1_000_000)
                } catch {
                    return
                }
                guard currentOpenToken == self.openToken,
                      self.state.activeConversationId == conversationId else { return }
                let context = await self.performRefreshContext(conversationId: conversationId)
                if context?.compacting != true { return }
            }
        }
    }

    /// Summarize the open conversation's older messages now.
    @discardableResult
    public func compact() async -> ChatCompactResult? {
        await track {
            await performCompact(conversationId: state.activeConversationId)
        }
    }

    private func performCompact(conversationId: String?) async -> ChatCompactResult? {
        guard !disposed, let conversationId, !conversationId.isEmpty else { return nil }
        let currentEpoch = epoch
        let currentOpenToken = openToken
        let before = state.context

        var optimistic = before ?? ChatContextStatus()
        optimistic.compacting = true
        updateState {
            $0.context = optimistic
            $0.error = nil
        }

        do {
            let result = try await client.compactConversation(conversationId)
            guard isCurrent(epoch: currentEpoch),
                  currentOpenToken == openToken,
                  state.activeConversationId == conversationId else {
                return result
            }
            var fallback = before ?? ChatContextStatus()
            fallback.compacting = false
            updateState { $0.context = result.context ?? fallback }
            return result
        } catch is CancellationError {
            return nil
        } catch {
            guard isCurrent(epoch: currentEpoch), currentOpenToken == openToken else { return nil }
            if state.activeConversationId == conversationId {
                var restored = before
                restored?.compacting = false
                updateState {
                    $0.context = restored
                    $0.error = errorOf(error)
                }
            }
            return nil
        }
    }

    /// Rename a conversation.
    @discardableResult
    public func rename(_ conversationId: String, title: String) async -> ChatConversation? {
        await track {
            await performRename(conversationId, title: title)
        }
    }

    private func performRename(_ conversationId: String, title: String) async -> ChatConversation? {
        guard !disposed else { return nil }
        let currentEpoch = epoch
        do {
            let updated = try await client.renameConversation(conversationId, title: title)
            guard isCurrent(epoch: currentEpoch) else { return nil }
            updateState {
                $0.conversations = $0.conversations.map { $0.id == updated.id ? updated : $0 }
                $0.error = nil
            }
            listChanged()
            return updated
        } catch is CancellationError {
            return nil
        } catch {
            guard isCurrent(epoch: currentEpoch) else { return nil }
            updateState { $0.error = errorOf(error) }
            return nil
        }
    }

    /// Delete a conversation; clears active state if it was open.
    @discardableResult
    public func remove(_ conversationId: String) async -> Bool {
        await track {
            await performRemove(conversationId)
        }
    }

    private func performRemove(_ conversationId: String) async -> Bool {
        guard !disposed else { return false }
        let currentEpoch = epoch
        if state.activeConversationId == conversationId {
            stopStream()
            stopCompactionWatch()
        }
        do {
            try await client.deleteConversation(conversationId)
            guard isCurrent(epoch: currentEpoch) else { return false }
            let clearActive = state.activeConversationId == conversationId
            if clearActive { openToken += 1 }
            updateState {
                $0.conversations = $0.conversations.filter { $0.id != conversationId }
                if clearActive {
                    $0.activeConversationId = nil
                    $0.messages = []
                    $0.proposals = []
                    $0.decidedProposals = []
                    $0.pendingUploads = []
                    $0.turn = nil
                    $0.running = false
                    $0.context = nil
                }
                $0.error = nil
            }
            listChanged()
            return true
        } catch is CancellationError {
            return false
        } catch {
            guard isCurrent(epoch: currentEpoch) else { return false }
            updateState { $0.error = errorOf(error) }
            return false
        }
    }

    // MARK: - Proposals

    /// Approve or reject a proposal at its current revision in state.
    @discardableResult
    public func decide(
        _ proposalId: String,
        _ decision: ProposalDecision,
        arguments editedArguments: JSONValue? = nil
    ) async throws -> ChatDecisionResult? {
        try await track {
            try await performDecide(proposalId, decision: decision, editedArguments: editedArguments)
        }
    }

    private func performDecide(
        _ proposalId: String,
        decision: ProposalDecision,
        editedArguments: JSONValue?
    ) async throws -> ChatDecisionResult? {
        guard !disposed else { return nil }
        let currentEpoch = epoch
        guard let proposal = state.proposals.first(where: { $0.id == proposalId }) else {
            throw ChatApiError(status: 404, code: "not_found", message: "The proposal is no longer available.")
        }
        if decision == .approve && proposal.location == .editor && editor == nil {
            let message = "Approving an editor change requires an editor adapter."
            setProposalError(proposalId, error: ChatTurnFailure(code: "editor_unavailable", message: message))
            throw ChatApiError(status: 400, code: "editor_unavailable", message: message)
        }

        let result: ProposalDecisionResult
        do {
            result = try await client.decideProposal(
                proposalId,
                decision: decision,
                revision: proposal.revision,
                arguments: editedArguments
            )
        } catch let api as ChatApiError {
            guard isCurrent(epoch: currentEpoch) else { return nil }
            if (api.status == 409 || (api.status == 422 && api.code == "refused")),
               let serverProposal = api.proposal {
                let failure = ChatTurnFailure(code: api.code, message: api.message)
                let adopted = adoptProposal(serverProposal, error: failure)
                return ChatDecisionResult(proposal: adopted, replayed: false, error: failure)
            }
            throw api
        }

        guard isCurrent(epoch: currentEpoch) else { return nil }
        let stored = adoptProposal(result.proposal)
        if decision == .approve && stored.location == .editor && stored.state == .approved {
            let final = try await applyEditorProposal(stored)
            return ChatDecisionResult(proposal: final, replayed: result.replayed, error: final.error)
        }
        return ChatDecisionResult(proposal: stored, replayed: result.replayed, error: nil)
    }

    /// Report what the host editor did with an approved editor proposal.
    @discardableResult
    public func reportOutcome(
        _ proposalId: String,
        status: ProposalOutcomeStatus,
        editorRevision: String? = nil,
        hunks: [ChatHunkOutcome]? = nil,
        message: String? = nil
    ) async throws -> ChatDecisionResult? {
        try await track {
            try await performReportOutcome(
                proposalId,
                status: status,
                editorRevision: editorRevision,
                hunks: hunks,
                message: message
            )
        }
    }

    private func performReportOutcome(
        _ proposalId: String,
        status: ProposalOutcomeStatus,
        editorRevision: String?,
        hunks: [ChatHunkOutcome]?,
        message: String?
    ) async throws -> ChatDecisionResult? {
        guard !disposed else { return nil }
        let currentEpoch = epoch
        guard let proposal = state.proposals.first(where: { $0.id == proposalId }) else {
            throw ChatApiError(status: 404, code: "not_found", message: "The proposal is no longer available.")
        }
        let result: ProposalDecisionResult
        do {
            result = try await client.reportOutcome(
                proposalId,
                revision: proposal.revision,
                status: status,
                editorRevision: editorRevision,
                hunks: hunks,
                message: message
            )
        } catch let api as ChatApiError {
            guard isCurrent(epoch: currentEpoch) else { return nil }
            if (api.status == 409 || (api.status == 422 && api.code == "refused")),
               let serverProposal = api.proposal {
                let failure = ChatTurnFailure(code: api.code, message: api.message)
                let adopted = adoptProposal(serverProposal, error: failure)
                return ChatDecisionResult(proposal: adopted, replayed: false, error: failure)
            }
            throw api
        }
        guard isCurrent(epoch: currentEpoch) else { return nil }
        let stored = adoptProposal(result.proposal)
        return ChatDecisionResult(proposal: stored, replayed: result.replayed, error: nil)
    }

    @discardableResult
    private func adoptProposal(_ proposal: ChatProposal, error: ChatTurnFailure? = nil) -> ChatProposalEntry {
        let entry = ChatProposalEntry(proposal: proposal, error: error)
        let merged = mergeProposalEntries(state.proposals, incoming: [entry])
        updateState { $0.proposals = merged }
        return merged.first(where: { $0.id == entry.id }) ?? entry
    }

    private func setProposalError(_ proposalId: String, error: ChatTurnFailure?) {
        updateState {
            $0.proposals = $0.proposals.map { item in
                guard item.id == proposalId else { return item }
                var copy = item
                copy.error = error
                return copy
            }
        }
    }

    private func applyEditorProposal(_ entry: ChatProposalEntry) async throws -> ChatProposalEntry {
        guard let editor else {
            let message = "Approving an editor change requires an editor adapter."
            setProposalError(entry.id, error: ChatTurnFailure(code: "editor_unavailable", message: message))
            throw ChatApiError(status: 400, code: "editor_unavailable", message: message)
        }

        do {
            let applicable = try await editor.validatePatch(entry.proposal)
            if !applicable {
                let rev = await editorRevision()
                return try await recordEditorOutcome(entry, status: .stale, editorRevision: rev, hunks: nil, message: nil)
            }
        } catch {
            return try await recordEditorOutcome(entry, status: .failed, editorRevision: nil, hunks: nil, message: errorOf(error).message)
        }

        let applied: ChatEditorApplyResult
        do {
            applied = try await editor.applyPatch(entry.proposal)
        } catch {
            return try await recordEditorOutcome(entry, status: .failed, editorRevision: nil, hunks: nil, message: errorOf(error).message)
        }

        if applied.revision.isEmpty {
            return try await recordEditorOutcome(
                entry,
                status: .failed,
                editorRevision: nil,
                hunks: applied.hunks,
                message: "The editor did not report a new revision."
            )
        }
        return try await recordEditorOutcome(
            entry,
            status: .applied,
            editorRevision: applied.revision,
            hunks: applied.hunks,
            message: nil
        )
    }

    private func recordEditorOutcome(
        _ entry: ChatProposalEntry,
        status: ProposalOutcomeStatus,
        editorRevision: String?,
        hunks: [ChatHunkOutcome]?,
        message: String?
    ) async throws -> ChatProposalEntry {
        do {
            let result = try await performReportOutcome(
                entry.id,
                status: status,
                editorRevision: editorRevision,
                hunks: hunks,
                message: message
            )
            return result?.proposal ?? entry
        } catch {
            setProposalError(entry.id, error: errorOf(error))
            throw error
        }
    }

    private func editorRevision() async -> String? {
        guard let editor else { return nil }
        do {
            let snap = try await editor.snapshot()
            if let rev = snap.revision, !rev.isEmpty { return rev }
        } catch {
            // Snapshot failure only means a stale report cannot name the revision.
        }
        return nil
    }

    // MARK: - Attachments

    /// Validate and upload files to the open conversation (creating one if none is open).
    @discardableResult
    public func attach(_ files: [ChatUploadFile], model: String? = nil) async throws -> [ChatUpload] {
        try await track {
            try await performAttach(files, model: model)
        }
    }

    private func performAttach(_ files: [ChatUploadFile], model: String?) async throws -> [ChatUpload] {
        guard !disposed, !files.isEmpty else { return [] }
        try requireUploads()
        let currentEpoch = epoch

        var conversationId = state.activeConversationId
        if conversationId == nil {
            guard let created = await performNewConversation(title: nil) else {
                throw ChatApiError(status: 400, code: "invalid_request", message: "A conversation is needed before attaching a file.")
            }
            conversationId = created.id
            guard isCurrent(epoch: currentEpoch) else { return [] }
        }
        guard let convId = conversationId else { return [] }

        let pendingCount = state.pendingUploads.filter { $0.conversationId == convId }.count
        let choice = (model?.isEmpty == false) ? resolveChoice(state.choices, requested: model) : nil
        try validateAttachments(files, alreadyPending: pendingCount, choice: choice)

        var uploads: [ChatUpload] = []
        for file in files {
            let upload = try await client.uploadFile(conversationId: convId, file: file, name: file.name)
            guard isCurrent(epoch: currentEpoch) else { return [] }
            guard !upload.id.isEmpty else {
                throw ChatApiError(status: 201, code: "invalid_response", message: "The server returned an invalid response.")
            }
            uploads.append(upload)
        }
        updateState {
            $0.pendingUploads += uploads
            $0.error = nil
        }
        return uploads
    }

    /// Forget every attachment that has not been sent yet.
    @discardableResult
    public func clearAttachments() -> ChatControllerState {
        updateState { $0.pendingUploads = [] }
        return state
    }

    private func requireUploads() throws {
        if let features = state.features, features.uploads == false {
            throw ChatApiError(status: 400, code: "invalid_request", message: "Attachments are not enabled on this server.")
        }
    }

    private func validateAttachments(_ files: [ChatUploadFile], alreadyPending: Int, choice: ChatModelChoice?) throws {
        try requireUploads()
        let limits = state.limits
        let maxBytes = (limits?.maxUploadBytes ?? 0) > 0 ? (limits?.maxUploadBytes ?? 0) : 0
        let maxCount = (limits?.maxAttachmentsPerTurn ?? 0) > 0 ? (limits?.maxAttachmentsPerTurn ?? 0) : 0
        let allowed = attachmentTypesFor(limits, choice: choice)?.map { $0.lowercased() }

        if maxCount > 0 && alreadyPending + files.count > maxCount {
            throw ChatApiError(
                status: 400,
                code: "invalid_attachment",
                message: "A message can carry at most \(maxCount) attachment(s)."
            )
        }

        for file in files {
            if maxBytes > 0 && file.size > maxBytes {
                throw ChatApiError(
                    status: 413,
                    code: "payload_too_large",
                    message: "An attachment may be at most \(maxBytes) bytes."
                )
            }
            let rawMime = file.mimeType.isEmpty ? inferAttachmentMime(file.name) : file.mimeType
            let mime = rawMime.lowercased()
            if let allowed, !mime.isEmpty, !allowed.contains(mime) {
                throw ChatApiError(
                    status: 415,
                    code: "unsupported_media",
                    message: "\(mime) files are not accepted."
                )
            }
        }
    }

    private func resolveAttachments(
        conversationId: String,
        attachments: [ChatAttachmentInput],
        epoch: Int,
        choice: ChatModelChoice?
    ) async throws -> [String]? {
        guard !attachments.isEmpty else { return [] }
        try requireUploads()
        var files: [ChatUploadFile] = []
        var existingCount = 0
        for item in attachments {
            switch item {
            case .uploadId: existingCount += 1
            case .file(let f): files.append(f)
            }
        }
        try validateAttachments(files, alreadyPending: existingCount, choice: choice)

        var uploaded: [ChatUpload] = []
        for file in files {
            let upload = try await client.uploadFile(conversationId: conversationId, file: file, name: file.name)
            guard isCurrent(epoch: epoch) else { return nil }
            guard !upload.id.isEmpty else {
                throw ChatApiError(status: 201, code: "invalid_response", message: "The server returned an invalid response.")
            }
            uploaded.append(upload)
        }
        if !uploaded.isEmpty {
            updateState { $0.pendingUploads += uploaded }
        }
        var uploadIndex = 0
        return attachments.map { item in
            switch item {
            case .uploadId(let id):
                return id
            case .file:
                let id = uploaded[uploadIndex].id
                uploadIndex += 1
                return id
            }
        }
    }

    // MARK: - Turns

    /// Read the page context once from the configured `context` provider.
    public func readContext() -> Task<[String: JSONValue]?, Error>? {
        guard let provider = contextProvider else { return nil }
        return Task {
            try await provider()
        }
    }

    /// Submit one message, then follow its turn stream in the background.
    @discardableResult
    public func send(
        _ text: String,
        model: String? = nil,
        provider: String? = nil,
        effort: String? = nil,
        attachments: [ChatAttachmentInput] = [],
        context explicitContext: [String: JSONValue]? = nil,
        contextTask explicitContextTask: Task<[String: JSONValue]?, Error>? = nil
    ) async throws -> ChatSubmitResult? {
        // Read page context synchronously when send() starts, before any upload or round trip.
        let wantedContext = state.features == nil || state.features?.context == true
        let contextTask: Task<[String: JSONValue]?, Error>?
        if let explicitContext {
            contextTask = Task { explicitContext }
        } else if let explicitContextTask {
            contextTask = explicitContextTask
        } else if wantedContext {
            contextTask = readContext()
        } else {
            contextTask = nil
        }

        let startEpoch = epoch
        sendsInFlight += 1
        if !state.sending {
            updateState { $0.sending = true }
        }
        defer {
            if startEpoch == epoch {
                sendsInFlight = max(0, sendsInFlight - 1)
                if sendsInFlight == 0 && state.sending {
                    updateState { $0.sending = false }
                }
            }
        }

        return try await track {
            try await performSend(
                text: text,
                model: model,
                provider: provider,
                effort: effort,
                attachments: attachments,
                contextTask: contextTask
            )
        }
    }

    private func performSend(
        text: String,
        model initialModel: String?,
        provider initialProvider: String?,
        effort initialEffort: String?,
        attachments: [ChatAttachmentInput],
        contextTask: Task<[String: JSONValue]?, Error>?
    ) async throws -> ChatSubmitResult? {
        guard let conversationId = state.activeConversationId, !conversationId.isEmpty else {
            throw ChatValidationError("ChatController.send() requires an open conversation")
        }
        guard !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw ChatValidationError("ChatController.send() requires non-empty text")
        }
        guard !disposed else { return nil }

        if state.error != nil {
            updateState { $0.error = nil }
        }

        // Wait for pending /status before resolving choices, upload limits, or context support.
        if state.features == nil, let pendingInit = initPendingTask {
            let epochBefore = epoch
            _ = await pendingInit.value
            guard isCurrent(epoch: epochBefore) else {
                contextTask?.cancel()
                return nil
            }
        }

        defer {
            // Ensure unused or superseded context reads don't linger.
            if !isCurrent(epoch: epoch) {
                contextTask?.cancel()
            }
        }

        var provider = initialProvider
        var model = initialModel
        var effort = initialEffort

        let selected = resolveChoice(state.choices, requested: model ?? state.choice)
        if let selected {
            provider = selected.provider
            model = selected.model
            updateState { $0.choice = selected.id }
        }

        let supportedEfforts = selected?.efforts ?? []
        if let e = effort, !supportedEfforts.contains(e) {
            effort = nil
        }

        let currentEpoch = epoch
        var attachmentIds: [String] = []
        if !attachments.isEmpty {
            guard let resolved = try await resolveAttachments(
                conversationId: conversationId,
                attachments: attachments,
                epoch: currentEpoch,
                choice: selected
            ) else {
                return nil
            }
            guard isCurrent(epoch: currentEpoch) else { return nil }
            attachmentIds = resolved
        }

        var turnContext: [String: JSONValue]? = nil
        if state.features == nil || state.features?.context == true {
            if let contextTask {
                turnContext = try await contextTask.value
                guard isCurrent(epoch: currentEpoch) else { return nil }
            }
        }

        let idempotencyKey = newIdempotencyKey()
        updateState {
            $0.running = true
            $0.error = nil
        }

        var submitResult: ChatSubmitResult? = nil
        for attempt in 0...3 {
            do {
                submitResult = try await client.submitTurn(
                    conversationId,
                    text: text,
                    provider: provider,
                    model: model,
                    effort: effort,
                    attachments: attachmentIds.isEmpty ? nil : attachmentIds,
                    context: turnContext,
                    idempotencyKey: idempotencyKey
                )
                break
            } catch {
                guard isCurrent(epoch: currentEpoch) else { return nil }
                let retryable = (error is ChatNetworkError) && attempt < 3
                if !retryable {
                    if error is CancellationError {
                        updateState { $0.running = false }
                        return nil
                    }
                    fail(error)
                    return nil
                }
                let delayMs = min(reconnect.maxDelayMs, reconnect.initialDelayMs * (attempt + 1))
                do {
                    try await Task.sleep(nanoseconds: UInt64(max(0, delayMs)) * 1_000_000)
                } catch {
                    return nil
                }
            }
        }

        guard let result = submitResult, isCurrent(epoch: currentEpoch) else { return nil }

        let sentIds = Set(attachmentIds)
        var view = ChatReducer.createTurnView(result.turn.id)
        view.state = result.turn.state
        view.error = result.turn.error

        updateState {
            $0.turn = view
            $0.running = !ChatReducer.isTerminalState(result.turn.state)
            $0.error = nil
            $0.pendingUploads = $0.pendingUploads.filter { !sentIds.contains($0.id) }
        }
        listChanged()

        if ChatReducer.isTerminalState(result.turn.state) {
            await reloadTranscript(conversationId: conversationId, epoch: currentEpoch, openToken: openToken)
            return result
        }
        startTurnStream(turnId: result.turn.id, after: 0, openToken: openToken)
        return result
    }

    /// Cancel the running turn on the server first, then drop the local stream.
    @discardableResult
    public func stop() async -> ChatControllerState {
        await track {
            await performStop()
        }
    }

    private func performStop() async -> ChatControllerState {
        guard let turn = state.turn, !turn.turnId.isEmpty, !disposed else { return state }
        let conversationId = state.activeConversationId
        let currentEpoch = epoch

        var cancelled: ChatTurn? = nil
        do {
            cancelled = try await client.cancelTurn(turn.turnId)
        } catch is CancellationError {
            return state
        } catch {
            guard isCurrent(epoch: currentEpoch) else { return state }
            updateState { $0.error = errorOf(error) }
        }
        guard isCurrent(epoch: currentEpoch) else { return state }

        // Cancel must reach the server before aborting the local stream.
        stopStream()

        updateState {
            var current = $0.turn ?? turn
            if let cancelled {
                current.state = cancelled.state
                current.error = cancelled.error ?? current.error
            }
            $0.turn = current
            $0.running = false
        }

        await reloadTranscript(conversationId: conversationId, epoch: currentEpoch, openToken: openToken)
        return state
    }

    /// Select a catalog choice id locally (mirrors picker change).
    public func selectChoice(_ choiceId: String) {
        guard !disposed else { return }
        updateState { $0.choice = choiceId }
    }
}

// MARK: - Concurrency helpers for stream idle timeout

private final class LockedFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var _value = false

    var value: Bool {
        lock.lock()
        defer { lock.unlock() }
        return _value
    }

    func set(_ newValue: Bool) {
        lock.lock()
        _value = newValue
        lock.unlock()
    }
}

private final class LockedInt: @unchecked Sendable {
    private let lock = NSLock()
    private var _value: Int

    init(_ value: Int) { _value = value }

    var value: Int {
        lock.lock()
        defer { lock.unlock() }
        return _value
    }

    func set(_ newValue: Int) {
        lock.lock()
        _value = newValue
        lock.unlock()
    }
}

private final class IdleTimerBox: @unchecked Sendable {
    private let lock = NSLock()
    private let timeoutMs: Int
    private var task: Task<Void, Never>?
    private var onFire: (@Sendable () -> Void)?
    private var cancelled = false

    init(timeoutMs: Int) {
        self.timeoutMs = timeoutMs
    }

    func arm(_ onFire: @escaping @Sendable () -> Void) {
        lock.lock()
        self.onFire = onFire
        let shouldStart = !cancelled && timeoutMs > 0
        lock.unlock()
        if shouldStart {
            reset()
        }
    }

    func reset() {
        lock.lock()
        defer { lock.unlock() }
        guard !cancelled, timeoutMs > 0, let handler = onFire else { return }
        task?.cancel()
        let ns = UInt64(timeoutMs) * 1_000_000
        task = Task {
            try? await Task.sleep(nanoseconds: ns)
            guard !Task.isCancelled else { return }
            handler()
        }
    }

    func cancel() {
        lock.lock()
        cancelled = true
        let existing = task
        task = nil
        onFire = nil
        lock.unlock()
        existing?.cancel()
    }
}
