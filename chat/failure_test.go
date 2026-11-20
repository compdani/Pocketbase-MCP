package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

type kindedErr string

func (e kindedErr) Error() string       { return string(e) }
func (e kindedErr) FailureKind() string { return string(e) }

func runOneStep(t *testing.T, step scripted.Step, cfg RunnerConfig) (Turn, *scripted.Provider) {
	t.Helper()
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "failure")
	prov := scripted.New(step)
	cfg.Store, cfg.Provider, cfg.Model = store, prov, "m"
	runner := mustRunner(t, cfg)
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return awaitTerminal(t, store, owner, turn.ID), prov
}

func TestRunnerFailsTruncatedFilteredAndEmptyReplies(t *testing.T) {
	cases := []struct {
		name string
		step scripted.Step
		code string
	}{
		{"openai length", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "half a rep"}, {Kind: provider.ChunkDone, StopReason: "length"}}}, FailureOutputLimit},
		{"anthropic max_tokens", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "half"}, {Kind: provider.ChunkDone, StopReason: "max_tokens"}}}, FailureOutputLimit},
		{"content filter", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkDone, StopReason: "content_filter"}}}, FailureContentFiltered},
		{"empty", scripted.Step{Chunks: []provider.Chunk{{Kind: provider.ChunkDone, StopReason: "stop"}}}, FailureEmptyReply},
		{"window full", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "part"}, {Kind: provider.ChunkDone, StopReason: "model_context_window_exceeded"}}}, FailureContextTooLong},
		{"usage limit", scripted.Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkError, Err: kindedErr(provider.KindUsageLimit)}}}, provider.KindUsageLimit},
		{"auth", scripted.Step{StreamErr: &provider.AuthError{Err: errors.New("refresh token revoked")}}, provider.KindAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			final, _ := runOneStep(t, tc.step, RunnerConfig{})
			if final.State != TurnStateFailed || final.Error == nil || final.Error.Code != tc.code {
				t.Fatalf("turn = %s %+v, want failed with %s", final.State, final.Error, tc.code)
			}
		})
	}
	// A normal stop still completes.
	final, _ := runOneStep(t, textStep("fine"), RunnerConfig{})
	if final.State != TurnStateCompleted {
		t.Fatalf("a normal reply: %s %+v", final.State, final.Error)
	}
}

func TestRunnerSendsParallelToolCalls(t *testing.T) {
	off := false
	_, prov := runOneStep(t, textStep("ok"), RunnerConfig{ParallelToolCalls: &off})
	if got := prov.Requests()[0].ParallelToolCalls; got == nil || *got {
		t.Fatalf("ParallelToolCalls = %v, want false", got)
	}
	on := true
	_, prov = runOneStep(t, textStep("ok"), RunnerConfig{ParallelToolCalls: &off, Setup: func(context.Context, TurnInput) (TurnSetup, error) {
		return TurnSetup{ParallelToolCalls: &on}, nil
	}})
	if got := prov.Requests()[0].ParallelToolCalls; got == nil || !*got {
		t.Fatalf("Setup's ParallelToolCalls = %v, want true", got)
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		status        int
		code, message string
		want          string
	}{
		{401, "invalid_api_key", "Incorrect API key", provider.KindAuth},
		{403, "", "forbidden", provider.KindAuth},
		{429, "insufficient_quota", "You exceeded your current quota", provider.KindUsageLimit},
		{429, "usage_limit_reached", "The usage limit has been reached", provider.KindUsageLimit},
		{429, "rate_limit_exceeded", "Rate limit reached", provider.KindRateLimited},
		{0, "rate_limit_exceeded", "", provider.KindRateLimited},
		{500, "", "server error", ""},
	}
	for _, tc := range cases {
		if got := provider.ClassifyFailure(tc.status, tc.code, tc.message); got != tc.want {
			t.Errorf("ClassifyFailure(%d, %q, %q) = %q, want %q", tc.status, tc.code, tc.message, got, tc.want)
		}
	}
}

func TestEmptyReplyStoresNoAssistantMessage(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	owner := ownerFixture()
	conv := mustConversation(t, store, owner, "blank")
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: scripted.New(textStep("  \n ")), Model: "m"})
	turn, _, err := runner.Submit(context.Background(), owner, conv.ID, "hi", "k", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if final := awaitTerminal(t, store, owner, turn.ID); final.Error == nil || final.Error.Code != FailureEmptyReply {
		t.Fatalf("turn = %+v", final)
	}
	for _, m := range drainMessages(t, store, owner, conv.ID) {
		if m.Role == RoleAssistant {
			t.Fatalf("a blank reply was stored: %+v", m)
		}
	}
}
