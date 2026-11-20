package chat

import (
	"context"
	"encoding/json"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// filterTools narrows an executor to the tools allow accepts (TurnSetup.Tools).
// A nil allow or executor returns the executor unchanged. The wrapper keeps
// the executor's planner, so reviewed writes stay reviewed.
func filterTools(exec ToolExecutor, allow func(name string) bool) ToolExecutor {
	if exec == nil || allow == nil {
		return exec
	}
	f := &filteredTools{inner: exec, allow: allow}
	if planner, ok := exec.(ProposalPlanner); ok {
		return &filteredPlanner{filteredTools: f, planner: planner}
	}
	return f
}

type filteredTools struct {
	inner ToolExecutor
	allow func(name string) bool
}

func (f *filteredTools) Tools(ctx context.Context) ([]provider.ToolDef, error) {
	defs, err := f.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ToolDef, 0, len(defs))
	for _, d := range defs {
		if f.allow(d.Name) {
			out = append(out, d)
		}
	}
	return out, nil
}

// errToolHidden is what the model hears for a tool this turn doesn't offer.
var errToolHidden error = ToolError{Message: "This tool isn't available here."}

func (f *filteredTools) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if !f.allow(name) {
		return nil, errToolHidden
	}
	return f.inner.Call(ctx, name, args)
}

type filteredPlanner struct {
	*filteredTools
	planner ProposalPlanner
}

func (f *filteredPlanner) Plan(ctx context.Context, name string, args json.RawMessage) (*ProposalDraft, error) {
	if !f.allow(name) {
		return nil, errToolHidden
	}
	return f.planner.Plan(ctx, name, args)
}
