package chat

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// collectEvents reads a subscription until it closes, failing if it stays open
// past the limit. Every wait is bounded, never a fixed sleep.
func collectEvents(t *testing.T, ch <-chan Event, limit time.Duration) []Event {
	t.Helper()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	var out []Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-timer.C:
			t.Fatalf("subscription stayed open with %d events: %v", len(out), typesOf(out))
		}
	}
}

func assertContiguous(t *testing.T, events []Event) {
	t.Helper()
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: %v", i, e.Seq, typesOf(events))
		}
	}
}

func TestSubscribeReplayAndLive(t *testing.T) {
	for i := 0; i < 20; i++ {
		store := NewMemStore(MemStoreOptions{})
		chunks := make([]provider.Chunk, 0, 13)
		for j := 0; j < 12; j++ {
			chunks = append(chunks, provider.Chunk{Kind: provider.ChunkText, Text: fmt.Sprintf("t%d", j)})
		}
		chunks = append(chunks, provider.Chunk{Kind: provider.ChunkDone})
		prov := scripted.New(scripted.Step{Chunks: chunks, Delay: time.Millisecond})
		runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
		owner := ownerFixture()
		conv := mustConversation(t, store, owner, "subscribe")

		turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
		if err != nil {
			t.Fatalf("iteration %d: Submit: %v", i, err)
		}

		subCtx, cancel := context.WithCancel(context.Background())
		ch, err := runner.Subscribe(subCtx, owner, turn.ID, 0)
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: Subscribe: %v", i, err)
		}
		events := collectEvents(t, ch, testWait)
		cancel()

		assertContiguous(t, events)
		if len(events) == 0 {
			t.Fatalf("iteration %d: no events", i)
		}
		if !events[len(events)-1].Type.Terminal() {
			t.Fatalf("iteration %d: stream ended on %s, not a terminal event: %v", i, events[len(events)-1].Type, typesOf(events))
		}

		var view TurnView
		for _, e := range events {
			if err := view.Apply(e); err != nil {
				t.Fatalf("iteration %d: reducer: %v", i, err)
			}
		}
		if view.State != TurnStateCompleted {
			t.Fatalf("iteration %d: reduced state = %s", i, view.State)
		}
		if view.Text != "t0t1t2t3t4t5t6t7t8t9t10t11" {
			t.Fatalf("iteration %d: text = %q", i, view.Text)
		}

		// The store and the subscription must agree: the same count, the same
		// terminal event, no gap and no duplicate.
		stored := mustEvents(t, store, owner, turn.ID)
		if len(stored) != len(events) {
			t.Fatalf("iteration %d: store has %d events, subscription saw %d", i, len(stored), len(events))
		}
	}
}

func TestSubscribeAfterTerminal(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(textStep("done"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "after terminal")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitTerminal(t, store, owner, turn.ID)

	ch, err := runner.Subscribe(context.Background(), owner, turn.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	events := collectEvents(t, ch, testWait)
	assertContiguous(t, events)
	if !events[len(events)-1].Type.Terminal() {
		t.Fatalf("replay did not end with the terminal event: %v", typesOf(events))
	}

	// A subscriber that already has every event gets a closed channel, not a
	// stream that never ends.
	last := events[len(events)-1].Seq
	ch, err = runner.Subscribe(context.Background(), owner, turn.ID, last)
	if err != nil {
		t.Fatalf("Subscribe at the end: %v", err)
	}
	if extra := collectEvents(t, ch, time.Second); len(extra) != 0 {
		t.Fatalf("subscriber at the end received %d events", len(extra))
	}
}

func TestSubscribeResyncRequired(t *testing.T) {
	store := NewMemStore(MemStoreOptions{MaxEventsPerTurn: 3})
	prov := scripted.New(textStep("a", "b", "c", "d", "e", "f"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "resync")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitTerminal(t, store, owner, turn.ID)

	if _, err := runner.Subscribe(context.Background(), owner, turn.ID, 0); !errors.Is(err, ErrResyncRequired) {
		t.Fatalf("Subscribe = %v, want ErrResyncRequired", err)
	}

	// The newest events are still reachable. Find the first retained cursor,
	// which is the oldest one the store can still replay.
	var retained []Event
	for after := int64(0); after < 64; after++ {
		events, err := store.EventsAfter(ctx(), owner, turn.ID, after)
		if errors.Is(err, ErrResyncRequired) {
			continue
		}
		if err != nil {
			t.Fatalf("EventsAfter(%d): %v", after, err)
		}
		retained = events
		break
	}
	if len(retained) == 0 {
		t.Fatal("no retained window found")
	}
	ch, err := runner.Subscribe(context.Background(), owner, turn.ID, retained[0].Seq-1)
	if err != nil {
		t.Fatalf("Subscribe inside the window: %v", err)
	}
	got := collectEvents(t, ch, testWait)
	if len(got) != len(retained) {
		t.Fatalf("retained replay = %v, want %d events", typesOf(got), len(retained))
	}
	for i := range got {
		if got[i].Seq != retained[i].Seq {
			t.Fatalf("retained replay seq %d = %d, want %d", i, got[i].Seq, retained[i].Seq)
		}
	}
}

func TestSubscribeOwnerMismatch(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(scripted.Step{BlockUntilCancel: true})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "private")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := runner.Subscribe(context.Background(), otherOwner(), turn.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner = %v, want ErrNotFound", err)
	}
	if _, err := runner.Cancel(context.Background(), owner, turn.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestSubscribeSlowSubscriberDoesNotBlockTurn(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	chunks := make([]provider.Chunk, 0, 201)
	for i := 0; i < 200; i++ {
		chunks = append(chunks, provider.Chunk{Kind: provider.ChunkText, Text: "."})
	}
	chunks = append(chunks, provider.Chunk{Kind: provider.ChunkDone})
	prov := scripted.New(scripted.Step{Chunks: chunks, Delay: time.Millisecond})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "slow")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Subscribe and never read: the turn must still finish.
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := runner.Subscribe(subCtx, owner, turn.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	final := awaitTerminal(t, store, owner, turn.ID)
	if final.State != TurnStateCompleted {
		t.Fatalf("turn state = %s", final.State)
	}

	// The disconnected subscriber sees a contiguous prefix and then a close.
	events := collectEvents(t, ch, testWait)
	assertContiguous(t, events)
	if len(events) >= 202 {
		t.Fatalf("slow subscriber somehow saw every event (%d)", len(events))
	}
}

func TestSubscribeCancelClosesChannel(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := scripted.New(scripted.Step{BlockUntilCancel: true})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m"})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "subscribe cancel")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	subCtx, cancel := context.WithCancel(context.Background())
	ch, err := runner.Subscribe(subCtx, owner, turn.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// The replay does not depend on new events, so the first frame must arrive
	// before any cancellation.
	select {
	case e := <-ch:
		if e.Seq != 1 {
			t.Fatalf("first replayed event seq = %d", e.Seq)
		}
	case <-time.After(testWait):
		t.Fatal("replay never arrived")
	}
	cancel()
	// Cancellation closes the channel promptly; whatever was already buffered
	// may still be delivered first.
	collectEvents(t, ch, testWait)
	if _, err := runner.Cancel(context.Background(), owner, turn.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}
