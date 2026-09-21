package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func waitUntil(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() { return }
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}

func TestRecoveryRequeuesClaimedDeliveryExactlyOnce(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	s := NewService(root, func(context.Context, Trigger, Delivery) (string, error) {
		calls.Add(1)
		return "ok", nil
	})
	tr, err := s.Create("recovery", "webui", "chat", "webui:chat", nil)
	if err != nil { t.Fatal(err) }
	d, err := s.Enqueue(tr.ID, "recover me")
	if err != nil { t.Fatal(err) }

	entries, err := os.ReadDir(s.inbox)
	if err != nil || len(entries) != 1 { t.Fatalf("inbox entries=%d err=%v", len(entries), err) }
	src := filepath.Join(s.inbox, entries[0].Name())
	dst := filepath.Join(s.processing, entries[0].Name())
	if err := os.Rename(src, dst); err != nil { t.Fatal(err) }

	restarted := NewService(root, s.executor)
	if err := restarted.Start(); err != nil { t.Fatal(err) }
	t.Cleanup(func(){ ctx,cancel:=context.WithTimeout(context.Background(),time.Second); defer cancel(); _=restarted.Close(ctx) })
	// Wait for the DURABLE record, not for the executor call. The service runs the
	// executor first and only then writes runs/<id>.json, so waiting on the call
	// count and then sleeping a fixed 30ms races the write: under load the file is
	// still missing when the assertion runs. Polling the file is the signal that
	// the delivery actually finished.
	record := filepath.Join(restarted.runs, d.ID+".json")
	waitUntil(t, 2*time.Second, func() bool { _, err := os.Stat(record); return err == nil })
	// Give a duplicate delivery time to show up before asserting exactly-once.
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 { t.Fatalf("delivery %s executed %d times", d.ID, calls.Load()) }
}

func TestRetryPreservesDeliveryIdentityAndEventuallySucceeds(t *testing.T) {
	root := t.TempDir()
	var attempts atomic.Int32
	var firstID atomic.Value
	s := NewService(root, func(_ context.Context, _ Trigger, d Delivery) (string, error) {
		n := attempts.Add(1)
		if n == 1 { firstID.Store(d.ID) } else if got, _ := firstID.Load().(string); got != d.ID { t.Errorf("delivery id changed across retry: %q -> %q", got, d.ID) }
		if n < 3 { return "", errors.New("injected transient failure") }
		return "ok", nil
	})
	tr, err := s.Create("retry", "webui", "chat", "webui:chat", nil)
	if err != nil { t.Fatal(err) }
	d, err := s.Enqueue(tr.ID, "retry me")
	if err != nil { t.Fatal(err) }
	if err := s.Start(); err != nil { t.Fatal(err) }
	t.Cleanup(func(){ ctx,cancel:=context.WithTimeout(context.Background(),time.Second); defer cancel(); _=s.Close(ctx) })
	var record map[string]any
	waitUntil(t, 2*time.Second, func() bool {
		if attempts.Load() < 3 {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(s.runs, d.ID+".json"))
		if err != nil {
			return false
		}
		var current map[string]any
		if json.Unmarshal(raw, &current) != nil || current["status"] != "ok" {
			return false
		}
		record = current
		return true
	})
	if attempts.Load() != 3 { t.Fatalf("attempts=%d want 3", attempts.Load()) }
	if record["status"] != "ok" { t.Fatalf("final status=%v want ok", record["status"]) }
}

func TestMalformedInboxPayloadIsQuarantined(t *testing.T) {
	root := t.TempDir()
	s := NewService(root, nil)
	if err := s.ensureDirs(); err != nil { t.Fatal(err) }
	name := "000-bad.json"
	if err := os.WriteFile(filepath.Join(s.inbox, name), []byte("{not-json"), 0o600); err != nil { t.Fatal(err) }
	if _, _, ok := s.claimOne(); ok { t.Fatal("malformed payload was claimed") }
	if _, err := os.Stat(filepath.Join(s.failed, name)); err != nil { t.Fatalf("malformed payload not quarantined: %v", err) }
}
