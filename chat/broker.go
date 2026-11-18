package chat

import (
	"context"
	"sync"
)

// brokerBufferSize bounds one live subscriber's backlog. A subscriber that
// falls further behind is disconnected instead of slowing the turn down; it
// reconnects with its last sequence and replays from the store.
const brokerBufferSize = 64

// broker is an in-process, per-turn notifier. It carries no durable state: the
// store is always the source of truth and every subscriber replays from it
// before following live events. It exists only to wake live followers without
// polling, which is why a single instance can serve the event stream and why
// multi-instance delivery is later work.
type broker struct {
	mu   sync.Mutex
	subs map[string]map[*brokerSub]struct{}
}

type brokerSub struct {
	ch     chan Event
	closed bool
}

func newBroker() *broker {
	return &broker{subs: make(map[string]map[*brokerSub]struct{})}
}

// add registers a subscriber for one turn key.
func (b *broker) add(key string) *brokerSub {
	sub := &brokerSub{ch: make(chan Event, brokerBufferSize)}
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.subs[key]
	if !ok {
		set = make(map[*brokerSub]struct{})
		b.subs[key] = set
	}
	set[sub] = struct{}{}
	return sub
}

// remove unregisters a subscriber and closes its channel exactly once.
func (b *broker) remove(key string, sub *brokerSub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeLocked(key, sub)
}

func (b *broker) removeLocked(key string, sub *brokerSub) {
	if set, ok := b.subs[key]; ok {
		delete(set, sub)
		if len(set) == 0 {
			delete(b.subs, key)
		}
	}
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
}

// publish delivers e to every live subscriber of key without ever blocking the
// caller. A subscriber whose buffer is full is disconnected: closing its
// channel ends its stream, and the next connection replays the gap.
func (b *broker) publish(key string, e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.subs[key]
	if !ok {
		return
	}
	for sub := range set {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			b.removeLocked(key, sub)
		}
	}
}

// Subscribe follows one turn's event stream, starting after the given sequence.
//
// The subscription to live notifications is registered before the store replay
// so an event appended in between cannot be lost; duplicates are dropped by
// sequence, so the caller sees every event exactly once and in order. The
// channel is closed after the terminal event, when ctx ends, or when a slow
// subscriber was disconnected. ErrNotFound and ErrResyncRequired from the
// store are returned directly, because the caller must reload canonical state
// instead of following the stream.
func (r *Runner) Subscribe(ctx context.Context, owner OwnerKey, turnID string, after int64) (<-chan Event, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	key := turnKey(owner, turnID)
	sub := r.broker.add(key)

	replay, err := r.cfg.Store.EventsAfter(ctx, owner, turnID, after)
	if err != nil {
		r.broker.remove(key, sub)
		return nil, err
	}
	if len(replay) == 0 {
		// Nothing to send and nothing more will be appended once the turn is
		// terminal, so close immediately rather than waiting forever for a
		// live event that cannot arrive.
		if turn, err := r.cfg.Store.GetTurn(ctx, owner, turnID); err == nil && turn.State.Terminal() {
			r.broker.remove(key, sub)
			closed := make(chan Event)
			close(closed)
			return closed, nil
		}
	}

	out := make(chan Event, 1)
	go r.pump(ctx, key, sub, out, after, replay)
	return out, nil
}

func (r *Runner) pump(ctx context.Context, key string, sub *brokerSub, out chan<- Event, after int64, replay []Event) {
	defer close(out)
	defer r.broker.remove(key, sub)

	last := after
	send := func(e Event) bool {
		if e.Seq <= last {
			return true
		}
		if e.Seq != last+1 {
			// A gap means part of the stream is unreachable; end the stream so
			// the caller reloads instead of rendering an incomplete turn.
			return false
		}
		select {
		case out <- e:
			last = e.Seq
			return !e.Type.Terminal()
		case <-ctx.Done():
			return false
		}
	}

	for _, e := range replay {
		if !send(e) {
			return
		}
	}
	for {
		select {
		case e, ok := <-sub.ch:
			if !ok {
				return
			}
			if !send(e) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
