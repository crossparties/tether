package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileOwnershipLeaseLockEnforcementHeartbeatAndExpiry(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "pkg", "worker.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(ws)
	eng.SetClockForTest(func() time.Time { return now })

	if err := eng.AcquireLeaseLock("crew-a", []string{"pkg/worker.go"}, time.Minute); err != nil {
		t.Fatalf("AcquireLeaseLock crew-a: %v", err)
	}

	// Holder crew-a can write; Main and crew-b are blocked; OpRead stays allowed.
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpWrite, "code", "crew-a"); !dec.Allowed {
		t.Fatalf("holder crew-a write denied: %s", dec.Reason)
	}
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpWrite, "code", "main"); dec.Allowed || !strings.Contains(dec.Reason, "lease-locked") {
		t.Fatalf("expected main write blocked by lease, got: %+v", dec)
	}
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpWrite, "code", "crew-b"); dec.Allowed {
		t.Fatalf("expected crew-b write blocked by lease")
	}
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpRead, "code", "crew-b"); !dec.Allowed {
		t.Fatalf("expected read to remain allowed during write lease: %s", dec.Reason)
	}

	// Acquiring overlapping lease for crew-b fails with ErrLeaseConflict.
	if err := eng.AcquireLeaseLock("crew-b", []string{"pkg/worker.go"}, time.Minute); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expected ErrLeaseConflict, got: %v", err)
	}

	// Heartbeat extends TTL beyond original expiry.
	now = now.Add(45 * time.Second)
	if err := eng.HeartbeatLease("crew-a", 2*time.Minute); err != nil {
		t.Fatalf("HeartbeatLease: %v", err)
	}
	now = now.Add(30 * time.Second) // 75s after start (past initial 60s, within heartbeat 165s)
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpWrite, "code", "main"); dec.Allowed {
		t.Fatalf("lease should still be active after HeartbeatLease")
	}

	// Advance past heartbeat expiry -> lease automatically expires and unlocks writes.
	now = now.Add(3 * time.Minute)
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpWrite, "code", "main"); !dec.Allowed {
		t.Fatalf("expected expired lease to allow write, got: %s", dec.Reason)
	}

	// Explicit ReleaseLeaseLock immediately unlocks active lease.
	if err := eng.AcquireLeaseLock("crew-b", []string{"pkg/worker.go"}, 5*time.Minute); err != nil {
		t.Fatalf("AcquireLeaseLock crew-b after expiry: %v", err)
	}
	if err := eng.ReleaseLeaseLock("crew-b"); err != nil {
		t.Fatalf("ReleaseLeaseLock: %v", err)
	}
	if dec := eng.AuthorizePathForActor("pkg/worker.go", OpWrite, "code", "main"); !dec.Allowed {
		t.Fatalf("expected write allowed after ReleaseLeaseLock, got: %s", dec.Reason)
	}
}
