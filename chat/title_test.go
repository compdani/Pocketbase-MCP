package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// titleProvider answers title requests (recognised by TitleSystemPrompt) with
// title and every other request with a short reply. release, when set, holds
// the title reply until it is closed.
type titleProvider struct {
	title   string
	fail    bool
	release chan struct{}

	mu       sync.Mutex
	requests []provider.Request
}

func (p *titleProvider) Name() string { return "title-test" }

func (p *titleProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	ch := make(chan provider.Chunk, 4)
	go func() {
		defer close(ch)
		if req.System != TitleSystemPrompt {
			ch <- provider.Chunk{Kind: provider.ChunkText, Text: "reply"}
			ch <- provider.Chunk{Kind: provider.ChunkDone}
			return
		}
		if p.release != nil {
			select {
			case <-p.release:
			case <-ctx.Done():
				ch <- provider.Chunk{Kind: provider.ChunkError, Err: ctx.Err()}
				return
			}
		}
		if p.fail {
			ch <- provider.Chunk{Kind: provider.ChunkError, Err: errors.New("title refused")}
			return
		}
		ch <- provider.Chunk{Kind: provider.ChunkText, Text: p.title}
		ch <- provider.Chunk{Kind: provider.ChunkDone}
	}()
	return ch, nil
}

func (p *titleProvider) titleRequests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []provider.Request
	for _, r := range p.requests {
		if r.System == TitleSystemPrompt {
			out = append(out, r)
		}
	}
	return out
}

func conversationTitle(t *testing.T, s Store, owner OwnerKey, id string) string {
	t.Helper()
	c, err := s.GetConversation(context.Background(), owner, id)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	return c.Title
}

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		"Accepted contracts closing soon":         "Accepted contracts closing soon",
		"  \"Contracts closing in 14 days.\"  ":   "Contracts closing in 14 days",
		"Title: Overdue tasks":                    "Overdue tasks",
		"## **Open deals summary**\nHere is why…": "Open deals summary",
		"\n\n“Latest inquiries”\n":                "Latest inquiries",
		"Tabs\tand   spaces":                      "Tabs and spaces",
		"":                                        "",
		"A very long title that goes on and on well past the sixty character limit": "A very long title that goes on and on well past the sixty…",
	}
	for in, want := range cases {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunnerTitlesAnUntitledConversation(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := &titleProvider{title: "\"Contracts closing soon.\""}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Titles: &TitleConfig{}})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "")

	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "Which accepted contracts close in the next 14 days?", "k1", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	// The completed turn waited for the title, so it is there at the terminal event.
	if got := conversationTitle(t, store, owner, conv.ID); got != "Contracts closing soon" {
		t.Fatalf("title = %q, want the cleaned generated title", got)
	}
	reqs := prov.titleRequests()
	if len(reqs) != 1 || len(reqs[0].Tools) != 0 || reqs[0].Model != "m" {
		t.Fatalf("title requests = %+v, want one tool-free request on the turn's model", reqs)
	}
	if reqs[0].Messages[0].Text != "Which accepted contracts close in the next 14 days?" {
		t.Fatalf("title input = %q", reqs[0].Messages[0].Text)
	}

	// A titled conversation is never titled again.
	turn2, _, err := runner.Submit(context.Background(), owner, conv.ID, "And the week after?", "k2", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit 2: %v", err)
	}
	awaitTerminal(t, store, owner, turn2.ID)
	if n := len(prov.titleRequests()); n != 1 {
		t.Fatalf("title requests after a titled turn = %d, want 1", n)
	}
}

func TestRunnerTitleOffAndSkipped(t *testing.T) {
	for name, titles := range map[string]*TitleConfig{
		"nil":      nil,
		"disabled": {Disabled: true},
		"model says off": {Model: func(context.Context, ResolvedModel) (ResolvedModel, bool) {
			return ResolvedModel{}, false
		}},
	} {
		t.Run(name, func(t *testing.T) {
			store := NewMemStore(MemStoreOptions{})
			prov := &titleProvider{title: "Never"}
			runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Titles: titles})
			owner := ownerFixture()
			conv := mustConversation(t, store, owner, "")
			turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hello", "k", nil, time.Time{})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			awaitTerminal(t, store, owner, turn.ID)
			if got := conversationTitle(t, store, owner, conv.ID); got != "" {
				t.Fatalf("title = %q, want none", got)
			}
			if n := len(prov.titleRequests()); n != 0 {
				t.Fatalf("title requests = %d, want 0", n)
			}
		})
	}
}

func TestRunnerTitleUsesTheConfiguredModelAndLowestEffort(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	turnProv := &titleProvider{title: "wrong provider"}
	titleProv := &titleProvider{title: "Quick title"}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: turnProv, Model: "big", Titles: &TitleConfig{
		Model: func(_ context.Context, turn ResolvedModel) (ResolvedModel, bool) {
			if turn.Model != "big" {
				t.Errorf("title picker got turn model %q", turn.Model)
			}
			return ResolvedModel{
				Choice:   ModelChoice{Provider: "fast", Model: "small", Efforts: []string{"high", "minimal", "low"}},
				Provider: titleProv,
				Model:    "small",
			}, true
		},
	}})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "")
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hello", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	if got := conversationTitle(t, store, owner, conv.ID); got != "Quick title" {
		t.Fatalf("title = %q", got)
	}
	reqs := titleProv.titleRequests()
	if len(reqs) != 1 || reqs[0].Model != "small" || reqs[0].ReasoningEffort != "minimal" {
		t.Fatalf("title request = %+v, want model small with effort minimal", reqs)
	}
	if n := len(turnProv.titleRequests()); n != 0 {
		t.Fatalf("the turn's provider got %d title requests", n)
	}
}

func TestRunnerTitleNeverOverwritesARename(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	release := make(chan struct{})
	prov := &titleProvider{title: "Generated", release: release}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Titles: &TitleConfig{Wait: -1}})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "")
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hello", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	if _, err := store.RenameConversation(context.Background(), owner, conv.ID, "Mine"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	close(release)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := runner.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := conversationTitle(t, store, owner, conv.ID); got != "Mine" {
		t.Fatalf("title = %q, want the user's rename", got)
	}
}

func TestRunnerTitleFailureLeavesTheTurnAlone(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	prov := &titleProvider{fail: true}
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Titles: &TitleConfig{}})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "")
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hello", "k", nil, time.Time{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if final := awaitTerminal(t, store, owner, turn.ID); final.State != TurnStateCompleted {
		t.Fatalf("state = %s, want completed", final.State)
	}
	if got := conversationTitle(t, store, owner, conv.ID); got != "" {
		t.Fatalf("title = %q, want none after a failure", got)
	}
}

func TestFirstMessageTitlesNeedNoModel(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "")
	prov := scripted.New(textStep("sure"))
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: prov, Model: "m", Titles: &TitleConfig{FromMessage: true}})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "  Draft the **spring** newsletter for VIP customers please, with a coupon\nand more", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	awaitTerminal(t, store, owner, turn.ID)
	got, err := store.GetConversation(context.Background(), owner, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title == "" || strings.Contains(got.Title, "\n") || len([]rune(got.Title)) > 61 {
		t.Fatalf("title = %q", got.Title)
	}
	if n := len(prov.Requests()); n != 1 {
		t.Fatalf("model requests = %d, want only the turn's", n)
	}
}
