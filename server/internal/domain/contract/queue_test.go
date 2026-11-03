package contract

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second},
		{1, 30 * time.Second},
		{2, 2 * time.Minute},
		{3, 2 * time.Minute},
	}
	for _, c := range cases {
		if got := RetryDelay(c.attempts); got != c.want {
			t.Errorf("RetryDelay(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

func TestCanRetry(t *testing.T) {
	for attempts, want := range map[int]bool{0: true, 1: true, 2: true, MaxAttempts: false, MaxAttempts + 1: false} {
		if got := CanRetry(attempts); got != want {
			t.Errorf("CanRetry(%d) = %v, want %v", attempts, got, want)
		}
	}
}

// A worker must be able to miss one heartbeat without losing its job.
func TestHeartbeatLeavesRoomInLease(t *testing.T) {
	if LeaseDuration < 2*HeartbeatInterval {
		t.Errorf("lease %v should be at least two heartbeats (%v)", LeaseDuration, 2*HeartbeatInterval)
	}
}
