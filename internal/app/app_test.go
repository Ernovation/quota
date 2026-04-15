package app

import (
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()

	stateFile := filepath.Join(t.TempDir(), "state.json")
	a, err := New(Config{
		StateFile:            stateFile,
		SessionTTL:           24 * time.Hour,
		InitialAdminPassword: "admin",
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return a
}

func drainQuotaWake(a *App) {
	for {
		select {
		case <-a.quotaWake:
		default:
			return
		}
	}
}

func TestNextQuotaCheckDelayNoOnlineUsersTargetsMidnight(t *testing.T) {
	a := newTestApp(t)
	now := time.Now()
	expected := time.Until(nextLocalMidnight(now))

	delay := a.nextQuotaCheckDelay()

	if delay <= 0 {
		t.Fatalf("delay must be > 0, got %v", delay)
	}
	if delay > expected+2*time.Second {
		t.Fatalf("delay too large: got %v, expected around %v", delay, expected)
	}
	if delay < expected-3*time.Second {
		t.Fatalf("delay too small: got %v, expected around %v", delay, expected)
	}
}

func TestNextQuotaCheckDelayUsesEarliestUserDeadline(t *testing.T) {
	a := newTestApp(t)
	nowUnix := time.Now().Unix()

	a.mu.Lock()
	a.state.Users["user-fast"] = &User{
		Username:           "user-fast",
		Role:               RoleUser,
		WeeklyQuotaMinutes: [7]int{1, 1, 1, 1, 1, 1, 1}, // 60 seconds total per day
		UsedSeconds:        20,
		InternetOn:         true,
		LastOnUnix:         nowUnix - 10, // effective used = 30, remaining = 30s
	}
	a.state.Users["user-slow"] = &User{
		Username:           "user-slow",
		Role:               RoleUser,
		WeeklyQuotaMinutes: [7]int{5, 5, 5, 5, 5, 5, 5},
		UsedSeconds:        0,
		InternetOn:         true,
		LastOnUnix:         nowUnix,
	}
	a.mu.Unlock()

	delay := a.nextQuotaCheckDelay()

	if delay < 20*time.Second || delay > 35*time.Second {
		t.Fatalf("delay should track earliest deadline (~30s), got %v", delay)
	}
}

func TestNextQuotaCheckDelayReturnsImmediateForExhaustedUser(t *testing.T) {
	a := newTestApp(t)
	nowUnix := time.Now().Unix()

	a.mu.Lock()
	a.state.Users["exhausted"] = &User{
		Username:           "exhausted",
		Role:               RoleUser,
		WeeklyQuotaMinutes: [7]int{1, 1, 1, 1, 1, 1, 1},
		UsedSeconds:        60,
		InternetOn:         true,
		LastOnUnix:         nowUnix,
	}
	a.mu.Unlock()

	delay := a.nextQuotaCheckDelay()
	if delay != 0 {
		t.Fatalf("delay should be 0 for exhausted user, got %v", delay)
	}
}

func TestSignalQuotaSchedulerIsBufferedToOne(t *testing.T) {
	a := newTestApp(t)
	drainQuotaWake(a)

	a.signalQuotaScheduler()
	a.signalQuotaScheduler()
	a.signalQuotaScheduler()

	if got := len(a.quotaWake); got != 1 {
		t.Fatalf("quotaWake buffer size should cap at 1, got %d", got)
	}
}

func TestSyncUsageLockedUpdatesUsedAndTimestamp(t *testing.T) {
	a := newTestApp(t)
	nowUnix := time.Now().Unix()

	u := &User{
		Username:    "u",
		InternetOn:  true,
		UsedSeconds: 7,
		LastOnUnix:  nowUnix - 13,
	}

	a.syncUsageLocked(u, nowUnix)

	if u.UsedSeconds != 20 {
		t.Fatalf("UsedSeconds mismatch: got %d, want 20", u.UsedSeconds)
	}
	if u.LastOnUnix != nowUnix {
		t.Fatalf("LastOnUnix mismatch: got %d, want %d", u.LastOnUnix, nowUnix)
	}
}

func TestSetQuotaSignalsScheduler(t *testing.T) {
	a := newTestApp(t)
	drainQuotaWake(a)

	if err := a.setWeeklyQuota("admin", [7]int{1440, 1440, 1440, 1440, 1440, 1440, 1440}); err != nil {
		t.Fatalf("setWeeklyQuota error = %v", err)
	}

	if got := len(a.quotaWake); got != 1 {
		t.Fatalf("expected scheduler wake signal after setQuota, got %d", got)
	}
}
