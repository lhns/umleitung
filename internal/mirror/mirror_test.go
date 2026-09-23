package mirror

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	short := time.Second
	for _, tc := range []struct {
		name         string
		prev, uptime time.Duration
		want         time.Duration
	}{
		{"first failure", 0, short, initialBackoff},
		{"doubles", 4 * time.Second, short, 8 * time.Second},
		{"capped", 4 * time.Minute, short, maxBackoff},
		{"stays capped", maxBackoff, short, maxBackoff},
		// Regression: the delay never reset, so after enough transient
		// disconnects over the process lifetime every reconnect waited 5m.
		{"resets after healthy session", maxBackoff, maxBackoff, initialBackoff},
	} {
		if got := backoff(tc.prev, tc.uptime); got != tc.want {
			t.Errorf("%s: backoff(%v, %v) = %v, want %v", tc.name, tc.prev, tc.uptime, got, tc.want)
		}
	}
}
