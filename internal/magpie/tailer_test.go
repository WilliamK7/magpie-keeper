package magpie

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

func setupRoutingDir(t *testing.T) (magpieDir, routingDir string) {
	t.Helper()
	magpieDir = filepath.Join(t.TempDir(), "magpie dir")
	routingDir = filepath.Join(magpieDir, "data", "routing")
	if err := os.MkdirAll(routingDir, 0o755); err != nil {
		t.Fatalf("mkdir routing dir: %v", err)
	}
	return magpieDir, routingDir
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func receiveAlert(t *testing.T, ch <-chan domain.RouteAlert) domain.RouteAlert {
	t.Helper()
	select {
	case a, ok := <-ch:
		if !ok {
			t.Fatal("alert channel closed unexpectedly")
		}
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RouteAlert")
		return domain.RouteAlert{}
	}
}

func assertNoAlert(t *testing.T, ch <-chan domain.RouteAlert, wait time.Duration) {
	t.Helper()
	select {
	case a, ok := <-ch:
		if ok {
			t.Fatalf("unexpected alert received: %+v", a)
		}
	case <-time.After(wait):
	}
}

func TestTailerClassificationsPartialLinesAndRotation(t *testing.T) {
	magpieDir, routingDir := setupRoutingDir(t)
	logFile := filepath.Join(routingDir, "2026-11-20.jsonl")

	// Include a non-trial provider try (ignored) and a free-quota-exhausted trial try.
	appendFile(t, logFile,
		`{"ts":"2026-11-20T12:00:00Z","model":"qwen-plus","tries":[{"provider":"openrouter","status":429,"error":"rate limited"},{"provider":"transcribe-bailian-trial-111111111111","status":403,"error":"Free quota exhausted for model"}]}`+"\n",
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tailer := NewTailer(magpieDir, WithPollInterval(20*time.Millisecond))
	ch, err := tailer.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	a1 := receiveAlert(t, ch)
	if a1.ProviderID != "transcribe-bailian-trial-111111111111" || a1.Model != "qwen-plus" || a1.Kind != domain.RouteAlertQuotaExhausted {
		t.Fatalf("unexpected first alert: %+v", a1)
	}

	// Write a partial line without trailing newline: must NOT emit or advance offset.
	appendFile(t, logFile, `{"tries":[{"provider":"transcribe-bailian-trial-222222222222","model":"qwen-turbo","error":"The free tier of this model is exhaust`)
	assertNoAlert(t, ch, 80*time.Millisecond)

	// Finish the partial line with newline.
	appendFile(t, logFile, `ed."}]}`+"\n")
	a2 := receiveAlert(t, ch)
	if a2.ProviderID != "transcribe-bailian-trial-222222222222" || a2.Model != "qwen-turbo" || a2.Kind != domain.RouteAlertQuotaExhausted {
		t.Fatalf("unexpected second alert after newline completion: %+v", a2)
	}

	// Append account credit and permission denied alerts (including top-level fallback without tries[]).
	appendFile(t, logFile,
		`{"tries":[{"member":"transcribe-bailian-trial-333333333333/qwen-max","fail":"credit","status":402,"error":"insufficient balance"}]}`+"\n"+
			`{"provider":"transcribe-bailian-trial-444444444444","model":"qwen-flash","status":401,"error":"invalid api key"}`+"\n",
	)
	a3 := receiveAlert(t, ch)
	if a3.Kind != domain.RouteAlertAccountCredit || a3.ProviderID != "transcribe-bailian-trial-333333333333" || a3.Model != "qwen-max" {
		t.Fatalf("unexpected credit alert: %+v", a3)
	}
	a4 := receiveAlert(t, ch)
	if a4.Kind != domain.RouteAlertPermissionDenied || a4.ProviderID != "transcribe-bailian-trial-444444444444" || a4.Status != 401 {
		t.Fatalf("unexpected permission alert: %+v", a4)
	}

	// Simulate file truncation/rotation: overwrite file with a smaller entry.
	if err := os.WriteFile(logFile, []byte(`{"provider":"transcribe-bailian-trial-555555555555","model":"qwen-long","status":403,"error":"forbidden"}`+"\n"), 0o600); err != nil {
		t.Fatalf("truncate/rotate log: %v", err)
	}
	a5 := receiveAlert(t, ch)
	if a5.Kind != domain.RouteAlertPermissionDenied || a5.ProviderID != "transcribe-bailian-trial-555555555555" || a5.Status != 403 {
		t.Fatalf("unexpected alert after truncation/rotation: %+v", a5)
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to close after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func TestTailerStartAtEOFAndCustomPrefix(t *testing.T) {
	magpieDir, routingDir := setupRoutingDir(t)
	existingFile := filepath.Join(routingDir, "existing.jsonl")
	appendFile(t, existingFile, `{"provider":"custom-trial-old","model":"m-old","fail":"credit"}`+"\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tailer := NewTailer(
		magpieDir,
		WithPollInterval(20*time.Millisecond),
		WithStartAtEOF(true),
		WithTrialPrefix("custom-trial-"),
	)
	ch, err := tailer.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Pre-existing record must be skipped.
	assertNoAlert(t, ch, 80*time.Millisecond)

	// New line appended to existing file must be emitted.
	appendFile(t, existingFile, `{"provider":"custom-trial-new","model":"m-new","status":403}`+"\n")
	a1 := receiveAlert(t, ch)
	if a1.ProviderID != "custom-trial-new" || a1.Kind != domain.RouteAlertPermissionDenied {
		t.Fatalf("unexpected alert on appended line: %+v", a1)
	}

	// Brand-new file created after Start must be read from byte 0 even when StartAtEOF is true.
	newFile := filepath.Join(routingDir, "next-day.jsonl")
	appendFile(t, newFile, `{"provider":"custom-trial-fresh","model":"m-fresh","error":"FREE QUOTA EXHAUSTED"}`+"\n")
	a2 := receiveAlert(t, ch)
	if a2.ProviderID != "custom-trial-fresh" || a2.Kind != domain.RouteAlertQuotaExhausted {
		t.Fatalf("unexpected alert on newly created file: %+v", a2)
	}
}

func TestTailerStartsBeforeRoutingDirExists(t *testing.T) {
	magpieDir := filepath.Join(t.TempDir(), "magpie dir")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tailer := NewTailer(magpieDir, WithPollInterval(20*time.Millisecond))
	ch, err := tailer.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed when routing dir missing: %v", err)
	}

	routingDir := filepath.Join(magpieDir, "data", "routing")
	if err := os.MkdirAll(routingDir, 0o755); err != nil {
		t.Fatalf("mkdir routing dir: %v", err)
	}
	appendFile(t, filepath.Join(routingDir, "late.jsonl"), `{"provider":"transcribe-bailian-trial-999999999999","model":"qwen-plus","fail":"credit"}`+"\n")

	a := receiveAlert(t, ch)
	if a.ProviderID != "transcribe-bailian-trial-999999999999" || a.Kind != domain.RouteAlertAccountCredit {
		t.Fatalf("unexpected alert after late directory creation: %+v", a)
	}
}
