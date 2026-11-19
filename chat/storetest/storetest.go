// Package storetest is the reusable behavioural contract suite for
// [chat.Store] implementations.
//
// A store adapter (the in-memory MemStore, the stock PocketBase ChatStore, a
// host's custom store) runs [Run] against a fresh, empty store for every
// subtest and [RunRetention] when it supports bounded event retention. The
// suite only uses the exported chat API, so it imports nothing but the
// standard library and the chat package.
//
// Freshness matters: every subtest assumes it starts from an empty partition
// and that its owner keys are not shared with anything else in the store.
// newStore is therefore called once per subtest, not once per run.
package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
)

// Run exercises the chat.Store contract. newStore must return an empty store
// that is isolated from every other call (for a database adapter that means a
// fresh data directory, or at least a fresh collection namespace).
func Run(t *testing.T, newStore func(t *testing.T) chat.Store) {
	t.Helper()

	t.Run("ConversationLifecycle", func(t *testing.T) {
		testConversationLifecycle(t, newStore(t))
	})
	t.Run("CreateConversationValidatesOwner", func(t *testing.T) {
		testCreateConversationValidatesOwner(t, newStore(t))
	})
	t.Run("OwnerMismatchLooksMissing", func(t *testing.T) {
		testOwnerMismatchLooksMissing(t, newStore(t))
	})
	t.Run("ListConversationsTargetAndOrder", func(t *testing.T) {
		testListConversationsTargetAndOrder(t, newStore(t))
	})
	t.Run("ConversationPagination", func(t *testing.T) {
		testConversationPagination(t, newStore(t))
	})
	t.Run("MessagesDeepCopyAndOrder", func(t *testing.T) {
		testMessagesDeepCopyAndOrder(t, newStore(t))
	})
	t.Run("MessagePagination", func(t *testing.T) {
		testMessagePagination(t, newStore(t))
	})
	t.Run("CreateTurnIdempotent", func(t *testing.T) {
		testCreateTurnIdempotent(t, newStore(t))
	})
	t.Run("ActiveTurnRule", func(t *testing.T) {
		testActiveTurnRule(t, newStore(t))
	})
	t.Run("CreateTurnConcurrent", func(t *testing.T) {
		testCreateTurnConcurrent(t, newStore(t))
	})
	t.Run("TransitionCAS", func(t *testing.T) {
		testTransitionCAS(t, newStore(t))
	})
	t.Run("SeqsArePerTurn", func(t *testing.T) {
		testSeqsArePerTurn(t, newStore(t))
	})
	t.Run("TerminalEventRules", func(t *testing.T) {
		testTerminalEventRules(t, newStore(t))
	})
	t.Run("FinishTurnValidation", func(t *testing.T) {
		testFinishTurnValidation(t, newStore(t))
	})
	t.Run("FinishTurnAtomic", func(t *testing.T) {
		testFinishTurnAtomic(t, newStore(t))
	})
	t.Run("AppendRacingFinishStaysOrdered", func(t *testing.T) {
		testAppendRacingFinishStaysOrdered(t, newStore(t))
	})
	t.Run("InterruptRunning", func(t *testing.T) {
		testInterruptRunning(t, newStore(t))
	})
	t.Run("DeleteBlockedByActiveTurn", func(t *testing.T) {
		testDeleteBlockedByActiveTurn(t, newStore(t))
	})
	t.Run("EventsDeepCopy", func(t *testing.T) {
		testEventsDeepCopy(t, newStore(t))
	})
	t.Run("ActiveTurnFastPath", func(t *testing.T) {
		testActiveTurnFastPath(t, newStore(t))
	})
	t.Run("RecentMessages", func(t *testing.T) {
		testRecentMessages(t, newStore(t))
	})
	t.Run("CreateTurnTouchesConversation", func(t *testing.T) {
		testCreateTurnTouchesConversation(t, newStore(t))
	})
	t.Run("Summaries", func(t *testing.T) {
		testSummaries(t, newStore(t))
	})
}

// testSummaries checks the optional SummaryStore: the latest summary only, a
// conditional write, owner isolation, no effect on the conversation itself,
// and deletion with the conversation. A store without it is skipped.
func testSummaries(t *testing.T, s chat.Store) {
	sums, ok := s.(chat.SummaryStore)
	if !ok {
		t.Skip("store does not implement chat.SummaryStore")
	}
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "long chat")
	if _, found, err := sums.LatestSummary(ctx(), owner, c.ID); err != nil || found {
		t.Fatalf("new conversation: found=%v err=%v", found, err)
	}
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	first := chat.Summary{ConversationID: c.ID, ThroughMessageID: "m1", Text: "The user wants a quote.", Provider: "p", Model: "m", CreatedAt: at}
	if stored, err := sums.PutSummary(ctx(), owner, first, ""); err != nil || !stored {
		t.Fatalf("first summary: stored=%v err=%v", stored, err)
	}
	// A writer that read no summary loses to the one that stored first.
	if stored, err := sums.PutSummary(ctx(), owner, chat.Summary{ConversationID: c.ID, ThroughMessageID: "m2", Text: "late", CreatedAt: at}, ""); err != nil || stored {
		t.Fatalf("stale write: stored=%v err=%v, want not stored", stored, err)
	}
	second := chat.Summary{ConversationID: c.ID, ThroughMessageID: "m5", Text: "The user wants a quote for 3 items.", CreatedAt: at.Add(time.Minute)}
	if stored, err := sums.PutSummary(ctx(), owner, second, "m1"); err != nil || !stored {
		t.Fatalf("second summary: stored=%v err=%v", stored, err)
	}
	got, found, err := sums.LatestSummary(ctx(), owner, c.ID)
	if err != nil || !found {
		t.Fatalf("LatestSummary: found=%v err=%v", found, err)
	}
	if got.ThroughMessageID != "m5" || got.Text != second.Text || got.ConversationID != c.ID || !got.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("latest summary = %+v, want %+v", got, second)
	}

	if _, _, err := sums.LatestSummary(ctx(), otherOwner(), c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("other owner read: err=%v, want ErrNotFound", err)
	}
	if _, err := sums.PutSummary(ctx(), otherOwner(), chat.Summary{ConversationID: c.ID, ThroughMessageID: "x", Text: "x"}, "m5"); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("other owner write: err=%v, want ErrNotFound", err)
	}

	conv, err := s.GetConversation(ctx(), owner, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Title != c.Title || !conv.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("a summary changed the conversation: %+v, was %+v", conv, c)
	}

	if err := s.DeleteConversation(ctx(), owner, c.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if _, _, err := sums.LatestSummary(ctx(), owner, c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("after delete: err=%v, want ErrNotFound", err)
	}
}

// testCreateTurnTouchesConversation checks that a new turn moves the
// conversation's UpdatedAt, so a list shows when it last had activity.
func testCreateTurnTouchesConversation(t *testing.T, s chat.Store) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "activity")
	at := conv.UpdatedAt.Add(time.Hour).UTC().Truncate(time.Millisecond)
	if _, created, err := s.CreateTurn(ctx(), owner, chat.Turn{
		ConversationID: conv.ID, Owner: owner, IdempotencyKey: "touch", Model: "m", CreatedAt: at,
	}); err != nil || !created {
		t.Fatalf("CreateTurn: created=%v err=%v", created, err)
	}
	got, err := s.GetConversation(ctx(), owner, conv.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Fatalf("UpdatedAt = %v, want the turn's creation time %v", got.UpdatedAt, at)
	}
	if got.Title != "activity" || !got.CreatedAt.Equal(conv.CreatedAt) {
		t.Fatalf("CreateTurn changed more than UpdatedAt: %+v", got)
	}
}

// testActiveTurnFastPath checks the optional ActiveTurnStore. A store without
// it is skipped.
func testActiveTurnFastPath(t *testing.T, s chat.Store) {
	fast, ok := s.(chat.ActiveTurnStore)
	if !ok {
		t.Skip("store does not implement chat.ActiveTurnStore")
	}
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	if _, found, err := fast.ActiveTurn(ctx(), owner, c.ID); err != nil || found {
		t.Fatalf("empty conversation: found=%v err=%v", found, err)
	}
	turn := mustCreateTurn(t, s, owner, c.ID, "k1")
	got, found, err := fast.ActiveTurn(ctx(), owner, c.ID)
	if err != nil || !found || got.ID != turn.ID {
		t.Fatalf("active turn = %+v found=%v err=%v, want %s", got, found, err, turn.ID)
	}
	if _, _, err := fast.ActiveTurn(ctx(), otherOwner(), c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("other owner: err=%v, want ErrNotFound", err)
	}
	ev, err := chat.NewEvent(c.ID, turn.ID, chat.EventTurnCancelled, chat.TurnCancelled{Reason: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, ev); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}
	if _, found, err := fast.ActiveTurn(ctx(), owner, c.ID); err != nil || found {
		t.Fatalf("after finishing: found=%v err=%v", found, err)
	}
}

// testRecentMessages checks the optional RecentMessageStore. A store without
// it is skipped.
func testRecentMessages(t *testing.T, s chat.Store) {
	fast, ok := s.(chat.RecentMessageStore)
	if !ok {
		t.Skip("store does not implement chat.RecentMessageStore")
	}
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendMessage(ctx(), owner, chat.Message{
			ConversationID: c.ID,
			Role:           chat.RoleUser,
			Parts:          []chat.Part{{Type: chat.PartText, Text: fmt.Sprintf("m%d", i)}},
			CreatedAt:      base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := fast.RecentMessages(ctx(), owner, c.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range got {
		texts = append(texts, m.Parts[0].Text)
	}
	if strings.Join(texts, ",") != "m2,m3,m4" {
		t.Fatalf("recent messages = %v, want m2,m3,m4 in ascending order", texts)
	}
	if all, err := fast.RecentMessages(ctx(), owner, c.ID, 50); err != nil || len(all) != 5 {
		t.Fatalf("limit above the count: %d messages, err=%v", len(all), err)
	}
	if _, err := fast.RecentMessages(ctx(), otherOwner(), c.ID, 3); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("other owner: err=%v, want ErrNotFound", err)
	}
}

// RunRetention exercises the optional bounded-event-log contract: a store
// keeps at most max events per turn, drops the oldest first and reports
// chat.ErrResyncRequired for a cursor older than the retained window.
func RunRetention(t *testing.T, newStoreWithMaxEvents func(t *testing.T, max int) chat.Store) {
	t.Helper()

	t.Run("EventRetention", func(t *testing.T) {
		s := newStoreWithMaxEvents(t, 3)
		owner := ownerKey()
		c := mustCreateConversation(t, s, owner, "chat")
		turn := mustCreateTurn(t, s, owner, c.ID, "key")

		for i := 0; i < 5; i++ {
			mustAppendEvent(t, s, owner, chat.Event{
				TurnID: turn.ID,
				Type:   chat.EventTextDelta,
				Data:   mustJSON(chat.TextDelta{Text: fmt.Sprintf("%d", i)}),
			})
		}

		if _, err := s.EventsAfter(ctx(), owner, turn.ID, 0); !errors.Is(err, chat.ErrResyncRequired) {
			t.Fatalf("stale cursor = %v, want ErrResyncRequired", err)
		}
		if _, err := s.EventsAfter(ctx(), owner, turn.ID, 1); !errors.Is(err, chat.ErrResyncRequired) {
			t.Fatalf("cursor before retained window = %v, want ErrResyncRequired", err)
		}
		recent, err := s.EventsAfter(ctx(), owner, turn.ID, 2)
		if err != nil {
			t.Fatalf("recent cursor: %v", err)
		}
		if len(recent) != 3 || recent[0].Seq != 3 || recent[2].Seq != 5 {
			t.Fatalf("retained window = %+v", recent)
		}

		// The owner and turn still resolve: retention only drops events.
		if _, err := s.GetTurn(ctx(), owner, turn.ID); err != nil {
			t.Fatalf("turn disappeared with retention: %v", err)
		}
	})
}

// --- fixtures and helpers ---

func ctx() context.Context { return context.Background() }

func ownerKey() chat.OwnerKey {
	return chat.OwnerKey{
		Namespace:        "app",
		Mount:            "/mcp",
		AuthCollectionID: "users0000000001",
		UserID:           "user00000000001",
	}
}

func otherOwner() chat.OwnerKey {
	k := ownerKey()
	k.UserID = "otheruser000001"
	return k
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("storetest: marshal literal payload: " + err.Error())
	}
	return raw
}

func mustCreateConversation(t *testing.T, s chat.Store, owner chat.OwnerKey, title string) chat.Conversation {
	t.Helper()
	c, err := s.CreateConversation(ctx(), chat.Conversation{Owner: owner, Title: title})
	if err != nil {
		t.Fatalf("CreateConversation(%q): %v", title, err)
	}
	if c.ID == "" {
		t.Fatal("CreateConversation did not assign an ID")
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		t.Fatal("CreateConversation did not assign timestamps")
	}
	return c
}

func mustCreateTurn(t *testing.T, s chat.Store, owner chat.OwnerKey, convID, key string) chat.Turn {
	t.Helper()
	turn, created, err := s.CreateTurn(ctx(), owner, chat.Turn{
		ConversationID: convID,
		Owner:          owner,
		IdempotencyKey: key,
		Provider:       "test-provider",
		Model:          "test-model",
		Effort:         "high",
	})
	if err != nil {
		t.Fatalf("CreateTurn(%q): %v", key, err)
	}
	if !created {
		t.Fatalf("CreateTurn(%q) reported created=false", key)
	}
	if turn.State != chat.TurnStateQueued {
		t.Fatalf("new turn state = %s, want queued", turn.State)
	}
	if turn.Owner != owner {
		t.Fatalf("CreateTurn did not record the owner key: %+v", turn.Owner)
	}
	if turn.Provider != "test-provider" {
		t.Fatalf("CreateTurn provider = %q, want test-provider", turn.Provider)
	}
	if turn.Effort != "high" {
		t.Fatalf("CreateTurn effort = %q, want high", turn.Effort)
	}
	if finder, ok := s.(chat.IdempotentTurnStore); ok {
		found, ok, err := finder.FindTurnByIdempotencyKey(ctx(), owner, convID, key)
		if err != nil {
			t.Fatalf("FindTurnByIdempotencyKey(%q): %v", key, err)
		}
		if !ok || found.ID != turn.ID || found.Provider != turn.Provider || found.Effort != turn.Effort {
			t.Fatalf("FindTurnByIdempotencyKey(%q) = %+v, %v; want %+v", key, found, ok, turn)
		}
	}
	return turn
}

func mustAppendEvent(t *testing.T, s chat.Store, owner chat.OwnerKey, e chat.Event) chat.Event {
	t.Helper()
	got, err := s.AppendEvent(ctx(), owner, e)
	if err != nil {
		t.Fatalf("AppendEvent(%s): %v", e.Type, err)
	}
	return got
}

func mustCreateTargeted(t *testing.T, s chat.Store, owner chat.OwnerKey, title, target string, at time.Time) chat.Conversation {
	t.Helper()
	c, err := s.CreateConversation(ctx(), chat.Conversation{Owner: owner, Title: title, Target: target, CreatedAt: at})
	if err != nil {
		t.Fatalf("create targeted: %v", err)
	}
	return c
}

// --- conversation tests ---

func testConversationLifecycle(t *testing.T, s chat.Store) {
	owner := ownerKey()

	c := mustCreateConversation(t, s, owner, "first")
	got, err := s.GetConversation(ctx(), owner, c.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Title != "first" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Owner != owner {
		t.Fatalf("owner = %+v, want %+v", got.Owner, owner)
	}

	renamed, err := s.RenameConversation(ctx(), owner, c.ID, "renamed")
	if err != nil {
		t.Fatalf("RenameConversation: %v", err)
	}
	if renamed.Title != "renamed" || renamed.UpdatedAt.Before(c.UpdatedAt) {
		t.Fatalf("rename result = %+v", renamed)
	}

	if err := s.DeleteConversation(ctx(), owner, c.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if _, err := s.GetConversation(ctx(), owner, c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("deleted conversation still readable: %v", err)
	}
	if _, err := s.RenameConversation(ctx(), owner, c.ID, "again"); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("rename after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConversation(ctx(), owner, c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func testCreateConversationValidatesOwner(t *testing.T, s chat.Store) {
	if _, err := s.CreateConversation(ctx(), chat.Conversation{Title: "x"}); err == nil {
		t.Fatal("conversation without owner accepted")
	}
}

func testOwnerMismatchLooksMissing(t *testing.T, s chat.Store) {
	owner := ownerKey()
	stranger := otherOwner()

	c := mustCreateConversation(t, s, owner, "private")
	turn := mustCreateTurn(t, s, owner, c.ID, "key-1")
	mustAppendEvent(t, s, owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnStarted})

	if _, err := s.GetConversation(ctx(), stranger, c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("GetConversation = %v, want ErrNotFound", err)
	}
	if _, err := s.RenameConversation(ctx(), stranger, c.ID, "hijack"); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("RenameConversation = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConversation(ctx(), stranger, c.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("DeleteConversation = %v, want ErrNotFound", err)
	}
	if _, _, err := s.CreateTurn(ctx(), stranger, chat.Turn{ConversationID: c.ID}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("CreateTurn = %v, want ErrNotFound", err)
	}
	if _, err := s.GetTurn(ctx(), stranger, turn.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("GetTurn = %v, want ErrNotFound", err)
	}
	if _, err := s.TransitionTurn(ctx(), stranger, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted, nil); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("TransitionTurn = %v, want ErrNotFound", err)
	}
	if _, err := s.AppendEvent(ctx(), stranger, chat.Event{TurnID: turn.ID, Type: chat.EventTextDelta}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("AppendEvent = %v, want ErrNotFound", err)
	}
	if _, err := s.EventsAfter(ctx(), stranger, turn.ID, 0); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("EventsAfter = %v, want ErrNotFound", err)
	}
	if _, err := s.AppendMessage(ctx(), stranger, chat.Message{ConversationID: c.ID, Role: chat.RoleUser}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("AppendMessage = %v, want ErrNotFound", err)
	}
	if _, _, err := s.ListMessages(ctx(), stranger, c.ID, chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("ListMessages = %v, want ErrNotFound", err)
	}
	if list, _, err := s.ListConversations(ctx(), stranger, "", chat.Page{}); err != nil || len(list) != 0 {
		t.Errorf("ListConversations = %v, %v; want empty", list, err)
	}
	// The owner still sees everything: the refusal changed nothing.
	if _, err := s.GetConversation(ctx(), owner, c.ID); err != nil {
		t.Errorf("owner lost the conversation: %v", err)
	}
	if _, err := s.GetTurn(ctx(), owner, turn.ID); err != nil {
		t.Errorf("owner lost the turn: %v", err)
	}
}

func testListConversationsTargetAndOrder(t *testing.T, s chat.Store) {
	owner := ownerKey()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	a := mustCreateTargeted(t, s, owner, "a", "record-a", base)
	b := mustCreateTargeted(t, s, owner, "b", "record-b", base.Add(time.Minute))
	c := mustCreateTargeted(t, s, owner, "c", "record-a", base.Add(2*time.Minute))

	got, next, err := s.ListConversations(ctx(), owner, "record-a", chat.Page{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if next != "" {
		t.Fatalf("unexpected cursor %q", next)
	}
	if len(got) != 2 || got[0].ID != c.ID || got[1].ID != a.ID {
		t.Fatalf("target filter/order wrong: %+v", got)
	}

	all, _, err := s.ListConversations(ctx(), owner, "", chat.Page{})
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d, %v", len(all), err)
	}
	if all[0].ID != c.ID || all[1].ID != b.ID || all[2].ID != a.ID {
		t.Fatalf("newest-first order wrong: %+v", all)
	}

	// A target nobody owns is an ordinary empty page.
	none, _, err := s.ListConversations(ctx(), owner, "record-missing", chat.Page{})
	if err != nil || len(none) != 0 {
		t.Fatalf("missing target = %v, %v", none, err)
	}
}

func testConversationPagination(t *testing.T, s chat.Store) {
	owner := ownerKey()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 5; i++ {
		c := mustCreateTargeted(t, s, owner, fmt.Sprintf("c%d", i), "", base.Add(time.Duration(i)*time.Second))
		ids = append(ids, c.ID)
	}

	var seen []string
	page := chat.Page{Limit: 2}
	for i := 0; i < 5; i++ {
		items, next, err := s.ListConversations(ctx(), owner, "", page)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, c := range items {
			seen = append(seen, c.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if len(seen) != len(ids) {
		t.Fatalf("paginated %d conversations, want %d: %v", len(seen), len(ids), seen)
	}
	for i, id := range ids {
		if seen[len(seen)-1-i] != id {
			t.Fatalf("pagination order wrong at %d: %v", i, seen)
		}
	}

	if _, _, err := s.ListConversations(ctx(), owner, "", chat.Page{Cursor: "not-a-cursor!"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

// --- message tests ---

func testMessagesDeepCopyAndOrder(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")

	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	args := json.RawMessage(`{"query":"x"}`)
	m, err := s.AppendMessage(ctx(), owner, chat.Message{
		ConversationID: c.ID,
		Role:           chat.RoleAssistant,
		Parts: []chat.Part{{
			Type:      chat.PartToolCall,
			CallID:    "call-1",
			Tool:      "search",
			Arguments: args,
		}},
		CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if m.ID == "" {
		t.Fatal("AppendMessage did not assign an ID")
	}
	args[2] = 'X' // caller mutation must not reach the store
	m.Parts[0].Arguments[2] = 'Y'
	m.Parts[0].Tool = "mutated"

	items, next, err := s.ListMessages(ctx(), owner, c.ID, chat.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if next != "" {
		t.Fatalf("unexpected cursor %q", next)
	}
	if len(items) != 1 {
		t.Fatalf("got %d messages", len(items))
	}
	if items[0].Parts[0].Tool != "search" {
		t.Fatalf("stored message mutated: %+v", items[0])
	}
	if string(items[0].Parts[0].Arguments) != `{"query":"x"}` {
		t.Fatalf("stored arguments mutated: %s", items[0].Parts[0].Arguments)
	}
	items[0].Parts[0].Arguments[2] = 'Z'
	again, _, err := s.ListMessages(ctx(), owner, c.ID, chat.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if string(again[0].Parts[0].Arguments) != `{"query":"x"}` {
		t.Fatalf("returned slice aliases stored state: %s", again[0].Parts[0].Arguments)
	}

	if _, err := s.AppendMessage(ctx(), owner, chat.Message{ConversationID: "missing", Role: chat.RoleUser}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("message for missing conversation: %v", err)
	}
}

func testMessagePagination(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendMessage(ctx(), owner, chat.Message{
			ConversationID: c.ID,
			Role:           chat.RoleUser,
			Parts:          []chat.Part{{Type: chat.PartText, Text: fmt.Sprintf("m%d", i)}},
			CreatedAt:      base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	page := chat.Page{Limit: 2}
	for i := 0; i < 5; i++ {
		items, next, err := s.ListMessages(ctx(), owner, c.ID, page)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, m := range items {
			seen = append(seen, m.Parts[0].Text)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d messages: %v", len(seen), seen)
	}
	for i, text := range seen {
		if want := fmt.Sprintf("m%d", i); text != want {
			t.Fatalf("message %d = %q, want %q", i, text, want)
		}
	}

	if _, _, err := s.ListMessages(ctx(), owner, c.ID, chat.Page{Cursor: "not-a-cursor!"}); err == nil {
		t.Fatal("invalid message cursor accepted")
	}
}

// --- turn tests ---

func testCreateTurnIdempotent(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")

	first := mustCreateTurn(t, s, owner, c.ID, "submit-1")
	second, created, err := s.CreateTurn(ctx(), owner, chat.Turn{ConversationID: c.ID, IdempotencyKey: "submit-1"})
	if err != nil {
		t.Fatalf("idempotent CreateTurn: %v", err)
	}
	if created {
		t.Fatal("duplicate submission created a second turn")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate returned turn %q, want %q", second.ID, first.ID)
	}
	if second.State != chat.TurnStateQueued {
		t.Fatalf("returned turn state = %s", second.State)
	}
	if second.IdempotencyKey != "submit-1" || second.ConversationID != c.ID || second.Owner != owner {
		t.Fatalf("returned turn = %+v", second)
	}
}

func testActiveTurnRule(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")

	turn := mustCreateTurn(t, s, owner, c.ID, "key-1")
	if _, _, err := s.CreateTurn(ctx(), owner, chat.Turn{ConversationID: c.ID, IdempotencyKey: "key-2"}); !errors.Is(err, chat.ErrActiveTurn) {
		t.Fatalf("second active turn = %v, want ErrActiveTurn", err)
	}

	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := s.CreateTurn(ctx(), owner, chat.Turn{ConversationID: c.ID, IdempotencyKey: "key-3"}); !errors.Is(err, chat.ErrActiveTurn) {
		t.Fatalf("active running turn not enforced: %v", err)
	}
	completed, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted,
		chat.Event{Type: chat.EventTurnCompleted})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.State != chat.TurnStateCompleted || completed.Error != nil {
		t.Fatalf("completed turn = %+v", completed)
	}
	next := mustCreateTurn(t, s, owner, c.ID, "key-4")
	if next.ID == turn.ID {
		t.Fatal("new turn reused the completed turn")
	}

	// A cancelled or interrupted turn also frees the conversation.
	if _, _, err := s.FinishTurn(ctx(), owner, next.ID, chat.TurnStateQueued, chat.TurnStateCancelled,
		chat.Event{Type: chat.EventTurnCancelled, Data: mustJSON(chat.TurnCancelled{Reason: "user"})}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	third := mustCreateTurn(t, s, owner, c.ID, "key-5")
	if _, _, err := s.FinishTurn(ctx(), owner, third.ID, chat.TurnStateQueued, chat.TurnStateInterrupted,
		chat.Event{Type: chat.EventTurnFailed, Data: mustJSON(chat.TurnFailed{Code: "interrupted"})}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	mustCreateTurn(t, s, owner, c.ID, "key-6")
}

func testCreateTurnConcurrent(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")

	const n = 20
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		active   int
		otherErr []error
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, wasCreated, err := s.CreateTurn(ctx(), owner, chat.Turn{
				ConversationID: c.ID,
				IdempotencyKey: fmt.Sprintf("key-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && wasCreated:
				created++
			case errors.Is(err, chat.ErrActiveTurn):
				active++
			case err != nil:
				otherErr = append(otherErr, err)
			default:
				otherErr = append(otherErr, fmt.Errorf("created=false without error"))
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(otherErr) > 0 {
		t.Fatalf("unexpected results: %v", otherErr)
	}
	if created != 1 {
		t.Fatalf("created %d turns, want exactly 1", created)
	}
	if active != n-1 {
		t.Fatalf("ErrActiveTurn count = %d, want %d", active, n-1)
	}
}

func testTransitionCAS(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	turn := mustCreateTurn(t, s, owner, c.ID, "key-1")

	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted, nil); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("CAS with wrong from = %v, want ErrConflict", err)
	}
	started, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.State != chat.TurnStateRunning {
		t.Fatalf("state = %s", started.State)
	}
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, nil); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("stale CAS = %v, want ErrConflict", err)
	}
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateQueued, nil); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("running->queued = %v, want ErrInvalidTransition", err)
	}
	// A plain transition may never end a turn: only FinishTurn stores a
	// terminal state with its terminal event.
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted, nil); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("running->completed = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCancelled, nil); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("running->cancelled = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, nil); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("stale cancel = %v, want ErrConflict", err)
	}

	failure := chat.TurnFailed{Code: "provider_error", Message: "boom"}
	failed, terminal, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateFailed,
		chat.Event{Type: chat.EventTurnFailed, Data: mustJSON(failure)})
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if failed.Error == nil || failed.Error.Code != "provider_error" {
		t.Fatalf("failure not recorded: %+v", failed.Error)
	}
	if terminal.Seq == 0 || terminal.Type != chat.EventTurnFailed {
		t.Fatalf("terminal event = %+v", terminal)
	}
	failure.Code = "mutated"
	stored, err := s.GetTurn(ctx(), owner, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Error == nil || stored.Error.Code != "provider_error" {
		t.Fatalf("stored failure mutated: %+v", stored.Error)
	}
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateFailed, chat.TurnStateCompleted, nil); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("failed->completed = %v, want ErrInvalidTransition", err)
	}

	if _, err := s.TransitionTurn(ctx(), owner, "missing", chat.TurnStateQueued, chat.TurnStateRunning, nil); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("missing turn = %v, want ErrNotFound", err)
	}
}

func testSeqsArePerTurn(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c1 := mustCreateConversation(t, s, owner, "one")
	c2 := mustCreateConversation(t, s, owner, "two")
	t1 := mustCreateTurn(t, s, owner, c1.ID, "a")
	t2 := mustCreateTurn(t, s, owner, c2.ID, "b")

	first := mustAppendEvent(t, s, owner, chat.Event{TurnID: t1.ID, Type: chat.EventTurnStarted})
	if first.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", first.Seq)
	}
	if first.Version != chat.ProtocolVersion || first.At.IsZero() {
		t.Fatalf("store did not fill version/time: %+v", first)
	}
	if first.TurnID != t1.ID || first.ConversationID != c1.ID {
		t.Fatalf("store did not fill ids: %+v", first)
	}
	for want := int64(2); want <= 3; want++ {
		e := mustAppendEvent(t, s, owner, chat.Event{TurnID: t1.ID, Type: chat.EventTextDelta, Data: mustJSON(chat.TextDelta{Text: "x"})})
		if e.Seq != want {
			t.Fatalf("seq = %d, want %d", e.Seq, want)
		}
	}

	other := mustAppendEvent(t, s, owner, chat.Event{TurnID: t2.ID, Type: chat.EventTurnStarted})
	if other.Seq != 1 {
		t.Fatalf("second turn seq = %d, want 1", other.Seq)
	}

	// A caller-supplied sequence is ignored: only the store may assign it.
	got := mustAppendEvent(t, s, owner, chat.Event{TurnID: t2.ID, Type: chat.EventTextDelta, Seq: 99})
	if got.Seq != 2 {
		t.Fatalf("supplied seq accepted: %d", got.Seq)
	}

	events, err := s.EventsAfter(ctx(), owner, t1.ID, 0)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events", len(events))
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d", i, ev.Seq)
		}
		if ev.At.IsZero() || ev.Version != chat.ProtocolVersion {
			t.Fatalf("event %d envelope incomplete: %+v", i, ev)
		}
	}
	afterTwo, err := s.EventsAfter(ctx(), owner, t1.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTwo) != 1 || afterTwo[0].Seq != 3 {
		t.Fatalf("EventsAfter(2) = %+v", afterTwo)
	}
}

func testTerminalEventRules(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	turn := mustCreateTurn(t, s, owner, c.ID, "key")

	if _, err := s.AppendEvent(ctx(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventType("bogus")}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("invalid event type = %v, want ErrConflict", err)
	}
	mustAppendEvent(t, s, owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnStarted})

	// A plain append may never store the terminal event: that would leave the
	// turn running with an outcome, or terminal without one.
	if _, err := s.AppendEvent(ctx(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnCompleted}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("terminal append = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.AppendEvent(ctx(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnFailed, Data: mustJSON(chat.TurnFailed{Code: "x"})}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("terminal append = %v, want ErrInvalidTransition", err)
	}
	events, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("refused terminal append stored events: %+v", events)
	}

	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted, chat.Event{Type: chat.EventTurnCompleted}); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}

	if _, err := s.AppendEvent(ctx(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTextDelta, Data: mustJSON(chat.TextDelta{Text: "late"})}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("event after terminal = %v, want ErrConflict", err)
	}
	if _, err := s.AppendEvent(ctx(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTurnFailed, Data: mustJSON(chat.TurnFailed{Code: "x"})}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("terminal append after terminal = %v, want ErrInvalidTransition", err)
	}

	events, err = s.EventsAfter(ctx(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, e := range events {
		if e.Type.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("stored %d terminal events, want 1", terminals)
	}
}

func testFinishTurnValidation(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	turn := mustCreateTurn(t, s, owner, c.ID, "key")

	assertUnchanged := func(label string) {
		t.Helper()
		got, err := s.GetTurn(ctx(), owner, turn.ID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got.State != chat.TurnStateQueued {
			t.Fatalf("%s: state changed to %s", label, got.State)
		}
		events, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(events) != 0 {
			t.Fatalf("%s: stored events %+v", label, events)
		}
	}

	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted, chat.Event{Type: chat.EventTurnCompleted}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("CAS mismatch = %v, want ErrConflict", err)
	}
	assertUnchanged("CAS mismatch")

	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, chat.Event{Type: chat.EventTurnStarted}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("non-terminal target = %v, want ErrInvalidTransition", err)
	}
	// queued -> failed is not a legal edge; only queued -> cancelled or
	// queued -> interrupted may end a queued turn.
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateFailed, chat.Event{Type: chat.EventTurnFailed, Data: mustJSON(chat.TurnFailed{Code: "x"})}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("illegal edge = %v, want ErrInvalidTransition", err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, chat.Event{Type: chat.EventTurnCompleted}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("mismatched event type = %v, want ErrInvalidTransition", err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, chat.Event{Type: chat.EventTextDelta, Data: mustJSON(chat.TextDelta{Text: "x"})}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("non-terminal event type = %v, want ErrInvalidTransition", err)
	}
	if _, _, err := s.FinishTurn(ctx(), otherOwner(), turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, chat.Event{Type: chat.EventTurnCancelled}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("other owner = %v, want ErrNotFound", err)
	}
	assertUnchanged("refused finishes")

	// A failed or interrupted turn must carry the failure it records.
	c2 := mustCreateConversation(t, s, owner, "chat2")
	t2 := mustCreateTurn(t, s, owner, c2.ID, "key2")
	if _, err := s.TransitionTurn(ctx(), owner, t2.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, t2.ID, chat.TurnStateRunning, chat.TurnStateFailed, chat.Event{Type: chat.EventTurnFailed}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("failed event without data = %v, want ErrConflict", err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, t2.ID, chat.TurnStateRunning, chat.TurnStateFailed, chat.Event{Type: chat.EventTurnFailed, Data: json.RawMessage(`not json`)}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("failed event with malformed data = %v, want ErrConflict", err)
	}
	if got, err := s.GetTurn(ctx(), owner, t2.ID); err != nil || got.State != chat.TurnStateRunning {
		t.Fatalf("malformed failure changed the turn: %+v, %v", got, err)
	}

	cancelled, ev, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled,
		chat.Event{Type: chat.EventTurnCancelled, Data: mustJSON(chat.TurnCancelled{Reason: "user"})})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != chat.TurnStateCancelled || cancelled.Error != nil {
		t.Fatalf("cancelled turn = %+v", cancelled)
	}
	if ev.Seq != 1 || ev.Type != chat.EventTurnCancelled {
		t.Fatalf("terminal event = %+v", ev)
	}
	if ev.TurnID != turn.ID || ev.ConversationID != c.ID {
		t.Fatalf("store did not fill the envelope: %+v", ev)
	}

	// The turn and its event are already terminal, so a second attempt fails
	// without adding a second outcome.
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled, chat.Event{Type: chat.EventTurnCancelled}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("second finish = %v, want ErrConflict", err)
	}
	events, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("second finish stored events: %+v", events)
	}
}

func testFinishTurnAtomic(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	turn := mustCreateTurn(t, s, owner, c.ID, "key")
	if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var (
		mu       sync.Mutex
		wins     int
		conflict int
		other    []error
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			to, typ, data := chat.TurnStateCompleted, chat.EventTurnCompleted, any(nil)
			if i%2 == 1 {
				to, typ, data = chat.TurnStateCancelled, chat.EventTurnCancelled, chat.TurnCancelled{Reason: "racer"}
			}
			ev, err := chat.NewEvent(c.ID, turn.ID, typ, data)
			if err != nil {
				mu.Lock()
				other = append(other, err)
				mu.Unlock()
				return
			}
			_, _, err = s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, to, ev)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, chat.ErrConflict):
				conflict++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if wins != 1 || conflict != racers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1 and %d", wins, conflict, racers-1)
	}
	stored, err := s.GetTurn(ctx(), owner, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Type.Terminal() {
		t.Fatalf("events = %+v", events)
	}
	wantTerminal := chat.EventTurnCancelled
	if stored.State == chat.TurnStateCompleted {
		wantTerminal = chat.EventTurnCompleted
	}
	if events[0].Type != wantTerminal {
		t.Fatalf("state %s has terminal event %s, want %s", stored.State, events[0].Type, wantTerminal)
	}
}

func testAppendRacingFinishStaysOrdered(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")

	for i := 0; i < 50; i++ {
		turn := mustCreateTurn(t, s, owner, c.ID, fmt.Sprintf("race-%d", i))
		if _, err := s.TransitionTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.AppendEvent(ctx(), owner, chat.Event{TurnID: turn.ID, Type: chat.EventTextDelta, Data: mustJSON(chat.TextDelta{Text: "x"})})
		}()
		go func() {
			defer wg.Done()
			_, _, _ = s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateRunning, chat.TurnStateCompleted, chat.Event{Type: chat.EventTurnCompleted})
		}()
		wg.Wait()

		events, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for j, e := range events {
			if e.Seq != int64(j+1) {
				t.Fatalf("turn %d event %d seq = %d", i, j, e.Seq)
			}
			if j == len(events)-1 && !e.Type.Terminal() {
				t.Fatalf("turn %d last event %s is not terminal", i, e.Type)
			}
			if j < len(events)-1 && e.Type.Terminal() {
				t.Fatalf("turn %d event after terminal", i)
			}
		}
		stored, err := s.GetTurn(ctx(), owner, turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != chat.TurnStateCompleted {
			t.Fatalf("turn %d state = %s", i, stored.State)
		}
	}
}

func testInterruptRunning(t *testing.T, s chat.Store) {
	owner := ownerKey()

	queuedConv := mustCreateConversation(t, s, owner, "queued")
	queued := mustCreateTurn(t, s, owner, queuedConv.ID, "q")

	runningConv := mustCreateConversation(t, s, owner, "running")
	running := mustCreateTurn(t, s, owner, runningConv.ID, "r")
	if _, err := s.TransitionTurn(ctx(), owner, running.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	mustAppendEvent(t, s, owner, chat.Event{TurnID: running.ID, Type: chat.EventTurnStarted})

	doneConv := mustCreateConversation(t, s, owner, "done")
	done := mustCreateTurn(t, s, owner, doneConv.ID, "d")
	if _, err := s.TransitionTurn(ctx(), owner, done.ID, chat.TurnStateQueued, chat.TurnStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, done.ID, chat.TurnStateRunning, chat.TurnStateCompleted, chat.Event{Type: chat.EventTurnCompleted}); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptRunning(ctx())
	if err != nil {
		t.Fatalf("InterruptRunning: %v", err)
	}
	if n != 2 {
		t.Fatalf("interrupted %d turns, want 2", n)
	}

	for _, id := range []string{queued.ID, running.ID} {
		turn, err := s.GetTurn(ctx(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if turn.State != chat.TurnStateInterrupted {
			t.Fatalf("turn %s state = %s, want interrupted", id, turn.State)
		}
		if turn.Error == nil || turn.Error.Code != "interrupted" {
			t.Fatalf("turn %s error = %+v", id, turn.Error)
		}
		events, err := s.EventsAfter(ctx(), owner, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		terminals := 0
		last := chat.Event{}
		for _, e := range events {
			if e.Type.Terminal() {
				terminals++
				last = e
			}
		}
		if terminals != 1 {
			t.Fatalf("turn %s has %d terminal events", id, terminals)
		}
		if last.Type != chat.EventTurnFailed {
			t.Fatalf("interrupt terminal event = %s", last.Type)
		}
		var payload chat.TurnFailed
		if err := json.Unmarshal(last.Data, &payload); err != nil {
			t.Fatalf("decode terminal payload: %v", err)
		}
		if payload.Code != "interrupted" {
			t.Fatalf("terminal code = %q", payload.Code)
		}
		if _, err := s.AppendEvent(ctx(), owner, chat.Event{TurnID: id, Type: chat.EventTextDelta, Data: mustJSON(chat.TextDelta{Text: "late"})}); !errors.Is(err, chat.ErrConflict) {
			t.Fatalf("event after interrupt = %v, want ErrConflict", err)
		}
	}

	doneTurn, err := s.GetTurn(ctx(), owner, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneTurn.State != chat.TurnStateCompleted {
		t.Fatalf("completed turn changed to %s", doneTurn.State)
	}
	doneEvents, err := s.EventsAfter(ctx(), owner, done.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(doneEvents) != 1 {
		t.Fatalf("completed turn gained events: %+v", doneEvents)
	}

	again, err := s.InterruptRunning(ctx())
	if err != nil || again != 0 {
		t.Fatalf("second InterruptRunning = %d, %v", again, err)
	}

	// An interrupted turn no longer blocks its conversation.
	mustCreateTurn(t, s, owner, queuedConv.ID, "after-interrupt")
}

func testDeleteBlockedByActiveTurn(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	turn := mustCreateTurn(t, s, owner, c.ID, "key")
	if _, err := s.AppendMessage(ctx(), owner, chat.Message{ConversationID: c.ID, Role: chat.RoleUser}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteConversation(ctx(), owner, c.ID); !errors.Is(err, chat.ErrActiveTurn) {
		t.Fatalf("delete with active turn = %v, want ErrActiveTurn", err)
	}
	if _, _, err := s.FinishTurn(ctx(), owner, turn.ID, chat.TurnStateQueued, chat.TurnStateCancelled,
		chat.Event{Type: chat.EventTurnCancelled, Data: mustJSON(chat.TurnCancelled{Reason: "user"})}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConversation(ctx(), owner, c.ID); err != nil {
		t.Fatalf("delete after cancel: %v", err)
	}
	if _, err := s.GetTurn(ctx(), owner, turn.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("turn survived conversation delete: %v", err)
	}
	if _, _, err := s.ListMessages(ctx(), owner, c.ID, chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("messages survived conversation delete: %v", err)
	}

	// The conversation id can be reused afterwards without inheriting data.
	again, err := s.CreateConversation(ctx(), chat.Conversation{ID: c.ID, Owner: owner, Title: "again"})
	if err != nil {
		t.Fatalf("recreate conversation: %v", err)
	}
	items, _, err := s.ListMessages(ctx(), owner, again.ID, chat.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("recreated conversation inherited messages: %+v", items)
	}
}

func testEventsDeepCopy(t *testing.T, s chat.Store) {
	owner := ownerKey()
	c := mustCreateConversation(t, s, owner, "chat")
	turn := mustCreateTurn(t, s, owner, c.ID, "key")

	stored := mustAppendEvent(t, s, owner, chat.Event{TurnID: turn.ID, Type: chat.EventTextDelta, Data: mustJSON(chat.TextDelta{Text: "hi"})})
	if string(stored.Data) != `{"text":"hi"}` {
		t.Fatalf("stored event data = %s", stored.Data)
	}
	stored.Data[0] = '['
	events, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(events[0].Data) != `{"text":"hi"}` {
		t.Fatalf("returned event aliases stored data: %s", events[0].Data)
	}
	events[0].Data[0] = '{'
	again, err := s.EventsAfter(ctx(), owner, turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(again[0].Data) != `{"text":"hi"}` {
		t.Fatalf("stored event mutated through returned slice: %s", again[0].Data)
	}
}

// --- upload contract ---

// uploadStore is the pair of ports RunUploads needs. Uploads belong to a
// conversation, so the fixtures have to create one through chat.Store.
type uploadStore = interface {
	chat.Store
	chat.UploadStore
}

// RunUploads exercises the chat.UploadStore contract. newStore must return an
// empty store that is isolated from every other call, exactly like Run.
func RunUploads(t *testing.T, newStore func(t *testing.T) uploadStore) {
	t.Helper()

	t.Run("UploadCreateGetReadListCount", func(t *testing.T) {
		testUploadCreateGetReadListCount(t, newStore(t))
	})
	t.Run("UploadOwnerIsolation", func(t *testing.T) {
		testUploadOwnerIsolation(t, newStore(t))
	})
	t.Run("UploadWrongConversation", func(t *testing.T) {
		testUploadWrongConversation(t, newStore(t))
	})
	t.Run("UploadDeleteCascade", func(t *testing.T) {
		testUploadDeleteCascade(t, newStore(t))
	})
	t.Run("UploadDeepCopy", func(t *testing.T) {
		testUploadDeepCopy(t, newStore(t))
	})
	t.Run("UploadSizeAndSHA256", func(t *testing.T) {
		testUploadSizeAndSHA256(t, newStore(t))
	})
}

// mustCreateUpload stores one attachment and asserts the identity and digest the
// store must assign.
func mustCreateUpload(t *testing.T, s uploadStore, owner chat.OwnerKey, convID, name, mime, kind string, data []byte) chat.Upload {
	t.Helper()
	u, err := s.CreateUpload(ctx(), owner, chat.Upload{
		ConversationID: convID,
		Name:           name,
		MIME:           mime,
		Kind:           kind,
	}, data)
	if err != nil {
		t.Fatalf("CreateUpload(%q): %v", name, err)
	}
	if u.ID == "" {
		t.Fatal("CreateUpload did not assign an ID")
	}
	if u.ConversationID != convID || u.Name != name || u.MIME != mime || u.Kind != kind {
		t.Fatalf("stored upload = %+v", u)
	}
	if u.Owner != owner {
		t.Fatalf("CreateUpload did not record the owner key: %+v", u.Owner)
	}
	if u.CreatedAt.IsZero() {
		t.Fatal("CreateUpload did not assign a timestamp")
	}
	if u.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", u.Size, len(data))
	}
	sum := sha256.Sum256(data)
	if want := "sha256:" + hex.EncodeToString(sum[:]); u.SHA256 != want {
		t.Fatalf("sha256 = %q, want %q", u.SHA256, want)
	}
	return u
}

func testUploadCreateGetReadListCount(t *testing.T, s uploadStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	first := mustCreateUpload(t, s, owner, conv.ID, "notes.txt", "text/plain", chat.UploadKindText, []byte("hello\n"))
	second := mustCreateUpload(t, s, owner, conv.ID, "shot.png", "image/png", chat.UploadKindImage, []byte("\x89PNG\r\n\x1a\nbody"))

	got, err := s.GetUpload(ctx(), owner, first.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if got.ID != first.ID || got.SHA256 != first.SHA256 || got.Size != first.Size {
		t.Fatalf("GetUpload = %+v, want %+v", got, first)
	}
	if _, err := s.GetUpload(ctx(), owner, "missing"); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("missing upload = %v, want ErrNotFound", err)
	}

	meta, data, err := s.ReadUpload(ctx(), owner, first.ID)
	if err != nil {
		t.Fatalf("ReadUpload: %v", err)
	}
	if meta.ID != first.ID {
		t.Fatalf("ReadUpload metadata = %+v", meta)
	}
	if string(data) != "hello\n" {
		t.Fatalf("ReadUpload data = %q", data)
	}

	n, err := s.CountUploads(ctx(), owner, conv.ID)
	if err != nil || n != 2 {
		t.Fatalf("CountUploads = %d, %v; want 2", n, err)
	}

	list, next, err := s.ListUploads(ctx(), owner, conv.ID, chat.Page{})
	if err != nil {
		t.Fatalf("ListUploads: %v", err)
	}
	if next != "" {
		t.Fatalf("unexpected cursor %q", next)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("newest-first order wrong: %+v", list)
	}

	// Pagination walks the same order without gaps or repeats.
	third := mustCreateUpload(t, s, owner, conv.ID, "doc.pdf", "application/pdf", chat.UploadKindPDF, []byte("%PDF-1.4"))
	var seen []string
	page := chat.Page{Limit: 2}
	for i := 0; i < 3; i++ {
		items, cursor, err := s.ListUploads(ctx(), owner, conv.ID, page)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, u := range items {
			seen = append(seen, u.ID)
		}
		if cursor == "" {
			break
		}
		page.Cursor = cursor
	}
	if len(seen) != 3 || seen[0] != third.ID || seen[1] != second.ID || seen[2] != first.ID {
		t.Fatalf("paginated uploads = %v", seen)
	}
	if _, _, err := s.ListUploads(ctx(), owner, conv.ID, chat.Page{Cursor: "not-a-cursor!"}); err == nil {
		t.Fatal("invalid upload cursor accepted")
	}
	if _, _, err := s.ListUploads(ctx(), owner, "missing", chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("list for missing conversation = %v, want ErrNotFound", err)
	}
	if _, err := s.CountUploads(ctx(), owner, "missing"); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("count for missing conversation = %v, want ErrNotFound", err)
	}
}

func testUploadOwnerIsolation(t *testing.T, s uploadStore) {
	owner := ownerKey()
	stranger := otherOwner()
	conv := mustCreateConversation(t, s, owner, "private")
	upload := mustCreateUpload(t, s, owner, conv.ID, "notes.txt", "text/plain", chat.UploadKindText, []byte("secret"))

	if _, err := s.CreateUpload(ctx(), stranger, chat.Upload{
		ConversationID: conv.ID, Name: "x", MIME: "text/plain", Kind: chat.UploadKindText,
	}, []byte("x")); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("CreateUpload into another owner's conversation = %v, want ErrNotFound", err)
	}
	if _, err := s.GetUpload(ctx(), stranger, upload.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("GetUpload = %v, want ErrNotFound", err)
	}
	if _, _, err := s.ReadUpload(ctx(), stranger, upload.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("ReadUpload = %v, want ErrNotFound", err)
	}
	if _, _, err := s.ListUploads(ctx(), stranger, conv.ID, chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("ListUploads = %v, want ErrNotFound", err)
	}
	if _, err := s.CountUploads(ctx(), stranger, conv.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("CountUploads = %v, want ErrNotFound", err)
	}

	// The owner still sees everything: the refusals changed nothing.
	if _, _, err := s.ReadUpload(ctx(), owner, upload.ID); err != nil {
		t.Errorf("owner lost the upload: %v", err)
	}
}

func testUploadWrongConversation(t *testing.T, s uploadStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	if _, err := s.CreateUpload(ctx(), owner, chat.Upload{
		Name: "x", MIME: "text/plain", Kind: chat.UploadKindText,
	}, []byte("x")); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("upload without conversation = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateUpload(ctx(), owner, chat.Upload{
		ConversationID: "missing", Name: "x", MIME: "text/plain", Kind: chat.UploadKindText,
	}, []byte("x")); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("upload for missing conversation = %v, want ErrNotFound", err)
	}

	other := mustCreateConversation(t, s, otherOwner(), "theirs")
	if _, err := s.CreateUpload(ctx(), owner, chat.Upload{
		ConversationID: other.ID, Name: "x", MIME: "text/plain", Kind: chat.UploadKindText,
	}, []byte("x")); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("upload into a foreign conversation = %v, want ErrNotFound", err)
	}

	// An upload is only ever visible through its own conversation.
	upload := mustCreateUpload(t, s, owner, conv.ID, "a.txt", "text/plain", chat.UploadKindText, []byte("a"))
	items, _, err := s.ListUploads(ctx(), owner, other.ID, chat.Page{})
	if err != nil {
		// The conversation is not the caller's, so ErrNotFound is correct.
		if !errors.Is(err, chat.ErrNotFound) {
			t.Fatalf("list foreign conversation = %v", err)
		}
	} else if len(items) != 0 {
		t.Fatalf("foreign conversation listed %d uploads", len(items))
	}
	if _, _, err := s.ReadUpload(ctx(), owner, upload.ID); err != nil {
		t.Fatalf("read own upload: %v", err)
	}
}

func testUploadDeleteCascade(t *testing.T, s uploadStore) {
	owner := ownerKey()
	keepConv := mustCreateConversation(t, s, owner, "keep")
	dropConv := mustCreateConversation(t, s, owner, "drop")

	kept := mustCreateUpload(t, s, owner, keepConv.ID, "keep.txt", "text/plain", chat.UploadKindText, []byte("keep"))
	dropped := mustCreateUpload(t, s, owner, dropConv.ID, "drop.txt", "text/plain", chat.UploadKindText, []byte("drop"))

	if err := s.DeleteConversation(ctx(), owner, dropConv.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if _, err := s.GetUpload(ctx(), owner, dropped.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("upload survived conversation delete: %v", err)
	}
	if _, _, err := s.ReadUpload(ctx(), owner, dropped.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("read after cascade = %v, want ErrNotFound", err)
	}

	// The other conversation's attachment is untouched.
	meta, data, err := s.ReadUpload(ctx(), owner, kept.ID)
	if err != nil {
		t.Fatalf("unrelated upload lost: %v", err)
	}
	if meta.ConversationID != keepConv.ID || string(data) != "keep" {
		t.Fatalf("unrelated upload = %+v %q", meta, data)
	}

	// A re-created conversation inherits nothing.
	again, err := s.CreateConversation(ctx(), chat.Conversation{ID: dropConv.ID, Owner: owner, Title: "again"})
	if err != nil {
		t.Fatalf("recreate conversation: %v", err)
	}
	if n, err := s.CountUploads(ctx(), owner, again.ID); err != nil || n != 0 {
		t.Fatalf("recreated conversation uploads = %d, %v; want 0", n, err)
	}
}

func testUploadDeepCopy(t *testing.T, s uploadStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	data := []byte("original")
	upload := mustCreateUpload(t, s, owner, conv.ID, "notes.txt", "text/plain", chat.UploadKindText, data)
	data[0] = 'X' // caller mutation must not reach the store

	_, stored, err := s.ReadUpload(ctx(), owner, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "original" {
		t.Fatalf("stored data mutated by caller: %q", stored)
	}
	stored[0] = 'Y'
	again, second, err := s.ReadUpload(ctx(), owner, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "original" {
		t.Fatalf("returned bytes alias stored state: %q", second)
	}
	again.Name = "mutated"
	again.Size = 999
	reloaded, err := s.GetUpload(ctx(), owner, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Name != "notes.txt" || reloaded.Size != int64(len("original")) {
		t.Fatalf("returned record aliases stored state: %+v", reloaded)
	}
}

func testUploadSizeAndSHA256(t *testing.T, s uploadStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")
	data := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}

	// A caller cannot spoof the derived fields.
	u, err := s.CreateUpload(ctx(), owner, chat.Upload{
		ConversationID: conv.ID,
		Name:           "blob.bin",
		MIME:           "application/pdf",
		Kind:           chat.UploadKindPDF,
		Size:           1,
		SHA256:         "sha256:deadbeef",
		CreatedAt:      time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	}, data)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if u.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", u.Size, len(data))
	}
	sum := sha256.Sum256(data)
	if want := "sha256:" + hex.EncodeToString(sum[:]); u.SHA256 != want {
		t.Fatalf("sha256 = %q, want %q", u.SHA256, want)
	}
	if u.CreatedAt.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("store accepted a caller-supplied timestamp")
	}
	meta, got, err := s.ReadUpload(ctx(), owner, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(data)) || len(got) != len(data) {
		t.Fatalf("stored upload = %+v with %d bytes", meta, len(got))
	}
}

// --- proposal contract ---

// proposalStore is the pair of ports RunProposals needs. Proposals are scoped
// to a conversation, so the fixtures have to create one through chat.Store.
type proposalStore = interface {
	chat.Store
	chat.ProposalStore
}

// RunProposals exercises the chat.ProposalStore contract. newStore must return
// an empty store that is isolated from every other call, exactly like Run.
func RunProposals(t *testing.T, newStore func(t *testing.T) proposalStore) {
	t.Helper()

	t.Run("ProposalCreateGetAndList", func(t *testing.T) {
		testProposalCreateGetAndList(t, newStore(t))
	})
	t.Run("ProposalOwnerIsolation", func(t *testing.T) {
		testProposalOwnerIsolation(t, newStore(t))
	})
	t.Run("ProposalAllowedEdges", func(t *testing.T) {
		testProposalAllowedEdges(t, newStore(t))
	})
	t.Run("ProposalRefusedEdges", func(t *testing.T) {
		testProposalRefusedEdges(t, newStore(t))
	})
	t.Run("ProposalCompareAndSwap", func(t *testing.T) {
		testProposalCompareAndSwap(t, newStore(t))
	})
	t.Run("ProposalConcurrentDecision", func(t *testing.T) {
		testProposalConcurrentDecision(t, newStore(t))
	})
	t.Run("ProposalRevise", func(t *testing.T) {
		testProposalRevise(t, newStore(t))
	})
	t.Run("ProposalExpiry", func(t *testing.T) {
		testProposalExpiry(t, newStore(t))
	})
	t.Run("ProposalRecoverExecuting", func(t *testing.T) {
		testProposalRecoverExecuting(t, newStore(t))
	})
	t.Run("ProposalDeleteCascade", func(t *testing.T) {
		testProposalDeleteCascade(t, newStore(t))
	})
	t.Run("ProposalDeepCopy", func(t *testing.T) {
		testProposalDeepCopy(t, newStore(t))
	})
}

// proposalPaths is the canonical route from pending to every reachable state
// per location. The conformance suite walks it to build a proposal in a given
// state without guessing which edges an adapter allows.
var proposalPaths = map[chat.ProposalLocation]map[chat.ProposalState][]chat.ProposalState{
	chat.ProposalLocationServer: {
		chat.ProposalStatePending:   nil,
		chat.ProposalStateExecuting: {chat.ProposalStateExecuting},
		chat.ProposalStateSucceeded: {chat.ProposalStateExecuting, chat.ProposalStateSucceeded},
		chat.ProposalStateFailed:    {chat.ProposalStateExecuting, chat.ProposalStateFailed},
		chat.ProposalStateUnknown:   {chat.ProposalStateExecuting, chat.ProposalStateUnknown},
		chat.ProposalStateRejected:  {chat.ProposalStateRejected},
		chat.ProposalStateExpired:   {chat.ProposalStateExpired},
	},
	chat.ProposalLocationEditor: {
		chat.ProposalStatePending:  nil,
		chat.ProposalStateApproved: {chat.ProposalStateApproved},
		chat.ProposalStateApplied:  {chat.ProposalStateApproved, chat.ProposalStateApplied},
		chat.ProposalStateStale:    {chat.ProposalStateApproved, chat.ProposalStateStale},
		chat.ProposalStateFailed:   {chat.ProposalStateApproved, chat.ProposalStateFailed},
		chat.ProposalStateRejected: {chat.ProposalStateRejected},
		chat.ProposalStateExpired:  {chat.ProposalStateExpired},
	},
}

func proposalHash(t *testing.T, raw string) (json.RawMessage, string) {
	t.Helper()
	canonical, hash, err := chat.HashArguments(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("HashArguments(%s): %v", raw, err)
	}
	return canonical, hash
}

func mustCreateProposal(t *testing.T, s proposalStore, owner chat.OwnerKey, convID string, loc chat.ProposalLocation, tool, args string) chat.Proposal {
	t.Helper()
	return mustCreateProposalAt(t, s, owner, convID, loc, tool, args, time.Time{}, time.Time{})
}

func mustCreateProposalAt(t *testing.T, s proposalStore, owner chat.OwnerKey, convID string, loc chat.ProposalLocation, tool, args string, created, expires time.Time) chat.Proposal {
	t.Helper()
	canonical, hash := proposalHash(t, args)
	p, err := s.CreateProposal(ctx(), owner, chat.Proposal{
		ConversationID:   convID,
		TurnID:           "turn-1",
		CallID:           "call-1",
		Tool:             tool,
		ToolFingerprint:  "fingerprint-1",
		ManifestRevision: "manifest-1",
		Location:         loc,
		Target:           "record-1",
		Arguments:        json.RawMessage(args),
		ArgumentsHash:    hash,
		Summary:          "changes one record",
		CreatedAt:        created,
		ExpiresAt:        expires,
	})
	if err != nil {
		t.Fatalf("CreateProposal(%s): %v", tool, err)
	}
	if p.ID == "" {
		t.Fatal("CreateProposal did not assign an ID")
	}
	if p.State != chat.ProposalStatePending || p.Revision != 1 {
		t.Fatalf("new proposal = %s revision %d, want pending/1", p.State, p.Revision)
	}
	if p.Owner != owner {
		t.Fatalf("CreateProposal did not record the owner key: %+v", p.Owner)
	}
	if string(p.Arguments) != string(canonical) || p.ArgumentsHash != hash {
		t.Fatalf("stored arguments = %s (%s), want canonical %s (%s)", p.Arguments, p.ArgumentsHash, canonical, hash)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatal("CreateProposal did not assign timestamps")
	}
	if p.DecidedAt != nil || p.Result != nil || p.Failure != "" {
		t.Fatalf("new proposal carries an outcome: %+v", p)
	}
	return p
}

// mustTransition applies one transition using the proposal's own state and
// revision, and asserts the contract's revision increment.
func mustTransition(t *testing.T, s proposalStore, owner chat.OwnerKey, p chat.Proposal, to chat.ProposalState, u chat.ProposalUpdate) chat.Proposal {
	t.Helper()
	got, err := s.TransitionProposal(ctx(), owner, p.ID, p.State, p.Revision, to, u)
	if err != nil {
		t.Fatalf("TransitionProposal(%s -> %s): %v", p.State, to, err)
	}
	if got.State != to {
		t.Fatalf("state = %s, want %s", got.State, to)
	}
	if got.Revision != p.Revision+1 {
		t.Fatalf("revision = %d after %s -> %s, want %d", got.Revision, p.State, to, p.Revision+1)
	}
	return got
}

// proposalIn creates a proposal and walks it to the requested state.
func proposalIn(t *testing.T, s proposalStore, owner chat.OwnerKey, convID string, loc chat.ProposalLocation, tool string, state chat.ProposalState) chat.Proposal {
	t.Helper()
	p := mustCreateProposal(t, s, owner, convID, loc, tool, `{"query":"x"}`)
	path, ok := proposalPaths[loc][state]
	if !ok {
		t.Fatalf("no path to %s at location %s", state, loc)
	}
	for _, next := range path {
		p = mustTransition(t, s, owner, p, next, chat.ProposalUpdate{})
	}
	if p.State != state {
		t.Fatalf("walked to %s, want %s", p.State, state)
	}
	return p
}

func mustGetProposal(t *testing.T, s proposalStore, owner chat.OwnerKey, id string) chat.Proposal {
	t.Helper()
	p, err := s.GetProposal(ctx(), owner, id)
	if err != nil {
		t.Fatalf("GetProposal(%s): %v", id, err)
	}
	return p
}

func testProposalCreateGetAndList(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	first := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1","name":"a"}`, base, time.Time{})
	second := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"main.go"}`, base.Add(time.Minute), time.Time{})

	got := mustGetProposal(t, s, owner, first.ID)
	if got.ID != first.ID || got.ConversationID != conv.ID {
		t.Fatalf("stored proposal = %+v", got)
	}
	if got.Tool != "update_record" || got.ToolFingerprint != "fingerprint-1" || got.ManifestRevision != "manifest-1" {
		t.Fatalf("stored tool binding = %+v", got)
	}
	if got.Location != chat.ProposalLocationServer || got.Target != "record-1" || got.Summary != "changes one record" {
		t.Fatalf("stored proposal = %+v", got)
	}
	if got.TurnID != "turn-1" || got.CallID != "call-1" {
		t.Fatalf("stored proposal lost its turn/call binding: %+v", got)
	}
	if !strings.HasPrefix(got.ArgumentsHash, "sha256:") {
		t.Fatalf("arguments hash = %q", got.ArgumentsHash)
	}
	if _, err := s.GetProposal(ctx(), owner, "missing"); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("missing proposal = %v, want ErrNotFound", err)
	}

	all, next, err := s.ListProposals(ctx(), owner, conv.ID, nil, chat.Page{})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if next != "" {
		t.Fatalf("unexpected cursor %q", next)
	}
	if len(all) != 2 || all[0].ID != first.ID || all[1].ID != second.ID {
		t.Fatalf("ascending order wrong: %+v", all)
	}

	// An empty filter means every state; a filter is applied before paging.
	pending, _, err := s.ListProposals(ctx(), owner, conv.ID, []chat.ProposalState{chat.ProposalStatePending}, chat.Page{})
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending filter = %d, %v; want 2", len(pending), err)
	}
	second = mustTransition(t, s, owner, second, chat.ProposalStateRejected, chat.ProposalUpdate{Decided: true})
	pending, _, err = s.ListProposals(ctx(), owner, conv.ID, []chat.ProposalState{chat.ProposalStatePending}, chat.Page{})
	if err != nil || len(pending) != 1 || pending[0].ID != first.ID {
		t.Fatalf("pending filter after reject = %+v, %v", pending, err)
	}
	decided, _, err := s.ListProposals(ctx(), owner, conv.ID, []chat.ProposalState{chat.ProposalStateRejected, chat.ProposalStateExpired}, chat.Page{})
	if err != nil || len(decided) != 1 || decided[0].ID != second.ID {
		t.Fatalf("rejected/expired filter = %+v, %v", decided, err)
	}
	none, _, err := s.ListProposals(ctx(), owner, conv.ID, []chat.ProposalState{chat.ProposalStateApplied}, chat.Page{})
	if err != nil || len(none) != 0 {
		t.Fatalf("unused state filter = %+v, %v", none, err)
	}

	// Pagination walks the ascending order without gaps or repeats.
	third := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r3"}`, base.Add(2*time.Minute), time.Time{})
	var seen []string
	page := chat.Page{Limit: 2}
	for i := 0; i < 3; i++ {
		items, cursor, err := s.ListProposals(ctx(), owner, conv.ID, nil, page)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, p := range items {
			seen = append(seen, p.ID)
		}
		if cursor == "" {
			break
		}
		page.Cursor = cursor
	}
	if len(seen) != 3 || seen[0] != first.ID || seen[1] != second.ID || seen[2] != third.ID {
		t.Fatalf("paginated proposals = %v", seen)
	}
	if _, _, err := s.ListProposals(ctx(), owner, conv.ID, nil, chat.Page{Cursor: "not-a-cursor!"}); err == nil {
		t.Fatal("invalid proposal cursor accepted")
	}
	if _, _, err := s.ListProposals(ctx(), owner, "missing", nil, chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("list for missing conversation = %v, want ErrNotFound", err)
	}

	// Creation validates the conversation, the arguments and the hash, and
	// never creates a proposal that is not pending at revision 1.
	canonical, _ := proposalHash(t, `{"a":1}`)
	cases := []struct {
		name string
		p    chat.Proposal
		want error
	}{
		{"missing conversation", chat.Proposal{ConversationID: "missing", Tool: "t", Location: chat.ProposalLocationServer, Arguments: canonical}, chat.ErrNotFound},
		{"non-object arguments", chat.Proposal{ConversationID: conv.ID, Tool: "t", Location: chat.ProposalLocationServer, Arguments: json.RawMessage(`[1]`)}, nil},
		{"empty arguments", chat.Proposal{ConversationID: conv.ID, Tool: "t", Location: chat.ProposalLocationServer, Arguments: nil}, nil},
		{"unknown location", chat.Proposal{ConversationID: conv.ID, Tool: "t", Location: "browser", Arguments: canonical}, nil},
		{"without tool", chat.Proposal{ConversationID: conv.ID, Location: chat.ProposalLocationServer, Arguments: canonical}, nil},
		{"mismatched hash", chat.Proposal{ConversationID: conv.ID, Tool: "t", Location: chat.ProposalLocationServer, Arguments: canonical, ArgumentsHash: "sha256:00"}, chat.ErrConflict},
		{"duplicate id", chat.Proposal{ID: first.ID, ConversationID: conv.ID, Tool: "t", Location: chat.ProposalLocationServer, Arguments: canonical}, chat.ErrConflict},
	}
	for _, tc := range cases {
		_, err := s.CreateProposal(ctx(), owner, tc.p)
		if tc.want == nil {
			if err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func testProposalOwnerIsolation(t *testing.T, s proposalStore) {
	owner := ownerKey()
	stranger := otherOwner()
	conv := mustCreateConversation(t, s, owner, "private")
	server := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1"}`)
	editor := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"main.go"}`)

	if _, err := s.GetProposal(ctx(), stranger, server.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("GetProposal = %v, want ErrNotFound", err)
	}
	if _, _, err := s.ListProposals(ctx(), stranger, conv.ID, nil, chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("ListProposals = %v, want ErrNotFound", err)
	}
	if _, err := s.TransitionProposal(ctx(), stranger, server.ID, chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("server TransitionProposal = %v, want ErrNotFound", err)
	}
	if _, err := s.TransitionProposal(ctx(), stranger, editor.ID, chat.ProposalStatePending, 1, chat.ProposalStateApproved, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("editor TransitionProposal = %v, want ErrNotFound", err)
	}
	if _, err := s.ReviseProposal(ctx(), stranger, server.ID, 1, json.RawMessage(`{"id":"r2"}`), ""); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("ReviseProposal = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateProposal(ctx(), stranger, chat.Proposal{
		ConversationID: conv.ID,
		Tool:           "update_record",
		Location:       chat.ProposalLocationServer,
		Arguments:      json.RawMessage(`{}`),
	}); !errors.Is(err, chat.ErrNotFound) {
		t.Errorf("CreateProposal in another owner's conversation = %v, want ErrNotFound", err)
	}

	// The refusals changed nothing, and the owner still decides.
	if got := mustGetProposal(t, s, owner, server.ID); got.State != chat.ProposalStatePending || got.Revision != 1 {
		t.Fatalf("refused stranger calls changed the proposal: %+v", got)
	}
	mustTransition(t, s, owner, server, chat.ProposalStateExecuting, chat.ProposalUpdate{})
}

func testProposalAllowedEdges(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	cases := []struct {
		name   string
		loc    chat.ProposalLocation
		states []chat.ProposalState
	}{
		{"server succeeds", chat.ProposalLocationServer, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateExecuting, chat.ProposalStateSucceeded}},
		{"server fails", chat.ProposalLocationServer, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateExecuting, chat.ProposalStateFailed}},
		{"server unknown", chat.ProposalLocationServer, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateExecuting, chat.ProposalStateUnknown}},
		{"server rejects", chat.ProposalLocationServer, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateRejected}},
		{"server expires", chat.ProposalLocationServer, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateExpired}},
		{"editor applies", chat.ProposalLocationEditor, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateApproved, chat.ProposalStateApplied}},
		{"editor goes stale", chat.ProposalLocationEditor, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateApproved, chat.ProposalStateStale}},
		{"editor fails", chat.ProposalLocationEditor, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateApproved, chat.ProposalStateFailed}},
		{"editor approved expires", chat.ProposalLocationEditor, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateApproved, chat.ProposalStateExpired}},
		{"editor rejects", chat.ProposalLocationEditor, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateRejected}},
		{"editor pending expires", chat.ProposalLocationEditor, []chat.ProposalState{chat.ProposalStatePending, chat.ProposalStateExpired}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustCreateProposal(t, s, owner, conv.ID, tc.loc, "tool", `{"a":1}`)
			for _, to := range tc.states[1:] {
				u := chat.ProposalUpdate{}
				switch to {
				case chat.ProposalStateRejected, chat.ProposalStateApproved:
					u.Decided = true
				case chat.ProposalStateSucceeded:
					u.Result = json.RawMessage(`{"ok":true}`)
				case chat.ProposalStateFailed:
					u.Failure = "the record was not found"
				case chat.ProposalStateApplied:
					u.Result = json.RawMessage(`{"applied":1}`)
					u.EditorRevision = "editor-rev-7"
					u.Hunks = []chat.HunkOutcome{{ID: "h1", Status: chat.HunkStatusApplied}}
				}
				p = mustTransition(t, s, owner, p, to, u)

				if u.Decided && p.DecidedAt == nil {
					t.Fatalf("%s did not record the decision time", to)
				}
				if u.Result != nil && string(p.Result) != string(u.Result) {
					t.Fatalf("%s result = %s, want %s", to, p.Result, u.Result)
				}
				if u.Failure != "" && p.Failure != u.Failure {
					t.Fatalf("%s failure = %q, want %q", to, p.Failure, u.Failure)
				}
				if u.EditorRevision != "" && p.EditorRevision != u.EditorRevision {
					t.Fatalf("%s editor revision = %q, want %q", to, p.EditorRevision, u.EditorRevision)
				}
				if len(u.Hunks) > 0 && (len(p.Hunks) != 1 || p.Hunks[0].Status != chat.HunkStatusApplied) {
					t.Fatalf("%s hunks = %+v", to, p.Hunks)
				}
			}
			if !p.State.Terminal() {
				t.Fatalf("final state %s is not terminal", p.State)
			}
			stored := mustGetProposal(t, s, owner, p.ID)
			if stored.State != p.State || stored.Revision != p.Revision {
				t.Fatalf("stored proposal = %s revision %d, want %s revision %d", stored.State, stored.Revision, p.State, p.Revision)
			}
		})
	}
}

func testProposalRefusedEdges(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	cases := []struct {
		name string
		loc  chat.ProposalLocation
		from chat.ProposalState
		to   chat.ProposalState
	}{
		{"server pending to approved", chat.ProposalLocationServer, chat.ProposalStatePending, chat.ProposalStateApproved},
		{"server pending to applied", chat.ProposalLocationServer, chat.ProposalStatePending, chat.ProposalStateApplied},
		{"server pending to succeeded", chat.ProposalLocationServer, chat.ProposalStatePending, chat.ProposalStateSucceeded},
		{"server pending to failed", chat.ProposalLocationServer, chat.ProposalStatePending, chat.ProposalStateFailed},
		{"server executing to rejected", chat.ProposalLocationServer, chat.ProposalStateExecuting, chat.ProposalStateRejected},
		{"server executing to approved", chat.ProposalLocationServer, chat.ProposalStateExecuting, chat.ProposalStateApproved},
		{"server succeeded to failed", chat.ProposalLocationServer, chat.ProposalStateSucceeded, chat.ProposalStateFailed},
		{"server rejected to executing", chat.ProposalLocationServer, chat.ProposalStateRejected, chat.ProposalStateExecuting},
		{"server unknown to executing", chat.ProposalLocationServer, chat.ProposalStateUnknown, chat.ProposalStateExecuting},
		{"editor pending to executing", chat.ProposalLocationEditor, chat.ProposalStatePending, chat.ProposalStateExecuting},
		{"editor pending to applied", chat.ProposalLocationEditor, chat.ProposalStatePending, chat.ProposalStateApplied},
		{"editor approved to succeeded", chat.ProposalLocationEditor, chat.ProposalStateApproved, chat.ProposalStateSucceeded},
		{"editor approved to rejected", chat.ProposalLocationEditor, chat.ProposalStateApproved, chat.ProposalStateRejected},
		{"editor applied to approved", chat.ProposalLocationEditor, chat.ProposalStateApplied, chat.ProposalStateApproved},
		{"editor stale to approved", chat.ProposalLocationEditor, chat.ProposalStateStale, chat.ProposalStateApproved},
		{"editor rejected to approved", chat.ProposalLocationEditor, chat.ProposalStateRejected, chat.ProposalStateApproved},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := proposalIn(t, s, owner, conv.ID, tc.loc, "tool", tc.from)
			if _, err := s.TransitionProposal(ctx(), owner, p.ID, tc.from, p.Revision, tc.to, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrInvalidTransition) {
				t.Fatalf("%s -> %s = %v, want ErrInvalidTransition", tc.from, tc.to, err)
			}
			if got := mustGetProposal(t, s, owner, p.ID); got.State != tc.from || got.Revision != p.Revision {
				t.Fatalf("refused edge changed the proposal: %+v", got)
			}
		})
	}
}

func testProposalCompareAndSwap(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")
	p := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1"}`)

	// A wrong from state is a conflict even when the requested edge is legal.
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStateExecuting, 1, chat.ProposalStateSucceeded, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("wrong from state = %v, want ErrConflict", err)
	}
	// A wrong revision is a conflict even when state and edge are right.
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStatePending, 2, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("wrong revision = %v, want ErrConflict", err)
	}
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStatePending, 0, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("zero revision = %v, want ErrConflict", err)
	}
	if got := mustGetProposal(t, s, owner, p.ID); got.State != chat.ProposalStatePending || got.Revision != 1 {
		t.Fatalf("lost compare-and-swap changed the proposal: %+v", got)
	}

	p = mustTransition(t, s, owner, p, chat.ProposalStateExecuting, chat.ProposalUpdate{})
	// Both the old state/revision pair and the new state with the old revision
	// are stale now.
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStatePending, 1, chat.ProposalStateRejected, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("stale state = %v, want ErrConflict", err)
	}
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStateExecuting, 1, chat.ProposalStateSucceeded, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("stale revision = %v, want ErrConflict", err)
	}
	if got := mustGetProposal(t, s, owner, p.ID); got.State != chat.ProposalStateExecuting || got.Revision != 2 {
		t.Fatalf("stale calls changed the proposal: %+v", got)
	}

	if _, err := s.TransitionProposal(ctx(), owner, "missing", chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("missing proposal = %v, want ErrNotFound", err)
	}
}

func testProposalConcurrentDecision(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	const racers = 20

	// One decision target: exactly one racer wins the transition.
	p := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1"}`)
	wins, conflicts, otherErr := raceProposal(t, s, owner, p, racers, func(i int) chat.ProposalState {
		return chat.ProposalStateExecuting
	})
	if len(otherErr) > 0 {
		t.Fatalf("unexpected results: %v", otherErr)
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1 and %d", wins, conflicts, racers-1)
	}
	if got := mustGetProposal(t, s, owner, p.ID); got.State != chat.ProposalStateExecuting || got.Revision != 2 {
		t.Fatalf("stored proposal = %s revision %d", got.State, got.Revision)
	}
	// A stored decision is never applied a second time.
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("replayed decision = %v, want ErrConflict", err)
	}

	// Two competing decisions: exactly one of them wins overall.
	editor := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"main.go"}`)
	wins, conflicts, otherErr = raceProposal(t, s, owner, editor, racers, func(i int) chat.ProposalState {
		if i%2 == 0 {
			return chat.ProposalStateApproved
		}
		return chat.ProposalStateRejected
	})
	if len(otherErr) > 0 {
		t.Fatalf("unexpected results: %v", otherErr)
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("competing decisions: wins=%d conflicts=%d, want 1 and %d", wins, conflicts, racers-1)
	}
	got := mustGetProposal(t, s, owner, editor.ID)
	if (got.State != chat.ProposalStateApproved && got.State != chat.ProposalStateRejected) || got.Revision != 2 {
		t.Fatalf("competing decision stored %s revision %d", got.State, got.Revision)
	}
}

// raceProposal runs racers simultaneous compare-and-swap transitions on the
// same starting state and revision, and counts how many of each result came
// back.
func raceProposal(t *testing.T, s proposalStore, owner chat.OwnerKey, p chat.Proposal, racers int, target func(i int) chat.ProposalState) (wins, conflicts int, otherErr []error) {
	t.Helper()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.TransitionProposal(ctx(), owner, p.ID, p.State, p.Revision, target(i), chat.ProposalUpdate{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, chat.ErrConflict):
				conflicts++
			default:
				otherErr = append(otherErr, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return wins, conflicts, otherErr
}

func testProposalRevise(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")
	p := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1","name":"a"}`)

	canonical, hash := proposalHash(t, `{"name":"b","id":"r1"}`)
	revised, err := s.ReviseProposal(ctx(), owner, p.ID, 1, json.RawMessage(`{"name":"b","id":"r1"}`), "")
	if err != nil {
		t.Fatalf("ReviseProposal: %v", err)
	}
	if revised.Revision != 2 {
		t.Fatalf("revision = %d after revise, want 2", revised.Revision)
	}
	if revised.State != chat.ProposalStatePending {
		t.Fatalf("revise changed the state to %s", revised.State)
	}
	if string(revised.Arguments) != string(canonical) || revised.ArgumentsHash != hash {
		t.Fatalf("revised arguments = %s (%s), want %s (%s)", revised.Arguments, revised.ArgumentsHash, canonical, hash)
	}
	if revised.UpdatedAt.Before(p.UpdatedAt) {
		t.Fatalf("revise moved UpdatedAt backwards: %s before %s", revised.UpdatedAt, p.UpdatedAt)
	}

	// The old revision is stale for both calls.
	if _, err := s.ReviseProposal(ctx(), owner, p.ID, 1, canonical, ""); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("stale revise = %v, want ErrConflict", err)
	}
	if _, err := s.TransitionProposal(ctx(), owner, p.ID, chat.ProposalStatePending, 1, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("decision on a stale revision = %v, want ErrConflict", err)
	}
	// Only a JSON object may be revised, and the refusal changes nothing.
	if _, err := s.ReviseProposal(ctx(), owner, p.ID, 2, json.RawMessage(`"b"`), ""); err == nil {
		t.Fatal("non-object revision accepted")
	}
	// A supplied hash that does not match the arguments is a conflict.
	if _, err := s.ReviseProposal(ctx(), owner, p.ID, 2, canonical, "sha256:00"); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("mismatched revision hash = %v, want ErrConflict", err)
	}
	if got := mustGetProposal(t, s, owner, p.ID); got.Revision != 2 || string(got.Arguments) != string(canonical) {
		t.Fatalf("refused revisions changed the proposal: %+v", got)
	}

	// A decision after the revision is bound to the new arguments.
	p = mustTransition(t, s, owner, revised, chat.ProposalStateExecuting, chat.ProposalUpdate{})
	if _, err := s.ReviseProposal(ctx(), owner, p.ID, p.Revision, canonical, ""); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("revise after executing = %v, want ErrConflict", err)
	}

	rejected := proposalIn(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", chat.ProposalStateRejected)
	if _, err := s.ReviseProposal(ctx(), owner, rejected.ID, rejected.Revision, canonical, ""); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("revise a rejected proposal = %v, want ErrConflict", err)
	}
}

func testProposalExpiry(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	base := time.Now().UTC()
	past := base.Add(-time.Minute)
	soon := base.Add(time.Minute)
	later := base.Add(time.Hour)
	expireAt := base.Add(2 * time.Minute)

	// A pending proposal past its expiry may only move to expired.
	overdue := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1"}`, time.Time{}, past)
	if _, err := s.TransitionProposal(ctx(), owner, overdue.ID, chat.ProposalStatePending, overdue.Revision, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("expired proposal executed = %v, want ErrConflict", err)
	}
	if _, err := s.TransitionProposal(ctx(), owner, overdue.ID, chat.ProposalStatePending, overdue.Revision, chat.ProposalStateRejected, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("expired proposal rejected = %v, want ErrConflict", err)
	}
	overdue = mustTransition(t, s, owner, overdue, chat.ProposalStateExpired, chat.ProposalUpdate{})
	if _, err := s.TransitionProposal(ctx(), owner, overdue.ID, chat.ProposalStateExpired, overdue.Revision, chat.ProposalStateExecuting, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("expired -> executing = %v, want ErrInvalidTransition", err)
	}

	// One proposal expires soon, one later, one never, one is already decided.
	soonPending := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r2"}`, time.Time{}, soon)
	laterPending := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r3"}`, time.Time{}, later)
	never := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r4"}`, time.Time{}, time.Time{})
	approved := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"a.go"}`, time.Time{}, soon)
	approved = mustTransition(t, s, owner, approved, chat.ProposalStateApproved, chat.ProposalUpdate{Decided: true})
	rejected := mustCreateProposalAt(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"b.go"}`, time.Time{}, soon)
	rejected = mustTransition(t, s, owner, rejected, chat.ProposalStateRejected, chat.ProposalUpdate{Decided: true})

	n, err := s.ExpireProposals(ctx(), expireAt)
	if err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}
	if n != 2 {
		t.Fatalf("ExpireProposals = %d, want 2 (a pending and an approved proposal)", n)
	}
	if got := mustGetProposal(t, s, owner, soonPending.ID); got.State != chat.ProposalStateExpired || got.Revision != 2 {
		t.Fatalf("expired pending = %s revision %d", got.State, got.Revision)
	}
	if got := mustGetProposal(t, s, owner, approved.ID); got.State != chat.ProposalStateExpired || got.Revision != 3 {
		t.Fatalf("expired approved = %s revision %d", got.State, got.Revision)
	}
	if got := mustGetProposal(t, s, owner, laterPending.ID); got.State != chat.ProposalStatePending {
		t.Fatalf("future proposal = %s, want pending", got.State)
	}
	if got := mustGetProposal(t, s, owner, never.ID); got.State != chat.ProposalStatePending {
		t.Fatalf("never-expiring proposal = %s, want pending", got.State)
	}
	if got := mustGetProposal(t, s, owner, rejected.ID); got.State != chat.ProposalStateRejected {
		t.Fatalf("decided proposal = %s, want rejected", got.State)
	}
	again, err := s.ExpireProposals(ctx(), expireAt)
	if err != nil || again != 0 {
		t.Fatalf("second ExpireProposals = %d, %v; want 0", again, err)
	}

	// An unexpired proposal still executes.
	mustTransition(t, s, owner, never, chat.ProposalStateExecuting, chat.ProposalUpdate{})
}

func testProposalRecoverExecuting(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	executing := proposalIn(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", chat.ProposalStateExecuting)
	pending := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r2"}`)
	done := proposalIn(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", chat.ProposalStateSucceeded)
	approved := proposalIn(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", chat.ProposalStateApproved)

	n, err := s.RecoverExecuting(ctx())
	if err != nil {
		t.Fatalf("RecoverExecuting: %v", err)
	}
	if n != 1 {
		t.Fatalf("RecoverExecuting = %d, want 1", n)
	}
	got := mustGetProposal(t, s, owner, executing.ID)
	if got.State != chat.ProposalStateUnknown {
		t.Fatalf("recovered state = %s, want unknown", got.State)
	}
	if got.Failure != chat.UnknownOutcomeMessage {
		t.Fatalf("recovered failure = %q, want %q", got.Failure, chat.UnknownOutcomeMessage)
	}
	if got.Revision != executing.Revision+1 {
		t.Fatalf("recovered revision = %d, want %d", got.Revision, executing.Revision+1)
	}
	// Unknown is terminal: nothing reruns it.
	if _, err := s.TransitionProposal(ctx(), owner, executing.ID, chat.ProposalStateUnknown, got.Revision, chat.ProposalStateSucceeded, chat.ProposalUpdate{}); !errors.Is(err, chat.ErrInvalidTransition) {
		t.Fatalf("unknown -> succeeded = %v, want ErrInvalidTransition", err)
	}
	for _, p := range []chat.Proposal{pending, done, approved} {
		if stored := mustGetProposal(t, s, owner, p.ID); stored.State != p.State || stored.Failure != "" {
			t.Fatalf("RecoverExecuting changed %s: %+v", p.State, stored)
		}
	}
	again, err := s.RecoverExecuting(ctx())
	if err != nil || again != 0 {
		t.Fatalf("second RecoverExecuting = %d, %v; want 0", again, err)
	}
}

func testProposalDeleteCascade(t *testing.T, s proposalStore) {
	owner := ownerKey()

	conv := mustCreateConversation(t, s, owner, "chat")
	first := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationServer, "update_record", `{"id":"r1"}`)
	second := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"a.go"}`)

	if err := s.DeleteConversation(ctx(), owner, conv.ID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, err := s.GetProposal(ctx(), owner, id); !errors.Is(err, chat.ErrNotFound) {
			t.Fatalf("proposal %s survived its conversation: %v", id, err)
		}
	}
	if _, _, err := s.ListProposals(ctx(), owner, conv.ID, nil, chat.Page{}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("proposals survived their conversation: %v", err)
	}

	// A conversation id can be reused without inheriting proposals.
	again, err := s.CreateConversation(ctx(), chat.Conversation{ID: conv.ID, Owner: owner, Title: "again"})
	if err != nil {
		t.Fatalf("recreate conversation: %v", err)
	}
	items, _, err := s.ListProposals(ctx(), owner, again.ID, nil, chat.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("recreated conversation inherited proposals: %+v", items)
	}
	if _, err := s.GetProposal(ctx(), owner, first.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("deleted proposal resurfaced: %v", err)
	}

	// An executing proposal blocks deletion until its outcome is recorded.
	busy := mustCreateConversation(t, s, owner, "busy")
	executing := proposalIn(t, s, owner, busy.ID, chat.ProposalLocationServer, "update_record", chat.ProposalStateExecuting)
	if err := s.DeleteConversation(ctx(), owner, busy.ID); !errors.Is(err, chat.ErrConflict) {
		t.Fatalf("delete while executing = %v, want ErrConflict", err)
	}
	if got := mustGetProposal(t, s, owner, executing.ID); got.State != chat.ProposalStateExecuting {
		t.Fatalf("refused delete changed the proposal: %s", got.State)
	}
	executing = mustTransition(t, s, owner, executing, chat.ProposalStateFailed, chat.ProposalUpdate{Failure: "denied"})
	if err := s.DeleteConversation(ctx(), owner, busy.ID); err != nil {
		t.Fatalf("delete after outcome: %v", err)
	}
	if _, err := s.GetProposal(ctx(), owner, executing.ID); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("outcome proposal survived the delete: %v", err)
	}
}

func testProposalDeepCopy(t *testing.T, s proposalStore) {
	owner := ownerKey()
	conv := mustCreateConversation(t, s, owner, "chat")

	raw := json.RawMessage(`{"id":"r1","name":"a"}`)
	canonical, hash, err := chat.HashArguments(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProposal(ctx(), owner, chat.Proposal{
		ConversationID: conv.ID,
		Tool:           "update_record",
		Location:       chat.ProposalLocationServer,
		Arguments:      raw,
		ArgumentsHash:  hash,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	raw[2] = 'X' // caller mutation must not reach the store
	if got := mustGetProposal(t, s, owner, p.ID); string(got.Arguments) != string(canonical) {
		t.Fatalf("stored arguments mutated through the caller: %s", got.Arguments)
	}

	// Mutating what a call returned must not mutate stored state either.
	p.Arguments[2] = 'Y'
	if got := mustGetProposal(t, s, owner, p.ID); string(got.Arguments) != string(canonical) {
		t.Fatalf("stored arguments alias a returned proposal: %s", got.Arguments)
	}

	editor := mustCreateProposal(t, s, owner, conv.ID, chat.ProposalLocationEditor, "edit_buffer", `{"path":"main.go"}`)
	approved, err := s.TransitionProposal(ctx(), owner, editor.ID, chat.ProposalStatePending, editor.Revision, chat.ProposalStateApproved, chat.ProposalUpdate{Decided: true})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	hunks := []chat.HunkOutcome{{ID: "h1", Status: chat.HunkStatusApplied}}
	upd := chat.ProposalUpdate{
		Result:         json.RawMessage(`{"applied":1}`),
		EditorRevision: "editor-rev-9",
		Hunks:          hunks,
	}
	decided, err := s.TransitionProposal(ctx(), owner, approved.ID, chat.ProposalStateApproved, approved.Revision, chat.ProposalStateApplied, upd)
	if err != nil {
		t.Fatalf("TransitionProposal: %v", err)
	}
	if decided.DecidedAt == nil {
		t.Fatal("decision time not recorded")
	}
	hunks[0].ID = "mutated"
	upd.Result[0] = '['
	decidedAt := *decided.DecidedAt
	*decided.DecidedAt = decidedAt.Add(time.Hour)
	decided.Hunks[0].Status = chat.HunkStatusFailed
	decided.Result[0] = '['

	stored := mustGetProposal(t, s, owner, editor.ID)
	if len(stored.Hunks) != 1 || stored.Hunks[0].ID != "h1" || stored.Hunks[0].Status != chat.HunkStatusApplied {
		t.Fatalf("stored hunks mutated: %+v", stored.Hunks)
	}
	if string(stored.Result) != `{"applied":1}` {
		t.Fatalf("stored result mutated: %s", stored.Result)
	}
	if stored.DecidedAt == nil || !stored.DecidedAt.Equal(decidedAt) {
		t.Fatalf("stored decision time = %v, want %v", stored.DecidedAt, decidedAt)
	}
	if stored.EditorRevision != "editor-rev-9" {
		t.Fatalf("stored editor revision = %q", stored.EditorRevision)
	}
}
