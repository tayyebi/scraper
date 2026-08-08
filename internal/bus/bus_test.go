package bus

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/tayyebi/scraper/internal/core"
)

func recv(t *testing.T, ch <-chan core.Event) core.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while waiting for an event")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
		return core.Event{}
	}
}

func TestFanOut(t *testing.T) {
	b := New()
	defer b.Close()

	a, cancelA := b.Subscribe("s_1", 8)
	defer cancelA()
	c, cancelC := b.Subscribe("s_1", 8)
	defer cancelC()

	b.Publish(core.Event{SessionID: "s_1", Type: core.EventNavigated})

	if got := recv(t, a); got.Type != core.EventNavigated {
		t.Errorf("subscriber A got %q", got.Type)
	}
	if got := recv(t, c); got.Type != core.EventNavigated {
		t.Errorf("subscriber C got %q", got.Type)
	}
}

func TestSessionScoping(t *testing.T) {
	b := New()
	defer b.Close()

	mine, cancel := b.Subscribe("s_1", 8)
	defer cancel()

	b.Publish(core.Event{SessionID: "s_2", Type: core.EventNavigated})
	b.Publish(core.Event{SessionID: "s_1", Type: core.EventConsole})

	got := recv(t, mine)
	if got.SessionID != "s_1" || got.Type != core.EventConsole {
		t.Errorf("got %+v: a session subscriber received another session's events", got)
	}
}

func TestSubscribeAllSeesEverySession(t *testing.T) {
	b := New()
	defer b.Close()

	all, cancel := b.SubscribeAll(8)
	defer cancel()

	b.Publish(core.Event{SessionID: "s_1", Type: core.EventNavigated})
	b.Publish(core.Event{SessionID: "s_2", Type: core.EventNavigated})

	seen := map[string]bool{}
	seen[recv(t, all).SessionID] = true
	seen[recv(t, all).SessionID] = true
	if !seen["s_1"] || !seen["s_2"] {
		t.Errorf("SubscribeAll saw %v, want both sessions", seen)
	}
}

// The property this package exists for: a subscriber that stops reading must
// not be able to block a publisher. Events come from a browser mid-page-load,
// and an operator's sleeping laptop must not be able to stall it.
func TestSlowSubscriberNeverBlocksPublisher(t *testing.T) {
	b := New()
	defer b.Close()

	_, cancel := b.Subscribe("s_1", 2)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for range 10_000 {
			b.Publish(core.Event{SessionID: "s_1", Type: core.EventMirrorMutation})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
}

// Loss must be visible. A consumer building a picture of a session is told its
// picture has a hole rather than quietly receiving a shorter story.
func TestDropsAreReportedToTheSubscriber(t *testing.T) {
	b := New()
	defer b.Close()

	ch, cancel := b.Subscribe("s_1", 2)
	defer cancel()

	for range 50 {
		b.Publish(core.Event{SessionID: "s_1", Type: core.EventMirrorMutation})
	}

	// Drain what fits, then let the marker through.
	<-ch
	<-ch

	b.Publish(core.Event{SessionID: "s_1", Type: core.EventNavigated})

	var marker core.Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type == core.EventError {
				marker = e
			}
		case <-deadline:
			t.Fatal("no drop marker was ever delivered")
		}
		if marker.Type == core.EventError {
			break
		}
	}

	var body struct {
		Dropped int64  `json:"dropped"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(marker.Body, &body); err != nil {
		t.Fatalf("drop marker body: %v", err)
	}
	if body.Dropped <= 0 {
		t.Errorf("drop marker reported %d drops, want a positive count", body.Dropped)
	}
	if body.Reason == "" {
		t.Error("drop marker gave no reason")
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	b := New()
	defer b.Close()

	ch, cancel := b.Subscribe("s_1", 4)
	cancel()

	b.Publish(core.Event{SessionID: "s_1", Type: core.EventNavigated})

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("a cancelled subscriber still received an event")
		}
	case <-time.After(time.Second):
		t.Error("a cancelled subscription's channel was never closed")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	b := New()
	defer b.Close()

	_, cancel := b.Subscribe("s_1", 4)
	cancel()
	cancel() // must not panic on a double close
	cancel()
}

func TestCloseClosesEverySubscriber(t *testing.T) {
	b := New()

	a, cancelA := b.Subscribe("s_1", 4)
	defer cancelA()
	c, cancelC := b.SubscribeAll(4)
	defer cancelC()

	b.Close()

	for name, ch := range map[string]<-chan core.Event{"session": a, "all": c} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("%s subscriber channel was not closed", name)
			}
		case <-time.After(time.Second):
			t.Errorf("%s subscriber channel was never closed", name)
		}
	}

	// Publishing after Close must be a no-op rather than a panic.
	b.Publish(core.Event{SessionID: "s_1", Type: core.EventNavigated})
	b.Close()
}

func TestSubscribeAfterCloseYieldsAClosedChannel(t *testing.T) {
	b := New()
	b.Close()

	ch, cancel := b.Subscribe("s_1", 4)
	defer cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("subscribing to a closed bus produced a live channel")
		}
	case <-time.After(time.Second):
		t.Error("subscribing to a closed bus produced a channel that never closes")
	}
}

// Publish, Subscribe and cancel all race by nature; the read/write lock split
// is the mechanism, and this is the check that it holds under -race.
func TestConcurrentPublishAndSubscribe(t *testing.T) {
	b := New()
	defer b.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(core.Event{SessionID: "s_1", Type: core.EventMirrorMutation})
				}
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ch, cancel := b.Subscribe("s_1", 2)
					<-time.After(time.Millisecond)
					select {
					case <-ch:
					default:
					}
					cancel()
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestStats(t *testing.T) {
	b := New()
	defer b.Close()

	_, cancel1 := b.Subscribe("s_1", 1)
	defer cancel1()
	_, cancel2 := b.SubscribeAll(1)
	defer cancel2()

	subs, _ := b.Stats()
	if subs != 2 {
		t.Errorf("Stats reported %d subscribers, want 2", subs)
	}

	for range 100 {
		b.Publish(core.Event{SessionID: "s_1", Type: core.EventMirrorMutation})
	}
	if _, dropped := b.Stats(); dropped == 0 {
		t.Error("Stats reported no drops after overflowing a 1-deep buffer")
	}
}

func TestEventHelper(t *testing.T) {
	e := Event("s_1", "a_1", core.EventNavigated, map[string]string{"url": "https://example.test"})
	if e.SessionID != "s_1" || e.AgentID != "a_1" || e.Type != core.EventNavigated {
		t.Errorf("Event built %+v", e)
	}
	if e.At.IsZero() {
		t.Error("Event left the timestamp unset")
	}
	var body map[string]string
	if err := json.Unmarshal(e.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["url"] != "https://example.test" {
		t.Errorf("body = %v", body)
	}

	if got := Event("s_1", "a_1", core.EventConsole, nil); got.Body != nil {
		t.Errorf("nil body encoded as %s, want nil", got.Body)
	}
}
