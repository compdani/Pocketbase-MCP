// Package scripted provides a deterministic in-memory provider for tests.
//
// Each Stream call consumes the next Step of the script, so a test writes the
// exact model responses it wants to exercise, one per model turn. Every
// request is recorded, so a test can assert on the history and tools that were
// actually sent.
package scripted

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// Step is one scripted model response.
type Step struct {
	// Chunks are emitted in order. Include a provider.ChunkDone chunk to end the
	// response successfully; a step that ends without one is a truncated
	// stream, which consumers must treat as an error.
	Chunks []provider.Chunk
	// Delay is waited before each chunk, which lets a test race readers against
	// events being produced. Zero means no delay.
	Delay time.Duration
	// BlockUntilCancel makes the step emit its chunks and then wait for ctx
	// cancellation before closing the stream. Use it to test stop and timeout
	// paths.
	BlockUntilCancel bool
	// StreamErr, when set, makes Stream return this error immediately instead of
	// a channel.
	StreamErr error
}

// Provider is a scripted provider. It is safe for concurrent use.
type Provider struct {
	name string

	mu sync.Mutex
	// attachments is the set of MIME types SupportsAttachment accepts. Nil
	// means every type; a non-nil empty set means none.
	attachments map[string]struct{}
	steps       []Step
	next        int
	reqs        []provider.Request
}

// New returns a provider that serves the given steps in order.
func New(steps ...Step) *Provider {
	return &Provider{name: "scripted", steps: steps}
}

// NewNamed is New with a distinct Name, for tests that run several providers.
func NewNamed(name string, steps ...Step) *Provider {
	return &Provider{name: name, steps: steps}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return p.name }

// SupportsAttachment implements provider.AttachmentSupport. Unless a test
// restricts it, a scripted provider accepts every type, because its job is to
// record what it was handed rather than to model a vendor's limits.
func (p *Provider) SupportsAttachment(mimeType string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attachments == nil {
		return true
	}
	_, ok := p.attachments[mimeType]
	return ok
}

// SupportOnly restricts SupportsAttachment to the given MIME types. A call with
// no types makes the provider support no attachments at all, which is how a
// text-only backend is simulated.
func (p *Provider) SupportOnly(mimes ...string) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attachments = make(map[string]struct{}, len(mimes))
	for _, m := range mimes {
		p.attachments[m] = struct{}{}
	}
	return p
}

// Requests returns a copy of every request received so far, oldest first.
func (p *Provider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.Request, len(p.reqs))
	for i, r := range p.reqs {
		out[i] = cloneRequest(r)
	}
	return out
}

// Remaining reports how many unserved steps are left.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.steps) - p.next
}

// Stream implements provider.Provider by serving the next step.
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, cloneRequest(req))
	if p.next >= len(p.steps) {
		n := len(p.steps)
		p.mu.Unlock()
		return nil, fmt.Errorf("scripted: stream call %d past the end of the script of %d steps", n+1, n)
	}
	step := p.steps[p.next]
	p.next++
	p.mu.Unlock()

	if step.StreamErr != nil {
		return nil, step.StreamErr
	}

	ch := make(chan provider.Chunk)
	go func() {
		defer close(ch)
		for _, c := range step.Chunks {
			if step.Delay > 0 {
				timer := time.NewTimer(step.Delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			select {
			case ch <- cloneChunk(c):
			case <-ctx.Done():
				return
			}
		}
		if step.BlockUntilCancel {
			<-ctx.Done()
		}
	}()
	return ch, nil
}

func cloneRequest(r provider.Request) provider.Request {
	if r.Messages != nil {
		msgs := make([]provider.Message, len(r.Messages))
		for i, m := range r.Messages {
			msgs[i] = cloneMessage(m)
		}
		r.Messages = msgs
	}
	if r.Tools != nil {
		tools := make([]provider.ToolDef, len(r.Tools))
		for i, t := range r.Tools {
			schema := make(map[string]any, len(t.InputSchema))
			for k, v := range t.InputSchema {
				schema[k] = v
			}
			tools[i] = provider.ToolDef{Name: t.Name, Description: t.Description, InputSchema: schema}
		}
		r.Tools = tools
	}
	return r
}

func cloneMessage(m provider.Message) provider.Message {
	if m.ToolCalls != nil {
		calls := make([]provider.ToolCall, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			calls[i] = provider.ToolCall{ID: c.ID, Name: c.Name, Arguments: cloneRaw(c.Arguments)}
		}
		m.ToolCalls = calls
	}
	if m.Attachments != nil {
		atts := make([]provider.Attachment, len(m.Attachments))
		for i, a := range m.Attachments {
			atts[i] = provider.Attachment{Name: a.Name, MIME: a.MIME, Data: append([]byte(nil), a.Data...)}
		}
		m.Attachments = atts
	}
	m.ToolResult = cloneRaw(m.ToolResult)
	return m
}

func cloneChunk(c provider.Chunk) provider.Chunk {
	if c.ToolCall != nil {
		call := *c.ToolCall
		call.Arguments = cloneRaw(call.Arguments)
		c.ToolCall = &call
	}
	if c.Usage != nil {
		usage := *c.Usage
		c.Usage = &usage
	}
	return c
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
