package chat

import (
	"bytes"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// Upload is one chat attachment. The server decides MIME and Kind from the
// bytes and the sanitized file name; a client's declared content type is never
// trusted, and no public URL is ever formed for an upload. Owner is hidden from
// JSON because it is identity, not content.
type Upload struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	Owner          OwnerKey  `json:"-"`
	Name           string    `json:"name"`
	MIME           string    `json:"mime"`
	Kind           string    `json:"kind"`
	Size           int64     `json:"size"`
	SHA256         string    `json:"sha256"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Upload kinds. They mirror the provider port's classification so the wire
// contract and the provider contract cannot drift.
const (
	UploadKindText  = provider.AttachmentText
	UploadKindImage = provider.AttachmentImage
	UploadKindPDF   = provider.AttachmentPDF
)

const (
	// defaultMaxUploadBytes bounds one uploaded file. Zero in the handler
	// configuration means this value.
	defaultMaxUploadBytes = 10 << 20
	// defaultMaxUploadsPerConversation bounds how many attachments one
	// conversation may keep.
	defaultMaxUploadsPerConversation = 50
	// defaultMaxAttachmentsPerTurn bounds how many uploads one user message may
	// reference.
	defaultMaxAttachmentsPerTurn = 5
	// defaultMaxAttachmentBytesPerRequest bounds the decoded attachment bytes
	// one provider request may carry. Newest messages win the budget.
	defaultMaxAttachmentBytesPerRequest = 20 << 20
	// maxUploadNameRunes bounds a stored attachment name.
	maxUploadNameRunes = 200
	// defaultUploadName replaces an empty or unusable file name.
	defaultUploadName = "upload"
	// maxMultipartOverhead is added to the upload size limit for the multipart
	// boundaries, headers and the conversationId field.
	maxMultipartOverhead = 64 << 10
)

// File magic the validator recognises regardless of the declared name. The
// server types a file by its bytes, so renaming an image to .pdf never changes
// what the model is sent.
var (
	magicPNG  = []byte("\x89PNG\r\n\x1a\n")
	magicJPEG = []byte("\xff\xd8\xff")
	magicGIF  = []byte("GIF8")
	magicPDF  = []byte("%PDF-")
)

// textMIMEs maps a file extension to the text MIME type the server assigns.
// Anything else falls back to text/plain, and is accepted only when the bytes
// really are text.
var textMIMEs = map[string]string{
	".txt":      "text/plain",
	".text":     "text/plain",
	".log":      "text/plain",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".csv":      "text/csv",
	".json":     "application/json",
}

// binaryUploadExtensions are the names whose type must be proven by magic
// bytes. A text file renamed .png or .pdf is refused rather than relabelled:
// the name is a claim the bytes did not support.
var binaryUploadExtensions = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {}, ".pdf": {},
}

// rejectedUploadExtensions are active-content types. Their text is valid UTF-8,
// but a chat attachment is never served inline, so accepting them would only
// carry markup the model should not treat as a document.
var rejectedUploadExtensions = map[string]struct{}{
	".html": {}, ".htm": {}, ".xhtml": {}, ".svg": {}, ".xml": {},
	".js": {}, ".mjs": {}, ".cjs": {}, ".css": {},
}

// DetectUpload validates data and returns the MIME type and kind the server
// assigns to it. The name only chooses among the text types and never decides
// whether a file is binary: magic bytes win, and text has to be valid UTF-8
// without NUL bytes. ok is false for anything the chat surface does not accept.
func DetectUpload(name string, data []byte) (mimeType, kind string, ok bool) {
	return DetectUploadWith(name, data, false)
}

// OtherImageTypes are the image formats AcceptOtherImages stores in addition
// to the canonical ones. Models generally can't read them: a turn sends them
// only to a provider that declares support, and otherwise tells the model the
// file was omitted. SVG is never accepted, because it can carry scripts.
var OtherImageTypes = []string{"image/bmp", "image/tiff", "image/x-icon", "image/heic", "image/avif"}

// DetectUploadWith is DetectUpload that, with otherImages, also recognizes
// OtherImageTypes by their signatures.
func DetectUploadWith(name string, data []byte, otherImages bool) (mimeType, kind string, ok bool) {
	if otherImages {
		if mimeType := otherImageType(data); mimeType != "" {
			return mimeType, UploadKindImage, true
		}
	}
	switch {
	case bytes.HasPrefix(data, magicPNG):
		return "image/png", UploadKindImage, true
	case bytes.HasPrefix(data, magicJPEG):
		return "image/jpeg", UploadKindImage, true
	case bytes.HasPrefix(data, magicGIF):
		return "image/gif", UploadKindImage, true
	case isWEBP(data):
		return "image/webp", UploadKindImage, true
	case bytes.HasPrefix(data, magicPDF):
		return "application/pdf", UploadKindPDF, true
	}

	ext := uploadExtension(name)
	if _, isBinary := binaryUploadExtensions[ext]; isBinary {
		return "", "", false
	}
	if _, rejected := rejectedUploadExtensions[ext]; rejected {
		return "", "", false
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", "", false
	}
	if mimeType, known := textMIMEs[ext]; known {
		return mimeType, UploadKindText, true
	}
	// An unrecognised name only falls back to text/plain when the bytes behave
	// like text; a sniffed binary such as an archive stays refused.
	if contentType := http.DetectContentType(data); !strings.HasPrefix(contentType, "text/") {
		return "", "", false
	}
	return "text/plain", UploadKindText, true
}

// isWEBP reports the RIFF container form of a WebP image.
func isWEBP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

// uploadExtension is the lowercased extension of a sanitized base name, with
// backslashes treated as separators so a Windows client cannot smuggle a path.
func uploadExtension(name string) string {
	return strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/")))
}

// SanitizeUploadName reduces a client-supplied name to a safe base name: no
// directory component, no control characters, at most maxUploadNameRunes runes
// and never empty.
func SanitizeUploadName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == ".." || name == "/" {
		name = ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimSpace(b.String())
	if utf8.RuneCountInString(name) > maxUploadNameRunes {
		runes := []rune(name)
		name = strings.TrimSpace(string(runes[:maxUploadNameRunes]))
	}
	if name == "" {
		return defaultUploadName
	}
	return name
}

// attachmentTypesFor returns the canonical attachment MIME list filtered by the
// provider's declared capability. A provider without AttachmentSupport supports
// none, so /status never advertises a type a turn would refuse.
func attachmentTypesFor(supports func(string) bool) []string {
	out := make([]string, 0, len(provider.AttachmentMIMETypes()))
	for _, mimeType := range provider.AttachmentMIMETypes() {
		if supports(mimeType) {
			out = append(out, mimeType)
		}
	}
	return out
}

// otherImageType recognizes an OtherImageTypes image by its signature.
func otherImageType(data []byte) string {
	switch {
	case len(data) >= 26 && data[0] == 'B' && data[1] == 'M':
		return "image/bmp"
	case bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*")):
		return "image/tiff"
	case len(data) >= 6 && bytes.HasPrefix(data, []byte{0, 0, 1, 0}) && data[4] > 0:
		return "image/x-icon"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		switch string(data[8:12]) {
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "hevc", "heim", "heis", "mif1", "msf1":
			return "image/heic"
		}
	}
	return ""
}

// isOtherImageType reports one of OtherImageTypes.
func isOtherImageType(mimeType string) bool {
	for _, t := range OtherImageTypes {
		if t == mimeType {
			return true
		}
	}
	return false
}
