package chat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventTypeSets(t *testing.T) {
	all := []EventType{
		EventTurnQueued, EventTurnStarted, EventTextDelta, EventToolStarted,
		EventToolCompleted, EventToolFailed, EventProposalCreated,
		EventProposalUpdated, EventUsageUpdated,
		EventTurnCompleted, EventTurnFailed, EventTurnCancelled,
	}
	if len(all) != 12 {
		t.Fatalf("expected 12 event types, got %d", len(all))
	}
	for _, typ := range all {
		if !typ.Valid() {
			t.Errorf("%s should be valid", typ)
		}
		want := typ == EventTurnCompleted || typ == EventTurnFailed || typ == EventTurnCancelled
		if typ.Terminal() != want {
			t.Errorf("%s Terminal() = %v, want %v", typ, typ.Terminal(), want)
		}
	}
	if EventType("nope").Valid() {
		t.Error("unknown event type reported valid")
	}
	if EventType("").Valid() {
		t.Error("empty event type reported valid")
	}
}

func TestEventTypeWireNames(t *testing.T) {
	want := map[EventType]string{
		EventTurnQueued:      "turn.queued",
		EventTurnStarted:     "turn.started",
		EventTextDelta:       "text.delta",
		EventToolStarted:     "tool.started",
		EventToolCompleted:   "tool.completed",
		EventToolFailed:      "tool.failed",
		EventProposalCreated: "proposal.created",
		EventProposalUpdated: "proposal.updated",
		EventUsageUpdated:    "usage.updated",
		EventTurnCompleted:   "turn.completed",
		EventTurnFailed:      "turn.failed",
		EventTurnCancelled:   "turn.cancelled",
	}
	for typ, name := range want {
		if string(typ) != name {
			t.Errorf("event type = %q, want %q", typ, name)
		}
	}
}

func TestNewEvent(t *testing.T) {
	e, err := NewEvent("conv1", "turn1", EventTextDelta, TextDelta{Text: "hi"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if e.Version != ProtocolVersion || ProtocolVersion != "1" {
		t.Fatalf("version = %q, want 1", e.Version)
	}
	if e.ConversationID != "conv1" || e.TurnID != "turn1" || e.Type != EventTextDelta {
		t.Fatalf("unexpected envelope: %+v", e)
	}
	if e.Seq != 0 || !e.At.IsZero() {
		t.Fatal("NewEvent must leave seq and time to the store")
	}
	if string(e.Data) != `{"text":"hi"}` {
		t.Fatalf("data = %s", e.Data)
	}

	noData, err := NewEvent("c", "t", EventTurnCompleted, nil)
	if err != nil {
		t.Fatalf("NewEvent nil data: %v", err)
	}
	if noData.Data != nil {
		t.Fatalf("nil data became %s", noData.Data)
	}

	if _, err := NewEvent("c", "t", EventType("bogus"), nil); err == nil {
		t.Fatal("invalid event type accepted")
	}
}

func TestPayloadJSONFieldNames(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"text", TextDelta{Text: "x"}, `{"text":"x"}`},
		{"tool started", ToolStarted{CallID: "c1", Tool: "search", Arguments: json.RawMessage(`{"q":"a"}`)}, `{"callId":"c1","tool":"search","arguments":{"q":"a"}}`},
		{"tool completed", ToolCompleted{CallID: "c1", Tool: "search", Result: json.RawMessage(`1`)}, `{"callId":"c1","tool":"search","result":1}`},
		{"tool failed", ToolFailed{CallID: "c1", Tool: "search", Message: "boom"}, `{"callId":"c1","tool":"search","message":"boom"}`},
		{"usage", Usage{InputTokens: 3, OutputTokens: 4}, `{"inputTokens":3,"outputTokens":4}`},
		{"turn failed", TurnFailed{Code: "provider_error", Message: "no"}, `{"code":"provider_error","message":"no"}`},
		{"turn cancelled", TurnCancelled{Reason: "user"}, `{"reason":"user"}`},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.val)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(raw) != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, raw, tc.want)
		}
	}
}

func TestEventWriteSSE(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e := Event{
		Version:        ProtocolVersion,
		Seq:            3,
		ConversationID: "c1",
		TurnID:         "t1",
		Type:           EventTextDelta,
		Data:           json.RawMessage(`{"text":"hi"}`),
		At:             at,
	}
	var buf bytes.Buffer
	if err := e.WriteSSE(&buf); err != nil {
		t.Fatalf("WriteSSE: %v", err)
	}
	want := "id: 3\n" +
		"event: text.delta\n" +
		`data: {"v":"1","seq":3,"conversationId":"c1","turnId":"t1","type":"text.delta","data":{"text":"hi"},"at":"2026-01-02T03:04:05Z"}` + "\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("SSE frame mismatch\n got: %q\nwant: %q", got, want)
	}
	if strings.Count(buf.String(), "\n\n") != 1 {
		t.Fatal("frame must end with exactly one blank line")
	}
}

func TestWriteHeartbeat(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHeartbeat(&buf); err != nil {
		t.Fatalf("WriteHeartbeat: %v", err)
	}
	if got := buf.String(); got != ": ping\n\n" {
		t.Fatalf("heartbeat = %q", got)
	}
	if strings.Contains(buf.String(), "id:") {
		t.Fatal("heartbeat must not carry an id")
	}
}

func TestRecordTypesUseCamelCase(t *testing.T) {
	c := Conversation{ID: "c1", Owner: ownerFixture(), Title: "t", Target: "rec1"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"title"`, `"target"`, `"createdAt"`, `"updatedAt"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("conversation JSON missing %s: %s", key, raw)
		}
	}
	// The owner partition is server-side state; it never goes on the wire.
	if strings.Contains(string(raw), `"owner"`) {
		t.Errorf("conversation JSON exposes the owner: %s", raw)
	}

	m := Message{ID: "m1", ConversationID: "c1", Role: RoleAssistant, Parts: []Part{
		{Type: PartText, Text: "hi"},
		{Type: PartToolCall, CallID: "x", Tool: "search", Arguments: json.RawMessage(`{}`)},
		{Type: PartToolResult, CallID: "x", Tool: "search", Result: json.RawMessage(`1`), IsError: true},
	}}
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"conversationId"`, `"role"`, `"parts"`, `"callId"`, `"isError"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("message JSON missing %s: %s", key, raw)
		}
	}

	turn := Turn{ID: "t1", ConversationID: "c1", Owner: ownerFixture(), State: TurnStateRunning, IdempotencyKey: "k", Model: "m", Error: &TurnFailed{Code: "x"}}
	raw, err = json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"conversationId"`, `"state"`, `"idempotencyKey"`, `"model"`, `"error"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("turn JSON missing %s: %s", key, raw)
		}
	}
}
