package chat

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// mustUpload stores one attachment directly, bypassing the HTTP surface.
func mustUpload(t *testing.T, s *MemStore, owner OwnerKey, convID, name, mime, kind string, data []byte) Upload {
	t.Helper()
	u, err := s.CreateUpload(context.Background(), owner, Upload{
		ConversationID: convID,
		Name:           name,
		MIME:           mime,
		Kind:           kind,
	}, data)
	if err != nil {
		t.Fatalf("CreateUpload(%q): %v", name, err)
	}
	return u
}

func TestRunnerSendsAttachments(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("reply"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Uploads: store})
	if !runner.UploadsEnabled() {
		t.Fatal("UploadsEnabled = false with an upload store")
	}
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "files")

	notes := mustUpload(t, store, owner, conv.ID, "notes.txt", "text/plain", UploadKindText, []byte("hello"))
	png := []byte("\x89PNG\r\n\x1a\nbody")
	shot := mustUpload(t, store, owner, conv.ID, "shot.png", "image/png", UploadKindImage, png)

	turn, _, err := runner.SubmitWithOptions(context.Background(), owner, conv.ID, SubmitOptions{
		Text:        "read these",
		Attachments: []Upload{notes, shot},
	})
	if err != nil {
		t.Fatalf("SubmitWithOptions: %v", err)
	}
	awaitTerminal(t, store, owner, turn.ID)

	// The transcript stores references, not bytes.
	msgs, _, err := store.ListMessages(context.Background(), owner, conv.ID, Page{})
	if err != nil {
		t.Fatal(err)
	}
	var user Message
	for _, m := range msgs {
		if m.Role == RoleUser && m.TurnID == turn.ID {
			user = m
		}
	}
	if len(user.Parts) != 3 {
		t.Fatalf("stored user parts = %+v", user.Parts)
	}
	if user.Parts[1].UploadID != notes.ID || user.Parts[1].Name != "notes.txt" || user.Parts[1].MIME != "text/plain" {
		t.Fatalf("stored attachment part = %+v", user.Parts[1])
	}

	// The provider got the decoded files, in order.
	reqs := prov.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests", len(reqs))
	}
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	if last.Text != "read these" || len(last.Attachments) != 2 {
		t.Fatalf("provider message = %+v", last)
	}
	if last.Attachments[0].Name != "notes.txt" || string(last.Attachments[0].Data) != "hello" {
		t.Fatalf("text attachment = %+v", last.Attachments[0])
	}
	if last.Attachments[1].MIME != "image/png" || !bytes.Equal(last.Attachments[1].Data, png) {
		t.Fatalf("image attachment = %+v", last.Attachments[1])
	}

	// No event carries attachment content.
	for _, ev := range mustEvents(t, store, owner, turn.ID) {
		if bytes.Contains(ev.Data, []byte("hello")) {
			t.Fatalf("event %s carries attachment content: %s", ev.Type, ev.Data)
		}
	}
}

func TestRunnerAttachmentBudgetPrefersNewest(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("one"), textStep("two"))
	runner := mustRunner(t, RunnerConfig{
		Store:                        store,
		Provider:                     prov,
		Model:                        "m",
		Uploads:                      store,
		MaxAttachmentBytesPerRequest: 4,
	})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "budget")

	big := mustUpload(t, store, owner, conv.ID, "big.txt", "text/plain", UploadKindText, []byte("0123456789"))
	small := mustUpload(t, store, owner, conv.ID, "small.txt", "text/plain", UploadKindText, []byte("ok"))

	first, _, err := runner.SubmitWithOptions(context.Background(), owner, conv.ID, SubmitOptions{Text: "first", Attachments: []Upload{big}})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, first.ID)
	second, _, err := runner.SubmitWithOptions(context.Background(), owner, conv.ID, SubmitOptions{Text: "second", Attachments: []Upload{small}})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, second.ID)

	reqs := prov.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider saw %d requests", len(reqs))
	}
	history := reqs[1].Messages
	if len(history) != 3 {
		t.Fatalf("second request history = %+v", history)
	}
	// The older, over-budget attachment is a note and carries no bytes.
	if history[0].Text != "first\n[attachment big.txt omitted]" {
		t.Fatalf("old message text = %q", history[0].Text)
	}
	if len(history[0].Attachments) != 0 {
		t.Fatalf("old message still carries attachments: %+v", history[0].Attachments)
	}
	// The newest attachment fits and is sent.
	if len(history[2].Attachments) != 1 || string(history[2].Attachments[0].Data) != "ok" {
		t.Fatalf("newest attachments = %+v", history[2].Attachments)
	}
}

func TestRunnerMissingAttachmentBecomesNote(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("ok"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Uploads: store})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "deleted")

	// A stored reference to an upload that no longer exists stands in for an
	// attachment deleted after the message was sent.
	if _, err := store.AppendMessage(context.Background(), owner, Message{
		ConversationID: conv.ID,
		Role:           RoleUser,
		Parts: []Part{
			{Type: PartText, Text: "see file"},
			{Type: PartAttachment, UploadID: "deleted", Name: "gone.txt", MIME: "text/plain"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "next", "", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)

	history := prov.Requests()[0].Messages
	if len(history) != 2 {
		t.Fatalf("history = %+v", history)
	}
	if history[0].Text != "see file\n[attachment gone.txt omitted]" {
		t.Fatalf("old message text = %q", history[0].Text)
	}
	if len(history[0].Attachments) != 0 {
		t.Fatalf("missing upload produced an attachment: %+v", history[0].Attachments)
	}
}

func TestRunnerAttachmentTypeSupport(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New().SupportOnly("text/plain")
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Uploads: store})
	if !runner.SupportsAttachment("text/plain") {
		t.Fatal("text/plain reported unsupported")
	}
	if runner.SupportsAttachment("image/png") {
		t.Fatal("image/png reported supported by a text-only provider")
	}

	// A stored image part on a text-only provider is omitted, never sent.
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "types")
	png := mustUpload(t, store, owner, conv.ID, "shot.png", "image/png", UploadKindImage, []byte("\x89PNG\r\n\x1a\nbody"))
	turn, _, err := runner.SubmitWithOptions(context.Background(), owner, conv.ID, SubmitOptions{
		Text:        "look",
		Attachments: []Upload{png},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	last := prov.Requests()[0].Messages[0]
	if len(last.Attachments) != 0 || !strings.Contains(last.Text, "[attachment shot.png omitted]") {
		t.Fatalf("text-only provider message = %+v", last)
	}
}

func TestRunnerWithoutUploadStoreOmitsAttachments(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("ok"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	if runner.UploadsEnabled() {
		t.Fatal("UploadsEnabled = true without an upload store")
	}
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "off")
	if _, err := store.AppendMessage(context.Background(), owner, Message{
		ConversationID: conv.ID,
		Role:           RoleUser,
		Parts: []Part{
			{Type: PartText, Text: "see file"},
			{Type: PartAttachment, UploadID: "any", Name: "a.txt", MIME: "text/plain"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "next", "", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	history := prov.Requests()[0].Messages
	if len(history[0].Attachments) != 0 || !strings.Contains(history[0].Text, "[attachment a.txt omitted]") {
		t.Fatalf("history without an upload store = %+v", history[0])
	}
}

func TestProviderMessagesKeepAttachmentsOffOtherRoles(t *testing.T) {
	// Attachments belong to user messages; a part on another role is never
	// turned into provider content.
	msgs := []Message{
		{Role: RoleAssistant, Parts: []Part{
			{Type: PartText, Text: "answer"},
			{Type: PartAttachment, UploadID: "u1", Name: "a.txt", MIME: "text/plain"},
		}},
	}
	out := providerMessagesWith(msgs, map[string][]byte{"u1": []byte("secret")})
	if len(out) != 1 || out[0].Text != "answer" || len(out[0].Attachments) != 0 {
		t.Fatalf("assistant message = %+v", out)
	}
}
