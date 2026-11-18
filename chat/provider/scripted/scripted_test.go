package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

func collect(t *testing.T, ch <-chan provider.Chunk) []provider.Chunk {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var out []provider.Chunk
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-deadline.C:
			t.Fatalf("stream did not close, saw %d chunks", len(out))
		}
	}
}

func TestStepsAreServedInOrder(t *testing.T) {
	p := New(
		Step{Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: "one"}, {Kind: provider.ChunkDone}}},
		Step{Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: "two"}, {Kind: provider.ChunkDone}}},
	)
	if p.Name() != "scripted" {
		t.Fatalf("Name = %q", p.Name())
	}

	first, err := p.Stream(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: "a"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := collect(t, first)
	if len(got) != 2 || got[0].Text != "one" || got[1].Kind != provider.ChunkDone {
		t.Fatalf("first step chunks = %+v", got)
	}

	second, err := p.Stream(context.Background(), provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	if got := collect(t, second); len(got) != 2 || got[0].Text != "two" {
		t.Fatalf("second step chunks = %+v", got)
	}
	if p.Remaining() != 0 {
		t.Fatalf("remaining = %d", p.Remaining())
	}

	if _, err := p.Stream(context.Background(), provider.Request{}); err == nil {
		t.Fatal("stream past the script accepted")
	}
	if _, err := p.Stream(context.Background(), provider.Request{}); err == nil {
		t.Fatal("repeated stream past the script accepted")
	}
	if reqs := p.Requests(); len(reqs) != 4 {
		t.Fatalf("recorded %d requests, want 4 (including exhausted ones)", len(reqs))
	}
}

func TestRequestsAreCopied(t *testing.T) {
	p := New(Step{Chunks: []provider.Chunk{{Kind: provider.ChunkDone}}})
	args := json.RawMessage(`{"q":"x"}`)
	req := provider.Request{
		Model: "m",
		Messages: []provider.Message{{
			Role: provider.RoleAssistant,
			ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "search", Arguments: args},
			},
		}},
		Tools: []provider.ToolDef{{Name: "search", InputSchema: map[string]any{"type": "object"}}},
	}
	if _, err := p.Stream(context.Background(), req); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Mutating the caller's request after Stream must not change the record.
	req.Messages[0].ToolCalls[0].Name = "hijacked"
	args[2] = 'X'
	req.Tools[0].InputSchema["type"] = "string"

	got := p.Requests()
	if len(got) != 1 {
		t.Fatalf("recorded %d requests", len(got))
	}
	call := got[0].Messages[0].ToolCalls[0]
	if call.Name != "search" {
		t.Fatalf("recorded call mutated: %+v", call)
	}
	if string(call.Arguments) != `{"q":"x"}` {
		t.Fatalf("recorded arguments mutated: %s", call.Arguments)
	}
	if got[0].Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("recorded schema mutated: %+v", got[0].Tools[0])
	}

	// Mutating a returned copy must not change the record either.
	got[0].Messages[0].ToolCalls[0].ID = "changed"
	got[0].Tools[0].InputSchema["type"] = "changed"
	again := p.Requests()
	if again[0].Messages[0].ToolCalls[0].ID != "c1" || again[0].Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("record exposed internal state: %+v", again[0])
	}
}

func TestStreamErrorStep(t *testing.T) {
	want := errors.New("no credential")
	p := New(Step{StreamErr: want})
	if _, err := p.Stream(context.Background(), provider.Request{Model: "m"}); !errors.Is(err, want) {
		t.Fatalf("Stream = %v, want %v", err, want)
	}
	if len(p.Requests()) != 1 {
		t.Fatal("failed call was not recorded")
	}
	if _, err := p.Stream(context.Background(), provider.Request{}); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("step was not consumed by the failed call: %v", err)
	}
}

func TestBlockUntilCancel(t *testing.T) {
	p := New(Step{
		Chunks:           []provider.Chunk{{Kind: provider.ChunkText, Text: "hi"}},
		BlockUntilCancel: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	select {
	case chunk := <-ch:
		if chunk.Text != "hi" {
			t.Fatalf("chunk = %+v", chunk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chunk never arrived")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range ch {
		}
	}()
	select {
	case <-closed:
		t.Fatal("stream closed before cancellation")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not close the stream")
	}
}

func TestCancellationDuringDelay(t *testing.T) {
	p := New(Step{
		Chunks: []provider.Chunk{{Kind: provider.ChunkText, Text: "late"}, {Kind: provider.ChunkDone}},
		Delay:  time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("chunk delivered after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close promptly on cancellation")
	}
}

func TestConcurrentStreamCalls(t *testing.T) {
	const n = 10
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Step{Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: string(rune('a' + i))},
			{Kind: provider.ChunkDone},
		}}
	}
	p := New(steps...)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := p.Stream(context.Background(), provider.Request{Model: "m"})
			if err != nil {
				t.Errorf("Stream: %v", err)
				return
			}
			if got := collect(t, ch); len(got) != 2 {
				t.Errorf("chunks = %+v", got)
			}
		}()
	}
	wg.Wait()
	if reqs := p.Requests(); len(reqs) != n {
		t.Fatalf("recorded %d requests, want %d", len(reqs), n)
	}
	if p.Remaining() != 0 {
		t.Fatalf("remaining = %d", p.Remaining())
	}
}

func TestNewNamed(t *testing.T) {
	p := NewNamed("fake", Step{Chunks: []provider.Chunk{{Kind: provider.ChunkDone}}})
	if p.Name() != "fake" {
		t.Fatalf("Name = %q", p.Name())
	}
}

func TestAttachmentsAreRecordedAndCopied(t *testing.T) {
	p := New(Step{Chunks: []provider.Chunk{{Kind: provider.ChunkDone}}})
	data := []byte("hello")
	req := provider.Request{Model: "m", Messages: []provider.Message{{
		Role:        provider.RoleUser,
		Text:        "look",
		Attachments: []provider.Attachment{{Name: "a.txt", MIME: "text/plain", Data: data}},
	}}}
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, ch)

	// Mutating the caller's bytes must not change what was recorded.
	data[0] = 'X'
	reqs := p.Requests()
	if len(reqs) != 1 || len(reqs[0].Messages) != 1 || len(reqs[0].Messages[0].Attachments) != 1 {
		t.Fatalf("recorded requests = %+v", reqs)
	}
	got := reqs[0].Messages[0].Attachments[0]
	if got.Name != "a.txt" || got.MIME != "text/plain" || string(got.Data) != "hello" {
		t.Fatalf("recorded attachment = %+v", got)
	}

	// A caller mutating the returned copy must not reach the recording.
	got.Data[0] = 'Y'
	again := p.Requests()[0].Messages[0].Attachments[0]
	if string(again.Data) != "hello" {
		t.Fatalf("Requests aliases stored attachments: %q", again.Data)
	}
}

func TestSupportsAttachmentIsConfigurable(t *testing.T) {
	p := New()
	for _, mime := range provider.AttachmentMIMETypes() {
		if !p.SupportsAttachment(mime) {
			t.Errorf("default SupportsAttachment(%q) = false", mime)
		}
	}

	p.SupportOnly("text/plain")
	if !p.SupportsAttachment("text/plain") {
		t.Error("text/plain unsupported after SupportOnly")
	}
	if p.SupportsAttachment("image/png") {
		t.Error("image/png supported after SupportOnly(text/plain)")
	}

	p.SupportOnly()
	if p.SupportsAttachment("text/plain") {
		t.Error("SupportOnly() still supports text/plain")
	}
}
