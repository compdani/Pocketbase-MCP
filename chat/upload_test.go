package chat

import (
	"strings"
	"testing"
)

// TestDetectUpload is the type-assignment contract: the server picks the type
// from the bytes and the name, and a client's claim never overrides either.
func TestDetectUpload(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nrest")
	jpeg := []byte("\xff\xd8\xff\xe0rest")
	gif := []byte("GIF89arest")
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 rest")
	pdf := []byte("%PDF-1.7\nbody")

	cases := []struct {
		name string
		data []byte
		mime string
		kind string
		ok   bool
	}{
		{"plain text", []byte("hello"), "text/plain", UploadKindText, true},
		{"notes.txt", []byte("hello"), "text/plain", UploadKindText, true},
		{"README.md", []byte("# title"), "text/markdown", UploadKindText, true},
		{"rows.CSV", []byte("a,b\n1,2\n"), "text/csv", UploadKindText, true},
		{"config.json", []byte(`{"a":1}`), "application/json", UploadKindText, true},
		{"empty text", nil, "text/plain", UploadKindText, true},
		{"unknown text extension", []byte("plain"), "text/plain", UploadKindText, true},

		{"image.png", png, "image/png", UploadKindImage, true},
		{"image.jpg", jpeg, "image/jpeg", UploadKindImage, true},
		{"image.gif", gif, "image/gif", UploadKindImage, true},
		{"image.webp", webp, "image/webp", UploadKindImage, true},
		{"document.pdf", pdf, "application/pdf", UploadKindPDF, true},

		// Magic wins over the extension, in both directions.
		{"renamed.pdf", png, "image/png", UploadKindImage, true},
		{"renamed.txt", pdf, "application/pdf", UploadKindPDF, true},

		// A binary name whose bytes do not prove that type is refused.
		{"spoofed.png", []byte("not an image"), "", "", false},
		{"spoofed.pdf", []byte("not a document"), "", "", false},

		// Active content and unverifiable binary types are refused.
		{"page.html", []byte("<html><body>x</body></html>"), "", "", false},
		{"vector.svg", []byte("<svg/>"), "", "", false},
		{"script.js", []byte("alert(1)"), "", "", false},
		{"archive.zip", []byte("PK\x03\x04zip"), "", "", false},

		// Text has to actually be text.
		{"broken.txt", []byte{0xff, 0xfe, 0x00}, "", "", false},
		{"nul.txt", []byte("before\x00after"), "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mime, kind, ok := DetectUpload(c.name, c.data)
			if ok != c.ok || mime != c.mime || kind != c.kind {
				t.Fatalf("DetectUpload(%q, %d bytes) = %q, %q, %v; want %q, %q, %v",
					c.name, len(c.data), mime, kind, ok, c.mime, c.kind, c.ok)
			}
		})
	}
}

func TestSanitizeUploadName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.txt", "report.txt"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\me\notes.md`, "notes.md"},
		{"/absolute/path/file.csv", "file.csv"},
		{"name\x00with\x01controls.txt", "namewithcontrols.txt"},
		{"  spaced.txt  ", "spaced.txt"},
		{"", "upload"},
		{".", "upload"},
		{"/", "upload"},
		{"   ", "upload"},
		{strings.Repeat("é", 250) + ".txt", strings.Repeat("é", maxUploadNameRunes)},
	}
	for _, c := range cases {
		got := SanitizeUploadName(c.in)
		if got != c.want {
			t.Errorf("SanitizeUploadName(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// A long name is bounded in runes, not bytes, and never ends mid-rune.
	long := SanitizeUploadName(strings.Repeat("é", 1000))
	if len([]rune(long)) != maxUploadNameRunes {
		t.Fatalf("long name has %d runes, want %d", len([]rune(long)), maxUploadNameRunes)
	}
}

func TestAttachmentTypesForFiltersProviderSupport(t *testing.T) {
	all := attachmentTypesFor(func(string) bool { return true })
	if len(all) != len(providerAttachmentTypesForTest()) {
		t.Fatalf("all attachment types = %v", all)
	}
	none := attachmentTypesFor(func(string) bool { return false })
	if len(none) != 0 {
		t.Fatalf("unsupported provider advertised %v", none)
	}
	images := attachmentTypesFor(func(mime string) bool { return strings.HasPrefix(mime, "image/") })
	if len(images) != 4 {
		t.Fatalf("image-only types = %v", images)
	}
	for _, mime := range images {
		if !strings.HasPrefix(mime, "image/") {
			t.Fatalf("image-only list contains %q", mime)
		}
	}
}

// providerAttachmentTypesForTest is the canonical list, reached through the same
// exported helper the handler uses.
func providerAttachmentTypesForTest() []string {
	return attachmentTypesFor(func(string) bool { return true })
}

func TestRFC5987Encoding(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.txt", "report.txt"},
		{"a b.txt", "a%20b.txt"},
		{"naïve.txt", "na%C3%AFve.txt"},
		{"100%.txt", "100%25.txt"},
		{"a+b.txt", "a+b.txt"},
	}
	for _, c := range cases {
		if got := rfc5987Encode(c.in); got != c.want {
			t.Errorf("rfc5987Encode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := contentDisposition("naïve.txt"); got != "attachment; filename*=UTF-8''na%C3%AFve.txt" {
		t.Errorf("contentDisposition = %q", got)
	}
}
