package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

type twoTools struct{ called []string }

func (t *twoTools) Tools(context.Context) ([]provider.ToolDef, error) {
	return []provider.ToolDef{{Name: "read_things"}, {Name: "propose_editor_patch"}}, nil
}

func (t *twoTools) Call(_ context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	t.called = append(t.called, name)
	return json.RawMessage(`{"ok":true}`), nil
}

func TestSetupNarrowsToolsAndSeesTheCaller(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "tools")
	prov := scripted.New(
		toolCallStep(provider.ToolCall{ID: "c1", Name: "propose_editor_patch", Arguments: json.RawMessage(`{}`)}),
		textStep("done"),
	)
	var seen any
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m",
		Setup: func(_ context.Context, in TurnInput) (TurnSetup, error) {
			seen = in.Identity
			return TurnSetup{Tools: func(name string) bool { return name != "propose_editor_patch" }}, nil
		}})
	exec := &twoTools{}
	turn, _, err := runner.SubmitWithOptions(context.Background(), owner, conv.ID, SubmitOptions{
		Text: "edit it", Tools: exec, Identity: "caller-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := awaitTerminal(t, store, owner, turn.ID); final.State != TurnStateCompleted {
		t.Fatalf("turn = %s %+v", final.State, final.Error)
	}
	if seen != "caller-1" {
		t.Fatalf("Setup saw identity %v", seen)
	}
	reqs := prov.Requests()
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "read_things" {
		t.Fatalf("tools offered = %+v, want only read_things", reqs[0].Tools)
	}
	if len(exec.called) != 0 {
		t.Fatalf("a hidden tool ran: %v", exec.called)
	}
	// The model is told the tool isn't available.
	var result string
	for _, m := range drainMessages(t, store, owner, conv.ID) {
		for _, p := range m.Parts {
			if p.Type == PartToolResult {
				result = string(p.Result)
			}
		}
	}
	if !strings.Contains(result, "isn't available here") {
		t.Fatalf("tool result = %s", result)
	}
	_ = time.Second
}
