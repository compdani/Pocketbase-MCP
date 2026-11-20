package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProposalStateValid(t *testing.T) {
	valid := []ProposalState{
		ProposalStatePending, ProposalStateRejected, ProposalStateExpired,
		ProposalStateExecuting, ProposalStateSucceeded, ProposalStateFailed,
		ProposalStateUnknown, ProposalStateApproved, ProposalStateApplied,
		ProposalStateStale,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Fatalf("%s should be valid", s)
		}
	}
	for _, s := range []ProposalState{"", "done", "PENDING", "approved_by_user"} {
		if s.Valid() {
			t.Fatalf("%q should be invalid", s)
		}
	}
}

func TestProposalStateTerminal(t *testing.T) {
	terminal := []ProposalState{
		ProposalStateRejected, ProposalStateExpired, ProposalStateSucceeded,
		ProposalStateFailed, ProposalStateUnknown, ProposalStateApplied,
		ProposalStateStale,
	}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range []ProposalState{ProposalStatePending, ProposalStateExecuting, ProposalStateApproved} {
		if s.Terminal() {
			t.Fatalf("%s should not be terminal", s)
		}
	}
}

func TestCanTransitionProposal(t *testing.T) {
	states := []ProposalState{
		ProposalStatePending, ProposalStateRejected, ProposalStateExpired,
		ProposalStateExecuting, ProposalStateSucceeded, ProposalStateFailed,
		ProposalStateUnknown, ProposalStateApproved, ProposalStateApplied,
		ProposalStateStale,
	}
	allowed := map[ProposalLocation]map[ProposalState]map[ProposalState]bool{
		ProposalLocationServer: {
			ProposalStatePending: {
				ProposalStateExecuting: true,
				ProposalStateRejected:  true,
				ProposalStateExpired:   true,
			},
			ProposalStateExecuting: {
				ProposalStateSucceeded: true,
				ProposalStateFailed:    true,
				ProposalStateUnknown:   true,
				// RefusedError: refused before writing, pending again.
				ProposalStatePending: true,
			},
		},
		ProposalLocationEditor: {
			ProposalStatePending: {
				ProposalStateApproved: true,
				ProposalStateRejected: true,
				ProposalStateExpired:  true,
			},
			ProposalStateApproved: {
				ProposalStateApplied: true,
				ProposalStateStale:   true,
				ProposalStateFailed:  true,
				ProposalStateExpired: true,
			},
		},
	}
	for loc, edges := range allowed {
		for _, from := range states {
			for _, to := range states {
				want := edges[from][to]
				if got := CanTransitionProposal(loc, from, to); got != want {
					t.Errorf("CanTransitionProposal(%s, %s, %s) = %v, want %v", loc, from, to, got, want)
				}
			}
		}
	}
	for _, s := range states {
		if CanTransitionProposal(ProposalLocation("browser"), ProposalStatePending, s) {
			t.Fatalf("unknown location allowed pending -> %s", s)
		}
	}
	if !ProposalLocationServer.Valid() || !ProposalLocationEditor.Valid() {
		t.Fatal("known locations must be valid")
	}
	if ProposalLocation("browser").Valid() {
		t.Fatal("unknown location reported valid")
	}
}

func TestTerminalProposalStatesAllowNothing(t *testing.T) {
	states := []ProposalState{
		ProposalStatePending, ProposalStateRejected, ProposalStateExpired,
		ProposalStateExecuting, ProposalStateSucceeded, ProposalStateFailed,
		ProposalStateUnknown, ProposalStateApproved, ProposalStateApplied,
		ProposalStateStale,
	}
	for _, loc := range []ProposalLocation{ProposalLocationServer, ProposalLocationEditor} {
		for _, from := range []ProposalState{
			ProposalStateRejected, ProposalStateExpired, ProposalStateSucceeded,
			ProposalStateFailed, ProposalStateUnknown, ProposalStateApplied,
			ProposalStateStale,
		} {
			for _, to := range states {
				if CanTransitionProposal(loc, from, to) {
					t.Errorf("%s: terminal %s must not transition to %s", loc, from, to)
				}
			}
		}
	}
}

func TestHashArguments(t *testing.T) {
	canonical, hash, err := HashArguments(json.RawMessage(`{ "b": 2, "a": {"d": 4, "c": 3} }`))
	if err != nil {
		t.Fatalf("HashArguments: %v", err)
	}
	if string(canonical) != `{"a":{"c":3,"d":4},"b":2}` {
		t.Fatalf("canonical = %s", canonical)
	}
	if !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64 {
		t.Fatalf("hash = %q, want a sha256 digest", hash)
	}

	// Key order and formatting must not change the hash.
	again, againHash, err := HashArguments(json.RawMessage(`{"a":{"c":3,"d":4},"b":2}`))
	if err != nil {
		t.Fatalf("HashArguments: %v", err)
	}
	if string(again) != string(canonical) || againHash != hash {
		t.Fatalf("reordered arguments hashed differently: %s %s", again, againHash)
	}
	// A number keeps its exact literal, so 2 and 2.0 stay distinct.
	_, expanded, err := HashArguments(json.RawMessage(`{"b":2.0,"a":{"c":3,"d":4}}`))
	if err != nil {
		t.Fatalf("HashArguments: %v", err)
	}
	if expanded == hash {
		t.Fatal("2 and 2.0 must not share a hash")
	}
	// Big integers survive canonicalization without floating-point rounding.
	big, _, err := HashArguments(json.RawMessage(`{"n":12345678901234567890}`))
	if err != nil {
		t.Fatalf("HashArguments: %v", err)
	}
	if string(big) != `{"n":12345678901234567890}` {
		t.Fatalf("big number = %s", big)
	}

	bad := []string{``, `null`, `[]`, `"x"`, `1`, `{"a":`, `{"a":1} {"b":2}`}
	for _, raw := range bad {
		if _, _, err := HashArguments(json.RawMessage(raw)); err == nil {
			t.Errorf("HashArguments(%q) accepted", raw)
		}
	}
}

func TestProposalEventDataJSON(t *testing.T) {
	p := Proposal{
		ID:             "p1",
		ConversationID: "c1",
		Owner:          ownerFixture(),
		Tool:           "update_record",
		Location:       ProposalLocationServer,
		Arguments:      json.RawMessage(`{"id":"r1"}`),
		State:          ProposalStatePending,
		Revision:       1,
	}
	raw, err := json.Marshal(ProposalEventData{Proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(envelope["proposal"], &inner); err != nil {
		t.Fatalf("proposal envelope = %s: %v", raw, err)
	}
	for _, key := range []string{"id", "conversationId", "tool", "location", "arguments", "argumentsHash", "state", "revision", "createdAt"} {
		if _, ok := inner[key]; !ok {
			t.Errorf("proposal JSON missing %s: %s", key, raw)
		}
	}
	// The owner never reaches the wire, and an open-ended proposal omits its
	// zero expiry instead of sending year-one.
	for _, key := range []string{"owner", "namespace", "userId", "expiresAt"} {
		if _, ok := inner[key]; ok {
			t.Errorf("proposal JSON must not carry %s: %s", key, raw)
		}
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
}

func TestProposalValidate(t *testing.T) {
	base := func() Proposal {
		return Proposal{
			ID:             "p1",
			ConversationID: "c1",
			Tool:           "update_record",
			Location:       ProposalLocationServer,
			Arguments:      json.RawMessage(`{"id":"r1"}`),
			State:          ProposalStatePending,
			Revision:       1,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	cases := map[string]func(*Proposal){
		"id":           func(p *Proposal) { p.ID = "" },
		"conversation": func(p *Proposal) { p.ConversationID = "" },
		"location":     func(p *Proposal) { p.Location = "" },
		"tool":         func(p *Proposal) { p.Tool = "" },
		"state":        func(p *Proposal) { p.State = "decided" },
		"revision":     func(p *Proposal) { p.Revision = 0 },
		"arguments":    func(p *Proposal) { p.Arguments = json.RawMessage(`[]`) },
		"hunk":         func(p *Proposal) { p.Hunks = []HunkOutcome{{ID: "h1", Status: "done"}} },
	}
	for name, mutate := range cases {
		p := base()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("invalid %s accepted", name)
		}
	}
}
