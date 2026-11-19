package pocketbase

// UploadStore acceptance tests: the shared contract runs against the real
// PocketBase-backed store, and the storage side is checked directly: the bytes
// live under the record's files path in the app filesystem, provisioning adds
// the collection to an existing install, a conflicting collection is refused,
// and deleting a conversation removes the record and its file.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/storetest"
)

// TestChatStoreUploadContract runs the shared chat.UploadStore contract against
// the PocketBase-backed store.
func TestChatStoreUploadContract(t *testing.T) {
	app := newChatTestApp(t)
	var seq atomic.Int64
	storetest.RunUploads(t, func(t *testing.T) interface {
		chat.Store
		chat.UploadStore
	} {
		t.Helper()
		n := seq.Add(1)
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: fmt.Sprintf("_uploadstoretest%d", n)})
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

// TestChatStoreUploadUpgradeFromFiveCollections proves a v1.2 installation with
// the five pre-upload collections (and data) gains the uploads collection on the
// next start without touching anything already stored.
func TestChatStoreUploadUpgradeFromFiveCollections(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := proposalOwner()
	conv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "pre-uploads"})
	if err != nil {
		t.Fatal(err)
	}

	uploadsCol, err := app.FindCollectionByNameOrId("_mcpChatUploads")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(uploadsCol); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindCollectionByNameOrId("_mcpChatUploads"); err == nil {
		t.Fatal("uploads collection still exists after the simulated downgrade")
	}
	ids := map[string]string{}
	for _, name := range []string{
		"_mcpChatConversations", "_mcpChatMessages", "_mcpChatTurns",
		"_mcpChatEvents", "_mcpChatProposals",
	} {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = col.Id
	}

	upgraded, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatalf("upgrade provisioning: %v", err)
	}
	if got := upgraded.Collections(); len(got) != 6 || got[5] != "_mcpChatUploads" {
		t.Fatalf("collections after upgrade = %v", got)
	}
	for name, id := range ids {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		if col.Id != id {
			t.Fatalf("collection %q was replaced by the upgrade: %s -> %s", name, id, col.Id)
		}
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
	app := newChatTestApp(t)
	col := core.NewBaseCollection("_mcpChatUploads")
	col.ListRule = types.Pointer(`@request.auth.id != ""`)
	col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	_, err := NewChatStore(app, ChatStoreOptions{})
	if err == nil {
		t.Fatal("conflicting uploads collection was accepted")
	}
	if !strings.Contains(err.Error(), "_mcpChatUploads") || !strings.Contains(err.Error(), "API rules") {
		t.Fatalf("error does not name the conflict: %v", err)
	}
}

// TestChatStoreUploadFileStorageAndCascade proves the bytes are stored in the
// app filesystem under the record's own files path and that deleting a
// conversation removes both the upload record and its file, leaving another
// conversation's attachment untouched.
func TestChatStoreUploadFileStorageAndCascade(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := proposalOwner()
	us := mustUploadStore(t, store)

	keepConv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	dropConv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "drop"})
	if err != nil {
		t.Fatal(err)
	}
	kept := mustStoredUpload(t, us, owner, keepConv.ID, "keep.txt", []byte("keep me"))
	dropped := mustStoredUpload(t, us, owner, dropConv.ID, "drop.txt", []byte("drop me"))

	keptPath, keptName := uploadFilePath(t, app, kept.ID)
	droppedPath, droppedName := uploadFilePath(t, app, dropped.ID)
	if keptName == "keep.txt" || droppedName == "drop.txt" {
		t.Fatalf("stored file names were not randomized: %q %q", keptName, droppedName)
	}
	if !fileExists(t, app, keptPath) || !fileExists(t, app, droppedPath) {
		t.Fatal("an upload file is not stored under its record's files path")
	}

	if err := store.DeleteConversation(ctx, owner, dropConv.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	// PocketBase deletes a deleted record's storage prefix in the background,
	// so the file disappears shortly after the delete returns.
	waitForFileGone(t, app, droppedPath)
	if _, err := app.FindRecordById("_mcpChatUploads", dropped.ID); err == nil {
		t.Fatal("the deleted conversation's upload record survived")
	}
	if !fileExists(t, app, keptPath) {
		t.Fatal("an unrelated conversation's upload file was deleted")
	}
	if meta, data, err := us.ReadUpload(ctx, owner, kept.ID); err != nil || string(data) != "keep me" || meta.ConversationID != keepConv.ID {
		t.Fatalf("unrelated upload after cascade = %+v %q, %v", meta, data, err)
	}
}

func mustStoredUpload(t *testing.T, us chat.UploadStore, owner chat.OwnerKey, convID, name string, data []byte) chat.Upload {
	t.Helper()
	u, err := us.CreateUpload(context.Background(), owner, chat.Upload{
		ConversationID: convID, Name: name, MIME: "text/plain", Kind: chat.UploadKindText,
	}, data)
	if err != nil {
		t.Fatalf("CreateUpload(%q): %v", name, err)
	}
	return u
}

// uploadFilePath returns the stored filesystem key and file name of one upload
// record, derived exactly as PocketBase does.
func uploadFilePath(t *testing.T, app core.App, uploadID string) (path, name string) {
	t.Helper()
	rec, err := app.FindRecordById("_mcpChatUploads", uploadID)
	if err != nil {
		t.Fatalf("find upload record %q: %v", uploadID, err)
	}
	name = rec.GetString(chatFieldUploadData)
	if name == "" {
		t.Fatal("upload record has no stored file name")
	}
	return rec.BaseFilesPath() + "/" + name, name
}

func fileExists(t *testing.T, app core.App, path string) bool {
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

// waitForFileGone polls until PocketBase's background file cleanup removed the
// storage prefix of a deleted record.
func waitForFileGone(t *testing.T, app core.App, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !fileExists(t, app, path) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the deleted upload file %q is still on disk", path)
}
