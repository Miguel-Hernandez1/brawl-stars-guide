package crawler

import (
	"testing"
	"time"
)

func TestBaseInterval(t *testing.T) {
	cases := []struct {
		priority int16
		want     time.Duration
	}{
		{1, 4 * time.Hour},
		{2, 4 * time.Hour},
		{3, 8 * time.Hour},
		{4, 8 * time.Hour},
		{5, 24 * time.Hour},
		{6, 24 * time.Hour},
		{7, 72 * time.Hour},
		{10, 72 * time.Hour},
	}
	for _, tc := range cases {
		got := baseInterval(tc.priority)
		if got != tc.want {
			t.Errorf("baseInterval(%d) = %v, want %v", tc.priority, got, tc.want)
		}
	}
}

func TestBackoffDuration(t *testing.T) {
	base := 24 * time.Hour
	maxBackoff := 7 * 24 * time.Hour

	cases := []struct {
		n    int32
		want time.Duration
	}{
		{0, 24 * time.Hour},         // 2^0 * 24h = 24h
		{1, 48 * time.Hour},         // 2^1 * 24h = 48h
		{2, 96 * time.Hour},         // 2^2 * 24h = 96h (4 days)
		{3, 7 * 24 * time.Hour},     // 2^3 * 24h = 192h, capped at 7 days
		{100, 7 * 24 * time.Hour},   // huge n, overflow guard applies
	}
	for _, tc := range cases {
		got := backoffDuration(base, tc.n)
		if got != tc.want {
			t.Errorf("backoffDuration(%v, n=%d) = %v, want %v", base, tc.n, got, tc.want)
		}
		if got > maxBackoff {
			t.Errorf("backoffDuration(%v, n=%d) = %v exceeds max %v", base, tc.n, got, maxBackoff)
		}
	}
}

func TestBackoffNeverExceedsMax(t *testing.T) {
	for _, base := range []time.Duration{time.Hour, 24 * time.Hour, 72 * time.Hour} {
		for _, n := range []int32{0, 1, 5, 10, 30, 62, 63, 100} {
			d := backoffDuration(base, n)
			if d > 7*24*time.Hour {
				t.Errorf("backoffDuration(%v, %d) = %v exceeds 7 days", base, n, d)
			}
			if d <= 0 {
				t.Errorf("backoffDuration(%v, %d) = %v is non-positive", base, n, d)
			}
		}
	}
}
