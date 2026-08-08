// Package bus is in-process pub/sub for the live event stream.
//
// The design constraint that shapes everything here: a slow subscriber must
// never be able to slow down a publisher. Events originate in a browser that is
// mid-page-load; if an operator leaves an SSE tab open on a laptop that goes to
// sleep, the queue behind it would grow until the hub is killed. So delivery is
// bounded and lossy, and loss is reported rather than hidden.
package bus

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

// DefaultBuffer is the per-subscriber queue depth.
//
// Big enough to absorb a burst of mutations from one page load, small enough
// that a thousand abandoned subscriptions cost megabytes rather than gigabytes.
const DefaultBuffer = 256

type subscriber struct {
	ch chan core.Event

	// sessionID is "" for a subscriber that wants every session.
	sessionID string

	// dropped counts events discarded since the last drop marker was
	// delivered. Kept atomic because it is written from every publisher.
	dropped atomic.Int64
}

// Bus fans events out to subscribers.
type Bus struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}
	closed bool
}

// New returns an empty bus.
func New() *Bus {
	return &Bus{subs: make(map[*subscriber]struct{})}
}

// Publish delivers e to every matching subscriber. It never blocks.
func (b *Bus) Publish(e core.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for s := range b.subs {
		if s.sessionID != "" && s.sessionID != e.SessionID {
			continue
		}
		deliver(s, e)
	}
}

// deliver enqueues one event, accounting for anything previously dropped.
//
// The drop marker is what makes lossy delivery honest: a consumer building a
// picture of a session is told its picture has a hole, rather than quietly
// receiving a shorter story than actually happened.
func deliver(s *subscriber, e core.Event) {
	if n := s.dropped.Load(); n > 0 {
		body, _ := json.Marshal(map[string]any{
			"dropped": n,
			"reason":  "subscriber fell behind; events were discarded rather than queued without bound",
		})
		marker := core.Event{
			SessionID: s.sessionID,
			Type:      core.EventError,
			Body:      body,
			At:        time.Now().UTC(),
		}
		select {
		case s.ch <- marker:
			s.dropped.Add(-n)
		default:
			// Still full. Count this event too and give up until there is room.
			s.dropped.Add(1)
			return
		}
	}

	select {
	case s.ch <- e:
	default:
		s.dropped.Add(1)
	}
}

// Subscribe returns a channel of events for one session and a cancel function.
//
// The cancel function must be called, or the subscription leaks and the bus
// keeps delivering into a channel nobody reads. Every caller in this codebase
// defers it.
func (b *Bus) Subscribe(sessionID string, buffer int) (<-chan core.Event, func()) {
	return b.subscribe(sessionID, buffer)
}

// SubscribeAll returns a feed of every session's events, for the console's
// global activity view.
func (b *Bus) SubscribeAll(buffer int) (<-chan core.Event, func()) {
	return b.subscribe("", buffer)
}

func (b *Bus) subscribe(sessionID string, buffer int) (<-chan core.Event, func()) {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	s := &subscriber{ch: make(chan core.Event, buffer), sessionID: sessionID}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// Removal and close happen under the write lock, while Publish
			// holds the read lock. That is what makes closing the channel safe:
			// no publisher can be inside deliver for this subscriber.
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.ch)
		})
	}
	return s.ch, cancel
}

// Close shuts the bus down and closes every subscriber channel.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
}

// Stats reports subscriber count and total drops, for the console.
func (b *Bus) Stats() (subscribers int, dropped int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		subscribers++
		dropped += s.dropped.Load()
	}
	return subscribers, dropped
}

// Event is a small helper for building an event with a JSON body.
func Event(sessionID, agentID, typ string, body any) core.Event {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"error": fmt.Sprintf("unserializable event body: %v", err)})
		}
		raw = b
	}
	return core.Event{
		SessionID: sessionID,
		AgentID:   agentID,
		Type:      typ,
		Body:      raw,
		At:        time.Now().UTC(),
	}
}
