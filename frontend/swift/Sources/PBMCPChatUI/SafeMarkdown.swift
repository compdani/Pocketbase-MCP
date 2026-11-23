import Foundation
import MarkdownUI
import SwiftUI

/// Safety helpers for model-authored Markdown:
/// - GFM tables, code blocks, and line breaks render.
/// - Raw HTML in model output is shown as literal text, never interpreted as HTML.
/// - Links and images (both inline and reference-style) only allow `http`, `https`,
///   `mailto`, and `#` fragment targets.
public enum SafeMarkdownSanitizer {
    public static let allowedSchemes: Set<String> = ["http", "https", "mailto"]

    /// Returns `true` only for `http`, `https`, `mailto`, or `#` fragment URLs.
    public static func isAllowedURL(_ url: URL) -> Bool {
        if let scheme = url.scheme?.lowercased(), !scheme.isEmpty {
            return allowedSchemes.contains(scheme)
        }
        return url.absoluteString.hasPrefix("#")
    }

    /// Returns `true` only for `http:`, `https:`, `mailto:`, or `#` fragment URL strings.
    public static func isAllowedURLString(_ raw: String) -> Bool {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return false }
        if trimmed.hasPrefix("#") { return true }
        guard let colon = trimmed.firstIndex(of: ":") else { return false }
        let scheme = String(trimmed[..<colon]).lowercased()
        // Reject scheme strings containing whitespace or control chars (e.g. "java\tscript:").
        guard scheme.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "+" || $0 == "-" || $0 == ".") }) else {
            return false
        }
        return allowedSchemes.contains(scheme)
    }

    /// Sanitize model-authored Markdown so raw HTML tags render as literal text
    /// and disallowed inline or reference-style link/image schemes are neutralized,
    /// while leaving fenced and indented code blocks and inline code spans intact.
    public static func sanitize(_ source: String) -> String {
        guard !source.isEmpty else { return "" }

        let normalized = source
            .replacingOccurrences(of: "\r\n", with: "\n")
            .replacingOccurrences(of: "\r", with: "\n")
        let lines = normalized.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)

        // Pass 1: classify code-block lines and collect reference-link definitions.
        var isCodeLine = Array(repeating: false, count: lines.count)
        var disallowedRefLabels = Set<String>()
        var disallowedDefLineIndices = Set<Int>()

        var activeFenceChar: Character? = nil
        var activeFenceCount: Int = 0
        var prevBlankOrCode = true

        for idx in lines.indices {
            let line = lines[idx]
            if let fence = parseFenceDelimiter(line) {
                if activeFenceChar == nil {
                    activeFenceChar = fence.char
                    activeFenceCount = fence.count
                } else if fence.char == activeFenceChar && fence.count >= activeFenceCount && fence.isClosingOnly {
                    activeFenceChar = nil
                    activeFenceCount = 0
                }
                isCodeLine[idx] = true
                prevBlankOrCode = false
                continue
            }

            if activeFenceChar != nil {
                isCodeLine[idx] = true
                prevBlankOrCode = true
                continue
            }

            // Indented code block (4+ leading spaces or leading tab, following a blank line or code block).
            if prevBlankOrCode && isIndentedCodeLine(line) {
                isCodeLine[idx] = true
                prevBlankOrCode = true
                continue
            }

            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty {
                prevBlankOrCode = true
                continue
            }

            if let refDef = parseLinkReferenceDefinition(line) {
                if !isAllowedURLString(refDef.destination) {
                    disallowedRefLabels.insert(refDef.normalizedLabel)
                    disallowedDefLineIndices.insert(idx)
                }
                prevBlankOrCode = false
                continue
            }

            prevBlankOrCode = false
        }

        // Pass 2: sanitize non-code prose lines and drop disallowed reference definitions.
        var outputLines: [String] = []
        outputLines.reserveCapacity(lines.count)

        for idx in lines.indices {
            if disallowedDefLineIndices.contains(idx) {
                continue
            }
            if isCodeLine[idx] {
                outputLines.append(lines[idx])
            } else {
                outputLines.append(sanitizeNonCodeLine(lines[idx], disallowedRefLabels: disallowedRefLabels))
            }
        }

        // Trim trailing blank lines introduced solely by stripping trailing disallowed definitions.
        while !disallowedDefLineIndices.isEmpty,
              let last = outputLines.last,
              last.trimmingCharacters(in: .whitespaces).isEmpty {
            outputLines.removeLast()
        }

        return outputLines.joined(separator: "\n")
    }

    private struct FenceInfo {
        let char: Character
        let count: Int
        let isClosingOnly: Bool
    }

    private struct LinkRefDefinition {
        let normalizedLabel: String
        let destination: String
    }

    private static func normalizeRefLabel(_ raw: String) -> String {
        raw.trimmingCharacters(in: .whitespacesAndNewlines)
            .split(whereSeparator: { $0.isWhitespace })
            .joined(separator: " ")
            .lowercased()
    }

    private static func isIndentedCodeLine(_ line: String) -> Bool {
        if line.hasPrefix("\t") { return !line.trimmingCharacters(in: .whitespaces).isEmpty }
        var spaces = 0
        for ch in line {
            if ch == " " {
                spaces += 1
                if spaces >= 4 {
                    return !line.trimmingCharacters(in: .whitespaces).isEmpty
                }
            } else {
                return false
            }
        }
        return false
    }

    /// Parse `[label]: destination` (up to 3 leading spaces).
    private static func parseLinkReferenceDefinition(_ line: String) -> LinkRefDefinition? {
        var idx = line.startIndex
        var leadingSpaces = 0
        while idx < line.endIndex && line[idx] == " " && leadingSpaces < 4 {
            leadingSpaces += 1
            idx = line.index(after: idx)
        }
        guard leadingSpaces <= 3, idx < line.endIndex, line[idx] == "[" else { return nil }

        let labelStart = line.index(after: idx)
        var closeIdx = labelStart
        while closeIdx < line.endIndex {
            if line[closeIdx] == "\\" && line.index(after: closeIdx) < line.endIndex {
                closeIdx = line.index(closeIdx, offsetBy: 2)
                continue
            }
            if line[closeIdx] == "]" { break }
            closeIdx = line.index(after: closeIdx)
        }
        guard closeIdx < line.endIndex, line[closeIdx] == "]" else { return nil }
        let label = normalizeRefLabel(String(line[labelStart..<closeIdx]))
        guard !label.isEmpty else { return nil }

        let colonIdx = line.index(after: closeIdx)
        guard colonIdx < line.endIndex, line[colonIdx] == ":" else { return nil }

        let rest = line[line.index(after: colonIdx)...].trimmingCharacters(in: .whitespaces)
        guard !rest.isEmpty else { return nil }

        let destination: String
        if rest.hasPrefix("<"), let gt = rest.firstIndex(of: ">") {
            destination = String(rest[rest.index(after: rest.startIndex)..<gt])
        } else {
            destination = String(rest.prefix(while: { !$0.isWhitespace }))
        }
        guard !destination.isEmpty else { return nil }
        return LinkRefDefinition(normalizedLabel: label, destination: destination)
    }

    private static func parseFenceDelimiter(_ line: String) -> FenceInfo? {
        var idx = line.startIndex
        var leadingSpaces = 0
        while idx < line.endIndex && line[idx] == " " && leadingSpaces < 4 {
            leadingSpaces += 1
            idx = line.index(after: idx)
        }
        guard leadingSpaces <= 3, idx < line.endIndex else { return nil }
        let ch = line[idx]
        guard ch == "`" || ch == "~" else { return nil }
        var count = 0
        while idx < line.endIndex && line[idx] == ch {
            count += 1
            idx = line.index(after: idx)
        }
        guard count >= 3 else { return nil }
        let rest = line[idx...].trimmingCharacters(in: .whitespaces)
        return FenceInfo(char: ch, count: count, isClosingOnly: rest.isEmpty)
    }

    /// Process one line outside code blocks: skip inline code spans (`...`),
    /// escape raw HTML `<...` tokens as literal text, and neutralize disallowed inline
    /// and reference-style links/images.
    private static func sanitizeNonCodeLine(_ line: String, disallowedRefLabels: Set<String>) -> String {
        var out = ""
        var i = line.startIndex

        while i < line.endIndex {
            // Inline code span: preserve verbatim.
            if line[i] == "`" {
                var tickEnd = i
                var ticks = 0
                while tickEnd < line.endIndex && line[tickEnd] == "`" {
                    ticks += 1
                    tickEnd = line.index(after: tickEnd)
                }
                let delimiter = String(repeating: "`", count: ticks)
                if let closeRange = findClosingBackticks(in: line, from: tickEnd, count: ticks) {
                    out += String(line[i..<closeRange.upperBound])
                    i = closeRange.upperBound
                    continue
                } else {
                    out += delimiter
                    i = tickEnd
                    continue
                }
            }

            // Markdown link or image: inline `[label](dest)` or reference `[label][ref]` / `[label][]` / `[label]`.
            if line[i] == "[" || (line[i] == "!" && line.index(after: i) < line.endIndex && line[line.index(after: i)] == "[") {
                if let replacement = sanitizeMarkdownLinkOrImage(in: line, from: i, disallowedRefLabels: disallowedRefLabels) {
                    out += replacement.text
                    i = replacement.nextIndex
                    continue
                }
            }

            // Raw HTML tag / comment / declaration / processing instruction / unsafe autolink.
            if line[i] == "<" {
                if let autolink = matchAllowedAutolink(in: line, from: i) {
                    out += String(line[i..<autolink.upperBound])
                    i = autolink.upperBound
                    continue
                }
                if looksLikeHTMLOrAutolink(in: line, from: i) {
                    out += "\\<"
                    i = line.index(after: i)
                    continue
                }
            }

            out.append(line[i])
            i = line.index(after: i)
        }

        return out
    }

    private static func findClosingBackticks(in line: String, from start: String.Index, count: Int) -> Range<String.Index>? {
        var i = start
        while i < line.endIndex {
            if line[i] == "`" {
                var end = i
                var n = 0
                while end < line.endIndex && line[end] == "`" {
                    n += 1
                    end = line.index(after: end)
                }
                if n == count {
                    return i..<end
                }
                i = end
            } else {
                i = line.index(after: i)
            }
        }
        return nil
    }

    private static func looksLikeHTMLOrAutolink(in line: String, from start: String.Index) -> Bool {
        let next = line.index(after: start)
        guard next < line.endIndex else { return false }
        let c = line[next]
        return c.isLetter || c == "/" || c == "!" || c == "?"
    }

    private static func matchAllowedAutolink(in line: String, from start: String.Index) -> Range<String.Index>? {
        let contentStart = line.index(after: start)
        guard contentStart < line.endIndex,
              let close = line[contentStart...].firstIndex(of: ">") else {
            return nil
        }
        let inner = String(line[contentStart..<close])
        guard !inner.contains(where: { $0.isWhitespace || $0 == "<" }) else { return nil }
        let lower = inner.lowercased()
        if lower.hasPrefix("http://") || lower.hasPrefix("https://") || lower.hasPrefix("mailto:") {
            return start..<line.index(after: close)
        }
        return nil
    }

    private static func findMatchingBracket(in line: String, openIndex: String.Index) -> String.Index? {
        var idx = line.index(after: openIndex)
        var depth = 1
        while idx < line.endIndex && depth > 0 {
            if line[idx] == "\\" && line.index(after: idx) < line.endIndex {
                idx = line.index(idx, offsetBy: 2)
                continue
            }
            if line[idx] == "[" { depth += 1 }
            else if line[idx] == "]" { depth -= 1 }
            if depth > 0 { idx = line.index(after: idx) }
        }
        return depth == 0 && idx < line.endIndex ? idx : nil
    }

    private static func sanitizeMarkdownLinkOrImage(
        in line: String,
        from start: String.Index,
        disallowedRefLabels: Set<String>
    ) -> (text: String, nextIndex: String.Index)? {
        let isImage = line[start] == "!"
        let bracketStart = isImage ? line.index(after: start) : start
        guard bracketStart < line.endIndex && line[bracketStart] == "[" else { return nil }
        guard let closeBracket = findMatchingBracket(in: line, openIndex: bracketStart) else { return nil }

        let labelRaw = String(line[line.index(after: bracketStart)..<closeBracket])
        let afterBracket = line.index(after: closeBracket)

        // 1. Inline link/image: `[label](target)` / `![alt](target)`
        if afterBracket < line.endIndex && line[afterBracket] == "(" {
            var pIdx = line.index(after: afterBracket)
            var pDepth = 1
            while pIdx < line.endIndex && pDepth > 0 {
                if line[pIdx] == "\\" && line.index(after: pIdx) < line.endIndex {
                    pIdx = line.index(pIdx, offsetBy: 2)
                    continue
                }
                if line[pIdx] == "(" { pDepth += 1 }
                else if line[pIdx] == ")" { pDepth -= 1 }
                if pDepth > 0 { pIdx = line.index(after: pIdx) }
            }
            guard pDepth == 0, pIdx < line.endIndex else { return nil }

            let targetRaw = String(line[line.index(after: afterBracket)..<pIdx])
            let nextIndex = line.index(after: pIdx)
            let destTrimmed = targetRaw.trimmingCharacters(in: .whitespaces)
            let destination: String
            if destTrimmed.hasPrefix("<"), let gt = destTrimmed.firstIndex(of: ">") {
                destination = String(destTrimmed[destTrimmed.index(after: destTrimmed.startIndex)..<gt])
            } else {
                destination = String(destTrimmed.prefix(while: { !$0.isWhitespace }))
            }
            let sanitizedLabel = sanitizeNonCodeLine(labelRaw, disallowedRefLabels: disallowedRefLabels)

            if isAllowedURLString(destination) {
                let prefix = isImage ? "!" : ""
                return ("\(prefix)[\(sanitizedLabel)](\(targetRaw))", nextIndex)
            } else {
                return (sanitizedLabel.isEmpty ? destination : sanitizedLabel, nextIndex)
            }
        }

        // 2. Full or collapsed reference link/image: `[label][ref]` or `[label][]`
        if !disallowedRefLabels.isEmpty,
           afterBracket < line.endIndex,
           line[afterBracket] == "[",
           let closeRef = findMatchingBracket(in: line, openIndex: afterBracket) {
            let refRaw = String(line[line.index(after: afterBracket)..<closeRef])
            let key = normalizeRefLabel(refRaw.isEmpty ? labelRaw : refRaw)
            if disallowedRefLabels.contains(key) {
                let sanitizedLabel = sanitizeNonCodeLine(labelRaw, disallowedRefLabels: disallowedRefLabels)
                return (sanitizedLabel, line.index(after: closeRef))
            }
        }

        // 3. Shortcut reference link/image: `[label]` / `![alt]`
        if !disallowedRefLabels.isEmpty {
            let key = normalizeRefLabel(labelRaw)
            if disallowedRefLabels.contains(key) {
                let sanitizedLabel = sanitizeNonCodeLine(labelRaw, disallowedRefLabels: disallowedRefLabels)
                return (sanitizedLabel, afterBracket)
            }
        }

        return nil
    }
}

/// Default SwiftUI Markdown renderer powered by `MarkdownUI` with raw-HTML escaping,
/// strict link scheme filtering (`http`, `https`, `mailto`, `#`), styled code blocks,
/// and readable GFM tables.
public struct SafeMarkdownView: View {
    public let markdown: String
    public let onLinkTap: ((URL) -> Void)?

    public init(_ markdown: String, onLinkTap: ((URL) -> Void)? = nil) {
        self.markdown = markdown
        self.onLinkTap = onLinkTap
    }

    public var body: some View {
        Markdown(SafeMarkdownSanitizer.sanitize(markdown))
            .markdownSoftBreakMode(.lineBreak)
            .markdownTextStyle {
                FontSize(14)
            }
            .markdownBlockStyle(\.codeBlock) { configuration in
                ScrollView(.horizontal, showsIndicators: true) {
                    configuration.label
                        .relativeLineSpacing(.em(0.22))
                        .markdownTextStyle {
                            FontFamilyVariant(.monospaced)
                            FontSize(.em(0.88))
                        }
                        .padding(12)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 8)
                        .fill(Color.secondary.opacity(0.09))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .stroke(Color.secondary.opacity(0.18), lineWidth: 1)
                )
                .markdownMargin(top: 4, bottom: 10)
            }
            .markdownBlockStyle(\.table) { configuration in
                configuration.label
                    .markdownTableBorderStyle(.init(color: Color.secondary.opacity(0.25)))
                    .markdownTableBackgroundStyle(
                        .alternatingRows(Color.clear, Color.secondary.opacity(0.05))
                    )
                    .markdownMargin(top: 4, bottom: 10)
            }
            .markdownBlockStyle(\.tableCell) { configuration in
                configuration.label
                    .markdownTextStyle {
                        if configuration.row == 0 {
                            FontWeight(.semibold)
                        }
                    }
                    .padding(.vertical, 6)
                    .padding(.horizontal, 12)
            }
            .textSelection(.enabled)
            .environment(\.openURL, OpenURLAction { url in
                guard SafeMarkdownSanitizer.isAllowedURL(url) else {
                    return .discarded
                }
                if let onLinkTap {
                    onLinkTap(url)
                    return .handled
                }
                return .systemAction
            })
    }
}
