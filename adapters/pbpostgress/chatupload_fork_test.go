package pbpostgress

// UploadStore acceptance tests against the disposable PostgreSQL database:
// the shared contract, the upgrade of an existing five-collection install, the
// conflict rule, and the storage side (bytes under the record's files path,
// removed with the conversation).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/storetest"
)

// TestChatStoreUploadContract runs the shared chat.UploadStore contract against
// the fork store on PostgreSQL.
func TestChatStoreUploadContract(t *testing.T) {
	app := newChatForkApp(t)
	storetest.RunUploads(t, func(t *testing.T) interface {
		chat.Store
		chat.UploadStore
	} {
		t.Helper()
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: chatTestPrefix(t)})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

func mustUploadStore(t *testing.T, store chat.Store) chat.UploadStore {
	t.Helper()
	us, ok := store.(chat.UploadStore)
	if !ok {
		t.Fatalf("store %T does not implement chat.UploadStore", store)
	}
	return us
}

// TestChatStoreUploadUpgradeFromFiveCollections proves an installation with the
// five pre-upload collections gains the uploads collection on its next start
// without touching the existing schema or data. The fork refuses to delete a
// System collection, so the legacy install is provisioned rather than simulated
// by a delete.
func TestChatStoreUploadUpgradeFromFiveCollections(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	ctx := context.Background()
	owner := forkProposalOwner("upload-upgrade", "/mcp")

	legacy := &ChatStore{
		app:           app,
		prefix:        prefix,
		conversations: prefix + "Conversations",
		messages:      prefix + "Messages",
		turns:         prefix + "Turns",
		events:        prefix + "Events",
		proposals:     prefix + "Proposals",
		uploads:       prefix + "Uploads",
		maxEvents:     chat.DefaultMaxEventsPerTurn,
	}
	for _, spec := range legacy.specs()[:5] {
		if err := provisionChatCollection(app, spec); err != nil {
			t.Fatalf("provision pre-upload collection: %v", err)
		}
	}
	if _, err := app.FindCollectionByNameOrId(prefix + "Uploads"); err == nil {
		t.Fatal("the legacy install unexpectedly has an uploads collection")
	}
	conv, err := legacy.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "pre-uploads"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{
		prefix + "Conversations", prefix + "Messages", prefix + "Turns",
		prefix + "Events", prefix + "Proposals",
	} {
		existing, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = existing.Id
	}

	upgraded, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
	if err != nil {
		t.Fatalf("upgrade provisioning: %v", err)
	}
	if got := upgraded.Collections(); len(got) != 6 || got[5] != prefix+"Uploads" {
		t.Fatalf("collections after upgrade = %v", got)
	}
	for name, id := range ids {
		existing, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		if existing.Id != id {
			t.Fatalf("collection %q was replaced by the upgrade: %s -> %s", name, id, existing.Id)
		}
	}
	if !app.HasTable(prefix + "Uploads") {
		t.Fatal("the upgraded uploads collection has no table")
	}
	if got, err := upgraded.GetConversation(ctx, owner, conv.ID); err != nil || got.Title != "pre-uploads" {
		t.Fatalf("conversation after upgrade = %+v, %v", got, err)
	}
	us := mustUploadStore(t, upgraded)
	stored, err := us.CreateUpload(ctx, owner, chat.Upload{
		ConversationID: conv.ID, Name: "after.txt", MIME: "text/plain", Kind: chat.UploadKindText,
	}, []byte("after upgrade"))
	if err != nil {
		t.Fatalf("CreateUpload after upgrade: %v", err)
	}
	if meta, data, err := us.ReadUpload(ctx, owner, stored.ID); err != nil || string(data) != "after upgrade" {
		t.Fatalf("ReadUpload after upgrade = %+v %q, %v", meta, data, err)
	}
}

// TestChatStoreRejectsConflictingUploadsCollection keeps the uploads collection
// under the same adoption rules as the other five.
func TestChatStoreRejectsConflictingUploadsCollection(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	col := core.NewBaseCollection(prefix + "Uploads")
	col.System = true
	col.ListRule = types.Pointer(`@request.auth.id != ""`)
	col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey, Max: chatMaxTextLen})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	_, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
	if err == nil {
		t.Fatal("conflicting uploads collection was accepted")
	}
	if !strings.Contains(err.Error(), prefix+"Uploads") || !strings.Contains(err.Error(), "API rules") {
		t.Fatalf("error does not name the conflict: %v", err)
	}
}

// TestChatStoreUploadFileStorageAndCascade proves the bytes are stored in the
// app filesystem under the record's own files path and that deleting a
// conversation removes both the upload record and its file, leaving another
// conversation's attachment untouched.
func TestChatStoreUploadFileStorageAndCascade(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	store, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := forkProposalOwner("upload-files", "/mcp")
	us := mustUploadStore(t, store)

	keepConv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	dropConv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "drop"})
	if err != nil {
		t.Fatal(err)
	}
	kept := mustForkStoredUpload(t, us, owner, keepConv.ID, "keep.txt", []byte("keep me"))
	dropped := mustForkStoredUpload(t, us, owner, dropConv.ID, "drop.txt", []byte("drop me"))

	keptPath, keptName := forkUploadFilePath(t, app, prefix, kept.ID)
	droppedPath, droppedName := forkUploadFilePath(t, app, prefix, dropped.ID)
	if keptName == "keep.txt" || droppedName == "drop.txt" {
		t.Fatalf("stored file names were not randomized: %q %q", keptName, droppedName)
	}
	if !forkFileExists(t, app, keptPath) || !forkFileExists(t, app, droppedPath) {
		t.Fatal("an upload file is not stored under its record's files path")
	}

	if err := store.DeleteConversation(ctx, owner, dropConv.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	forkWaitForFileGone(t, app, droppedPath)
	if _, err := app.FindRecordById(prefix+"Uploads", dropped.ID); err == nil {
		t.Fatal("the deleted conversation's upload record survived")
	}
	if !forkFileExists(t, app, keptPath) {
		t.Fatal("an unrelated conversation's upload file was deleted")
	}
	if meta, data, err := us.ReadUpload(ctx, owner, kept.ID); err != nil || string(data) != "keep me" || meta.ConversationID != keepConv.ID {
		t.Fatalf("unrelated upload after cascade = %+v %q, %v", meta, data, err)
	}
}

func mustForkStoredUpload(t *testing.T, us chat.UploadStore, owner chat.OwnerKey, convID, name string, data []byte) chat.Upload {
	t.Helper()
	u, err := us.CreateUpload(context.Background(), owner, chat.Upload{
		ConversationID: convID, Name: name, MIME: "text/plain", Kind: chat.UploadKindText,
	}, data)
	if err != nil {
		t.Fatalf("CreateUpload(%q): %v", name, err)
	}
	if u.Size != int64(len(data)) || !strings.HasPrefix(u.SHA256, "sha256:") {
		t.Fatalf("stored upload = %+v", u)
	}
	return u
}

func forkUploadFilePath(t *testing.T, app core.App, prefix, uploadID string) (path, name string) {
	t.Helper()
	rec, err := app.FindRecordById(prefix+"Uploads", uploadID)
	if err != nil {
		t.Fatalf("find upload record %q: %v", uploadID, err)
	}
	name = rec.GetString(chatFieldUploadData)
	if name == "" {
		t.Fatal("upload record has no stored file name")
	}
	return rec.BaseFilesPath() + "/" + name, name
}

func forkFileExists(t *testing.T, app core.App, path string) bool {
	t.Helper()
	fsys, err := app.NewFilesystem()
	if err != nil {
		t.Fatalf("open filesystem: %v", err)
	}
	defer fsys.Close()
	ok, err := fsys.Exists(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return ok
}

func forkWaitForFileGone(t *testing.T, app core.App, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !forkFileExists(t, app, path) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the deleted upload file %q is still on disk", path)
}
