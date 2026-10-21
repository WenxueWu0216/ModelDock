package events_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/service"
	"github.com/wenxuewu0216/modeldock/store"
	"github.com/wenxuewu0216/modeldock/store/storetest"
)

// filePublisher is a durable stand-in for the broker that survives the
// publishing process being SIGKILLed: every acknowledged message is an
// fsynced JSON line. kill, if set, kills the process right after the
// broker has acknowledged, i.e. before the outbox rows are marked.
type filePublisher struct {
	path string
	kill bool
}

func (p filePublisher) Publish(_ context.Context, msgs []events.Message) error {
	f, err := os.OpenFile(p.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	f.Close()
	if p.kill {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // never returns
	}
	return nil
}

func (filePublisher) Close() error { return nil }

func readBroker(t *testing.T, path string) []events.Message {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []events.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m events.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

var deployer = domain.User{ID: "dana", Role: domain.RoleDeployer}

// TestCrashHelper is the child process body. It does nothing unless run by
// TestOutboxSurvivesCrash.
func TestCrashHelper(t *testing.T) {
	mode := os.Getenv("CRASH_MODE")
	if mode == "" {
		t.Skip("helper process only")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, os.Getenv("CRASH_DB"))
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(s, nil, nil, false)
	switch mode {
	case "mid_tx":
		// Killed after writing the deployment and its event, before COMMIT.
		_ = s.InTx(ctx, func(tx *store.Tx) error {
			m := domain.Model{Name: "iris", CreatedBy: "dana"}
			if err := tx.CreateModel(ctx, &m); err != nil {
				return err
			}
			ev := events.New(events.TypeModelRegistered, "dana", nil)
			ev.Model = "iris"
			if err := tx.AppendEvent(ctx, &ev); err != nil {
				return err
			}
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		})
	case "after_commit", "after_publish":
		if _, err := svc.CreateModel(ctx, deployer, "iris", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RegisterVersion(ctx, deployer, "iris", domain.ModelVersion{Version: "1", Image: "img:1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DeployToStaging(ctx, deployer, "iris", "1"); err != nil {
			t.Fatal(err)
		}
		if mode == "after_commit" {
			// Three state changes are committed; the relay never ran.
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
		relay := &events.Relay{Source: s, Publisher: filePublisher{path: os.Getenv("CRASH_BROKER"), kill: true}}
		_, _ = relay.PublishOnce(ctx) // killed inside, after broker ack
	}
	t.Fatal("helper should have been killed")
}

func runCrashChild(t *testing.T, mode, dbURL, broker string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CRASH_MODE="+mode, "CRASH_DB="+dbURL, "CRASH_BROKER="+broker)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("child exited cleanly or failed to start: %v\n%s", err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("child was not SIGKILLed: %v\n%s", err, out)
	}
}

func TestOutboxSurvivesCrash(t *testing.T) {
	cases := []struct {
		mode string
		// eventsWanted is how many audit events must exist after restart.
		eventsWanted int
		// publishedBeforeRestart is how many outbox rows the broker had
		// already acknowledged when the process died.
		publishedBeforeRestart int
	}{
		// Killed mid-transaction: neither the model nor its event exist.
		{"mid_tx", 0, 0},
		// Killed between DB commit and publish: nothing lost, published on restart.
		{"after_commit", 3, 0},
		// Killed after the broker acked but before rows were marked: the
		// restarted relay republishes (at-least-once), consumers dedupe.
		{"after_publish", 3, 3},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			dbURL := storetest.URL(t)
			broker := filepath.Join(t.TempDir(), "broker.jsonl")
			runCrashChild(t, c.mode, dbURL, broker)

			ctx := context.Background()
			s, err := store.Open(ctx, dbURL)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			audit, err := s.ListAudit(ctx, "", 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(audit) != c.eventsWanted {
				t.Fatalf("audit events after crash = %d, want %d", len(audit), c.eventsWanted)
			}
			models, _ := s.ListModels(ctx)
			if (len(models) == 1) != (c.eventsWanted > 0) {
				t.Fatalf("state and audit diverged: %d models, %d events", len(models), len(audit))
			}
			before := readBroker(t, broker)
			// Each outbox row is published to its domain topic + the audit topic.
			if len(before) != 2*c.publishedBeforeRestart {
				t.Fatalf("broker had %d messages before restart, want %d", len(before), 2*c.publishedBeforeRestart)
			}
			backlog, _, err := s.OutboxBacklog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if backlog != c.eventsWanted {
				t.Fatalf("backlog after crash = %d, want %d (rows must stay unpublished)", backlog, c.eventsWanted)
			}

			// "Restart": a fresh relay drains the outbox.
			relay := &events.Relay{Source: s, Publisher: filePublisher{path: broker}}
			if _, err := relay.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if n, _, _ := s.OutboxBacklog(ctx); n != 0 {
				t.Fatalf("backlog after restart = %d", n)
			}

			// No lost events: every audit event reached both topics.
			got := readBroker(t, broker)
			perTopic := map[string]map[string]int{}
			for _, m := range got {
				if perTopic[m.Topic] == nil {
					perTopic[m.Topic] = map[string]int{}
				}
				perTopic[m.Topic][m.EventID]++
			}
			for _, ev := range audit {
				if perTopic[events.AuditTopic][ev.ID] == 0 || perTopic[ev.Topic()][ev.ID] == 0 {
					t.Errorf("event %s (%s) lost", ev.ID, ev.Type)
				}
			}
			if c.mode == "after_publish" && len(got) != 2*len(before) {
				t.Errorf("expected every pre-crash message to be redelivered once: %d -> %d", len(before), len(got))
			}

			// The consumer side turns at-least-once into exactly-once effects.
			for _, m := range got {
				if m.Topic != events.AuditTopic {
					continue
				}
				var ev events.Event
				if err := json.Unmarshal(m.Value, &ev); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ApplyDashboardEvent(ctx, ev); err != nil {
					t.Fatal(err)
				}
			}
			dash, err := s.ListDashboard(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if c.eventsWanted > 0 {
				if len(dash) != 1 || dash[0].LastDeployStatus != "pending" || dash[0].LastDeployVersion != "1" {
					t.Fatalf("dashboard = %+v", dash)
				}
			}
		})
	}
}

func TestRelayBrokerOutageRetries(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	svc := service.New(s, nil, nil, false)
	if _, err := svc.CreateModel(ctx, deployer, "iris", ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "2", "3"} {
		if _, err := svc.RegisterVersion(ctx, deployer, "iris", domain.ModelVersion{Version: v, Image: "img:" + v}); err != nil {
			t.Fatal(err)
		}
	}
	pub := &events.MemoryPublisher{FailNext: 2}
	relay := &events.Relay{Source: s, Publisher: pub, BatchSize: 2}
	for i := 0; i < 2; i++ {
		if _, err := relay.PublishOnce(ctx); err == nil {
			t.Fatal("expected simulated broker failure")
		}
	}
	if n, _, _ := s.OutboxBacklog(ctx); n != 4 {
		t.Fatalf("backlog = %d, want 4 after failed publishes", n)
	}
	n, err := relay.Drain(ctx)
	if err != nil || n != 4 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	// Per-model order is preserved on the domain topic.
	evs, err := pub.Events(events.TopicRegistry)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "1", "2", "3"}
	for i, ev := range evs {
		if ev.Version != want[i] {
			t.Fatalf("event %d version %q, want %q (order broken)", i, ev.Version, want[i])
		}
	}
}
