package catalog

import (
	"context"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat"
)

func TestRouterTitleModel(t *testing.T) {
	ctx := context.Background()
	turnModel := chat.ResolvedModel{Choice: chat.ModelChoice{Provider: "turn"}, Model: "turn-model"}

	cases := []struct {
		option, want, wantModel, wantProvider string
		off                                   bool
	}{
		{option: "", want: chat.TitleChoiceOff, off: true},
		{option: "AUTO", want: chat.TitleChoiceAuto, wantModel: "turn-model", wantProvider: "turn"},
		{option: "off", want: chat.TitleChoiceOff, off: true},
		{option: "b::shared", want: "b::shared", wantModel: "shared", wantProvider: "b"},
		// A model one provider offers resolves through the default provider first.
		{option: "shared", want: "shared", wantModel: "shared", wantProvider: "a"},
		// Unavailable: fall back to the turn's model rather than stop titling.
		{option: "gone::model", want: "gone::model", wantModel: "turn-model", wantProvider: "turn"},
	}
	for _, tc := range cases {
		r, err := NewRouter(ctx, RouterOptions{Scope: testScope(), Static: staticProviders(), TitleChoice: tc.option})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.TitleChoice(); got != tc.want {
			t.Errorf("%q: TitleChoice = %q, want %q", tc.option, got, tc.want)
		}
		model, ok := r.TitleModel(ctx, turnModel)
		if ok == tc.off {
			t.Errorf("%q: ok = %v", tc.option, ok)
		}
		if !tc.off && (model.Model != tc.wantModel || model.Choice.Provider != tc.wantProvider) {
			t.Errorf("%q: title model = %s/%s, want %s/%s", tc.option, model.Choice.Provider, model.Model, tc.wantProvider, tc.wantModel)
		}
		r.Close()
	}
}

func TestAdminSetTitleChoiceOverridesTheOption(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	for _, p := range staticProviders() {
		if err := store.SaveProvider(ctx, testScope(), p); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := NewAdmin(RouterOptions{Scope: testScope(), Store: store, TitleChoice: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Router().Close()
	var _ chat.TitleAdmin = admin

	if got, _ := admin.TitleChoice(ctx); got != chat.TitleChoiceOff {
		t.Fatalf("initial = %q, want the option's off", got)
	}
	if err := admin.SetTitleChoice(ctx, "", "b", "dup"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if got, _ := admin.TitleChoice(ctx); got != "b::dup" {
		t.Fatalf("after set = %q", got)
	}
	model, ok := admin.Router().TitleModel(ctx, chat.ResolvedModel{Model: "turn"})
	if !ok || model.Model != "dup" || model.Choice.Provider != "b" {
		t.Fatalf("title model = %+v ok=%v", model, ok)
	}
	if err := admin.SetTitleChoice(ctx, "", "b", "missing"); err == nil {
		t.Fatal("an unavailable model was accepted")
	}
	if err := admin.SetTitleChoice(ctx, "sometimes", "", ""); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := admin.SetTitleChoice(ctx, "", "", ""); err == nil {
		t.Fatal("an empty choice was accepted")
	}
	if err := admin.SetTitleChoice(ctx, chat.TitleChoiceAuto, "", ""); err != nil {
		t.Fatalf("auto: %v", err)
	}
	if got, _ := admin.TitleChoice(ctx); got != chat.TitleChoiceAuto {
		t.Fatalf("after auto = %q", got)
	}
	if model, ok := admin.Router().TitleModel(ctx, chat.ResolvedModel{Model: "turn"}); !ok || model.Model != "turn" {
		t.Fatalf("auto title model = %+v ok=%v", model, ok)
	}
	if err := admin.SetTitleChoice(ctx, chat.TitleChoiceOff, "", ""); err != nil {
		t.Fatalf("off: %v", err)
	}
	if _, ok := admin.Router().TitleModel(ctx, chat.ResolvedModel{Model: "turn"}); ok {
		t.Fatal("titles still on after off")
	}
}
