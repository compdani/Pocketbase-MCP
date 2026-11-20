package pocketbasemcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// blockPatchSchema is a host-defined structured editor patch: block
// operations rather than text hunks.
var blockPatchSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"editorId":     map[string]any{"type": "string"},
		"baseRevision": map[string]any{"type": "string"},
		"ops": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":    map[string]any{"type": "string", "enum": []any{"replace", "insert", "remove"}},
				"block": map[string]any{"type": "string"},
			},
			"required": []string{"op", "block"},
		}},
	},
	"required": []string{"editorId", "baseRevision", "ops"},
}

// validateBlockOps is a check JSON Schema cannot express: one op per block.
func validateBlockOps(_ context.Context, _ Principal, args map[string]any) error {
	ops, _ := args["ops"].([]any)
	if len(ops) == 0 {
		return Invalid("ops must contain at least one operation")
	}
	seen := map[string]bool{}
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		block, _ := op["block"].(string)
		if seen[block] {
			return Invalid("block " + block + " appears in more than one operation")
		}
		seen[block] = true
	}
	return nil
}

func summarizeBlockOps(args map[string]any) string {
	ops, _ := args["ops"].([]any)
	parts := []string{}
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		parts = append(parts, op["op"].(string)+" "+op["block"].(string))
	}
	return strings.Join(parts, ", ")
}

type planner interface {
	Plan(ctx context.Context, name string, args json.RawMessage) (*chat.ProposalDraft, error)
}

func hookFixture(t *testing.T) (*proposalsFixture, planner, *toolCounter) {
	t.Helper()
	f := newProposalsFixture(t)
	internalErr := &toolCounter{}
	f.srv.cfg.CustomTools = append(f.srv.cfg.CustomTools,
		ToolSpec{Name: "edit_blocks", Title: "Edit blocks", Channels: "chat", ExecutionLocation: "editor", InputSchema: blockPatchSchema, Validate: validateBlockOps, Summarize: summarizeBlockOps},
		ToolSpec{Name: "guarded_publish", Title: "Publish", Channels: "both", Write: true, InputSchema: map[string]any{"type": "object"},
			Validate: func(context.Context, Principal, map[string]any) error {
				return errors.New("db password is hunter2")
			},
			Handler: internalErr.handler},
	)
	return f, f.exec(t, false).(planner), internalErr
}

func TestToolValidateGatesEditorProposals(t *testing.T) {
	_, plan, _ := hookFixture(t)
	ctx := context.Background()

	_, err := plan.Plan(ctx, "edit_blocks", json.RawMessage(`{"editorId":"body","baseRevision":"3","ops":[{"op":"replace","block":"hero"},{"op":"remove","block":"hero"}]}`))
	var te chat.ToolError
	if !errors.As(err, &te) || !strings.Contains(te.Message, "block hero appears in more than one operation") {
		t.Fatalf("duplicate-block patch: err = %v", err)
	}

	draft, err := plan.Plan(ctx, "edit_blocks", json.RawMessage(`{"editorId":"body","baseRevision":"3","ops":[{"op":"replace","block":"hero"},{"op":"insert","block":"cta"}]}`))
	if err != nil {
		t.Fatalf("valid patch refused: %v", err)
	}
	want := "Edit blocks: replace hero, insert cta in body (applies in the editor after approval)"
	if draft.Summary != want || draft.Location != chat.ProposalLocationEditor || draft.Target != "body" {
		t.Fatalf("draft = %+v, want summary %q", draft, want)
	}
}

func TestToolValidateHidesInternalErrors(t *testing.T) {
	f, plan, handler := hookFixture(t)

	// Chat: refused before it becomes a proposal, with a generic reason.
	_, err := plan.Plan(context.Background(), "guarded_publish", json.RawMessage(`{}`))
	var te chat.ToolError
	if !errors.As(err, &te) || strings.Contains(te.Message, "hunter2") || !strings.Contains(te.Message, "refused") {
		t.Fatalf("chat refusal = %v", err)
	}

	// MCP: refused as invalid params, the handler never runs.
	var spec ToolSpec
	for _, s := range f.srv.cfg.CustomTools {
		if s.Name == "guarded_publish" {
			spec = s
		}
	}
	_, _, rpcErr := f.srv.callCustom(context.Background(), chatPrincipal(f.host, "good"), "http://test", "mcp", spec, map[string]any{}, 1)
	if rpcErr == nil || strings.Contains(rpcErr.Data.(string), "hunter2") || rpcErr.Data != "the arguments were refused" {
		t.Fatalf("mcp refusal = %+v", rpcErr)
	}
	if handler.calls() != 0 {
		t.Fatalf("handler ran %d times after a refusal", handler.calls())
	}
}

func TestCustomSummaryIsBounded(t *testing.T) {
	spec := &ToolSpec{Summarize: func(map[string]any) string { return "line one\n\tline two " + strings.Repeat("x", 400) }}
	got := customSummary(spec, nil)
	if strings.ContainsAny(got, "\n\t") || len([]rune(got)) != maxCustomSummaryRunes || !strings.HasPrefix(got, "line one line two x") {
		t.Fatalf("summary = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestToolPrepareAndWarnShapeTheProposal(t *testing.T) {
	f := newProposalsFixture(t)
	snapshot := map[string]any{"index.js": "v1"}
	f.srv.cfg.CustomTools = append(f.srv.cfg.CustomTools, ToolSpec{
		Name: "propose_plugin", Title: "Install plugin", Channels: "chat", Write: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"draft": map[string]any{"type": "string"}, "files": map[string]any{"type": "object"}},
			"required":   []string{"draft", "files"},
		},
		// The reviewer approves the draft's files as they are now.
		Prepare: func(_ context.Context, _ Principal, args map[string]any) (map[string]any, error) {
			if args["draft"] == "missing" {
				return nil, Invalid("draft missing does not exist")
			}
			out := map[string]any{"draft": args["draft"], "files": map[string]any{}}
			for k, v := range snapshot {
				out["files"].(map[string]any)[k] = v
			}
			return out, nil
		},
		Warn: func(_ context.Context, _ Principal, args map[string]any) []string {
			return []string{"  the plugin has no tests  ", "", strings.Repeat("w", 900)}
		},
		Handler: (&toolCounter{}).handler,
	})
	plan := f.exec(t, false).(planner)
	ctx := context.Background()

	draft, err := plan.Plan(ctx, "propose_plugin", json.RawMessage(`{"draft":"d1"}`))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// The required "files" was added by Prepare, before validation.
	var args map[string]any
	if err := json.Unmarshal(draft.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if files, _ := args["files"].(map[string]any); files["index.js"] != "v1" {
		t.Fatalf("stored arguments = %s, want the snapshot", draft.Arguments)
	}
	snapshot["index.js"] = "v2" // a later change to the draft doesn't touch the proposal
	if !strings.Contains(string(draft.Arguments), `"v1"`) {
		t.Fatal("the proposal changed with the draft")
	}
	if len(draft.Warnings) != 2 || draft.Warnings[0] != "the plugin has no tests" || len([]rune(draft.Warnings[1])) != maxToolWarningRunes+1 {
		t.Fatalf("warnings = %q", draft.Warnings)
	}

	_, err = plan.Plan(ctx, "propose_plugin", json.RawMessage(`{"draft":"missing"}`))
	var te chat.ToolError
	if !errors.As(err, &te) || !strings.Contains(te.Message, "draft missing does not exist") {
		t.Fatalf("Prepare refusal = %v", err)
	}
}

// A host handler's Refused becomes chat.RefusedError on an approved proposal
// (the proposal goes back to pending), and stays a plain error on MCP.
func TestRefusedHandlerOnApproval(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "send_campaign", Arguments: json.RawMessage(`{}`)}
	f := newProposalsFixture(t, proposalCallStep(call), textStep("waiting"))
	f.srv.cfg.CustomTools = append(f.srv.cfg.CustomTools, ToolSpec{
		Name: "send_campaign", Channels: "both", Write: true, InputSchema: map[string]any{"type": "object"},
		Handler: func(context.Context, CallContext, map[string]any) (any, error) {
			return nil, Refused("The subject line is required.")
		},
	})
	_, p := f.proposeTurn(t, call)
	_, err := f.exec(t, false).(chat.ProposalExecutor).Execute(context.Background(), p)
	var refused chat.RefusedError
	if !errors.As(err, &refused) || refused.Message != "The subject line is required." {
		t.Fatalf("Execute = %v, want chat.RefusedError", err)
	}
	// MCP sees the same message as a tool error.
	var spec ToolSpec
	for _, s := range f.srv.cfg.CustomTools {
		if s.Name == "send_campaign" {
			spec = s
		}
	}
	_, userErr, rpcErr := f.srv.callCustom(context.Background(), chatPrincipal(f.host, "good"), "http://test", "mcp", spec, map[string]any{}, 1)
	if rpcErr != nil || userErr != "The subject line is required." {
		t.Fatalf("mcp = %q %v", userErr, rpcErr)
	}
}
