package pocketbasemcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat"
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
