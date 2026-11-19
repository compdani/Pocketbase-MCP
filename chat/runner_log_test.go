package chat

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

// syncBuffer is a bytes.Buffer safe for the runner's worker goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A failed turn records only the generic failure, so the operator log is the
// one place the provider's own reason appears.
func TestRunnerLogsProviderFailureCause(t *testing.T) {
	cases := map[string]scripted.Step{
		"stream error chunk": {Chunks: []provider.Chunk{{
			Kind: provider.ChunkError,
			Err:  errors.New("openai: http 401 (invalid_request_error/invalid_api_key): Incorrect API key provided"),
		}}},
		"stream refused": {StreamErr: errors.New("openai: http 401 (invalid_request_error/invalid_api_key): Incorrect API key provided")},
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			var logs syncBuffer
			store := NewMemStore(MemStoreOptions{})
			runner := mustRunner(t, RunnerConfig{
				Store:    store,
				Provider: scripted.New(step),
				Model:    "test-model",
				Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
			})
			owner := ownerFixture()
			conv := mustConversation(t, store, owner, "fail")
			turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			final := awaitTerminal(t, store, owner, turn.ID)
			if final.State != TurnStateFailed || final.Error == nil || final.Error.Code != FailureProvider {
				t.Fatalf("turn = %+v, want failed with %s", final, FailureProvider)
			}
			if strings.Contains(final.Error.Message, "API key") {
				t.Fatalf("provider text leaked into the turn: %q", final.Error.Message)
			}

			out := logs.String()
			for _, want := range []string{"chat: turn failed", "code=provider_error", "model=test-model", "turn=" + turn.ID, "invalid_api_key"} {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %q:\n%s", want, out)
				}
			}
			assertNoSecret(t, "Incorrect API key", mustEvents(t, store, owner, turn.ID), drainMessages(t, store, owner, conv.ID), nil)
		})
	}
}
