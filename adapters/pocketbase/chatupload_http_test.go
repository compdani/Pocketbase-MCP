package pocketbase

// End-to-end upload tests through the real PocketBase router: a host turns the
// surface on with MCP-style options, the store persists the bytes in the app
// filesystem, and the scripted provider receives them on the turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// uploadFile posts one multipart attachment as token and returns its metadata.
func (env *chatEnv) uploadFile(token, convID, filename string, data []byte) chat.Upload {
	env.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("conversationId", convID); err != nil {
		env.t.Fatalf("write conversationId: %v", err)
	}
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		env.t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		env.t.Fatalf("write file part: %v", err)
	}
	if err := w.Close(); err != nil {
		env.t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, chatTestPrefix+"/uploads", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		env.t.Fatalf("upload %q = HTTP %d %s", filename, rec.Code, rec.Body.String())
	}
	var out struct {
		Upload chat.Upload `json:"upload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		env.t.Fatalf("decode upload: %v", err)
	}
	return out.Upload
}

// downloadUpload GETs one attachment and returns the recorder for header and
// body assertions.
func (env *chatEnv) downloadUpload(token, id string) *httptest.ResponseRecorder {
	env.t.Helper()
	req := httptest.NewRequest(http.MethodGet, chatTestPrefix+"/uploads/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	return rec
}

// TestChatHTTPUploadsEndToEnd is the whole attachment surface through
// EnableChat: status advertises it, bytes round-trip with the download headers,
// another user sees a missing upload, a turn sends the attachments to the
// provider, and deleting the conversation removes the records and their files.
func TestChatHTTPUploadsEndToEnd(t *testing.T) {
	env := newChatEnv(t, chatEnvOptions{
		uploads: true,
		steps: fixedSteps(scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "read them"},
			{Kind: provider.ChunkDone},
		}}),
	})

	var status struct {
		Features struct {
			Uploads bool `json:"uploads"`
		} `json:"features"`
		Limits struct {
			MaxUploadBytes        int64     `json:"maxUploadBytes"`
			MaxAttachmentsPerTurn int       `json:"maxAttachmentsPerTurn"`
			AttachmentTypes       *[]string `json:"attachmentTypes"`
		} `json:"limits"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/status", "", &status); code != http.StatusOK {
		t.Fatalf("status: HTTP %d", code)
	}
	if !status.Features.Uploads {
		t.Fatal("status does not report uploads after EnableChat")
	}
	if status.Limits.MaxUploadBytes <= 0 || status.Limits.MaxAttachmentsPerTurn <= 0 {
		t.Fatalf("upload limits = %+v", status.Limits)
	}
	if status.Limits.AttachmentTypes == nil || len(*status.Limits.AttachmentTypes) == 0 {
		t.Fatalf("attachmentTypes = %v, want the scripted provider's types", status.Limits.AttachmentTypes)
	}

	var conv chat.Conversation
	if code := env.json(env.userToken, http.MethodPost, chatTestPrefix+"/conversations", `{"title":"files"}`, &conv); code != http.StatusCreated {
		t.Fatalf("create conversation: HTTP %d", code)
	}

	text := []byte("hello attachment\n")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRdemo")
	notes := env.uploadFile(env.userToken, conv.ID, "notes.txt", text)
	shot := env.uploadFile(env.userToken, conv.ID, "shot.png", png)
	if notes.ConversationID != conv.ID || notes.MIME != "text/plain" || notes.Kind != chat.UploadKindText {
		t.Fatalf("text upload = %+v", notes)
	}
	if shot.MIME != "image/png" || shot.Kind != chat.UploadKindImage {
		t.Fatalf("png upload = %+v", shot)
	}

	// The bytes are stored under the record's own files path.
	paths := map[string]string{}
	for _, u := range []chat.Upload{notes, shot} {
		path, _ := uploadFilePath(t, env.app, u.ID)
		if !fileExists(t, env.app, path) {
			t.Fatalf("upload %q is not stored at %q", u.ID, path)
		}
		paths[u.ID] = path
	}

	// Downloads return identical bytes with the opaque attachment headers.
	res := env.downloadUpload(env.userToken, notes.ID)
	if res.Code != http.StatusOK {
		t.Fatalf("download = HTTP %d %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(res.Body.Bytes(), text) {
		t.Fatalf("download body = %q", res.Body.Bytes())
	}
	for header, want := range map[string]string{
		"Content-Type":            "text/plain",
		"Content-Disposition":     "attachment; filename*=UTF-8''notes.txt",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Cache-Control":           "private, no-store",
	} {
		if got := res.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := env.downloadUpload(env.userToken, shot.ID); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), png) {
		t.Fatalf("png download = HTTP %d with %d bytes", got.Code, got.Body.Len())
	}

	// Another user cannot read, list or download them.
	if got := env.downloadUpload(env.otherToken, notes.ID); got.Code != http.StatusNotFound {
		t.Fatalf("foreign download = HTTP %d, want 404", got.Code)
	}
	if code, _ := env.do(env.otherToken, http.MethodGet, chatTestPrefix+"/uploads/"+notes.ID+"/meta", ""); code != http.StatusNotFound {
		t.Fatalf("foreign meta = HTTP %d, want 404", code)
	}

	// A turn with the attachments hands the bytes to the provider.
	var submitted struct {
		Turn chat.Turn `json:"turn"`
	}
	if code := env.json(env.userToken, http.MethodPost, chatTestPrefix+"/conversations/"+conv.ID+"/turns",
		fmt.Sprintf(`{"text":"read these","attachments":["%s","%s"]}`, notes.ID, shot.ID), &submitted); code != http.StatusAccepted {
		t.Fatalf("submit turn: HTTP %d", code)
	}
	srv := httptest.NewServer(env.mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, events := env.streamTurn(ctx, srv.URL, env.userToken, submitted.Turn.ID)
	defer resp.Body.Close()
	if terminal := waitEvent(t, events, func(e chat.Event) bool { return e.Type.Terminal() }); terminal.Type != chat.EventTurnCompleted {
		t.Fatalf("terminal = %+v", terminal)
	}

	reqs := env.provider.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests", len(reqs))
	}
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	if len(last.Attachments) != 2 {
		t.Fatalf("provider attachments = %+v", last.Attachments)
	}
	if last.Attachments[0].Name != "notes.txt" || !bytes.Equal(last.Attachments[0].Data, text) {
		t.Fatalf("text attachment = %+v", last.Attachments[0])
	}
	if last.Attachments[1].MIME != "image/png" || !bytes.Equal(last.Attachments[1].Data, png) {
		t.Fatalf("image attachment = %+v", last.Attachments[1])
	}

	// The transcript stores references, never the bytes.
	var detail struct {
		Messages []chat.Message `json:"messages"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/conversations/"+conv.ID, "", &detail); code != http.StatusOK {
		t.Fatalf("get conversation: HTTP %d", code)
	}
	var parts int
	for _, m := range detail.Messages {
		for _, p := range m.Parts {
			if p.Type == chat.PartAttachment {
				parts++
			}
		}
	}
	if parts != 2 {
		t.Fatalf("stored attachment parts = %d, want 2", parts)
	}

	// Deleting the conversation removes the records and their files.
	if code, body := env.do(env.userToken, http.MethodDelete, chatTestPrefix+"/conversations/"+conv.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete conversation = HTTP %d %s", code, body)
	}
	for _, u := range []chat.Upload{notes, shot} {
		if _, err := env.app.FindRecordById("_mcpChatUploads", u.ID); err == nil {
			t.Fatalf("upload %q survived the conversation delete", u.ID)
		}
		waitForFileGone(t, env.app, paths[u.ID])
	}
	if got := env.downloadUpload(env.userToken, notes.ID); got.Code != http.StatusNotFound {
		t.Fatalf("download after delete = HTTP %d, want 404", got.Code)
	}
}
