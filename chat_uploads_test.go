package pocketbasemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// chatMultipart posts one upload through the handler.
func chatMultipart(t *testing.T, h http.Handler, path, token, convID, filename string, data []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if convID != "" {
		if err := w.WriteField("conversationId", convID); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if filename != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestChatUploadsEndToEnd enables uploads on a MemStore through the root bridge
// and walks the whole surface: status, upload, download and a turn that sends
// the attachment to the model.
func TestChatUploadsEndToEnd(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	store := chat.NewMemStore(chat.MemStoreOptions{})
	prov := scripted.New(textStep("read it"))

	runner, err := chat.NewRunner(chat.RunnerConfig{
		Store:    store,
		Provider: prov,
		Model:    "test-model",
		Uploads:  store,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})

	h, err := srv.ChatHandler(ChatOptions{
		Namespace:                 "tests",
		Runner:                    runner,
		Store:                     store,
		Models:                    []string{"test-model"},
		Uploads:                   true,
		MaxUploadBytes:            1024,
		MaxAttachmentsPerTurn:     2,
		MaxUploadsPerConversation: 3,
	})
	if err != nil {
		t.Fatalf("ChatHandler: %v", err)
	}

	// /status advertises the enabled feature and the configured limits.
	code, body := chatHTTP(t, h, http.MethodGet, DefaultChatPrefix+"/status", "good", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, body)
	}
	var status struct {
		Features struct {
			Uploads bool `json:"uploads"`
		} `json:"features"`
		Limits struct {
			MaxUploadBytes        int64    `json:"maxUploadBytes"`
			MaxAttachmentsPerTurn int      `json:"maxAttachmentsPerTurn"`
			AttachmentTypes       []string `json:"attachmentTypes"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Features.Uploads {
		t.Fatal("features.uploads = false")
	}
	if status.Limits.MaxUploadBytes != 1024 || status.Limits.MaxAttachmentsPerTurn != 2 || len(status.Limits.AttachmentTypes) == 0 {
		t.Fatalf("limits = %+v", status.Limits)
	}

	code, body = chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/conversations", "good", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create conversation: %d %s", code, body)
	}
	var conv chat.Conversation
	if err := json.Unmarshal(body, &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}

	content := []byte("attachment payload\n")
	code, body = chatMultipart(t, h, DefaultChatPrefix+"/uploads", "good", conv.ID, "notes.txt", content)
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, body)
	}
	var stored struct {
		Upload chat.Upload `json:"upload"`
	}
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if stored.Upload.ID == "" || stored.Upload.MIME != "text/plain" || stored.Upload.Size != int64(len(content)) {
		t.Fatalf("upload = %+v", stored.Upload)
	}

	// The download is owner-scoped bytes.
	code, body = chatHTTP(t, h, http.MethodGet, DefaultChatPrefix+"/uploads/"+stored.Upload.ID, "good", "")
	if code != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("download: %d %q", code, body)
	}
	if code, _ := chatHTTP(t, h, http.MethodGet, DefaultChatPrefix+"/uploads/"+stored.Upload.ID, "admin", ""); code != http.StatusNotFound {
		t.Fatalf("foreign download: %d", code)
	}

	// A turn references the upload and the provider receives its bytes.
	turnBody, err := json.Marshal(map[string]any{
		"text":        "read the file",
		"attachments": []string{stored.Upload.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body = chatHTTP(t, h, http.MethodPost, DefaultChatPrefix+"/conversations/"+conv.ID+"/turns", "good", string(turnBody))
	if code != http.StatusAccepted {
		t.Fatalf("submit turn: %d %s", code, body)
	}
	var submitted struct {
		Turn chat.Turn `json:"turn"`
	}
	if err := json.Unmarshal(body, &submitted); err != nil {
		t.Fatalf("decode turn: %v", err)
	}

	owner := chat.OwnerKey{Namespace: "tests", Mount: "/mcp", AuthCollectionID: "col_users", UserID: "u1"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		turn, err := store.GetTurn(context.Background(), owner, submitted.Turn.ID)
		if err == nil && turn.State.Terminal() {
			if turn.State != chat.TurnStateCompleted {
				t.Fatalf("turn ended as %s: %+v", turn.State, turn.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s did not finish", submitted.Turn.ID)
		}
		time.Sleep(5 * time.Millisecond)
	}

	reqs := prov.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests", len(reqs))
	}
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	if len(last.Attachments) != 1 || string(last.Attachments[0].Data) != string(content) {
		t.Fatalf("provider attachments = %+v", last.Attachments)
	}
	if last.Attachments[0].Name != "notes.txt" || last.Attachments[0].MIME != "text/plain" {
		t.Fatalf("provider attachment metadata = %+v", last.Attachments[0])
	}
}

// storeOnly hides MemStore's additive ports, so a test can prove the bridge
// refuses an upload surface the store cannot back.
type storeOnly struct {
	chat.Store
}

// TestChatUploadsStartupValidation covers the pairing the bridge enforces: the
// handler's store must be able to keep uploads, and the runner must be able to
// resolve them.
func TestChatUploadsStartupValidation(t *testing.T) {
	host := newChatTestHost()
	srv := newChatTestServer(host, nil, Config{})
	mem := chat.NewMemStore(chat.MemStoreOptions{})

	narrow := storeOnly{Store: mem}
	plainRunner, err := chat.NewRunner(chat.RunnerConfig{Store: narrow, Provider: scripted.New(), Model: "m"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = plainRunner.Shutdown(ctx)
	})

	if _, err := srv.ChatHandler(ChatOptions{
		Namespace: "tests", Runner: plainRunner, Store: narrow, Uploads: true,
	}); err == nil {
		t.Fatal("uploads accepted with a store that does not implement chat.UploadStore")
	}

	// The store can keep uploads but the runner cannot send them.
	withoutRunner, err := chat.NewRunner(chat.RunnerConfig{Store: mem, Provider: scripted.New(), Model: "m"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = withoutRunner.Shutdown(ctx)
	})
	if _, err := srv.ChatHandler(ChatOptions{
		Namespace: "tests", Runner: withoutRunner, Store: mem, Uploads: true,
	}); err == nil {
		t.Fatal("uploads accepted with a runner that has no upload store")
	}

	enabled, err := chat.NewRunner(chat.RunnerConfig{Store: mem, Provider: scripted.New(), Model: "m", Uploads: mem})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = enabled.Shutdown(ctx)
	})
	if _, err := srv.ChatHandler(ChatOptions{
		Namespace: "tests", Runner: enabled, Store: mem, Uploads: true,
	}); err != nil {
		t.Fatalf("valid upload options rejected: %v", err)
	}

	// Uploads stay off unless asked for, even when everything is capable.
	if _, err := srv.ChatHandler(ChatOptions{
		Namespace: "tests", Runner: enabled, Store: mem,
	}); err != nil {
		t.Fatalf("uploads-off options rejected: %v", err)
	}
}
