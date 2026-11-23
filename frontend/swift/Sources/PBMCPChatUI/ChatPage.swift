import Foundation
import PBMCPChat
import SwiftUI
#if canImport(AppKit)
import AppKit
#elseif canImport(UIKit)
import UIKit
#endif

/// Native SwiftUI chat page binding a `ChatController` (equivalent of agent-panel's `<agent-page>`).
///
/// Designed to work cleanly inside a host `NavigationSplitView` detail pane as well as standalone.
public struct ChatPage<TranscriptEnd: View>: View {
    @State private var viewModel: ChatPageViewModel
    @State private var sidebarVisible: Bool
    @State private var isRenamingHeader: Bool = false
    @State private var headerRenameText: String = ""
    @State private var isNearBottom: Bool = true

    private let placeholder: String
    private let emptyTitle: String
    private let emptyText: String
    private let renamable: Bool
    private let disabled: Bool
    private let onLinkTap: ((URL) -> Void)?
    private let markdownRenderer: ((String) -> AnyView)?
    private let transcriptEnd: () -> TranscriptEnd

    public init(
        controller: ChatController,
        placeholder: String = "Message…",
        emptyTitle: String = "How can I help?",
        emptyText: String = "",
        renamable: Bool = true,
        showSidebar: Bool = true,
        disabled: Bool = false,
        autoCreate: Bool = true,
        disableWhenNotReady: Bool = true,
        mentionTrigger: String = "@",
        onLinkTap: ((URL) -> Void)? = nil,
        onAttachmentDownload: ((String, ChatAgentReference) async throws -> Void)? = nil,
        onSend: ((ChatSendOutcome) -> Void)? = nil,
        onMentionChange: ((ChatMentionState) -> Void)? = nil,
        contextReferences: (([String: JSONValue], ChatMessage) -> [ChatAgentReference])? = nil,
        errorTextOverride: ((String, String, Error?) -> String?)? = nil,
        markdownRenderer: ((String) -> AnyView)? = nil,
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
        _sidebarVisible = State(initialValue: showSidebar)
        self.placeholder = placeholder
        self.emptyTitle = emptyTitle
        self.emptyText = emptyText
        self.renamable = renamable
        self.disabled = disabled
        self.onLinkTap = onLinkTap
        self.markdownRenderer = markdownRenderer
        self.transcriptEnd = transcriptEnd
    }

    public var body: some View {
        GeometryReader { geo in
            let isCompact = geo.size.width < 680
            HStack(spacing: 0) {
                if sidebarVisible && !isCompact {
                    ConversationSidebarView(viewModel: viewModel, renamable: renamable, onSelect: {})
                        .frame(width: 240)
                    Divider()
                }

                mainChatColumn(isCompact: isCompact)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .sheet(isPresented: Binding(
                get: { isCompact && sidebarVisible },
                set: { sidebarVisible = $0 }
            )) {
                ConversationSidebarView(viewModel: viewModel, renamable: renamable) {
                    sidebarVisible = false
                }
                .frame(minWidth: 280, minHeight: 400)
            }
        }
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

    @ViewBuilder
    private func mainChatColumn(isCompact: Bool) -> some View {
        VStack(spacing: 0) {
            headerBar(isCompact: isCompact)
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
                .padding(.horizontal, 16)
                .padding(.top, 8)
            }

            ComposerView(viewModel: viewModel, placeholder: placeholder)
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
        }
    }

    @ViewBuilder
    private func headerBar(isCompact: Bool) -> some View {
        HStack(spacing: 10) {
            Button {
                sidebarVisible.toggle()
            } label: {
                Image(systemName: "sidebar.left")
            }
            .buttonStyle(.borderless)
            .accessibilityLabel(sidebarVisible ? "Hide conversations" : "Show conversations")

            if isRenamingHeader {
                TextField("Title", text: $headerRenameText)
                    .textFieldStyle(.roundedBorder)
                    .frame(maxWidth: 360)
                    .accessibilityLabel("Conversation title")
                    .onSubmit { commitHeaderRename() }
                Button("Save") { commitHeaderRename() }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                Button("Cancel") { isRenamingHeader = false }
                    .buttonStyle(.borderless)
                    .controlSize(.small)
            } else {
                let heading = viewModel.activeConversationTitle.isEmpty
                    ? "New chat"
                    : viewModel.activeConversationTitle
                if renamable && viewModel.state.activeConversationId != nil {
                    Button {
                        headerRenameText = viewModel.activeConversationTitle
                        isRenamingHeader = true
                    } label: {
                        Text(heading)
                            .font(.headline)
                            .lineLimit(1)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Rename conversation \(heading)")
                } else {
                    Text(heading)
                        .font(.headline)
                        .lineLimit(1)
                }
            }

            Spacer()

            if isCompact {
                Button {
                    Task { await viewModel.startNewConversation() }
                } label: {
                    Image(systemName: "square.and.pencil")
                }
                .buttonStyle(.borderless)
                .accessibilityLabel("New chat")
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }

    private func commitHeaderRename() {
        let submitted = headerRenameText
        isRenamingHeader = false
        Task { await viewModel.renameActiveConversation(submitted) }
    }

    @ViewBuilder
    private var emptyStateView: some View {
        VStack(alignment: .leading, spacing: 8) {
            Spacer()
            Text(emptyTitle)
                .font(.title2)
                .fontWeight(.semibold)
            if !emptyText.isEmpty {
                Text(emptyText)
                    .font(.body)
                    .foregroundStyle(.secondary)
            }
            transcriptEnd()
                .padding(.top, 16)
            Spacer()
        }
        .frame(maxWidth: 760, alignment: .leading)
        .padding(24)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    @ViewBuilder
    private func transcriptScrollView(entries: [ChatAgentEntry]) -> some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
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
                        .id("__transcript_bottom__")
                }
                .frame(maxWidth: 820, alignment: .leading)
                .padding(.horizontal, 20)
                .padding(.vertical, 16)
                .frame(maxWidth: .infinity)
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
                proxy.scrollTo("__transcript_bottom__", anchor: .bottom)
            }
            .onChange(of: viewModel.state.activeConversationId) { _, _ in
                isNearBottom = true
                proxy.scrollTo("__transcript_bottom__", anchor: .bottom)
            }
            .onChange(of: entries.count) { _, _ in
                guard isNearBottom else { return }
                withAnimation(.easeOut(duration: 0.15)) {
                    proxy.scrollTo("__transcript_bottom__", anchor: .bottom)
                }
            }
            .onChange(of: viewModel.state.turn?.text) { _, _ in
                guard isNearBottom else { return }
                proxy.scrollTo("__transcript_bottom__", anchor: .bottom)
            }
        }
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

public extension ChatPage where TranscriptEnd == EmptyView {
    init(
        controller: ChatController,
        placeholder: String = "Message…",
        emptyTitle: String = "How can I help?",
        emptyText: String = "",
        renamable: Bool = true,
        showSidebar: Bool = true,
        disabled: Bool = false,
        autoCreate: Bool = true,
        disableWhenNotReady: Bool = true,
        mentionTrigger: String = "@",
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
            placeholder: placeholder,
            emptyTitle: emptyTitle,
            emptyText: emptyText,
            renamable: renamable,
            showSidebar: showSidebar,
            disabled: disabled,
            autoCreate: autoCreate,
            disableWhenNotReady: disableWhenNotReady,
            mentionTrigger: mentionTrigger,
            onLinkTap: onLinkTap,
            onAttachmentDownload: onAttachmentDownload,
            onSend: onSend,
            onMentionChange: onMentionChange,
            contextReferences: contextReferences,
            errorTextOverride: errorTextOverride,
            markdownRenderer: markdownRenderer,
            transcriptEnd: { EmptyView() }
        )
    }
}
