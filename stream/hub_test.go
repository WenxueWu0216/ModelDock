package stream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wenxuewu0216/modeldock/events"
)

func ev(seq int64, model string) events.Event {
	return events.Event{Seq: seq, ID: fmt.Sprint("e", seq), Type: events.TypeDeployReady, Model: model}
}

func recv(t *testing.T, s *Subscriber) events.Event {
	t.Helper()
	select {
	case e := <-s.C:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return events.Event{}
}

func TestHubFanOutAndFilter(t *testing.T) {
	h := NewHub(8, nil)
	all1, wm := h.Subscribe("")
	all2, _ := h.Subscribe("")
	iris, _ := h.Subscribe("iris")
	if wm != 0 {
		t.Fatalf("watermark = %d", wm)
	}
	h.Publish(ev(1, "iris"))
	h.Publish(ev(2, "wine"))
	for _, s := range []*Subscriber{all1, all2} {
		if a, b := recv(t, s), recv(t, s); a.Seq != 1 || b.Seq != 2 {
			t.Fatalf("got %d, %d", a.Seq, b.Seq)
		}
	}
	if e := recv(t, iris); e.Seq != 1 {
		t.Fatalf("iris got %d", e.Seq)
	}
	select {
	case e := <-iris.C:
		t.Fatalf("filtered subscriber got %+v", e)
	default:
	}
	if _, wm := h.Subscribe(""); wm != 2 {
		t.Fatalf("watermark after publish = %d, want 2", wm)
	}
}

func TestHubDropsSlowClientWithoutBlocking(t *testing.T) {
	m := NewMetrics(nil)
	h := NewHub(2, m)
	slow, _ := h.Subscribe("")
	fast, _ := h.Subscribe("")

	var got []int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for len(got) < 100 {
			got = append(got, (<-fast.C).Seq)
		}
	}()
	done := make(chan struct{})
	go func() {
		for i := int64(1); i <= 100; i++ {
			h.Publish(ev(i, "iris"))
			time.Sleep(50 * time.Microsecond) // let the fast reader keep up
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	wg.Wait()

	select {
	case <-slow.Done():
	default:
		t.Fatal("slow subscriber was not dropped")
	}
	if !h.Dropped(slow) {
		t.Fatal("Dropped(slow) = false")
	}
	if h.Dropped(fast) {
		t.Fatal("fast subscriber dropped")
	}
	for i, seq := range got {
		if seq != int64(i+1) {
			t.Fatalf("fast subscriber got %v", got)
		}
	}
	if n := testutil.ToFloat64(m.SlowDrops); n != 1 {
		t.Fatalf("slow drops = %v, want 1", n)
	}
	if h.Len() != 1 {
		t.Fatalf("subscribers = %d, want 1", h.Len())
	}
	// The slow subscriber's buffered events are still readable, in order.
	if a, b := <-slow.C, <-slow.C; a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("slow buffer = %d, %d", a.Seq, b.Seq)
	}
}

func TestHubUnsubscribeOnCancel(t *testing.T) {
	h := NewHub(4, nil)
	ctx, cancel := context.WithCancel(context.Background())
	s, _ := h.SubscribeContext(ctx, "")
	if h.Len() != 1 {
		t.Fatalf("subscribers = %d", h.Len())
	}
	cancel()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber not removed after cancel")
	}
	if h.Len() != 0 {
		t.Fatalf("subscribers = %d after cancel", h.Len())
	}
	h.Publish(ev(1, "iris")) // must not panic or deliver
	select {
	case e := <-s.C:
		t.Fatalf("got %+v after unsubscribe", e)
	default:
	}
	h.Unsubscribe(s) // idempotent
}

func TestHubClose(t *testing.T) {
	h := NewHub(4, nil)
	a, _ := h.Subscribe("")
	h.Close()
	<-a.Done()
	b, _ := h.Subscribe("")
	select {
	case <-b.Done():
	default:
		t.Fatal("subscribe after Close should be done immediately")
	}
}

func TestHubConcurrent(t *testing.T) {
	h := NewHub(16, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			s, _ := h.SubscribeContext(ctx, "")
			for j := 0; j < 10; j++ {
				select {
				case <-s.C:
				case <-time.After(time.Millisecond):
				}
			}
			cancel()
		}()
	}
	for i := int64(1); i <= 200; i++ {
		h.Publish(ev(i, "iris"))
	}
	wg.Wait()
}
