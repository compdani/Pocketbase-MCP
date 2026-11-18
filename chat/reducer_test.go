package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testEvent(t *testing.T, turn string, seq int64, typ EventType, data any) Event {
	t.Helper()
	e, err := NewEvent("conv1", turn, typ, data)
	if err != nil {
		t.Fatalf("NewEvent(%s): %v", typ, err)
	}
	e.Seq = seq
	e.At = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(time.Duration(seq) * time.Millisecond)
	return e
}

func applyAll(t *testing.T, v *TurnView, events ...Event) {
	t.Helper()
	for _, e := range events {
		if err := v.Apply(e); err != nil {
			t.Fatalf("Apply(%s seq %d): %v", e.Type, e.Seq, err)
		}
	}
}

func TestReducerHappyPath(t *testing.T) {
	var v TurnView
	applyAll(t, &v,
		testEvent(t, "turn1", 1, EventTurnQueued, nil),
		testEvent(t, "turn1", 2, EventTurnStarted, nil),
		testEvent(t, "turn1", 3, EventTextDelta, TextDelta{Text: "héllo "}),
		testEvent(t, "turn1", 4, EventTextDelta, TextDelta{Text: "🌍 wörld"}),
		testEvent(t, "turn1", 5, EventUsageUpdated, Usage{InputTokens: 10, OutputTokens: 7}),
		testEvent(t, "turn1", 6, EventTurnCompleted, nil),
	)
	if v.TurnID != "turn1" {
		t.Fatalf("turn id = %q", v.TurnID)
	}
	if v.Text != "héllo 🌍 wörld" {
		t.Fatalf("text = %q", v.Text)
	}
	if v.State != TurnStateCompleted || !v.State.Terminal() {
		t.Fatalf("state = %s", v.State)
	}
	if v.Usage == nil || v.Usage.InputTokens != 10 || v.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", v.Usage)
	}
	if v.LastSeq != 6 {
		t.Fatalf("last seq = %d", v.LastSeq)
	}
}

func TestReducerIgnoresDuplicates(t *testing.T) {
	var v TurnView
	e1 := testEvent(t, "turn1", 1, EventTurnStarted, nil)
	e2 := testEvent(t, "turn1", 2, EventTextDelta, TextDelta{Text: "ab"})
	e3 := testEvent(t, "turn1", 3, EventTextDelta, TextDelta{Text: "cd"})
	applyAll(t, &v, e1, e2, e3)
	// Replay the same frames, as a reconnect would.
	applyAll(t, &v, e1, e2, e3)
	if v.Text != "abcd" {
		t.Fatalf("duplicate deltas applied: %q", v.Text)
	}
	if v.LastSeq != 3 {
		t.Fatalf("last seq = %d", v.LastSeq)
	}

	// A duplicate of the terminal event is also harmless.
	done := testEvent(t, "turn1", 4, EventTurnCompleted, nil)
	applyAll(t, &v, done)
	applyAll(t, &v, done)
	if v.State != TurnStateCompleted {
		t.Fatalf("state = %s", v.State)
	}
}

func TestReducerDetectsGaps(t *testing.T) {
	var v TurnView
	applyAll(t, &v, testEvent(t, "turn1", 1, EventTurnStarted, nil), testEvent(t, "turn1", 2, EventTextDelta, TextDelta{Text: "a"}))
	before := v
	if err := v.Apply(testEvent(t, "turn1", 4, EventTextDelta, TextDelta{Text: "b"})); err == nil {
		t.Fatal("gap accepted")
	}
	if v.Text != before.Text || v.LastSeq != before.LastSeq {
		t.Fatal("view changed on a rejected gap")
	}

	var fresh TurnView
	if err := fresh.Apply(testEvent(t, "turn2", 2, EventTurnStarted, nil)); err == nil {
		t.Fatal("stream starting at seq 2 accepted")
	}
}

func TestReducerRejectsEventsAfterTerminal(t *testing.T) {
	cases := []struct {
		name  string
		term  Event
		state TurnState
		extra Event
	}{
		{"completed", testEvent(t, "t", 2, EventTurnCompleted, nil), TurnStateCompleted, testEvent(t, "t", 3, EventTextDelta, TextDelta{Text: "late"})},
		{"failed", testEvent(t, "t", 2, EventTurnFailed, TurnFailed{Code: "provider_error", Message: "no"}), TurnStateFailed, testEvent(t, "t", 3, EventTurnCompleted, nil)},
		{"cancelled", testEvent(t, "t", 2, EventTurnCancelled, TurnCancelled{Reason: "user"}), TurnStateCancelled, testEvent(t, "t", 3, EventTextDelta, TextDelta{Text: "late"})},
	}
	for _, tc := range cases {
		var v TurnView
		applyAll(t, &v, testEvent(t, "t", 1, EventTurnStarted, nil), tc.term)
		if v.State != tc.state {
			t.Fatalf("%s: state = %s, want %s", tc.name, v.State, tc.state)
		}
		if err := v.Apply(tc.extra); err == nil {
			t.Fatalf("%s: event accepted after terminal state", tc.name)
		}
		if v.LastSeq != 2 {
			t.Fatalf("%s: last seq moved to %d", tc.name, v.LastSeq)
		}
	}
}

func TestReducerTools(t *testing.T) {
	var v TurnView
	applyAll(t, &v,
		testEvent(t, "t", 1, EventTurnStarted, nil),
		testEvent(t, "t", 2, EventToolStarted, ToolStarted{CallID: "a", Tool: "search", Arguments: json.RawMessage(`{"q":"x"}`)}),
		testEvent(t, "t", 3, EventToolStarted, ToolStarted{CallID: "b", Tool: "fetch"}),
		testEvent(t, "t", 4, EventToolCompleted, ToolCompleted{CallID: "a", Tool: "search", Result: json.RawMessage(`{"n":1}`)}),
		testEvent(t, "t", 5, EventToolFailed, ToolFailed{CallID: "b", Tool: "fetch", Message: "boom"}),
	)
	if len(v.Tools) != 2 {
		t.Fatalf("tools = %+v", v.Tools)
	}
	a := v.Tools[0]
	if a.CallID != "a" || a.Tool != "search" || a.Status != ToolStatusCompleted {
		t.Fatalf("tool a = %+v", a)
	}
	if string(a.Result) != `{"n":1}` {
		t.Fatalf("tool a result = %s", a.Result)
	}
	b := v.Tools[1]
	if b.CallID != "b" || b.Status != ToolStatusFailed || b.Message != "boom" {
		t.Fatalf("tool b = %+v", b)
	}

	// A result without a prior start is still represented, so a client never
	// loses an outcome.
	var orphan TurnView
	applyAll(t, &orphan,
		testEvent(t, "t2", 1, EventTurnStarted, nil),
		testEvent(t, "t2", 2, EventToolCompleted, ToolCompleted{CallID: "z", Tool: "search", Result: json.RawMessage(`true`)}),
	)
	if len(orphan.Tools) != 1 || orphan.Tools[0].Status != ToolStatusCompleted {
		t.Fatalf("orphan tool = %+v", orphan.Tools)
	}
}

func TestReducerInterruptedFailure(t *testing.T) {
	var v TurnView
	applyAll(t, &v,
		testEvent(t, "t", 1, EventTurnStarted, nil),
		testEvent(t, "t", 2, EventTurnFailed, TurnFailed{Code: "interrupted", Message: "server restart"}),
	)
	if v.State != TurnStateInterrupted {
		t.Fatalf("state = %s, want interrupted", v.State)
	}
	if v.Error == nil || v.Error.Code != "interrupted" {
		t.Fatalf("error = %+v", v.Error)
	}
}

func TestReducerRejectsBadEvents(t *testing.T) {
	var v TurnView
	applyAll(t, &v, testEvent(t, "t", 1, EventTurnStarted, nil))

	if err := v.Apply(testEvent(t, "other", 2, EventTextDelta, TextDelta{Text: "x"})); err == nil {
		t.Fatal("event for another turn accepted")
	} else if !strings.Contains(err.Error(), "turn") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := v.Apply(testEvent(t, "t", 2, EventTextDelta, nil)); err == nil {
		t.Fatal("text.delta without data accepted")
	}

	bad := testEvent(t, "t", 2, EventTextDelta, TextDelta{Text: "x"})
	bad.Data = json.RawMessage(`not json`)
	if err := v.Apply(bad); err == nil {
		t.Fatal("malformed data accepted")
	}

	unknown := testEvent(t, "t", 2, EventTextDelta, nil)
	unknown.Type = EventType("future.event")
	unknown.Data = json.RawMessage(`{}`)
	if err := v.Apply(unknown); err == nil {
		t.Fatal("unknown event type accepted")
	}
}

func TestTurnViewJSONShape(t *testing.T) {
	v := TurnView{TurnID: "t", State: TurnStateRunning, Text: "hi", LastSeq: 4}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"turnId"`, `"state"`, `"text"`, `"lastSeq"`, `"proposals"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("view JSON missing %s: %s", key, raw)
		}
	}
}

func proposalFixture(id string, revision int, state ProposalState) Proposal {
	return Proposal{
		ID:             id,
		ConversationID: "conv1",
		TurnID:         "t",
		Tool:           "update_record",
		Location:       ProposalLocationServer,
		Arguments:      json.RawMessage(`{"id":"r1"}`),
		ArgumentsHash:  "sha256:00",
		State:          state,
		Revision:       revision,
	}
}

func proposalPayload(t *testing.T, p Proposal) ProposalEventData {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("invalid proposal fixture: %v", err)
	}
	return ProposalEventData{Proposal: p}
}

func TestReducerProposals(t *testing.T) {
	var v TurnView
	applyAll(t, &v,
		testEvent(t, "t", 1, EventTurnStarted, nil),
		testEvent(t, "t", 2, EventProposalCreated, proposalPayload(t, proposalFixture("p1", 1, ProposalStatePending))),
		testEvent(t, "t", 3, EventProposalCreated, proposalPayload(t, proposalFixture("p2", 1, ProposalStatePending))),
		testEvent(t, "t", 4, EventProposalUpdated, proposalPayload(t, proposalFixture("p1", 2, ProposalStateExecuting))),
		testEvent(t, "t", 5, EventProposalUpdated, proposalPayload(t, proposalFixture("p1", 3, ProposalStateSucceeded))),
	)
	if len(v.Proposals) != 2 {
		t.Fatalf("proposals = %+v", v.Proposals)
	}
	if v.Proposals[0].ID != "p1" || v.Proposals[0].State != ProposalStateSucceeded || v.Proposals[0].Revision != 3 {
		t.Fatalf("updated proposal = %+v", v.Proposals[0])
	}
	if v.Proposals[1].ID != "p2" || v.Proposals[1].State != ProposalStatePending || v.Proposals[1].Revision != 1 {
		t.Fatalf("untouched proposal = %+v", v.Proposals[1])
	}
	if v.LastSeq != 5 || v.State != TurnStateRunning {
		t.Fatalf("view = %+v", v)
	}

	// An update may arrive before its creation (a client that reloads pending
	// proposals and then follows the stream); the entry is still single.
	var late TurnView
	applyAll(t, &late,
		testEvent(t, "t2", 1, EventTurnStarted, nil),
		testEvent(t, "t2", 2, EventProposalUpdated, proposalPayload(t, proposalFixture("p1", 2, ProposalStateExecuting))),
		testEvent(t, "t2", 3, EventProposalCreated, proposalPayload(t, proposalFixture("p1", 1, ProposalStatePending))),
	)
	if len(late.Proposals) != 1 {
		t.Fatalf("proposal duplicated: %+v", late.Proposals)
	}
	if late.Proposals[0].State != ProposalStateExecuting || late.Proposals[0].Revision != 2 {
		t.Fatalf("older frame overwrote a newer state: %+v", late.Proposals[0])
	}
}

func TestReducerProposalSetProposal(t *testing.T) {
	var v TurnView
	v.setProposal(ProposalView(proposalFixture("p1", 3, ProposalStateSucceeded)))
	v.setProposal(ProposalView(proposalFixture("p1", 2, ProposalStateExecuting)))
	if len(v.Proposals) != 1 || v.Proposals[0].Revision != 3 {
		t.Fatalf("older revision replaced a newer one: %+v", v.Proposals)
	}
	v.setProposal(ProposalView(proposalFixture("p1", 3, ProposalStateExecuting)))
	if v.Proposals[0].State != ProposalStateExecuting {
		t.Fatalf("equal revision did not replace: %+v", v.Proposals[0])
	}
	v.setProposal(ProposalView(proposalFixture("p2", 1, ProposalStatePending)))
	if len(v.Proposals) != 2 || v.Proposals[1].ID != "p2" {
		t.Fatalf("proposal not appended: %+v", v.Proposals)
	}
}

func TestReducerRejectsBadProposalData(t *testing.T) {
	var v TurnView
	applyAll(t, &v, testEvent(t, "t", 1, EventTurnStarted, nil))

	if err := v.Apply(testEvent(t, "t", 2, EventProposalCreated, ProposalEventData{Proposal: Proposal{ID: "p1"}})); err == nil {
		t.Fatal("incomplete proposal accepted")
	}
	if len(v.Proposals) != 0 || v.LastSeq != 1 {
		t.Fatalf("rejected proposal changed the view: %+v", v)
	}
	if err := v.Apply(testEvent(t, "t", 2, EventProposalUpdated, nil)); err == nil {
		t.Fatal("proposal.updated without data accepted")
	}
	bad := testEvent(t, "t", 2, EventProposalUpdated, proposalPayload(t, proposalFixture("p9", 1, ProposalStatePending)))
	bad.Data = json.RawMessage(`{"proposal":`)
	if err := v.Apply(bad); err == nil {
		t.Fatal("malformed proposal data accepted")
	}
}
