import Foundation
import PBMCPChat
import SwiftUI
#if canImport(AppKit)
import AppKit
#elseif canImport(UIKit)
import UIKit
#endif

/// Compact, dockable or floating SwiftUI side-panel chat component binding a `ChatController`
/// (native equivalent of `agent-panel`'s `<agent-panel>` v0.4.1 / v0.5.1).
///
/// Features:
/// - Compact header with optional `headerStart` mark, conversation switcher popover
///   (`conversationsMenu`), `headerActions`, New chat button, and optional Close button.
/// - `heading` is displayed while no conversations exist (or when `conversationsMenu` is false);
///   otherwise the switcher button shows the active conversation's title.
/// - Optional `switcherEnd` slot below the conversation list (e.g. "Open in full page").
/// - Optional `floating` card styling (rounded corners, subtle border, soft shadow).
/// - Optional quick `suggestions` chips in the empty state.
/// - Same transcript, streaming, tool rows, proposals, attachments, model/effort menus,
///   context ring + Compact, and safe Markdown as `ChatPage`.
public struct ChatPanel<
    HeaderStart: View,
    HeaderActions: View,
    SwitcherEnd: View,
    TranscriptEnd: View
>: View {
    @Environment(\.colorScheme) private var colorScheme

    @State private var viewModel: ChatPageViewModel
    @State private var showSwitcher: Bool = false
    @State private var isNearBottom: Bool = true

    private let heading: String
    private let placeholder: String
    private let emptyTitle: String
    private let emptyText: String
    private let suggestions: [String]
    private let conversationsMenu: Bool
    private let floating: Bool
    private let disabled: Bool
    private let onClose: (() -> Void)?
    private let onLinkTap: ((URL) -> Void)?
    private let markdownRenderer: ((String) -> AnyView)?
    private let headerStart: () -> HeaderStart
    private let headerActions: () -> HeaderActions
    private let switcherEnd: () -> SwitcherEnd
    private let transcriptEnd: () -> TranscriptEnd

    public init(
        controller: ChatController,
        heading: String = "Assistant",
        placeholder: String = "Message…",
        emptyTitle: String = "How can I help?",
        emptyText: String = "",
        suggestions: [String] = [],
        conversationsMenu: Bool = true,
        floating: Bool = false,
        disabled: Bool = false,
        autoCreate: Bool = true,
        disableWhenNotReady: Bool = true,
        mentionTrigger: String = "@",
        onClose: (() -> Void)? = nil,
        onLinkTap: ((URL) -> Void)? = nil,
        onAttachmentDownload: ((String, ChatAgentReference) async throws -> Void)? = nil,
        onSend: ((ChatSendOutcome) -> Void)? = nil,
        onMentionChange: ((ChatMentionState) -> Void)? = nil,
        contextReferences: (([String: JSONValue], ChatMessage) -> [ChatAgentReference])? = nil,
        errorTextOverride: ((String, String, Error?) -> String?)? = nil,
        markdownRenderer: ((String) -> AnyView)? = nil,
        @ViewBuilder headerStart: @escaping () -> HeaderStart,
        @ViewBuilder headerActions: @escaping () -> HeaderActions,
        @ViewBuilder switcherEnd: @escaping () -> SwitcherEnd,
        @ViewBuilder transcriptEnd: @escaping () -> TranscriptEnd
    ) {
        let vm = ChatPageViewModel(
            controller: controller,
            autoCreate: autoCreate,
            disableWhenNotReady: disableWhenNotReady,
            mentionTrigger: mentionTrigger
        )
        vm.isHostDisabled = disabled
        vm.onAttachmentDownload = onAttachmentDownload
        vm.onSend = onSend
        vm.onMentionChange = onMentionChange
        vm.contextReferences = contextReferences
        vm.errorTextOverride = errorTextOverride

        _viewModel = State(initialValue: vm)
        self.heading = heading
        self.placeholder = placeholder
        self.emptyTitle = emptyTitle
        self.emptyText = emptyText
        self.suggestions = suggestions
        self.conversationsMenu = conversationsMenu
        self.floating = floating
        self.disabled = disabled
        self.onClose = onClose
        self.onLinkTap = onLinkTap
        self.markdownRenderer = markdownRenderer
        self.headerStart = headerStart
        self.headerActions = headerActions
        self.switcherEnd = switcherEnd
        self.transcriptEnd = transcriptEnd
    }

    /// The resolved title shown in the panel header (exposed for testing and accessibility).
    static func resolvedHeaderTitle(
        heading: String,
        conversationsMenu: Bool,
        conversations: [ChatConversation],
        activeConversationId: String?
    ) -> String {
        guard conversationsMenu, !conversations.isEmpty else {
            return heading
        }
        if let activeId = activeConversationId,
           let conv = conversations.first(where: { $0.id == activeId }) {
            return conv.title.isEmpty ? "Untitled" : conv.title
        }
        return "New chat"
    }

    private var hasSwitcher: Bool {
        conversationsMenu && !viewModel.state.conversations.isEmpty
    }

    private var headerTitle: String {
        Self.resolvedHeaderTitle(
            heading: heading,
            conversationsMenu: conversationsMenu,
            conversations: viewModel.state.conversations,
            activeConversationId: viewModel.state.activeConversationId
        )
    }

    public var body: some View {
        panelContent
            .modifier(FloatingPanelChrome(floating: floating, colorScheme: colorScheme))
            .onChange(of: disabled) { _, newValue in
                viewModel.isHostDisabled = newValue
            }
            .onChange(of: viewModel.state) { _, _ in
                viewModel.syncWithControllerState()
            }
            .onChange(of: viewModel.lastCompletedAssistantMessage) { _, message in
                guard let message, !message.isEmpty else { return }
                announceAccessibility(message)
            }
    }

    private var panelContent: some View {
        VStack(spacing: 0) {
            headerBar
            Divider()

            let entries = viewModel.entries
            if entries.isEmpty && !viewModel.isRunning {
                emptyStateView
            } else {
                transcriptScrollView(entries: entries)
            }

            if !viewModel.bannerError.isEmpty {
                ErrorBannerView(message: viewModel.bannerError) {
                    viewModel.dismissError()
                }
                .padding(.horizontal, 12)
                .padding(.top, 8)
            }

            ComposerView(viewModel: viewModel, placeholder: placeholder)
                .padding(.horizontal, 12)
                .padding(.vertical, 10)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // MARK: - Header & Conversation Switcher

    private var headerBar: some View {
        HStack(spacing: 6) {
            headerStart()

            if hasSwitcher {
                Button {
                    showSwitcher.toggle()
                } label: {
                    HStack(spacing: 4) {
                        Text(headerTitle)
                            .font(.subheadline)
                            .fontWeight(.semibold)
                            .lineLimit(1)
                            .foregroundStyle(.primary)
                        Image(systemName: "chevron.down")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                    .padding(.horizontal, 6)
                    .padding(.vertical, 4)
                    .background(
                        RoundedRectangle(cornerRadius: 6)
                            .fill(showSwitcher ? Color.secondary.opacity(0.14) : Color.clear)
                    )
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Switch conversation, current: \(headerTitle)")
                .popover(isPresented: $showSwitcher, arrowEdge: .bottom) {
                    switcherPopoverContent
                }
            } else {
                Text(headerTitle)
                    .font(.subheadline)
                    .fontWeight(.semibold)
                    .lineLimit(1)
            }

            Spacer(minLength: 4)

            headerActions()

            if conversationsMenu {
                Button {
                    showSwitcher = false
                    Task { await viewModel.startNewConversation() }
                } label: {
                    Image(systemName: "square.and.pencil")
                        .font(.subheadline)
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("New chat")
                .help("New chat")
            }

            if let onClose {
                Button {
                    showSwitcher = false
                    onClose()
                } label: {
                    Image(systemName: "xmark")
                        .font(.subheadline)
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("Close panel")
                .help("Close")
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
    }

    private var switcherPopoverContent: some View {
        VStack(alignment: .leading, spacing: 0) {
            ScrollView {
                LazyVStack(spacing: 2) {
                    ForEach(viewModel.state.conversations) { conversation in
                        switcherRow(conversation)
                    }

                    if !viewModel.state.nextCursor.isEmpty {
                        Button("Load more") {
                            Task { await viewModel.loadMoreConversations() }
                        }
                        .buttonStyle(.borderless)
                        .font(.caption)
                        .padding(.vertical, 6)
                        .frame(maxWidth: .infinity)
                        .accessibilityLabel("Load more conversations")
                    }
                }
                .padding(6)
            }
            .frame(maxHeight: 280)

            if SwitcherEnd.self != EmptyView.self {
                Divider()
                switcherEnd()
                    .padding(8)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .frame(width: 270)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Conversations")
    }

    @ViewBuilder
    private func switcherRow(_ conversation: ChatConversation) -> some View {
        let isActive = viewModel.state.activeConversationId == conversation.id
        let title = conversation.title.isEmpty ? "Untitled" : conversation.title

        HStack(spacing: 8) {
            Button {
                showSwitcher = false
                Task { await viewModel.openConversation(conversation.id) }
            } label: {
                VStack(alignment: .leading, spacing: 2) {
                    Text(title)
                        .font(.subheadline)
                        .fontWeight(isActive ? .semibold : .regular)
                        .lineLimit(1)
                        .foregroundStyle(.primary)
                    if let formatted = formatSwitcherDate(conversation.updatedAt) {
                        Text(formatted)
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                    }
                }
                .padding(.trailing, 4)
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Open conversation \(title)")
            .accessibilityAddTraits(isActive ? .isSelected : [])

            Button(role: .destructive) {
                Task { await viewModel.deleteConversation(conversation.id) }
            } label: {
                Image(systemName: "trash")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(4)
            }
            .buttonStyle(.borderless)
            .accessibilityLabel("Delete conversation \(title)")
            .help("Delete")
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .background(
            RoundedRectangle(cornerRadius: 6)
                .fill(isActive ? Color.accentColor.opacity(0.14) : Color.clear)
        )
    }

    // MARK: - Empty state & Transcript

    private var emptyStateView: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 8) {
                Text(emptyTitle)
                    .font(.title3)
                    .fontWeight(.semibold)
                if !emptyText.isEmpty {
                    Text(emptyText)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }

                if !suggestions.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        ForEach(suggestions, id: \.self) { suggestion in
                            Button {
                                Task { _ = await viewModel.submit(textOverride: suggestion) }
                            } label: {
                                Text(suggestion)
                                    .font(.caption)
                                    .multilineTextAlignment(.leading)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    .padding(.horizontal, 10)
                                    .padding(.vertical, 7)
                                    .background(
                                        RoundedRectangle(cornerRadius: 8)
                                            .fill(Color.secondary.opacity(0.08))
                                    )
                                    .overlay(
                                        RoundedRectangle(cornerRadius: 8)
                                            .stroke(Color.secondary.opacity(0.18), lineWidth: 1)
                                    )
                            }
                            .buttonStyle(.plain)
                            .disabled(viewModel.isComposerDisabled || viewModel.isRunning)
                            .accessibilityLabel("Send suggestion: \(suggestion)")
                        }
                    }
                    .padding(.top, 8)
                }

                transcriptEnd()
                    .padding(.top, 12)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(16)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    @ViewBuilder
    private func transcriptScrollView(entries: [ChatAgentEntry]) -> some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    ForEach(entries) { entry in
                        TranscriptEntryRow(
                            entry: entry,
                            viewModel: viewModel,
                            onLinkTap: onLinkTap,
                            markdownRenderer: markdownRenderer
                        )
                        .id(entry.id)
                    }

                    if let usage = viewModel.state.turn?.usage {
                        TurnUsageView(usage: usage)
                    }

                    transcriptEnd()

                    Color.clear
                        .frame(height: 1)
                        .id("__panel_bottom__")
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 12)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .onScrollGeometryChange(for: Bool.self) { geo in
                let maxOffset = max(0, geo.contentSize.height - geo.containerSize.height)
                let current = geo.contentOffset.y + geo.contentInsets.top
                return (maxOffset - current) <= 80
            } action: { _, nearBottom in
                isNearBottom = nearBottom
            }
            .onAppear {
                isNearBottom = true
                proxy.scrollTo("__panel_bottom__", anchor: .bottom)
            }
            .onChange(of: viewModel.state.activeConversationId) { _, _ in
                isNearBottom = true
                proxy.scrollTo("__panel_bottom__", anchor: .bottom)
            }
            .onChange(of: entries.count) { _, _ in
                guard isNearBottom else { return }
                withAnimation(.easeOut(duration: 0.15)) {
                    proxy.scrollTo("__panel_bottom__", anchor: .bottom)
                }
            }
            .onChange(of: viewModel.state.turn?.text) { _, _ in
                guard isNearBottom else { return }
                proxy.scrollTo("__panel_bottom__", anchor: .bottom)
            }
        }
    }

    private func formatSwitcherDate(_ iso: String) -> String? {
        guard !iso.isEmpty else { return nil }
        let withFrac = ISO8601DateFormatter()
        withFrac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        guard let date = withFrac.date(from: iso) ?? plain.date(from: iso) else { return nil }
        let calendar = Calendar.current
        if calendar.isDateInToday(date) {
            return date.formatted(date: .omitted, time: .shortened)
        }
        if calendar.component(.year, from: date) == calendar.component(.year, from: Date()) {
            return date.formatted(.dateTime.month(.abbreviated).day())
        }
        return date.formatted(.dateTime.month(.abbreviated).day().year())
    }

    private func announceAccessibility(_ text: String) {
        #if canImport(AppKit)
        NSAccessibility.post(
            element: NSApp as Any,
            notification: .announcementRequested,
            userInfo: [.announcement: text]
        )
        #elseif canImport(UIKit)
        UIAccessibility.post(notification: .announcement, argument: text)
        #endif
    }
}

private struct FloatingPanelChrome: ViewModifier {
    let floating: Bool
    let colorScheme: ColorScheme

    func body(content: Content) -> some View {
        if floating {
            content
                .background(floatingBackground)
                .clipShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
                .overlay(
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .stroke(Color.secondary.opacity(colorScheme == .dark ? 0.28 : 0.2), lineWidth: 1)
                )
                .shadow(
                    color: Color.black.opacity(colorScheme == .dark ? 0.45 : 0.14),
                    radius: 18,
                    x: 0,
                    y: 8
                )
        } else {
            content
        }
    }

    private var floatingBackground: Color {
        #if canImport(AppKit)
        Color(nsColor: .windowBackgroundColor)
        #elseif canImport(UIKit)
        Color(uiColor: .systemBackground)
        #else
        Color.clear
        #endif
    }
}

// MARK: - Convenience Initializers

public extension ChatPanel where
    HeaderStart == EmptyView,
    HeaderActions == EmptyView,
    SwitcherEnd == EmptyView,
    TranscriptEnd == EmptyView {
    init(
        controller: ChatController,
        heading: String = "Assistant",
        placeholder: String = "Message…",
        emptyTitle: String = "How can I help?",
        emptyText: String = "",
        suggestions: [String] = [],
        conversationsMenu: Bool = true,
        floating: Bool = false,
        disabled: Bool = false,
        autoCreate: Bool = true,
        disableWhenNotReady: Bool = true,
        mentionTrigger: String = "@",
        onClose: (() -> Void)? = nil,
        onLinkTap: ((URL) -> Void)? = nil,
        onAttachmentDownload: ((String, ChatAgentReference) async throws -> Void)? = nil,
        onSend: ((ChatSendOutcome) -> Void)? = nil,
        onMentionChange: ((ChatMentionState) -> Void)? = nil,
        contextReferences: (([String: JSONValue], ChatMessage) -> [ChatAgentReference])? = nil,
        errorTextOverride: ((String, String, Error?) -> String?)? = nil,
        markdownRenderer: ((String) -> AnyView)? = nil
    ) {
        self.init(
            controller: controller,
            heading: heading,
            placeholder: placeholder,
            emptyTitle: emptyTitle,
            emptyText: emptyText,
            suggestions: suggestions,
            conversationsMenu: conversationsMenu,
            floating: floating,
            disabled: disabled,
            autoCreate: autoCreate,
            disableWhenNotReady: disableWhenNotReady,
            mentionTrigger: mentionTrigger,
            onClose: onClose,
            onLinkTap: onLinkTap,
            onAttachmentDownload: onAttachmentDownload,
            onSend: onSend,
            onMentionChange: onMentionChange,
            contextReferences: contextReferences,
            errorTextOverride: errorTextOverride,
            markdownRenderer: markdownRenderer,
            headerStart: { EmptyView() },
            headerActions: { EmptyView() },
            switcherEnd: { EmptyView() },
            transcriptEnd: { EmptyView() }
        )
    }
}

public extension ChatPanel where
    HeaderStart == EmptyView,
    HeaderActions == EmptyView,
    SwitcherEnd == EmptyView {
    init(
        controller: ChatController,
        heading: String = "Assistant",
        placeholder: String = "Message…",
        emptyTitle: String = "How can I help?",
        emptyText: String = "",
        suggestions: [String] = [],
        conversationsMenu: Bool = true,
        floating: Bool = false,
        disabled: Bool = false,
        autoCreate: Bool = true,
        disableWhenNotReady: Bool = true,
        mentionTrigger: String = "@",
        onClose: (() -> Void)? = nil,
        onLinkTap: ((URL) -> Void)? = nil,
        onAttachmentDownload: ((String, ChatAgentReference) async throws -> Void)? = nil,
        onSend: ((ChatSendOutcome) -> Void)? = nil,
        onMentionChange: ((ChatMentionState) -> Void)? = nil,
        contextReferences: (([String: JSONValue], ChatMessage) -> [ChatAgentReference])? = nil,
        errorTextOverride: ((String, String, Error?) -> String?)? = nil,
        markdownRenderer: ((String) -> AnyView)? = nil,
        @ViewBuilder transcriptEnd: @escaping () -> TranscriptEnd
    ) {
        self.init(
            controller: controller,
            heading: heading,
            placeholder: placeholder,
            emptyTitle: emptyTitle,
            emptyText: emptyText,
            suggestions: suggestions,
            conversationsMenu: conversationsMenu,
            floating: floating,
            disabled: disabled,
            autoCreate: autoCreate,
            disableWhenNotReady: disableWhenNotReady,
            mentionTrigger: mentionTrigger,
            onClose: onClose,
            onLinkTap: onLinkTap,
            onAttachmentDownload: onAttachmentDownload,
            onSend: onSend,
            onMentionChange: onMentionChange,
            contextReferences: contextReferences,
            errorTextOverride: errorTextOverride,
            markdownRenderer: markdownRenderer,
            headerStart: { EmptyView() },
            headerActions: { EmptyView() },
            switcherEnd: { EmptyView() },
            transcriptEnd: transcriptEnd
        )
    }
}
