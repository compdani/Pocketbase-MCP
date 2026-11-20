package pocketbase

// ChatStore acceptance tests: the shared Store contract runs against the real
// PocketBase-backed store, and provisioning is checked to be idempotent across
// restarts and to refuse foreign collections.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/storetest"
)

// newChatTestApp bootstraps a fresh PocketBase app on a temporary data dir.
func newChatTestApp(t *testing.T) core.App {
	t.Helper()
	return newChatTestAppAt(t, t.TempDir())
}

func newChatTestAppAt(t *testing.T, dataDir string) core.App {
	t.Helper()
	t.Setenv("PB_TEST_ENCRYPTION_KEY", strings.Repeat("k", 32))
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:       dataDir,
		DefaultEncryptionEnv: "PB_TEST_ENCRYPTION_KEY",
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	return app
}

func TestChatStoreContract(t *testing.T) {
	app := newChatTestApp(t)
	var seq atomic.Int64
	// Every contract subtest gets its own collection namespace on one app, so
	// a subtest can never observe another subtest's rows while the suite stays
	// fast (a fresh app per subtest would re-bootstrap PocketBase each time).
	storetest.Run(t, func(t *testing.T) chat.Store {
		t.Helper()
		n := seq.Add(1)
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: fmt.Sprintf("_storetest%d", n)})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

func TestChatStoreRetention(t *testing.T) {
	app := newChatTestApp(t)
	var seq atomic.Int64
	storetest.RunRetention(t, func(t *testing.T, max int) chat.Store {
		t.Helper()
		n := seq.Add(1)
		store, err := NewChatStore(app, ChatStoreOptions{
			Prefix:           fmt.Sprintf("_retention%d", n),
			MaxEventsPerTurn: max,
		})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

func TestChatStoreSchema(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"_mcpChatConversations", "_mcpChatMessages", "_mcpChatTurns", "_mcpChatEvents", "_mcpChatProposals", "_mcpChatUploads"}
	got := store.Collections()
	if len(got) != len(want) {
		t.Fatalf("collections = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collections = %v, want %v", got, want)
		}
		col, err := app.FindCollectionByNameOrId(want[i])
		if err != nil {
			t.Fatalf("%s: %v", want[i], err)
		}
		if !col.IsBase() {
			t.Fatalf("%s type = %s, want base", col.Name, col.Type)
		}
		if col.ListRule != nil || col.ViewRule != nil || col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
			t.Fatalf("%s is not private: rules must all be nil", col.Name)
		}
	}
	turns, err := app.FindCollectionByNameOrId("_mcpChatTurns")
	if err != nil {
		t.Fatal(err)
	}
	active := turns.GetIndex("idx__mcpChatTurns_active_slot")
	if active == "" || !strings.Contains(active, "UNIQUE") || !strings.Contains(strings.ToLower(active), "where") {
		t.Fatalf("active-slot index = %q, want a partial UNIQUE index", active)
	}
	if drops := turns.Fields.GetByName(chatFieldDroppedBefore); drops == nil || drops.Type() != core.FieldTypeNumber {
		t.Fatalf("droppedBefore field = %v", drops)
	}
	events, err := app.FindCollectionByNameOrId("_mcpChatEvents")
	if err != nil {
		t.Fatal(err)
	}
	seqIdx := events.GetIndex("idx__mcpChatEvents_turn_seq")
	if seqIdx == "" || !strings.Contains(seqIdx, "UNIQUE") {
		t.Fatalf("turn/seq index = %q, want UNIQUE", seqIdx)
	}

	proposals, err := app.FindCollectionByNameOrId("_mcpChatProposals")
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []string{
		"idx__mcpChatProposals_owner_conversation",
		"idx__mcpChatProposals_state",
		"idx__mcpChatProposals_state_expires",
	} {
		if proposals.GetIndex(idx) == "" {
			t.Fatalf("proposal index %q is missing", idx)
		}
	}
	for _, field := range []string{
		chatFieldProposalConversation, chatFieldProposalTurn, chatFieldCallID,
		chatFieldTool, chatFieldToolFingerprint, chatFieldManifestRevision,
		chatFieldLocation, chatFieldTarget, chatFieldArguments,
		chatFieldArgumentsHash, chatFieldSummary, chatFieldState, chatFieldRevision,
		chatFieldExpiresAt, chatFieldDecidedAt, chatFieldResult, chatFieldFailure,
		chatFieldEditorRevision, chatFieldHunks,
	} {
		if got := proposals.Fields.GetByName(field); got == nil {
			t.Fatalf("proposal field %q is missing", field)
		}
	}
	if rev, ok := proposals.Fields.GetByName(chatFieldRevision).(*core.NumberField); !ok || !rev.OnlyInt {
		t.Fatalf("proposal revision field = %v, want an integer number field", proposals.Fields.GetByName(chatFieldRevision))
	}

	uploads, err := app.FindCollectionByNameOrId("_mcpChatUploads")
	if err != nil {
		t.Fatal(err)
	}
	if uploads.GetIndex("idx__mcpChatUploads_owner_conversation") == "" {
		t.Fatal("uploads owner/conversation index is missing")
	}
	for _, field := range []string{
		chatFieldOwnerKey, chatFieldUploadConversation, chatFieldUploadName,
		chatFieldUploadMIME, chatFieldUploadKind, chatFieldUploadSize,
		chatFieldUploadSHA256, chatFieldUploadData, chatFieldCreatedAt, chatFieldUpdatedAt,
	} {
		if got := uploads.Fields.GetByName(field); got == nil {
			t.Fatalf("upload field %q is missing", field)
		}
	}
	data, ok := uploads.Fields.GetByName(chatFieldUploadData).(*core.FileField)
	if !ok {
		t.Fatalf("upload data field = %T, want a file field", uploads.Fields.GetByName(chatFieldUploadData))
	}
	if !data.Protected || data.MaxSelect != 1 || data.MaxSize != chatMaxUploadFileBytes {
		t.Fatalf("upload data field = %+v, want a protected single file bounded by the storage limit", data)
	}
	if size, ok := uploads.Fields.GetByName(chatFieldUploadSize).(*core.NumberField); !ok || !size.OnlyInt {
		t.Fatalf("upload size field = %v, want an integer number field", uploads.Fields.GetByName(chatFieldUploadSize))
	}
}

func TestChatStoreProvisioningIdempotentAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	first := newChatTestAppAt(t, dir)
	store1, err := NewChatStore(first, ChatStoreOptions{})
	if err != nil {
		t.Fatalf("first provisioning: %v", err)
	}
	ids := map[string]string{}
	for _, name := range store1.Collections() {
		col, err := first.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = col.Id
	}
	// A second EnableChat/provisioning pass on the same data directory must
	// adopt the existing schema untouched.
	first.ResetBootstrapState()

	second := newChatTestAppAt(t, dir)
	store2, err := NewChatStore(second, ChatStoreOptions{})
	if err != nil {
		t.Fatalf("second provisioning after restart: %v", err)
	}
	for _, name := range store2.Collections() {
		col, err := second.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		if ids[name] != col.Id {
			t.Fatalf("collection %q id changed across restart: %s -> %s", name, ids[name], col.Id)
		}
	}

	// A third pass on a still-running app is idempotent too.
	if _, err := NewChatStore(second, ChatStoreOptions{}); err != nil {
		t.Fatalf("third provisioning: %v", err)
	}
}

func TestChatStoreRejectsConflictingCollections(t *testing.T) {
	t.Run("public-rules", func(t *testing.T) {
		app := newChatTestApp(t)
		col := core.NewBaseCollection("_mcpChatConversations")
		col.ListRule = types.Pointer(`@request.auth.id != ""`)
		col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey})
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		_, err := NewChatStore(app, ChatStoreOptions{})
		if err == nil {
			t.Fatal("conflicting collection was accepted")
		}
		if !strings.Contains(err.Error(), "_mcpChatConversations") || !strings.Contains(err.Error(), "API rules") {
			t.Fatalf("error does not name the conflict: %v", err)
		}
	})

	t.Run("missing-field", func(t *testing.T) {
		app := newChatTestApp(t)
		col := core.NewBaseCollection("_mcpChatMessages")
		col.Fields.Add(&core.TextField{Name: "somethingElse"})
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		_, err := NewChatStore(app, ChatStoreOptions{})
		if err == nil {
			t.Fatal("conflicting collection was accepted")
		}
		if !strings.Contains(err.Error(), "_mcpChatMessages") || !strings.Contains(err.Error(), "missing field") {
			t.Fatalf("error does not name the conflict: %v", err)
		}
	})

	t.Run("wrong-field-type", func(t *testing.T) {
		app := newChatTestApp(t)
		col := core.NewBaseCollection("_mcpChatEvents")
		// Every expected field exists, but seq is text rather than a number.
		col.Fields.Add(
			&core.TextField{Name: chatFieldOwnerKey},
			&core.TextField{Name: chatFieldConversation},
			&core.TextField{Name: chatFieldTurn},
			&core.TextField{Name: chatFieldSeq},
			&core.TextField{Name: chatFieldType},
			&core.TextField{Name: chatFieldVersion},
			&core.JSONField{Name: chatFieldData},
			&core.DateField{Name: chatFieldAt},
		)
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		_, err := NewChatStore(app, ChatStoreOptions{})
		if err == nil {
			t.Fatal("conflicting collection was accepted")
		}
		if !strings.Contains(err.Error(), "_mcpChatEvents") || !strings.Contains(err.Error(), chatFieldSeq) {
			t.Fatalf("error does not name the conflict: %v", err)
		}
	})
}

func TestChatStoreRejectsBadPrefix(t *testing.T) {
	app := newChatTestApp(t)
	for _, prefix := range []string{"has space", "-leading", "quote'", "semi;colon"} {
		if _, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix}); err == nil {
			t.Fatalf("prefix %q accepted", prefix)
		}
	}
}

// TestChatStoreInterruptRunningAcrossInstances is the restart-recovery corner
// at the store level: a second store over the same collections still finds and
// interrupts turns left non-terminal by the first process.
func TestChatStoreInterruptRunningAcrossInstances(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner := chat.OwnerKey{Namespace: "ns", Mount: "/mcp", AuthCollectionID: "users0000000001", UserID: "user00000000001"}
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "c"})
	if err != nil {
		t.Fatal(err)
	}
	turn, created, err := store.CreateTurn(context.Background(), owner, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "k"})
	if err != nil || !created {
		t.Fatalf("CreateTurn = %v, %v", created, err)
	}
	if _, err := store.AppendEvent(context.Background(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnStarted}); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n, err := restarted.InterruptRunning(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("InterruptRunning = %d, %v", n, err)
	}
	events, err := restarted.EventsAfter(context.Background(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, ev := range events {
		if ev.Type == chat.EventTurnFailed {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1: %+v", terminals, events)
	}
	if again, err := restarted.InterruptRunning(context.Background()); err != nil || again != 0 {
		t.Fatalf("second interruption = %d, %v", again, err)
	}
}

// proposalOwner is the fixture owner used by the proposal tests below.
func proposalOwner() chat.OwnerKey {
	return chat.OwnerKey{
		Namespace:        "app",
		Mount:            "/mcp",
		AuthCollectionID: "users0000000001",
		UserID:           "user00000000001",
	}
}

func mustProposalStore(t *testing.T, store chat.Store) chat.ProposalStore {
	t.Helper()
	ps, ok := store.(chat.ProposalStore)
	if !ok {
		t.Fatalf("store %T does not implement chat.ProposalStore", store)
	}
	return ps
}

// TestChatStoreProposalContract runs the shared chat.ProposalStore contract
// against the PocketBase-backed store.
func TestChatStoreProposalContract(t *testing.T) {
	app := newChatTestApp(t)
	var seq atomic.Int64
	storetest.RunProposals(t, func(t *testing.T) interface {
		chat.Store
		chat.ProposalStore
	} {
		t.Helper()
		n := seq.Add(1)
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: fmt.Sprintf("_proposalstoretest%d", n)})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

// TestChatStoreProposalUpgradeFromFourCollections proves a v1.1 installation
// (the four chat collections, with data) gains the proposal collection on the
// next start without touching what is already stored.
func TestChatStoreProposalUpgradeFromFourCollections(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner := proposalOwner()
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "v1.1"})
	if err != nil {
		t.Fatal(err)
	}

	// Remove the proposal and upload collections so the app looks exactly like
	// a v1.1 installation, and prove the remaining four keep working.
	for _, name := range []string{"_mcpChatProposals", "_mcpChatUploads"} {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Delete(col); err != nil {
			t.Fatal(err)
		}
		if _, err := app.FindCollectionByNameOrId(name); err == nil {
			t.Fatalf("%s still exists after the simulated downgrade", name)
		}
	}
	ids := map[string]string{}
	for _, name := range []string{"_mcpChatConversations", "_mcpChatMessages", "_mcpChatTurns", "_mcpChatEvents"} {
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
	if got := upgraded.Collections(); len(got) != 6 || got[4] != "_mcpChatProposals" || got[5] != "_mcpChatUploads" {
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
	// The existing conversation is untouched and the new collection is usable.
	if got, err := upgraded.GetConversation(context.Background(), owner, conv.ID); err != nil || got.Title != "v1.1" {
		t.Fatalf("conversation after upgrade = %+v, %v", got, err)
	}
	p, err := upgraded.CreateProposal(context.Background(), owner, chat.Proposal{
		ConversationID: conv.ID,
		Tool:           "update_record",
		Location:       chat.ProposalLocationServer,
		Arguments:      json.RawMessage(`{"id":"r1"}`),
	})
	if err != nil {
		t.Fatalf("CreateProposal after upgrade: %v", err)
	}
	if p.State != chat.ProposalStatePending || p.Revision != 1 {
		t.Fatalf("proposal after upgrade = %+v", p)
	}
}

// TestChatStoreRejectsConflictingProposalsCollection keeps the proposal
// collection under the same adoption rules as the other four.
func TestChatStoreRejectsConflictingProposalsCollection(t *testing.T) {
	app := newChatTestApp(t)
	col := core.NewBaseCollection("_mcpChatProposals")
	col.ListRule = types.Pointer(`@request.auth.id != ""`)
	col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	_, err := NewChatStore(app, ChatStoreOptions{})
	if err == nil {
		t.Fatal("conflicting proposals collection was accepted")
	}
	if !strings.Contains(err.Error(), "_mcpChatProposals") || !strings.Contains(err.Error(), "API rules") {
		t.Fatalf("error does not name the conflict: %v", err)
	}
}

// TestChatStoreProposalRecoveryAndExpiry is the restart-recovery corner at the
// store level: a second store over the same collections turns an executing
// proposal into unknown and ends overdue proposals, and neither runs twice.
func TestChatStoreProposalRecoveryAndExpiry(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := proposalOwner()
	conv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	ps := mustProposalStore(t, store)

	create := func(args string, expires time.Time) chat.Proposal {
		t.Helper()
		p, err := ps.CreateProposal(ctx, owner, chat.Proposal{
			ConversationID: conv.ID,
			Tool:           "update_record",
			Location:       chat.ProposalLocationServer,
			Arguments:      json.RawMessage(args),
			ExpiresAt:      expires,
		})
		if err != nil {
			t.Fatalf("CreateProposal: %v", err)
		}
		return p
	}

	executing := create(`{"id":"r1"}`, time.Time{})
	executing, err = ps.TransitionProposal(ctx, owner, executing.ID, chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{Decided: true})
	if err != nil {
		t.Fatalf("start executing: %v", err)
	}
	overdue := create(`{"id":"r2"}`, time.Now().UTC().Add(-time.Minute))
	future := create(`{"id":"r3"}`, time.Now().UTC().Add(time.Hour))

	restarted, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	restartedPS := mustProposalStore(t, restarted)
	n, err := restartedPS.RecoverExecuting(ctx)
	if err != nil || n != 1 {
		t.Fatalf("RecoverExecuting = %d, %v; want 1", n, err)
	}
	got, err := restartedPS.GetProposal(ctx, owner, executing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != chat.ProposalStateUnknown || got.Revision != executing.Revision+1 || got.Failure != chat.UnknownOutcomeMessage {
		t.Fatalf("recovered proposal = %+v", got)
	}
	if n, err := restartedPS.ExpireProposals(ctx, time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("ExpireProposals = %d, %v; want 1", n, err)
	}
	if got, err := restartedPS.GetProposal(ctx, owner, overdue.ID); err != nil || got.State != chat.ProposalStateExpired || got.Revision != 2 {
		t.Fatalf("expired proposal = %+v, %v", got, err)
	}
	if got, err := restartedPS.GetProposal(ctx, owner, future.ID); err != nil || got.State != chat.ProposalStatePending {
		t.Fatalf("future proposal = %+v, %v", got, err)
	}
	// Nothing is applied twice.
	if n, err := restartedPS.RecoverExecuting(ctx); err != nil || n != 0 {
		t.Fatalf("second RecoverExecuting = %d, %v; want 0", n, err)
	}
	if n, err := restartedPS.ExpireProposals(ctx, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("second ExpireProposals = %d, %v; want 0", n, err)
	}
}

// TestChatUniqueViolationDetector keeps a NOT NULL or CHECK failure from being
// mistaken for a lost uniqueness race: only a real unique violation may be
// retried or mapped to chat.ErrConflict.
func TestChatUniqueViolationDetector(t *testing.T) {
	unique := []string{
		`constraint failed: UNIQUE constraint failed: _mcpChatTurns.activeSlot (2067)`,
		`constraint failed: UNIQUE constraint failed: _mcpChatProposals.id (1555)`,
		`idx_x: Value must be unique.`,
	}
	for _, msg := range unique {
		if !chatUniqueViolation(errors.New(msg)) {
			t.Fatalf("unique error not recognised: %q", msg)
		}
	}
	notUnique := []string{
		`constraint failed: NOT NULL constraint failed: _mcpChatProposals.state (1299)`,
		`constraint failed: CHECK constraint failed: nonempty (275)`,
		`constraint failed: FOREIGN KEY constraint failed (787)`,
		`timeout`,
	}
	for _, msg := range notUnique {
		if chatUniqueViolation(errors.New(msg)) {
			t.Fatalf("non-unique error treated as unique: %q", msg)
		}
	}
	if chatUniqueViolation(nil) {
		t.Fatal("nil error treated as unique")
	}
}

// An import message over the parts column's limit is a 400-class import
// error naming the message, not a failed transaction.
func TestChatStoreImportRefusesOversizedMessage(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner := chat.OwnerKey{Namespace: "ns", Mount: "/mcp", AuthCollectionID: "users", UserID: "u1"}
	big := strings.Repeat("x", 900<<10)
	parts := make([]chat.Part, 5)
	for i := range parts {
		parts[i] = chat.Part{Type: chat.PartText, Text: big}
	}
	_, _, err = store.ImportConversation(context.Background(), owner, chat.ImportedConversation{
		SourceID: "big", Messages: []chat.Message{{Role: chat.RoleAssistant, Parts: parts}},
	})
	if !errors.Is(err, chat.ErrInvalidImport) || !strings.Contains(err.Error(), "message 0") {
		t.Fatalf("err = %v, want ErrInvalidImport naming message 0", err)
	}
	if list, _, _ := store.ListConversations(context.Background(), owner, "", chat.Page{}); len(list) != 0 {
		t.Fatalf("a refused import left %d conversations", len(list))
	}
}
