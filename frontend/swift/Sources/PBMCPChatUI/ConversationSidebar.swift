import Foundation
import PBMCPChat
import SwiftUI

/// Sidebar list of conversations with New Chat, inline rename, delete, and Load More.
struct ConversationSidebarView: View {
    let viewModel: ChatPageViewModel
    let renamable: Bool
    let onSelect: () -> Void

    @State private var renamingId: String? = nil
    @State private var renameText: String = ""

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Button {
                    Task {
                        await viewModel.startNewConversation()
                        onSelect()
                    }
                } label: {
                    Label("New chat", systemImage: "square.and.pencil")
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.regular)
                .accessibilityLabel("New chat")
            }
            .padding(10)

            Divider()

            if viewModel.state.conversations.isEmpty {
                VStack(spacing: 6) {
                    Text("No conversations")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .padding()
            } else {
                ScrollView {
                    LazyVStack(spacing: 2) {
                        ForEach(viewModel.state.conversations) { conversation in
                            conversationRow(conversation)
                        }

                        if !viewModel.state.nextCursor.isEmpty {
                            Button("Load more") {
                                Task { await viewModel.loadMoreConversations() }
                            }
                            .buttonStyle(.borderless)
                            .font(.caption)
                            .padding(.vertical, 8)
                            .accessibilityLabel("Load more conversations")
                        }
                    }
                    .padding(6)
                }
            }
        }
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Conversations")
    }

    @ViewBuilder
    private func conversationRow(_ conversation: ChatConversation) -> some View {
        let isActive = viewModel.state.activeConversationId == conversation.id
        let title = conversation.title.isEmpty ? "Untitled" : conversation.title

        if renamingId == conversation.id {
            HStack(spacing: 6) {
                TextField("Title", text: $renameText)
                    .textFieldStyle(.roundedBorder)
                    .accessibilityLabel("Conversation title")
                    .onSubmit {
                        let submitted = renameText
                        renamingId = nil
                        Task { await viewModel.renameConversation(id: conversation.id, title: submitted) }
                    }
                Button("Save") {
                    let submitted = renameText
                    renamingId = nil
                    Task { await viewModel.renameConversation(id: conversation.id, title: submitted) }
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
                .accessibilityLabel("Save conversation title")
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 6)
        } else {
            HStack(spacing: 8) {
                Button {
                    Task {
                        await viewModel.openConversation(conversation.id)
                        onSelect()
                    }
                } label: {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(title)
                            .font(.subheadline)
                            .fontWeight(isActive ? .semibold : .regular)
                            .lineLimit(1)
                            .foregroundStyle(.primary)
                        if let formattedDate = formatRelativeDate(conversation.updatedAt) {
                            Text(formattedDate)
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

                Menu {
                    if renamable {
                        Button("Rename") {
                            renameText = conversation.title
                            renamingId = conversation.id
                        }
                    }
                    Button("Delete", role: .destructive) {
                        Task { await viewModel.deleteConversation(conversation.id) }
                    }
                } label: {
                    Image(systemName: "ellipsis")
                        .foregroundStyle(.secondary)
                        .padding(4)
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .accessibilityLabel("Actions for \(title)")
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 6)
            .background(
                RoundedRectangle(cornerRadius: 6)
                    .fill(isActive ? Color.accentColor.opacity(0.14) : Color.clear)
            )
            .contextMenu {
                if renamable {
                    Button("Rename") {
                        renameText = conversation.title
                        renamingId = conversation.id
                    }
                }
                Button("Delete", role: .destructive) {
                    Task { await viewModel.deleteConversation(conversation.id) }
                }
            }
        }
    }

    private func formatRelativeDate(_ iso: String) -> String? {
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
}
