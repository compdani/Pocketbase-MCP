package chat

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

type fakeChoiceRouter struct {
	choices   []ModelChoice
	providers map[string]provider.Provider
	fail      bool
}

func (r *fakeChoiceRouter) Choices(context.Context) ([]ModelChoice, error) {
	return append([]ModelChoice{}, r.choices...), nil
}

func (r *fakeChoiceRouter) Default(context.Context) (ModelChoice, error) {
	for _, choice := range r.choices {
		if choice.Default {
			return choice, nil
		}
	}
	if len(r.choices) > 0 {
		return r.choices[0], nil
	}
	return ModelChoice{}, ErrNoModelConfigured
}

func (r *fakeChoiceRouter) Resolve(_ context.Context, providerID, model string) (ResolvedModel, error) {
	if r.fail {
		return ResolvedModel{}, ErrModelUnknown
	}
	if providerID == "" && model == "" {
		def, err := r.Default(context.Background())
		if err != nil {
			return ResolvedModel{}, err
		}
		providerID, model = def.Provider, def.Model
	}
	for _, choice := range r.choices {
		if choice.Provider == providerID && (model == "" || choice.Model == model) {
			impl := r.providers[choice.ID]
			if impl == nil {
				return ResolvedModel{}, ErrModelUnknown
			}
			return ResolvedModel{Choice: choice, Provider: impl, Model: choice.Model}, nil
		}
	}
	return ResolvedModel{}, ErrModelUnknown
}

func (r *fakeChoiceRouter) SupportsAttachment(string) bool { return true }

func TestHTTPTurnSelectsProviderChoice(t *testing.T) {
	step := func(text string) *scripted.Provider {
		return scripted.New(scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: text},
			{Kind: provider.ChunkDone},
		}})
	}
	a, b := step("A answered"), step("B answered")
	router := &fakeChoiceRouter{
		choices: []ModelChoice{
			{ID: ChoiceID("a", "m1"), Provider: "a", Model: "m1", Label: "A", Default: true},
			{ID: ChoiceID("b", "m2"), Provider: "b", Model: "m2", Label: "B"},
		},
		providers: map[string]provider.Provider{ChoiceID("a", "m1"): a, ChoiceID("b", "m2"): b},
	}
	f := newFixture(t, fixtureOptions{router: router})
	conv := f.createConversation(nil)

	res := f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/turns", map[string]any{
		"text": "hello", "provider": "b", "model": "m2",
	}, nil).expect(http.StatusAccepted)
	var out submitTurnView
	res.decode(&out)

	turn := waitForTurnState(t, f.store, ownerFor("user-a"), out.Turn.ID, TurnStateCompleted)
	if turn.Provider != "b" || turn.Model != "m2" {
		t.Fatalf("turn target = %q/%q", turn.Provider, turn.Model)
	}
	detail := f.conversation(conv.ID)
	var text strings.Builder
	for _, message := range detail.Messages {
		for _, part := range message.Parts {
			if part.Type == PartText {
				text.WriteString(part.Text)
			}
		}
	}
	if !strings.Contains(text.String(), "B answered") {
		t.Fatalf("transcript = %q", text.String())
	}
}

func TestHTTPIdempotentRetryReturnsStoredTurnBeforeResolving(t *testing.T) {
	prov := scripted.New(scripted.Step{Chunks: []provider.Chunk{
		{Kind: provider.ChunkText, Text: "done"},
		{Kind: provider.ChunkDone},
	}})
	router := &fakeChoiceRouter{
		choices:   []ModelChoice{{ID: ChoiceID("a", "m1"), Provider: "a", Model: "m1", Default: true}},
		providers: map[string]provider.Provider{ChoiceID("a", "m1"): prov},
	}
	f := newFixture(t, fixtureOptions{router: router})
	conv := f.createConversation(nil)
	path := testPrefix + "/conversations/" + conv.ID + "/turns"
	body := map[string]any{"text": "hello", "provider": "a", "model": "m1"}
	hdr := map[string]string{"Idempotency-Key": "lost-response-1"}

	first := f.call(http.MethodPost, path, body, hdr).expect(http.StatusAccepted)
	var firstOut submitTurnView
	first.decode(&firstOut)
	waitForTurnState(t, f.store, ownerFor("user-a"), firstOut.Turn.ID, TurnStateCompleted)

	// A retry after the catalog changed must return the stored turn instead of
	// failing to resolve the old provider/model.
	router.fail = true
	second := f.call(http.MethodPost, path, body, hdr).expect(http.StatusAccepted)
	var secondOut submitTurnView
	second.decode(&secondOut)
	if secondOut.Created || secondOut.Turn.ID != firstOut.Turn.ID {
		t.Fatalf("retry = %+v", secondOut)
	}
}
