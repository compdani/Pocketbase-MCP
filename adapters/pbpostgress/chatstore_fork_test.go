package pbpostgress

// ChatStore acceptance tests against a disposable PostgreSQL database
// (PB_DB_DSN). They cover the shared chat.Store contract, the fork's portable
// schema, concurrent provisioning by two instances, scoped restart recovery and
// the unique-violation detector.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/storetest"
)

var chatTestSeq atomic.Int64

// chatTestPrefix returns a per-call collection namespace. A random salt makes
// repeated test runs (go test -count=2, or another run on the same database)
// independent even though the database is shared.
func chatTestPrefix(t *testing.T) string {
	t.Helper()
	prefix := fmt.Sprintf("_chattest%s%d", strings.ToLower(security.RandomString(6)), chatTestSeq.Add(1))
	if len(prefix+"Conversations") > chatCollectionNameMaxLen {
		t.Fatalf("test prefix %q is too long", prefix)
	}
	return prefix
}

func newChatForkApp(t *testing.T) core.App {
	t.Helper()
	return newForkOAuthApp(t)
}

func TestChatStoreContract(t *testing.T) {
	app := newChatForkApp(t)
	storetest.Run(t, func(t *testing.T) chat.Store {
		t.Helper()
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: chatTestPrefix(t)})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

func TestChatStoreRetention(t *testing.T) {
	app := newChatForkApp(t)
	storetest.RunRetention(t, func(t *testing.T, max int) chat.Store {
		t.Helper()
		store, err := NewChatStore(app, ChatStoreOptions{
			Prefix:           chatTestPrefix(t),
			MaxEventsPerTurn: max,
		})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

func TestChatStoreSchema(t *testing.T) {
	app := newChatForkApp(t)
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
		if !col.System {
			// The fork hides non-system collections from an instance whose
			// scope excludes them; chat must never depend on that.
			t.Fatalf("%s is not a System collection", col.Name)
		}
		if !app.HasTable(col.Name) {
			t.Fatalf("%s has no table", col.Name)
		}
		if col.ListRule != nil || col.ViewRule != nil || col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
			t.Fatalf("%s is not private: rules must all be nil", col.Name)
		}
	}

	turns, err := app.FindCollectionByNameOrId("_mcpChatTurns")
	if err != nil {
		t.Fatal(err)
	}
	active := turns.GetIndex("idx__mcpChatTurns_active")
	if active == "" || !strings.Contains(active, "UNIQUE") {
		t.Fatalf("active-slot index = %q, want a UNIQUE index", active)
	}
	if strings.Contains(strings.ToUpper(active), "WHERE") {
		t.Fatalf("active-slot index is partial, which the store never relies on: %q", active)
	}
	idem := turns.GetIndex("idx__mcpChatTurns_idempotency")
	if idem == "" || !strings.Contains(idem, "UNIQUE") || strings.Contains(strings.ToUpper(idem), "WHERE") {
		t.Fatalf("idempotency index = %q, want a plain UNIQUE index", idem)
	}
	if !strings.Contains(idem, chatFieldIdempotencySlot) {
		t.Fatalf("idempotency index does not cover the digest column: %q", idem)
	}
	if drops := turns.Fields.GetByName(chatFieldDroppedBefore); drops == nil || drops.Type() != core.FieldTypeNumber {
		t.Fatalf("droppedBefore field = %v", drops)
	}
	for _, name := range []string{chatFieldIdempotencyKey, chatFieldIdempotencySlot, chatFieldActiveSlot} {
		if f := turns.Fields.GetByName(name); f == nil || f.Type() != core.FieldTypeText {
			t.Fatalf("%s field = %v, want text", name, f)
		}
	}

	events, err := app.FindCollectionByNameOrId("_mcpChatEvents")
	if err != nil {
		t.Fatal(err)
	}
	seqIdx := events.GetIndex("idx__mcpChatEvents_turn_seq")
	if seqIdx == "" || !strings.Contains(seqIdx, "UNIQUE") {
		t.Fatalf("turn/seq index = %q, want UNIQUE", seqIdx)
	}
	// JSON payloads must round-trip byte for byte, so they are text columns
	// rather than the fork's json/jsonb column (which normalizes them).
	if f := events.Fields.GetByName(chatFieldData); f == nil || f.Type() != core.FieldTypeText {
		t.Fatalf("event data field = %v, want text", f)
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
		raw := proposals.GetIndex(idx)
		if raw == "" || strings.Contains(strings.ToUpper(raw), "WHERE") {
			t.Fatalf("proposal index %q = %q, want a plain index", idx, raw)
		}
	}
	if f := proposals.Fields.GetByName(chatFieldArguments); f == nil || f.Type() != core.FieldTypeText {
		t.Fatalf("proposal arguments field = %v, want text", f)
	}
	if rev, ok := proposals.Fields.GetByName(chatFieldRevision).(*core.NumberField); !ok || !rev.OnlyInt {
		t.Fatalf("proposal revision field = %v, want an integer number field", proposals.Fields.GetByName(chatFieldRevision))
	}

	uploads, err := app.FindCollectionByNameOrId("_mcpChatUploads")
	if err != nil {
		t.Fatal(err)
	}
	rawUploadIdx := uploads.GetIndex("idx__mcpChatUploads_owner_conversation")
	if rawUploadIdx == "" || strings.Contains(strings.ToUpper(rawUploadIdx), "WHERE") {
		t.Fatalf("uploads owner/conversation index = %q, want a plain index", rawUploadIdx)
	}
	for _, name := range []string{
		chatFieldOwnerKey, chatFieldUploadConversation, chatFieldUploadName,
		chatFieldUploadMIME, chatFieldUploadKind, chatFieldUploadSHA256,
	} {
		if f := uploads.Fields.GetByName(name); f == nil || f.Type() != core.FieldTypeText {
			t.Fatalf("%s field = %v, want text", name, f)
		}
	}
	if f := uploads.Fields.GetByName(chatFieldUploadSize); f == nil || f.Type() != core.FieldTypeNumber {
		t.Fatalf("upload size field = %v, want number", f)
	}
	data, ok := uploads.Fields.GetByName(chatFieldUploadData).(*core.FileField)
	if !ok || !data.Protected || data.MaxSelect != 1 || data.MaxSize != chatMaxUploadFileBytes {
		t.Fatalf("upload data field = %v, want a protected single file bounded by the storage limit", uploads.Fields.GetByName(chatFieldUploadData))
	}
}

func TestChatStoreProvisioningIdempotentAcrossInstances(t *testing.T) {
	prefix := chatTestPrefix(t)
	first := newChatForkApp(t)
	store1, err := NewChatStore(first, ChatStoreOptions{Prefix: prefix})
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
	// A second pass on the first app is idempotent.
	if _, err := NewChatStore(first, ChatStoreOptions{Prefix: prefix}); err != nil {
		t.Fatalf("second provisioning on the same app: %v", err)
	}
	// A second *instance* over the same database adopts the same collections.
	second := newChatForkApp(t)
	store2, err := NewChatStore(second, ChatStoreOptions{Prefix: prefix})
	if err != nil {
		t.Fatalf("provisioning on the second instance: %v", err)
	}
	for _, name := range store2.Collections() {
		col, err := second.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		if ids[name] != col.Id {
			t.Fatalf("collection %q id changed across instances: %s -> %s", name, ids[name], col.Id)
		}
	}
}

// TestChatStoreConcurrentProvisioning starts two instances against the same
// database at the same time: the loser of a collection create must adopt the
// winner's collection instead of failing startup.
func TestChatStoreConcurrentProvisioning(t *testing.T) {
	prefix := chatTestPrefix(t)
	appA := newChatForkApp(t)
	appB := newChatForkApp(t)

	start := make(chan struct{})
	type result struct {
		store *ChatStore
		err   error
	}
	results := make(chan result, 2)
	for _, app := range []core.App{appA, appB} {
		go func(app core.App) {
			<-start
			store, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
			results <- result{store, err}
		}(app)
	}
	close(start)

	var stores []*ChatStore
	for i := 0; i < 2; i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("concurrent provisioning failed: %v", res.err)
		}
		stores = append(stores, res.store)
	}
	// Both instances must see one shared schema.
	colA, err := appA.FindCollectionByNameOrId(prefix + "Turns")
	if err != nil {
		t.Fatal(err)
	}
	colB, err := appB.FindCollectionByNameOrId(prefix + "Turns")
	if err != nil {
		t.Fatal(err)
	}
	if colA.Id != colB.Id {
		t.Fatalf("two turn collections exist: %s and %s", colA.Id, colB.Id)
	}
	// And both stores must be able to use it for the same conversation.
	owner := chat.OwnerKey{Namespace: "concurrent", Mount: "/mcp", AuthCollectionID: "users0000000001", UserID: "user00000000001"}
	conv, err := stores[0].CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stores[1].GetConversation(context.Background(), owner, conv.ID); err != nil {
		t.Fatalf("second store cannot read the first store's conversation: %v", err)
	}
}

// TestChatStoreCreateTurnConcurrentAcrossInstances is the cross-instance
// version of the contract's CreateTurn race: two apps write to the same
// collection through separate connections, so both can pass the pre-check and
// one must lose the unique index and be classified as "conversation busy".
func TestChatStoreCreateTurnConcurrentAcrossInstances(t *testing.T) {
	prefix := chatTestPrefix(t)
	appA := newChatForkApp(t)
	appB := newChatForkApp(t)
	storeA, err := NewChatStore(appA, ChatStoreOptions{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := NewChatStore(appB, ChatStoreOptions{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	owner := chat.OwnerKey{Namespace: "race", Mount: "/mcp", AuthCollectionID: "users0000000001", UserID: "user00000000001"}
	conv, err := storeA.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "race"})
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	var (
		wg      sync.WaitGroup
		created int
		active  int
		other   []error
		mu      sync.Mutex
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := storeA
			if i%2 == 1 {
				store = storeB
			}
			<-start
			_, wasCreated, err := store.CreateTurn(context.Background(), owner, chat.Turn{
				ConversationID: conv.ID,
				IdempotencyKey: fmt.Sprintf("race-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && wasCreated:
				created++
			case errors.Is(err, chat.ErrActiveTurn):
				active++
			case err != nil:
				other = append(other, err)
			default:
				other = append(other, errors.New("created=false without error"))
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected results: %v", other)
	}
	if created != 1 || active != n-1 {
		t.Fatalf("created=%d active=%d, want 1 and %d", created, active, n-1)
	}
	turns, err := storeB.app.FindRecordsByFilter(storeB.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation}", "", 0, 0,
		dbx.Params{chatFieldOwnerKey: owner.String(), chatFieldConversation: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("stored turns = %d, want 1", len(turns))
	}
}

func TestChatStoreRejectsConflictingCollections(t *testing.T) {
	t.Run("public-rules", func(t *testing.T) {
		app := newChatForkApp(t)
		prefix := chatTestPrefix(t)
		col := core.NewBaseCollection(prefix + "Conversations")
		col.System = true
		col.ListRule = types.Pointer(`@request.auth.id != ""`)
		col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey, Max: chatMaxTextLen})
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		_, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
		if err == nil {
			t.Fatal("conflicting collection was accepted")
		}
		if !strings.Contains(err.Error(), prefix+"Conversations") || !strings.Contains(err.Error(), "API rules") {
			t.Fatalf("error does not name the conflict: %v", err)
		}
	})

	t.Run("not-system", func(t *testing.T) {
		app := newChatForkApp(t)
		prefix := chatTestPrefix(t)
		// A collection that is not a System collection would be invisible to
		// an instance whose scope excludes it, so it must never be adopted.
		col := core.NewBaseCollection(prefix + "Conversations")
		col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey, Max: chatMaxTextLen})
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		if _, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix}); err == nil {
			t.Fatal("non-system collection was accepted")
		} else if !strings.Contains(err.Error(), "System") {
			t.Fatalf("error does not explain the conflict: %v", err)
		}
	})

	t.Run("missing-field", func(t *testing.T) {
		app := newChatForkApp(t)
		prefix := chatTestPrefix(t)
		col := core.NewBaseCollection(prefix + "Messages")
		col.System = true
		col.Fields.Add(&core.TextField{Name: "somethingElse", Max: chatMaxTextLen})
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		_, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
		if err == nil {
			t.Fatal("conflicting collection was accepted")
		}
		if !strings.Contains(err.Error(), prefix+"Messages") || !strings.Contains(err.Error(), "missing field") {
			t.Fatalf("error does not name the conflict: %v", err)
		}
	})

	t.Run("wrong-field-type", func(t *testing.T) {
		app := newChatForkApp(t)
		prefix := chatTestPrefix(t)
		col := core.NewBaseCollection(prefix + "Events")
		col.System = true
		col.Fields.Add(
			&core.TextField{Name: chatFieldOwnerKey, Max: chatMaxTextLen},
			&core.TextField{Name: chatFieldConversation, Max: chatMaxTextLen},
			&core.TextField{Name: chatFieldTurn, Max: chatMaxTextLen},
			&core.TextField{Name: chatFieldSeq, Max: chatMaxTextLen},
			&core.TextField{Name: chatFieldType, Max: chatMaxTextLen},
			&core.TextField{Name: chatFieldVersion, Max: chatMaxTextLen},
			&core.TextField{Name: chatFieldData, Max: chatMaxTextLen},
			&core.DateField{Name: chatFieldAt},
		)
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
		_, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
		if err == nil {
			t.Fatal("conflicting collection was accepted")
		}
		if !strings.Contains(err.Error(), prefix+"Events") || !strings.Contains(err.Error(), chatFieldSeq) {
			t.Fatalf("error does not name the conflict: %v", err)
		}
	})

	t.Run("partial-index", func(t *testing.T) {
		app := newChatForkApp(t)
		prefix := chatTestPrefix(t)
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
		if err != nil {
			t.Fatal(err)
		}
		turns, err := app.FindCollectionByNameOrId(store.turns)
		if err != nil {
			t.Fatal(err)
		}
		turns.RemoveIndex("idx_" + store.turns + "_active")
		turns.AddIndex("idx_"+store.turns+"_active", true, chatFieldActiveSlot, `"`+chatFieldActiveSlot+`" != ''`)
		if err := app.Save(turns); err != nil {
			t.Fatal(err)
		}
		if _, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix}); err == nil {
			t.Fatal("partial active-slot index was accepted")
		} else if !strings.Contains(err.Error(), "partial") {
			t.Fatalf("error does not explain the conflict: %v", err)
		}
	})
}

func TestChatStoreRejectsBadPrefix(t *testing.T) {
	app := newChatForkApp(t)
	for _, prefix := range []string{"has space", "-leading", "quote'", "semi;colon"} {
		if _, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix}); err == nil {
			t.Fatalf("prefix %q accepted", prefix)
		}
	}
	if _, err := NewChatStore(app, ChatStoreOptions{Prefix: strings.Repeat("a", chatCollectionNameMaxLen)}); err == nil {
		t.Fatal("over-long collection name accepted")
	}
	if _, err := NewChatStore(app, ChatStoreOptions{Namespace: "only-namespace"}); err == nil {
		t.Fatal("half-set recovery scope accepted")
	}
}

// TestChatStoreInterruptRunningAcrossInstances is the restart-recovery corner
// at the store level: a second store over the same collections still finds and
// interrupts turns left non-terminal by the first process.
func TestChatStoreInterruptRunningAcrossInstances(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	store, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
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

	// A store scoped to another app must not touch this turn.
	other, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix, Namespace: "other-ns", Mount: "/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := other.InterruptRunning(context.Background()); err != nil || n != 0 {
		t.Fatalf("scoped InterruptRunning = %d, %v; want 0", n, err)
	}
	if stored, err := store.GetTurn(context.Background(), owner, turn.ID); err != nil || stored.State != chat.TurnStateQueued {
		t.Fatalf("another scope changed the turn: %+v, %v", stored, err)
	}

	restarted, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
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
	// Recovery freed the conversation while keeping the terminal slot unique.
	if _, _, err := restarted.CreateTurn(context.Background(), owner, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "after"}); err != nil {
		t.Fatalf("conversation still blocked after recovery: %v", err)
	}
}

// TestChatStoreScopedRecoveryByMount proves the mount half of the scope: the
// same app on two mounts must recover only its own mount's turns.
func TestChatStoreScopedRecoveryByMount(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	base, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	ownerA := chat.OwnerKey{Namespace: "ns", Mount: "/mcp", AuthCollectionID: "users0000000001", UserID: "user00000000001"}
	ownerB := chat.OwnerKey{Namespace: "ns", Mount: "/mcp2", AuthCollectionID: "users0000000001", UserID: "user00000000001"}
	for _, owner := range []chat.OwnerKey{ownerA, ownerB} {
		conv, err := base.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "c"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := base.CreateTurn(context.Background(), owner, chat.Turn{ConversationID: conv.ID}); err != nil {
			t.Fatal(err)
		}
	}
	mcpStore, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix, Namespace: "ns", Mount: "/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := mcpStore.InterruptRunning(context.Background()); err != nil || n != 1 {
		t.Fatalf("scoped recovery = %d, %v; want 1", n, err)
	}
	if n, err := base.InterruptRunning(context.Background()); err != nil || n != 1 {
		t.Fatalf("unscoped recovery after scoped one = %d, %v; want 1", n, err)
	}
}

// TestChatStoreKeyPlaceholders proves the two storage placeholders never leak
// into the API: a keyless turn reports no key, two keyless turns coexist, and
// the same client key stays scoped to its owner.
func TestChatStoreKeyPlaceholders(t *testing.T) {
	app := newChatForkApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{Prefix: chatTestPrefix(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ownerA := chat.OwnerKey{Namespace: "ns-a", Mount: "/mcp", AuthCollectionID: "users0000000001", UserID: "user00000000001"}
	ownerB := chat.OwnerKey{Namespace: "ns-b", Mount: "/mcp", AuthCollectionID: "users0000000001", UserID: "user00000000001"}

	// Keyless turns in two conversations both succeed, and neither exposes a
	// placeholder as its client key.
	var keyless []chat.Turn
	for i := 0; i < 2; i++ {
		conv, err := store.CreateConversation(ctx, chat.Conversation{Owner: ownerA, Title: "keyless"})
		if err != nil {
			t.Fatal(err)
		}
		turn, created, err := store.CreateTurn(ctx, ownerA, chat.Turn{ConversationID: conv.ID})
		if err != nil || !created {
			t.Fatalf("keyless CreateTurn %d = %v, %v", i, created, err)
		}
		if turn.IdempotencyKey != "" {
			t.Fatalf("keyless turn exposes a placeholder key: %q", turn.IdempotencyKey)
		}
		stored, err := store.GetTurn(ctx, ownerA, turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.IdempotencyKey != "" {
			t.Fatalf("stored keyless turn exposes a placeholder key: %q", stored.IdempotencyKey)
		}
		keyless = append(keyless, turn)
	}
	if keyless[0].ID == keyless[1].ID {
		t.Fatal("keyless turns share an id")
	}

	// The same client key in two namespaces is two independent submissions:
	// the digest covers the owner key, not just the conversation and the key.
	for _, owner := range []chat.OwnerKey{ownerA, ownerB} {
		conv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "shared-key"})
		if err != nil {
			t.Fatal(err)
		}
		turn, created, err := store.CreateTurn(ctx, owner, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "same-key"})
		if err != nil || !created {
			t.Fatalf("%s CreateTurn = %v, %v", owner.Namespace, created, err)
		}
		if turn.IdempotencyKey != "same-key" {
			t.Fatalf("%s key = %q, want the client key", owner.Namespace, turn.IdempotencyKey)
		}
		again, created, err := store.CreateTurn(ctx, owner, chat.Turn{ConversationID: conv.ID, IdempotencyKey: "same-key"})
		if err != nil || created {
			t.Fatalf("%s replay = %v, %v", owner.Namespace, created, err)
		}
		if again.ID != turn.ID {
			t.Fatalf("%s replay returned turn %q, want %q", owner.Namespace, again.ID, turn.ID)
		}
	}
}

func TestChatUniqueViolationDetector(t *testing.T) {
	unique := []string{
		`ERROR: duplicate key value violates unique constraint "idx_x" (SQLSTATE 23505)`,
		`ERROR: duplicate key value violates unique constraint "idx_x"`,
		`slot: Value must be unique.`,
	}
	for _, msg := range unique {
		if !chatUniqueViolation(errors.New(msg)) {
			t.Fatalf("unique error not recognised: %q", msg)
		}
	}
	notUnique := []string{
		`ERROR: null value in column "slot" violates not-null constraint (SQLSTATE 23502)`,
		`ERROR: insert or update on table "x" violates foreign key constraint "y" (SQLSTATE 23503)`,
		`ERROR: new row for relation "x" violates check constraint "y" (SQLSTATE 23514)`,
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

// forkProposalOwner is the fixture owner used by the proposal tests below.
func forkProposalOwner(namespace, mount string) chat.OwnerKey {
	return chat.OwnerKey{
		Namespace:        namespace,
		Mount:            mount,
		AuthCollectionID: "users0000000001",
		UserID:           "user00000000001",
	}
}

// TestChatStoreProposalContract runs the shared chat.ProposalStore contract
// against the fork store on PostgreSQL.
func TestChatStoreProposalContract(t *testing.T) {
	app := newChatForkApp(t)
	storetest.RunProposals(t, func(t *testing.T) interface {
		chat.Store
		chat.ProposalStore
	} {
		t.Helper()
		store, err := NewChatStore(app, ChatStoreOptions{Prefix: chatTestPrefix(t)})
		if err != nil {
			t.Fatalf("NewChatStore: %v", err)
		}
		return store
	})
}

// TestChatStoreProposalUpgradeFromFourCollections proves an installation with
// only the v1.1 collections gains the proposal (and upload) collections on its
// next start without touching the existing schema or data.
func TestChatStoreProposalUpgradeFromFourCollections(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	owner := forkProposalOwner("upgrade", "/mcp")

	// Build exactly the four v1.1 collections (the fork refuses to delete a
	// System collection, so the legacy install is created, not simulated by a
	// delete).
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
	specs := legacy.specs()
	for _, spec := range specs[:4] {
		if _, err := provisionChatCollection(app, spec); err != nil {
			t.Fatalf("provision v1.1 collection: %v", err)
		}
	}
	for _, name := range []string{prefix + "Proposals", prefix + "Uploads"} {
		if _, err := app.FindCollectionByNameOrId(name); err == nil {
			t.Fatalf("the legacy install unexpectedly has %q", name)
		}
	}
	conv, err := legacy.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "v1.1"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{prefix + "Conversations", prefix + "Messages", prefix + "Turns", prefix + "Events"} {
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
	if got := upgraded.Collections(); len(got) != 6 || got[4] != prefix+"Proposals" || got[5] != prefix+"Uploads" {
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
	if !app.HasTable(prefix + "Proposals") {
		t.Fatal("the upgraded proposal collection has no table")
	}
	if got, err := upgraded.GetConversation(context.Background(), owner, conv.ID); err != nil || got.Title != "v1.1" {
		t.Fatalf("conversation after upgrade = %+v, %v", got, err)
	}
	if _, err := upgraded.CreateProposal(context.Background(), owner, chat.Proposal{
		ConversationID: conv.ID,
		Tool:           "update_record",
		Location:       chat.ProposalLocationServer,
		Arguments:      json.RawMessage(`{"id":"r1"}`),
	}); err != nil {
		t.Fatalf("CreateProposal after upgrade: %v", err)
	}
}

// TestChatStoreRejectsConflictingProposalsCollection keeps the proposal
// collection under the same adoption rules as the other four.
func TestChatStoreRejectsConflictingProposalsCollection(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	col := core.NewBaseCollection(prefix + "Proposals")
	col.System = true
	col.ListRule = types.Pointer(`@request.auth.id != ""`)
	col.Fields.Add(&core.TextField{Name: chatFieldOwnerKey, Max: chatMaxTextLen})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	_, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
	if err == nil {
		t.Fatal("conflicting proposals collection was accepted")
	}
	if !strings.Contains(err.Error(), prefix+"Proposals") || !strings.Contains(err.Error(), "API rules") {
		t.Fatalf("error does not name the conflict: %v", err)
	}
}

// TestChatStoreProposalRecoveryScopedToNamespaceAndMount proves one app's
// startup recovery and expiry sweep never reconcile another app's proposals on
// the shared database, while an unscoped store covers all of them.
func TestChatStoreProposalRecoveryScopedToNamespaceAndMount(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	ctx := context.Background()

	ownerOf := func(namespace, mount string) chat.OwnerKey {
		return forkProposalOwner(namespace, mount)
	}

	// seed creates one executing and one overdue proposal for the given scope
	// and returns their ids.
	seed := func(store *ChatStore, namespace, mount string) (chat.OwnerKey, string, string) {
		t.Helper()
		owner := ownerOf(namespace, mount)
		conv, err := store.CreateConversation(ctx, chat.Conversation{Owner: owner, Title: "scoped"})
		if err != nil {
			t.Fatalf("%s/%s conversation: %v", namespace, mount, err)
		}
		executing, err := store.CreateProposal(ctx, owner, chat.Proposal{
			ConversationID: conv.ID,
			Tool:           "update_record",
			Location:       chat.ProposalLocationServer,
			Arguments:      json.RawMessage(`{"id":"r1"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.TransitionProposal(ctx, owner, executing.ID,
			chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{Decided: true}); err != nil {
			t.Fatal(err)
		}
		overdue, err := store.CreateProposal(ctx, owner, chat.Proposal{
			ConversationID: conv.ID,
			Tool:           "update_record",
			Location:       chat.ProposalLocationServer,
			Arguments:      json.RawMessage(`{"id":"r2"}`),
			ExpiresAt:      time.Now().UTC().Add(-time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		return owner, executing.ID, overdue.ID
	}

	checkUntouched := func(t *testing.T, base *ChatStore, owner chat.OwnerKey, executingID, overdueID string) {
		t.Helper()
		got, err := base.GetProposal(ctx, owner, executingID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != chat.ProposalStateExecuting {
			t.Fatalf("another scope changed an executing proposal to %s", got.State)
		}
		got, err = base.GetProposal(ctx, owner, overdueID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != chat.ProposalStatePending {
			t.Fatalf("another scope expired a pending proposal: %s", got.State)
		}
	}

	for _, tc := range []struct {
		name                  string
		scopedNS, scopedMount string
		otherNS, otherMount   string
	}{
		{"mount", "ns-m", "/mcp", "ns-m", "/mcp2"},
		{"namespace", "ns-n1", "/mcp", "ns-n2", "/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix})
			if err != nil {
				t.Fatal(err)
			}
			scopedOwner, scopedExecuting, scopedOverdue := seed(base, tc.scopedNS, tc.scopedMount)
			otherOwner, otherExecuting, otherOverdue := seed(base, tc.otherNS, tc.otherMount)

			store, err := NewChatStore(app, ChatStoreOptions{Prefix: prefix, Namespace: tc.scopedNS, Mount: tc.scopedMount})
			if err != nil {
				t.Fatal(err)
			}
			if n, err := store.RecoverExecuting(ctx); err != nil || n != 1 {
				t.Fatalf("scoped RecoverExecuting = %d, %v; want 1", n, err)
			}
			if n, err := store.ExpireProposals(ctx, time.Now().UTC()); err != nil || n != 1 {
				t.Fatalf("scoped ExpireProposals = %d, %v; want 1", n, err)
			}
			// Its own rows are reconciled, the other scope's are not.
			got, err := base.GetProposal(ctx, scopedOwner, scopedExecuting)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != chat.ProposalStateUnknown || got.Failure != chat.UnknownOutcomeMessage || got.Revision != 3 {
				t.Fatalf("recovered proposal = %+v", got)
			}
			got, err = base.GetProposal(ctx, scopedOwner, scopedOverdue)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != chat.ProposalStateExpired || got.Revision != 2 {
				t.Fatalf("expired proposal = %+v", got)
			}
			checkUntouched(t, base, otherOwner, otherExecuting, otherOverdue)

			// An unscoped store now covers the other scope, and does nothing
			// twice.
			if n, err := base.RecoverExecuting(ctx); err != nil || n != 1 {
				t.Fatalf("unscoped RecoverExecuting = %d, %v; want 1", n, err)
			}
			if n, err := base.ExpireProposals(ctx, time.Now().UTC()); err != nil || n != 1 {
				t.Fatalf("unscoped ExpireProposals = %d, %v; want 1", n, err)
			}
			if n, err := base.RecoverExecuting(ctx); err != nil || n != 0 {
				t.Fatalf("second unscoped RecoverExecuting = %d, %v; want 0", n, err)
			}
			if n, err := base.ExpireProposals(ctx, time.Now().UTC()); err != nil || n != 0 {
				t.Fatalf("second unscoped ExpireProposals = %d, %v; want 0", n, err)
			}
		})
	}
}

// A message may be up to 4 MiB as on stock PocketBase, including on a store provisioned with the old
// 1 MiB field maximum; past that, the store refuses before the database.
func TestChatStorePostgresMessageLimit(t *testing.T) {
	app := newChatForkApp(t)
	prefix := chatTestPrefix(t)
	opts := ChatStoreOptions{Prefix: prefix, Namespace: "ns", Mount: "/mcp"}
	store, err := NewChatStore(app, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an install from before the raise.
	col, err := app.FindCollectionByNameOrId(store.messages)
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.GetByName(chatFieldParts).(*core.TextField).Max = 1 << 20
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if store, err = NewChatStore(app, opts); err != nil {
		t.Fatalf("re-provisioning an older store: %v", err)
	}
	col, err = app.FindCollectionByNameOrId(store.messages)
	if err != nil {
		t.Fatal(err)
	}
	if got := col.Fields.GetByName(chatFieldParts).(*core.TextField).Max; got != chatJSONFieldMaxChars {
		t.Fatalf("parts field max after upgrade = %d, want %d", got, chatJSONFieldMaxChars)
	}

	owner := chat.OwnerKey{Namespace: "ns", Mount: "/mcp", AuthCollectionID: "users", UserID: "u1"}
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a long tool result ", 2<<20/19)
	if _, err := store.AppendMessage(context.Background(), owner, chat.Message{
		ConversationID: conv.ID, Role: chat.RoleUser, Parts: []chat.Part{{Type: chat.PartText, Text: long}},
	}); err != nil {
		t.Fatalf("a 2 MiB message was refused on PostgreSQL: %v", err)
	}
	msgs, _, err := store.ListMessages(context.Background(), owner, conv.ID, chat.Page{})
	if err != nil || len(msgs) != 1 || msgs[0].Parts[0].Text != long {
		t.Fatalf("the 2 MiB message did not round-trip: %d messages, err %v", len(msgs), err)
	}
	huge := strings.Repeat("x", chatMaxJSONBytes)
	_, err = store.AppendMessage(context.Background(), owner, chat.Message{
		ConversationID: conv.ID, Role: chat.RoleUser, Parts: []chat.Part{{Type: chat.PartText, Text: huge}},
	})
	if err == nil || !strings.Contains(err.Error(), "over this database's limit") {
		t.Fatalf("an over-limit message: err = %v", err)
	}
}
