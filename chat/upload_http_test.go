package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// postMultipart builds one multipart/form-data body. An empty conversationID or
// filename omits that field, which is how a malformed form is exercised.
func (f *apiFixture) postMultipart(token, conversationID, filename string, data []byte, extra map[string]string) apiResponse {
	f.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if conversationID != "" {
		if err := w.WriteField("conversationId", conversationID); err != nil {
			f.t.Fatalf("write field: %v", err)
		}
	}
	for k, v := range extra {
		if err := w.WriteField(k, v); err != nil {
			f.t.Fatalf("write field %s: %v", k, err)
		}
	}
	if filename != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			f.t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			f.t.Fatalf("write file part: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		f.t.Fatalf("close multipart: %v", err)
	}
	return f.callAs(token, http.MethodPost, testPrefix+"/uploads", buf.String(), map[string]string{
		"Content-Type": w.FormDataContentType(),
	})
}

// uploadAs stores one attachment through the API and returns its metadata.
func (f *apiFixture) uploadAs(token, convID, filename string, data []byte) Upload {
	f.t.Helper()
	res := f.postMultipart(token, convID, filename, data, nil).expect(http.StatusCreated)
	var out uploadView
	res.decode(&out)
	return out.Upload
}

func (f *apiFixture) submitTurnWithAttachments(token, convID, text string, ids []string) apiResponse {
	f.t.Helper()
	return f.callAs(token, http.MethodPost, testPrefix+"/conversations/"+convID+"/turns", map[string]any{
		"text":        text,
		"attachments": ids,
	}, nil)
}

func decodeStatus(t *testing.T, res apiResponse) statusView {
	t.Helper()
	var status statusView
	res.decode(&status)
	return status
}

func TestUploadHTTP(t *testing.T) {
	f := newFixture(t, fixtureOptions{uploads: true, maxUploadBytes: 64})
	conv := f.createConversation(map[string]any{"title": "files"})
	text := []byte("hello attachment\n")

	// The status advertises the surface and the server limits.
	status := decodeStatus(t, f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK))
	if !status.Features.Uploads {
		t.Fatal("status does not report uploads")
	}
	if status.Limits.MaxUploadBytes != 64 || status.Limits.MaxAttachmentsPerTurn != defaultMaxAttachmentsPerTurn {
		t.Fatalf("upload limits = %+v", status.Limits)
	}
	if status.Limits.AttachmentTypes == nil || len(*status.Limits.AttachmentTypes) != len(provider.AttachmentMIMETypes()) {
		t.Fatalf("attachmentTypes = %v", status.Limits.AttachmentTypes)
	}

	upload := f.uploadAs(tokenA, conv.ID, "../../etc/notes.txt", text)
	if upload.ID == "" || upload.ConversationID != conv.ID {
		t.Fatalf("stored upload = %+v", upload)
	}
	if upload.Name != "notes.txt" {
		t.Fatalf("name = %q, want the sanitized base name", upload.Name)
	}
	if upload.MIME != "text/plain" || upload.Kind != UploadKindText {
		t.Fatalf("type = %s/%s", upload.MIME, upload.Kind)
	}
	if upload.Size != int64(len(text)) || !strings.HasPrefix(upload.SHA256, "sha256:") {
		t.Fatalf("size/sha = %d %q", upload.Size, upload.SHA256)
	}

	// The download is an opaque, sandboxed, non-cacheable attachment.
	res := f.call(http.MethodGet, testPrefix+"/uploads/"+upload.ID, nil, nil).expect(http.StatusOK)
	if got := string(res.body); got != string(text) {
		t.Fatalf("download body = %q", got)
	}
	wantHeaders := map[string]string{
		"Content-Type":            "text/plain",
		"Content-Disposition":     "attachment; filename*=UTF-8''notes.txt",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Cache-Control":           "private, no-store",
	}
	for k, want := range wantHeaders {
		if got := res.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// Metadata is available without the bytes.
	var meta uploadView
	f.call(http.MethodGet, testPrefix+"/uploads/"+upload.ID+"/meta", nil, nil).expect(http.StatusOK).decode(&meta)
	if meta.Upload.ID != upload.ID || meta.Upload.SHA256 != upload.SHA256 {
		t.Fatalf("meta = %+v", meta.Upload)
	}

	if res := f.call(http.MethodGet, testPrefix+"/uploads/missing", nil, nil); res.status != http.StatusNotFound {
		t.Errorf("missing download = %d", res.status)
	}
	if res := f.call(http.MethodGet, testPrefix+"/uploads/missing/meta", nil, nil); res.status != http.StatusNotFound {
		t.Errorf("missing meta = %d", res.status)
	}

	// Another owner sees exactly a missing record, including on metadata.
	if res := f.callAs(tokenB, http.MethodGet, testPrefix+"/uploads/"+upload.ID, nil, nil); res.status != http.StatusNotFound {
		t.Errorf("foreign download = %d", res.status)
	}
	if res := f.callAs(tokenB, http.MethodGet, testPrefix+"/uploads/"+upload.ID+"/meta", nil, nil); res.status != http.StatusNotFound {
		t.Errorf("foreign meta = %d", res.status)
	}
}

func TestUploadHTTPRejectsContent(t *testing.T) {
	f := newFixture(t, fixtureOptions{uploads: true})
	conv := f.createConversation(nil)

	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"active content", []byte("<html>no</html>"), http.StatusUnsupportedMediaType},
		{"binary", []byte{0xff, 0xfe, 0x00}, http.StatusUnsupportedMediaType},
		{"spoofed image", []byte("not an image"), http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A .html/.bin name and a .png name exercise the extension path.
			name := "page.html"
			switch c.name {
			case "binary":
				name = "blob.bin"
			case "spoofed image":
				name = "fake.png"
			}
			res := f.postMultipart(tokenA, conv.ID, name, c.data, nil)
			if res.status != c.want {
				t.Fatalf("upload = %d, want %d, body %s", res.status, c.want, res.body)
			}
			if code := res.errorCode(); code != CodeUnsupportedMedia {
				t.Fatalf("code = %q", code)
			}
		})
	}

	// The type is assigned from the bytes: a PNG named .pdf is an image.
	png := []byte("\x89PNG\r\n\x1a\nbody")
	upload := f.uploadAs(tokenA, conv.ID, "renamed.pdf", png)
	if upload.MIME != "image/png" || upload.Kind != UploadKindImage {
		t.Fatalf("renamed PNG = %s/%s", upload.MIME, upload.Kind)
	}
}

func TestUploadHTTPFormErrors(t *testing.T) {
	f := newFixture(t, fixtureOptions{uploads: true, maxUploadBytes: 16})
	conv := f.createConversation(nil)

	if res := f.postMultipart(tokenA, "", "a.txt", []byte("x"), nil); res.status != http.StatusBadRequest {
		t.Errorf("missing conversationId = %d", res.status)
	}
	if res := f.postMultipart(tokenA, conv.ID, "", nil, nil); res.status != http.StatusBadRequest {
		t.Errorf("missing file = %d", res.status)
	}
	// A non-multipart body is a plain bad request, not a panic.
	if res := f.call(http.MethodPost, testPrefix+"/uploads", map[string]any{"file": "x"}, nil); res.status != http.StatusBadRequest {
		t.Errorf("json body = %d", res.status)
	}
	// Unknown form fields are ignored, so a normal browser form still works.
	if res := f.postMultipart(tokenA, conv.ID, "a.txt", []byte("x"), map[string]string{"note": "ignored"}); res.status != http.StatusCreated {
		t.Errorf("extra field = %d", res.status)
	}

	// The file limit is enforced exactly.
	if res := f.postMultipart(tokenA, conv.ID, "big.txt", bytes.Repeat([]byte("a"), 17), nil); res.status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d, body %s", res.status, res.body)
	} else if code := res.errorCode(); code != CodePayloadTooLarge {
		t.Errorf("oversize code = %q", code)
	}

	// A conversation that reaches its upload limit refuses more work.
	limited := newFixture(t, fixtureOptions{uploads: true, maxUploadsPerConv: 1})
	lc := limited.createConversation(nil)
	limited.uploadAs(tokenA, lc.ID, "one.txt", []byte("one"))
	res := limited.postMultipart(tokenA, lc.ID, "two.txt", []byte("two"), nil)
	if res.status != http.StatusConflict {
		t.Fatalf("upload over the conversation limit = %d", res.status)
	}
	if code := res.errorCode(); code != CodeTooManyUploads {
		t.Fatalf("limit code = %q", code)
	}
}

func TestUploadHTTPRequiresOwnedTarget(t *testing.T) {
	deny := false
	f := newFixture(t, fixtureOptions{
		uploads: true,
		targetCheck: func(*http.Request, Caller, string) error {
			if deny {
				return errors.New("revoked")
			}
			return nil
		},
	})
	conv := f.createConversation(map[string]any{"title": "t", "target": "record-1"})
	upload := f.uploadAs(tokenA, conv.ID, "a.txt", []byte("hello"))

	deny = true
	if res := f.call(http.MethodGet, testPrefix+"/uploads/"+upload.ID, nil, nil); res.status != http.StatusNotFound {
		t.Errorf("download after target revocation = %d", res.status)
	}
	if res := f.call(http.MethodGet, testPrefix+"/uploads/"+upload.ID+"/meta", nil, nil); res.status != http.StatusNotFound {
		t.Errorf("meta after target revocation = %d", res.status)
	}
	if res := f.postMultipart(tokenA, conv.ID, "b.txt", []byte("x"), nil); res.status != http.StatusNotFound {
		t.Errorf("upload after target revocation = %d", res.status)
	}
}

func TestUploadsDisabledKeepsPreviousSurface(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{textStep("ok")}})
	conv := f.createConversation(nil)

	status := decodeStatus(t, f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK))
	if status.Features.Uploads {
		t.Fatal("status reports uploads on a deployment without them")
	}
	if status.Limits.MaxUploadBytes != 0 || status.Limits.MaxAttachmentsPerTurn != 0 || status.Limits.AttachmentTypes != nil {
		t.Fatalf("disabled limits = %+v", status.Limits)
	}

	for _, res := range []apiResponse{
		f.postMultipart(tokenA, conv.ID, "a.txt", []byte("x"), nil),
		f.call(http.MethodGet, testPrefix+"/uploads/anything", nil, nil),
		f.call(http.MethodGet, testPrefix+"/uploads/anything/meta", nil, nil),
	} {
		if res.status != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", res.method, res.path, res.status)
		}
	}

	// A turn body that carries attachments is a plain bad request.
	if res := f.submitTurnWithAttachments(tokenA, conv.ID, "hi", []string{"u1"}); res.status != http.StatusBadRequest {
		t.Errorf("turn with attachments while uploads are off = %d", res.status)
	}
}

func TestTurnWithAttachments(t *testing.T) {
	prov := scripted.New(textStep("done"))
	f := newFixture(t, fixtureOptions{provider: prov, uploads: true})
	conv := f.createConversation(nil)
	notes := f.uploadAs(tokenA, conv.ID, "notes.txt", []byte("hello"))
	png := []byte("\x89PNG\r\n\x1a\nbody")
	shot := f.uploadAs(tokenA, conv.ID, "shot.png", png)

	res := f.submitTurnWithAttachments(tokenA, conv.ID, "read these", []string{notes.ID, shot.ID})
	res.expect(http.StatusAccepted)
	var submitted submitTurnView
	res.decode(&submitted)
	turn := waitForTurnState(t, f.store, ownerFor("user-a"), submitted.Turn.ID, TurnStateCompleted)

	// The transcript records references, never the bytes.
	var user Message
	for _, m := range f.conversation(conv.ID).Messages {
		if m.Role == RoleUser && m.TurnID == turn.ID {
			user = m
		}
	}
	if user.ID == "" {
		t.Fatal("user message not stored")
	}
	if len(user.Parts) != 3 {
		t.Fatalf("user parts = %+v", user.Parts)
	}
	if user.Parts[0].Type != PartText || user.Parts[0].Text != "read these" {
		t.Fatalf("text part = %+v", user.Parts[0])
	}
	if p := user.Parts[1]; p.Type != PartAttachment || p.UploadID != notes.ID || p.Name != "notes.txt" || p.MIME != "text/plain" {
		t.Fatalf("first attachment part = %+v", p)
	}
	if p := user.Parts[2]; p.Type != PartAttachment || p.UploadID != shot.ID || p.Name != "shot.png" || p.MIME != "image/png" {
		t.Fatalf("second attachment part = %+v", p)
	}

	// The provider received the decoded attachments in order.
	reqs := prov.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests", len(reqs))
	}
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	if last.Role != provider.RoleUser || last.Text != "read these" {
		t.Fatalf("provider user message = %+v", last)
	}
	if len(last.Attachments) != 2 {
		t.Fatalf("provider attachments = %+v", last.Attachments)
	}
	if last.Attachments[0].Name != "notes.txt" || string(last.Attachments[0].Data) != "hello" {
		t.Fatalf("text attachment = %+v", last.Attachments[0])
	}
	if last.Attachments[1].MIME != "image/png" || !bytes.Equal(last.Attachments[1].Data, png) {
		t.Fatalf("image attachment = %+v", last.Attachments[1])
	}

	// Attachment bytes never reach an event.
	for _, ev := range mustEvents(t, f.store, ownerFor("user-a"), turn.ID) {
		if bytes.Contains(ev.Data, []byte("hello")) && ev.Type != EventTextDelta {
			t.Fatalf("event %s carries attachment content: %s", ev.Type, ev.Data)
		}
	}
}

func TestTurnAttachmentValidation(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{textStep("ok")}, uploads: true, maxAttachments: 2})
	conv := f.createConversation(nil)
	upload := f.uploadAs(tokenA, conv.ID, "a.txt", []byte("x"))

	otherConv := f.createConversationAs(tokenB, nil)
	foreign := f.uploadAs(tokenB, otherConv.ID, "theirs.txt", []byte("x"))

	cases := []struct {
		name string
		ids  []string
		code string
	}{
		{"duplicate", []string{upload.ID, upload.ID}, CodeInvalidAttachment},
		{"empty id", []string{""}, CodeInvalidAttachment},
		{"unknown", []string{"missing"}, CodeInvalidAttachment},
		{"foreign", []string{foreign.ID}, CodeInvalidAttachment},
		{"other conversation", []string{upload.ID}, CodeInvalidAttachment},
		{"too many", []string{upload.ID, upload.ID, upload.ID}, CodeInvalidAttachment},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			convID := conv.ID
			if c.name == "other conversation" {
				// The upload exists, in another of the caller's conversations.
				second := f.createConversation(nil)
				convID = second.ID
			}
			res := f.submitTurnWithAttachments(tokenA, convID, "hi", c.ids)
			if res.status != http.StatusBadRequest {
				t.Fatalf("turn = %d, body %s", res.status, res.body)
			}
			if code := res.errorCode(); code != c.code {
				t.Fatalf("code = %q, want %q", code, c.code)
			}
		})
	}
}

func TestTurnAttachmentProviderSupport(t *testing.T) {
	// A provider that can only carry text must refuse an image turn and must
	// advertise only the text types.
	prov := scripted.New(textStep("ok")).SupportOnly("text/plain")
	f := newFixture(t, fixtureOptions{provider: prov, uploads: true})
	conv := f.createConversation(nil)
	png := f.uploadAs(tokenA, conv.ID, "shot.png", []byte("\x89PNG\r\n\x1a\nbody"))

	res := f.submitTurnWithAttachments(tokenA, conv.ID, "look", []string{png.ID})
	if res.status != http.StatusBadRequest {
		t.Fatalf("unsupported attachment turn = %d, body %s", res.status, res.body)
	}
	if code := res.errorCode(); code != CodeUnsupportedAttachment {
		t.Fatalf("code = %q", code)
	}
	status := decodeStatus(t, f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK))
	if status.Limits.AttachmentTypes == nil || fmt.Sprint(*status.Limits.AttachmentTypes) != "[text/plain]" {
		t.Fatalf("attachmentTypes = %v", status.Limits.AttachmentTypes)
	}
}

func TestStatusAttachmentTypesEmptyList(t *testing.T) {
	// An enabled surface whose provider supports no attachment must report an
	// empty list, not omit the field: a client has to be able to tell "none"
	// from "this deployment has no uploads".
	f := newFixture(t, fixtureOptions{provider: scripted.New().SupportOnly(), uploads: true})
	res := f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK)
	var body map[string]any
	res.decode(&body)
	limits, ok := body["limits"].(map[string]any)
	if !ok {
		t.Fatalf("limits = %v", body["limits"])
	}
	types, ok := limits["attachmentTypes"].([]any)
	if !ok || len(types) != 0 {
		t.Fatalf("attachmentTypes = %v (present %v)", limits["attachmentTypes"], ok)
	}
}

func TestUploadPartJSONShape(t *testing.T) {
	raw, err := json.Marshal(Part{Type: PartAttachment, UploadID: "u1", Name: "a.txt", MIME: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"attachment","uploadId":"u1","name":"a.txt","mime":"text/plain"}`
	if string(raw) != want {
		t.Fatalf("attachment part JSON = %s, want %s", raw, want)
	}
}

func TestOtherImageTypes(t *testing.T) {
	bmp := append([]byte("BM"), make([]byte, 60)...)
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic")...)
	heic = append(heic, make([]byte, 20)...)

	off := newFixture(t, fixtureOptions{uploads: true})
	conv := off.createConversation(nil)
	off.postMultipart(tokenA, conv.ID, "scan.bmp", bmp, nil).expect(http.StatusUnsupportedMediaType)

	prov := scripted.New(textStep("I can't see that one."))
	f := newFixture(t, fixtureOptions{uploads: true, otherImages: true, provider: prov.SupportOnly("image/png")})
	conv = f.createConversation(nil)
	up := f.uploadAs(tokenA, conv.ID, "scan.bmp", bmp)
	if up.MIME != "image/bmp" || up.Kind != UploadKindImage {
		t.Fatalf("upload = %+v", up)
	}
	if photo := f.uploadAs(tokenA, conv.ID, "IMG_1.heic", heic); photo.MIME != "image/heic" {
		t.Fatalf("heic upload = %+v", photo)
	}
	status := decodeStatus(t, f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK))
	if types := *status.Limits.AttachmentTypes; !slices.Contains(types, "image/bmp") || !slices.Contains(types, "image/avif") {
		t.Fatalf("attachment types = %v", types)
	}
	// A turn may carry it; the model is told it was omitted.
	res := f.submitTurnWithAttachments(tokenA, conv.ID, "what is this?", []string{up.ID}).expect(http.StatusAccepted)
	var out submitTurnView
	res.decode(&out)
	awaitTerminal(t, f.store, ownerFor("user-a"), out.Turn.ID)
	last := prov.Requests()[0].Messages[len(prov.Requests()[0].Messages)-1]
	if len(last.Attachments) != 0 || !strings.Contains(last.Text, "[attachment scan.bmp omitted]") {
		t.Fatalf("model got %+v", last)
	}
}
