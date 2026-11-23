import Foundation
import PBMCPChat
import SwiftUI

/// Renders one entry from `entriesFromState`: text bubble, collapsible tool calls,
/// proposal card, notice line, or context-summary divider.
struct TranscriptEntryRow: View {
    let entry: ChatAgentEntry
    let viewModel: ChatPageViewModel
    let onLinkTap: ((URL) -> Void)?
    let markdownRenderer: ((String) -> AnyView)?

    var body: some View {
        switch entry {
        case .text(let textEntry):
            TextEntryView(
                entry: textEntry,
                onReferenceTap: { ref in
                    if ref.isAttachment {
                        Task { await viewModel.downloadAttachment(ref) }
                    }
                },
                onLinkTap: onLinkTap,
                markdownRenderer: markdownRenderer
            )

        case .tools(let toolsEntry):
            ToolsEntryView(entry: toolsEntry)

        case .proposal(let proposalEntry):
            ProposalCardView(entry: proposalEntry) { decision in
                Task { await viewModel.decide(proposalId: proposalEntry.proposalId, decision: decision) }
            }

        case .notice(let noticeEntry):
            NoticeEntryView(entry: noticeEntry)

        case .contextMarker(let marker):
            ContextMarkerView(marker: marker)
        }
    }
}

struct TextEntryView: View {
    let entry: ChatAgentTextEntry
    let onReferenceTap: (ChatAgentReference) -> Void
    let onLinkTap: ((URL) -> Void)?
    let markdownRenderer: ((String) -> AnyView)?

    private var isUser: Bool { entry.role == "user" }

    var body: some View {
        HStack(alignment: .top) {
            if isUser { Spacer(minLength: 48) }

            VStack(alignment: isUser ? .trailing : .leading, spacing: 6) {
                if !entry.references.isEmpty {
                    FlowChipRow(references: entry.references, onTap: onReferenceTap)
                }

                if !entry.content.isEmpty {
                    if isUser {
                        Text(entry.content)
                            .font(.body)
                            .textSelection(.enabled)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 8)
                            .background(
                                RoundedRectangle(cornerRadius: 12)
                                    .fill(Color.accentColor.opacity(0.14))
                            )
                            .accessibilityLabel("You: \(entry.content)")
                    } else {
                        Group {
                            if let markdownRenderer {
                                markdownRenderer(entry.content)
                            } else {
                                SafeMarkdownView(entry.content, onLinkTap: onLinkTap)
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        // VoiceOver: while streaming, avoid announcing every token; final message is announced on completion.
                        .accessibilityLabel(entry.isStreaming ? "Assistant is replying" : "Assistant message")
                        .accessibilityValue(entry.isStreaming ? "" : entry.content)
                    }
                }
            }

            if !isUser { Spacer(minLength: 24) }
        }
    }
}

private struct FlowChipRow: View {
    let references: [ChatAgentReference]
    let onTap: (ChatAgentReference) -> Void

    var body: some View {
        HStack(spacing: 6) {
            ForEach(references) { ref in
                Button {
                    onTap(ref)
                } label: {
                    HStack(spacing: 4) {
                        Image(systemName: ref.isAttachment ? "paperclip" : "doc.text")
                            .font(.caption)
                        Text(ref.label)
                            .font(.caption)
                            .lineLimit(1)
                    }
                    .padding(.horizontal, 8)
                    .padding(.vertical, 4)
                    .background(
                        Capsule()
                            .fill(Color.secondary.opacity(0.12))
                    )
                }
                .buttonStyle(.plain)
                .accessibilityLabel(ref.isAttachment ? "Download attachment \(ref.label)" : "Reference \(ref.label)")
            }
        }
    }
}

struct ToolsEntryView: View {
    let entry: ChatAgentToolsEntry
    @State private var expanded: Bool = false

    private var hasRunning: Bool {
        entry.tools.contains { $0.status == .running }
    }

    private var summaryLabel: String {
        let count = entry.tools.count
        if hasRunning {
            return count == 1 ? "Running \(humanizeToolName(entry.tools[0].name))…" : "Running \(count) tools…"
        }
        return count == 1 ? "Used \(humanizeToolName(entry.tools[0].name))" : "Used \(count) tools"
    }

    var body: some View {
        DisclosureGroup(isExpanded: $expanded) {
            VStack(alignment: .leading, spacing: 8) {
                ForEach(entry.tools) { tool in
                    ToolCallDetailRow(tool: tool)
                }
            }
            .padding(.top, 6)
        } label: {
            HStack(spacing: 8) {
                if hasRunning {
                    ProgressView()
                        .controlSize(.small)
                } else if entry.tools.contains(where: { $0.status == .failed }) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                        .font(.caption)
                } else {
                    Image(systemName: "wrench.and.screwdriver")
                        .foregroundStyle(.secondary)
                        .font(.caption)
                }
                Text(summaryLabel)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(10)
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(Color.secondary.opacity(0.07))
        )
        .accessibilityLabel(summaryLabel)
    }
}

private struct ToolCallDetailRow: View {
    let tool: ChatAgentToolCall
    @State private var showJSON: Bool = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                switch tool.status {
                case .running:
                    ProgressView().controlSize(.mini)
                case .completed:
                    Image(systemName: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                        .font(.caption)
                case .failed:
                    Image(systemName: "xmark.circle.fill")
                        .foregroundStyle(.red)
                        .font(.caption)
                }
                Text(tool.name)
                    .font(.caption)
                    .fontWeight(.medium)
                    .fontDesign(.monospaced)

                Spacer()

                if tool.detail != nil {
                    Button(showJSON ? "Hide details" : "Details") {
                        showJSON.toggle()
                    }
                    .buttonStyle(.borderless)
                    .font(.caption2)
                    .accessibilityLabel("\(showJSON ? "Hide" : "Show") details for \(tool.name)")
                }
            }

            if showJSON, let detail = tool.detail {
                VStack(alignment: .leading, spacing: 4) {
                    if let args = detail.arguments {
                        jsonBlock(title: "Arguments", content: args.prettyPrinted())
                    }
                    if let result = detail.result {
                        jsonBlock(title: "Result", content: result.prettyPrinted())
                    }
                    if let error = detail.error, !error.isEmpty {
                        Text(error)
                            .font(.caption)
                            .foregroundStyle(.red)
                    }
                }
                .padding(.leading, 18)
            }
        }
    }

    @ViewBuilder
    private func jsonBlock(title: String, content: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(.caption2)
                .foregroundStyle(.secondary)
            ScrollView(.horizontal, showsIndicators: false) {
                Text(content)
                    .font(.caption2)
                    .fontDesign(.monospaced)
                    .textSelection(.enabled)
                    .padding(6)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(
                        RoundedRectangle(cornerRadius: 4)
                            .fill(Color.secondary.opacity(0.08))
                    )
            }
        }
    }
}

/// Proposal review card showing summary, state badge, metadata, status, warnings,
/// error banner, and Approve / Reject actions when pending.
public struct ProposalCardView: View {
    @Environment(\.colorScheme) private var colorScheme

    public let entry: ChatAgentProposalEntry
    public let onDecide: (ProposalDecision) -> Void

    public init(entry: ChatAgentProposalEntry, onDecide: @escaping (ProposalDecision) -> Void) {
        self.entry = entry
        self.onDecide = onDecide
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(entry.summary)
                    .font(.headline)
                    .fixedSize(horizontal: false, vertical: true)
                Spacer()
                Text(entry.stateLabel)
                    .font(.caption)
                    .fontWeight(.semibold)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background(Capsule().fill(badgeBackgroundColor))
                    .overlay(Capsule().stroke(badgeBorderColor, lineWidth: 1))
                    .foregroundStyle(badgeForegroundColor)
            }

            Text(entry.metaText)
                .font(.caption)
                .foregroundStyle(.secondary)

            Text(entry.status)
                .font(.subheadline)
                .fixedSize(horizontal: false, vertical: true)

            if !entry.detail.isEmpty && entry.state != .failed && entry.detail != entry.status {
                Text(entry.detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            ForEach(Array(entry.warnings.enumerated()), id: \.offset) { _, warning in
                HStack(alignment: .top, spacing: 6) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                        .font(.caption)
                    Text(warning)
                        .font(.caption)
                        .foregroundStyle(.primary)
                }
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 6)
                        .fill(Color.orange.opacity(colorScheme == .dark ? 0.14 : 0.09))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 6)
                        .stroke(Color.orange.opacity(colorScheme == .dark ? 0.35 : 0.25), lineWidth: 1)
                )
            }

            if !entry.error.isEmpty {
                HStack(alignment: .top, spacing: 6) {
                    Image(systemName: "exclamationmark.circle.fill")
                        .foregroundStyle(.red)
                        .font(.caption)
                    Text(entry.error)
                        .font(.caption)
                        .foregroundStyle(.red)
                }
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 6)
                        .fill(Color.red.opacity(colorScheme == .dark ? 0.16 : 0.09))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 6)
                        .stroke(Color.red.opacity(0.3), lineWidth: 1)
                )
            }

            if entry.canDecide {
                HStack(spacing: 8) {
                    Button {
                        onDecide(.approve)
                    } label: {
                        Text("Approve")
                            .fontWeight(.medium)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(.accentColor)
                    .controlSize(.small)
                    .accessibilityLabel("Approve \(entry.summary)")

                    Button("Reject") {
                        onDecide(.reject)
                    }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                    .accessibilityLabel("Reject \(entry.summary)")
                }
                .padding(.top, 2)
            }
        }
        .padding(12)
        .background(
            RoundedRectangle(cornerRadius: 10)
                .fill(Color.secondary.opacity(0.06))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 10)
                .stroke(Color.secondary.opacity(0.2), lineWidth: 1)
        )
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Proposal: \(entry.summary), \(entry.stateLabel)")
    }

    private var badgeBaseColor: Color {
        switch entry.stateTone {
        case .info: return .blue
        case .muted: return .secondary
        case .success: return .green
        case .error: return .red
        case .warning: return .orange
        }
    }

    private var badgeBackgroundColor: Color {
        badgeBaseColor.opacity(colorScheme == .dark ? 0.22 : 0.12)
    }

    private var badgeBorderColor: Color {
        badgeBaseColor.opacity(colorScheme == .dark ? 0.45 : 0.28)
    }

    private var badgeForegroundColor: Color {
        if colorScheme == .dark {
            switch entry.stateTone {
            case .info: return Color(red: 0.56, green: 0.80, blue: 1.0)
            case .muted: return .secondary
            case .success: return Color(red: 0.48, green: 0.90, blue: 0.58)
            case .error: return Color(red: 1.0, green: 0.55, blue: 0.52)
            case .warning: return Color(red: 1.0, green: 0.78, blue: 0.42)
            }
        } else {
            switch entry.stateTone {
            case .info: return Color(red: 0.04, green: 0.34, blue: 0.72)
            case .muted: return .secondary
            case .success: return Color(red: 0.08, green: 0.46, blue: 0.20)
            case .error: return Color(red: 0.72, green: 0.11, blue: 0.11)
            case .warning: return Color(red: 0.66, green: 0.36, blue: 0.0)
            }
        }
    }
}

struct NoticeEntryView: View {
    let entry: ChatAgentNoticeEntry

    var body: some View {
        Text(entry.content)
            .font(.caption)
            .foregroundStyle(entry.isError ? .red : .secondary)
            .multilineTextAlignment(.center)
            .frame(maxWidth: .infinity)
            .padding(.horizontal, 12)
            .padding(.vertical, 2)
            .accessibilityLabel(entry.content)
    }
}

struct ContextMarkerView: View {
    let marker: ChatContextMarkerEntry

    var body: some View {
        HStack(spacing: 10) {
            Rectangle().fill(Color.secondary.opacity(0.25)).frame(height: 1)
            Text(marker.content)
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize()
            Rectangle().fill(Color.secondary.opacity(0.25)).frame(height: 1)
        }
        .padding(.vertical, 6)
        .accessibilityLabel(marker.content)
    }
}

struct TurnUsageView: View {
    let usage: TurnUsage

    var body: some View {
        HStack(spacing: 8) {
            Text("Tokens: \(formatTokenCount(usage.inputTokens)) in · \(formatTokenCount(usage.outputTokens)) out")
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .trailing)
        .accessibilityLabel("Token usage: \(usage.inputTokens) input, \(usage.outputTokens) output")
    }
}

struct ErrorBannerView: View {
    let message: String
    let onDismiss: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: "exclamationmark.circle.fill")
                .foregroundStyle(.red)
            Text(message)
                .font(.caption)
                .foregroundStyle(.primary)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button {
                onDismiss()
            } label: {
                Image(systemName: "xmark")
                    .font(.caption)
            }
            .buttonStyle(.borderless)
            .accessibilityLabel("Dismiss error")
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(Color.red.opacity(0.1))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(Color.red.opacity(0.25), lineWidth: 1)
        )
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Error: \(message)")
    }
}
