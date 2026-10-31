package stream

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/wenxuewu0216/modeldock/events"
)

// fakeLog is an in-memory audit log whose rows can become visible out of
// sequence order, like concurrent Postgres transactions.
type fakeLog struct {
	mu   sync.Mutex
	rows map[int64]events.Event
}

func (f *fakeLog) commit(seqs ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[int64]events.Event{}
	}
	for _, s := range seqs {
		f.rows[s] = ev(s, "iris")
	}
}

func (f *fakeLog) ListAuditAfter(_ context.Context, model string, after, upto int64, limit int) ([]events.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []events.Event
	for s, e := range f.rows {
		if s > after && (upto <= 0 || s <= upto) && (model == "" || e.Model == model) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeLog) MaxAuditSeq(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m int64
	for s := range f.rows {
		if s > m {
			m = s
		}
	}
	return m, nil
}

func drain(s *Subscriber) []int64 {
	var out []int64
	for {
		select {
		case e := <-s.C:
			out = append(out, e.Seq)
		default:
			return out
		}
	}
}

func TestTailerStartsAtEndAndPublishesInOrder(t *testing.T) {
	ctx := context.Background()
	log := &fakeLog{}
	log.commit(1, 2, 3)
	h := NewHub(100, nil)
	tl := &Tailer{Source: log, Hub: h, BatchSize: 2}
	if err := tl.Init(ctx); err != nil {
		t.Fatal(err)
	}
	s, wm := h.Subscribe("")
	if wm != 3 {
		t.Fatalf("watermark = %d, want 3 (history is replayed, not re-published)", wm)
	}
	log.commit(4, 5, 6, 7, 8)
	if n, err := tl.PollOnce(ctx); err != nil || n != 5 {
		t.Fatalf("PollOnce = %d, %v", n, err)
	}
	if got := drain(s); len(got) != 5 || got[0] != 4 || got[4] != 8 {
		t.Fatalf("got %v", got)
	}
}

func TestTailerHoldsBackBehindGapUntilFilled(t *testing.T) {
	ctx := context.Background()
	log := &fakeLog{}
	h := NewHub(100, nil)
	now := time.Unix(0, 0)
	tl := &Tailer{Source: log, Hub: h, GapTimeout: 3 * time.Second, now: func() time.Time { return now }}
	if err := tl.Init(ctx); err != nil {
		t.Fatal(err)
	}
	tl.now = func() time.Time { return now }
	s, _ := h.Subscribe("")

	log.commit(1, 3, 4) // 2 is still in flight
	_, _ = tl.PollOnce(ctx)
	if got := drain(s); len(got) != 1 || got[0] != 1 {
		t.Fatalf("before gap filled: %v", got)
	}
	now = now.Add(time.Second)
	log.commit(2)
	_, _ = tl.PollOnce(ctx)
	if got := drain(s); len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Fatalf("after gap filled: %v", got)
	}
	if tl.Cursor() != 4 {
		t.Fatalf("cursor = %d", tl.Cursor())
	}
}

func TestTailerSkipsPermanentGapAfterTimeout(t *testing.T) {
	ctx := context.Background()
	log := &fakeLog{}
	h := NewHub(100, nil)
	now := time.Unix(0, 0)
	tl := &Tailer{Source: log, Hub: h, GapTimeout: 3 * time.Second}
	if err := tl.Init(ctx); err != nil {
		t.Fatal(err)
	}
	tl.now = func() time.Time { return now }
	s, _ := h.Subscribe("")

	log.commit(2, 3) // 1 was rolled back and never appears
	_, _ = tl.PollOnce(ctx)
	if got := drain(s); len(got) != 0 {
		t.Fatalf("published past an open gap: %v", got)
	}
	now = now.Add(2 * time.Second)
	_, _ = tl.PollOnce(ctx)
	if got := drain(s); len(got) != 0 {
		t.Fatalf("published before timeout: %v", got)
	}
	now = now.Add(2 * time.Second)
	_, _ = tl.PollOnce(ctx)
	if got := drain(s); len(got) != 2 || got[0] != 2 {
		t.Fatalf("after timeout: %v", got)
	}
}
