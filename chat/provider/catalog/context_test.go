package catalog

import (
	"context"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat"
)

func TestChoicesCarryTheContextWindow(t *testing.T) {
	r, err := NewRouter(context.Background(), RouterOptions{Scope: testScope(), Static: []Provider{{
		ID: "a", Label: "A", Kind: KindOpenAI, Enabled: true, BaseURL: "https://example.test/v1",
		Models: []Model{
			{ID: "gpt-4o"},
			{ID: "local-model", ContextWindow: 32_000},
			{ID: "mystery"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	choices, err := r.Choices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"gpt-4o": 128_000, "local-model": 32_000, "mystery": 0}
	for _, c := range choices {
		if c.ContextWindow != want[c.Model] {
			t.Errorf("%s window = %d, want %d", c.Model, c.ContextWindow, want[c.Model])
		}
	}
	resolved, err := r.Resolve(context.Background(), "a", "local-model")
	if err != nil || resolved.Choice.ContextWindow != 32_000 {
		t.Fatalf("resolved = %+v err = %v", resolved.Choice, err)
	}
}

func TestNegativeContextWindowIsInvalid(t *testing.T) {
	p := Provider{ID: "a", Kind: KindOpenAI, Enabled: true, BaseURL: "https://example.test/v1",
		Models: []Model{{ID: "m", ContextWindow: -1}}}
	if err := p.Normalized().Validate(); err == nil {
		t.Fatal("a negative context window was accepted")
	}
}

func TestRouterCompactionModel(t *testing.T) {
	ctx := context.Background()
	turn := chat.ResolvedModel{Model: "turn"}
	for _, tc := range []struct {
		choice  string
		ok      bool
		model   string
		enabled bool
	}{
		{"", false, "", false},
		{"off", false, "", false},
		{"auto", true, "turn", true},
		{chat.ChoiceID("b", "dup"), true, "dup", true},
		{chat.ChoiceID("a", "gone"), true, "turn", true},
	} {
		r, err := NewRouter(ctx, RouterOptions{Scope: testScope(), Static: staticProviders(), CompactionChoice: tc.choice})
		if err != nil {
			t.Fatal(err)
		}
		model, ok := r.CompactionModel(ctx, turn)
		if ok != tc.ok || (ok && model.Model != tc.model) || r.CompactionEnabled() != tc.enabled {
			t.Errorf("choice %q: model=%q ok=%v enabled=%v, want %q %v %v", tc.choice, model.Model, ok, r.CompactionEnabled(), tc.model, tc.ok, tc.enabled)
		}
		r.Close()
	}
}
