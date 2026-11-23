import Foundation
import PBMCPChat
import SwiftUI
import UniformTypeIdentifiers

/// Multi-line chat composer with ⌘↩ send, paste/drop/picker attachments, mention
/// trigger indicator, model & reasoning effort menus, context ring + Compact popover,
/// cancel while streaming, and disabled state.
struct ComposerView: View {
    @Bindable var viewModel: ChatPageViewModel
    let placeholder: String

    @State private var showFileImporter: Bool = false
    @State private var showContextPopover: Bool = false
    @State private var isDropTargeted: Bool = false

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if !viewModel.pendingAttachments.isEmpty {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 6) {
                        ForEach(viewModel.pendingAttachments) { item in
                            attachmentChip(item)
                        }
                    }
                }
            }

            if viewModel.mentionState.isOpen {
                HStack(spacing: 6) {
                    Image(systemName: "at")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(viewModel.mentionState.query.isEmpty ? "Mention…" : "Mention: \(viewModel.mentionState.query)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(Capsule().fill(Color.secondary.opacity(0.1)))
                .accessibilityLabel("Mention active: \(viewModel.mentionState.query)")
            }

            ZStack(alignment: .topLeading) {
                TextEditor(text: $viewModel.draft)
                    .font(.body)
                    .scrollContentBackground(.hidden)
                    .frame(minHeight: 24, maxHeight: 140)
                    .fixedSize(horizontal: false, vertical: true)
                    .disabled(viewModel.isComposerDisabled)
                    .accessibilityLabel(placeholder)

                if viewModel.draft.isEmpty {
                    Text(placeholder)
                        .font(.body)
                        .foregroundStyle(.tertiary)
                        .padding(.horizontal, 5)
                        .padding(.vertical, 2)
                        .allowsHitTesting(false)
                }
            }

            HStack(spacing: 8) {
                if viewModel.attachmentsEnabled {
                    Button {
                        showFileImporter = true
                    } label: {
                        Image(systemName: "paperclip")
                    }
                    .buttonStyle(.borderless)
                    .disabled(viewModel.isComposerDisabled || viewModel.isRunning)
                    .accessibilityLabel("Attach files")
                    .help("Attach files")
                }

                modelMenu
                effortMenu

                Spacer(minLength: 4)

                if viewModel.shouldShowContextRing, let usage = viewModel.contextUsage {
                    contextRingButton(usage: usage)
                        .fixedSize()
                        .layoutPriority(1)
                }

                if viewModel.isRunning {
                    Button {
                        Task { await viewModel.stop() }
                    } label: {
                        Label("Stop", systemImage: "stop.fill")
                    }
                    .buttonStyle(.bordered)
                    .controlSize(.regular)
                    .fixedSize()
                    .layoutPriority(2)
                    .accessibilityLabel("Stop generating")
                } else {
                    Button {
                        Task { _ = await viewModel.submit() }
                    } label: {
                        Label("Send", systemImage: "arrow.up.circle.fill")
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.regular)
                    .fixedSize()
                    .layoutPriority(2)
                    .disabled(!viewModel.canSend)
                    .keyboardShortcut(.return, modifiers: .command)
                    .accessibilityLabel("Send message")
                }
            }
        }
        .padding(10)
        .background(
            RoundedRectangle(cornerRadius: 12)
                .fill(Color.secondary.opacity(0.06))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 12)
                .stroke(
                    isDropTargeted ? Color.accentColor : Color.secondary.opacity(0.22),
                    lineWidth: isDropTargeted ? 2 : 1
                )
        )
        .onDrop(of: [.fileURL, .image, .plainText, .data], isTargeted: $isDropTargeted) { providers in
            guard viewModel.attachmentsEnabled, !viewModel.isComposerDisabled else { return false }
            handleDroppedProviders(providers)
            return true
        }
        #if os(macOS)
        .onPasteCommand(of: [.fileURL, .png, .jpeg, .tiff, .pdf]) { providers in
            guard viewModel.attachmentsEnabled, !viewModel.isComposerDisabled else { return }
            handlePastedProviders(providers)
        }
        #endif
        .fileImporter(
            isPresented: $showFileImporter,
            allowedContentTypes: allowedContentTypes,
            allowsMultipleSelection: true
        ) { result in
            guard case .success(let urls) = result else { return }
            importURLs(urls)
        }
    }

    // MARK: - Subviews

    @ViewBuilder
    private func attachmentChip(_ item: ComposerAttachment) -> some View {
        HStack(spacing: 5) {
            if item.isBusy {
                ProgressView().controlSize(.mini)
            } else if item.error != nil {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(.red)
                    .font(.caption2)
            } else {
                Image(systemName: "doc")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            Text(item.file.name)
                .font(.caption)
                .lineLimit(1)
            Button {
                viewModel.removeAttachment(id: item.id)
            } label: {
                Image(systemName: "xmark")
                    .font(.caption2)
            }
            .buttonStyle(.borderless)
            .disabled(item.isBusy)
            .accessibilityLabel("Remove attachment \(item.file.name)")
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
        .background(
            Capsule().fill(item.error != nil ? Color.red.opacity(0.12) : Color.secondary.opacity(0.12))
        )
        .help(item.error ?? item.file.name)
    }

    @ViewBuilder
    private var modelMenu: some View {
        let options = viewModel.modelOptions
        if !options.isEmpty {
            let currentLabel = options.first(where: { $0.id == viewModel.effectiveModelId })?.label
                ?? options.first?.label
                ?? "Model"
            let grouped = groupedModels(options)

            Menu {
                ForEach(grouped, id: \.group) { section in
                    if let groupName = section.group {
                        Section(groupName) {
                            ForEach(section.items) { option in
                                modelMenuButton(option)
                            }
                        }
                    } else {
                        ForEach(section.items) { option in
                            modelMenuButton(option)
                        }
                    }
                }
            } label: {
                HStack(spacing: 4) {
                    Text(currentLabel)
                        .font(.caption)
                        .lineLimit(1)
                        .truncationMode(.tail)
                    Image(systemName: "chevron.up.chevron.down")
                        .font(.caption2)
                }
            }
            .menuStyle(.borderlessButton)
            .disabled(viewModel.isComposerDisabled)
            .accessibilityLabel("Select model, current: \(currentLabel)")
        }
    }

    @ViewBuilder
    private func modelMenuButton(_ option: ChatAgentModelOption) -> some View {
        Button {
            viewModel.selectModel(option.id)
        } label: {
            if option.id == viewModel.effectiveModelId {
                Label(option.label, systemImage: "checkmark")
            } else {
                Text(option.label)
            }
        }
        .disabled(option.disabled)
    }

    @ViewBuilder
    private var effortMenu: some View {
        if let option = viewModel.effortOption {
            let currentLabel = option.choices.first(where: { $0.value == viewModel.selectedEffort })?.label
                ?? "Model default"
            Menu {
                ForEach(option.choices) { choice in
                    Button {
                        viewModel.selectEffort(choice.value)
                    } label: {
                        if choice.value == viewModel.selectedEffort {
                            Label(choice.label, systemImage: "checkmark")
                        } else {
                            Text(choice.label)
                        }
                    }
                }
            } label: {
                HStack(spacing: 4) {
                    Image(systemName: "slider.horizontal.3")
                        .font(.caption2)
                    Text(currentLabel)
                        .font(.caption)
                        .lineLimit(1)
                        .truncationMode(.tail)
                }
            }
            .menuStyle(.borderlessButton)
            .disabled(viewModel.isComposerDisabled)
            .accessibilityLabel("Reasoning effort, current: \(currentLabel)")
        }
    }

    @ViewBuilder
    private func contextRingButton(usage: ChatContextUsageView) -> some View {
        Button {
            showContextPopover.toggle()
        } label: {
            HStack(spacing: 5) {
                ZStack {
                    Circle()
                        .stroke(Color.secondary.opacity(0.25), lineWidth: 2)
                    if usage.compacting {
                        ProgressView().controlSize(.mini)
                    } else {
                        Circle()
                            .trim(from: 0, to: usage.share)
                            .stroke(
                                usage.share >= 0.85 ? Color.orange : Color.accentColor,
                                style: StrokeStyle(lineWidth: 2, lineCap: .round)
                            )
                            .rotationEffect(.degrees(-90))
                    }
                }
                .frame(width: 14, height: 14)

                Text(usage.compacting ? "Compacting…" : "\(usage.percent)%")
                    .font(.caption2)
                    .fontWeight(.medium)
                    .foregroundStyle(.secondary)
                    .monospacedDigit()
            }
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(Capsule().fill(Color.secondary.opacity(0.1)))
        }
        .buttonStyle(.plain)
        .accessibilityLabel(usage.compacting ? "Compacting context" : "Context \(usage.percent)% used")
        .popover(isPresented: $showContextPopover) {
            VStack(alignment: .leading, spacing: 8) {
                Text("Context window")
                    .font(.headline)
                Text(
                    "\(formatTokenCount(usage.used)) / \(formatTokenCount(usage.window)) tokens (\(usage.percent)%)"
                        + (usage.estimated ? " (estimated)" : "")
                )
                .font(.caption)
                .foregroundStyle(.secondary)
                if let summarizedAt = usage.summarizedAt, !summarizedAt.isEmpty {
                    Text("Summarized: \(summarizedAt)")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
                if usage.canCompact {
                    Button(usage.compacting ? "Compacting…" : "Compact") {
                        Task { await viewModel.compact() }
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
                    .disabled(usage.compacting || viewModel.isRunning)
                    .accessibilityLabel("Compact conversation")
                }
            }
            .padding(12)
            .frame(minWidth: 220)
        }
    }

    // MARK: - Helpers

    private struct ModelSection {
        let group: String?
        let items: [ChatAgentModelOption]
    }

    private func groupedModels(_ options: [ChatAgentModelOption]) -> [ModelSection] {
        var order: [String?] = []
        var buckets: [String?: [ChatAgentModelOption]] = [:]
        for opt in options {
            if buckets[opt.group] == nil {
                order.append(opt.group)
            }
            buckets[opt.group, default: []].append(opt)
        }
        return order.map { ModelSection(group: $0, items: buckets[$0] ?? []) }
    }

    private var allowedContentTypes: [UTType] {
        guard let mimes = viewModel.state.limits?.attachmentTypes, !mimes.isEmpty else {
            return [.item]
        }
        let types = mimes.compactMap { UTType(mimeType: $0) }
        return types.isEmpty ? [.item] : types
    }

    private func importURLs(_ urls: [URL]) {
        var files: [ChatUploadFile] = []
        for url in urls {
            let scoped = url.startAccessingSecurityScopedResource()
            defer { if scoped { url.stopAccessingSecurityScopedResource() } }
            guard let data = try? Data(contentsOf: url) else { continue }
            let name = url.lastPathComponent
            let mime = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType
                ?? inferAttachmentMime(name)
            files.append(ChatUploadFile(data: data, name: name, mimeType: mime))
        }
        viewModel.addAttachmentFiles(files)
    }

    private func handleDroppedProviders(_ providers: [NSItemProvider]) {
        for provider in providers {
            if provider.hasItemConformingToTypeIdentifier(UTType.fileURL.identifier) {
                provider.loadItem(forTypeIdentifier: UTType.fileURL.identifier, options: nil) { item, _ in
                    let url: URL?
                    if let u = item as? URL {
                        url = u
                    } else if let data = item as? Data {
                        url = URL(dataRepresentation: data, relativeTo: nil)
                    } else {
                        url = nil
                    }
                    guard let url, let data = try? Data(contentsOf: url) else { return }
                    let name = url.lastPathComponent
                    let mime = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType
                        ?? inferAttachmentMime(name)
                    let upload = ChatUploadFile(data: data, name: name, mimeType: mime)
                    Task { @MainActor in
                        viewModel.addAttachmentFiles([upload])
                    }
                }
            }
        }
    }

    private func handlePastedProviders(_ providers: [NSItemProvider]) {
        for provider in providers {
            if provider.hasItemConformingToTypeIdentifier(UTType.fileURL.identifier) {
                handleDroppedProviders([provider])
            } else if provider.hasItemConformingToTypeIdentifier(UTType.png.identifier) {
                provider.loadDataRepresentation(forTypeIdentifier: UTType.png.identifier) { data, _ in
                    guard let data else { return }
                    let upload = ChatUploadFile(data: data, name: "pasted-image.png", mimeType: "image/png")
                    Task { @MainActor in viewModel.addAttachmentFiles([upload]) }
                }
            } else if provider.hasItemConformingToTypeIdentifier(UTType.jpeg.identifier) {
                provider.loadDataRepresentation(forTypeIdentifier: UTType.jpeg.identifier) { data, _ in
                    guard let data else { return }
                    let upload = ChatUploadFile(data: data, name: "pasted-image.jpg", mimeType: "image/jpeg")
                    Task { @MainActor in viewModel.addAttachmentFiles([upload]) }
                }
            } else if provider.hasItemConformingToTypeIdentifier(UTType.pdf.identifier) {
                provider.loadDataRepresentation(forTypeIdentifier: UTType.pdf.identifier) { data, _ in
                    guard let data else { return }
                    let upload = ChatUploadFile(data: data, name: "pasted.pdf", mimeType: "application/pdf")
                    Task { @MainActor in viewModel.addAttachmentFiles([upload]) }
                }
            }
        }
    }
}
