package push

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	for _, n := range []int{10, 30, 100, 1000} {
		if got := Backoff(n); got != time.Hour {
			t.Errorf("Backoff(%d) = %v, want the 1h cap", n, got)
		}
	}
	if Backoff(0) != 30*time.Second || Backoff(-3) != 30*time.Second {
		t.Error("non-positive attempts should use the base delay")
	}
}
