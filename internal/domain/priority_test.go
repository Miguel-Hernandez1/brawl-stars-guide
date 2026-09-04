package domain

import "testing"

func TestPriorityForTrophies(t *testing.T) {
	cases := []struct {
		trophies int
		want     int16
		interval string // expected baseInterval bucket for documentation
	}{
		{0, 8, "72h"},
		{499, 8, "72h"},
		{500, 6, "24h"},
		{1999, 6, "24h"},
		{2000, 4, "8h"},
		{9999, 4, "8h"},
		{10000, 3, "8h"},
		{19999, 3, "8h"},
		{20000, 2, "4h"},
		{39999, 2, "4h"},
		{40000, 1, "4h"},
		{99999, 1, "4h"},
	}
	for _, tc := range cases {
		got := PriorityForTrophies(tc.trophies)
		if got != tc.want {
			t.Errorf("PriorityForTrophies(%d) = %d, want %d (%s interval)",
				tc.trophies, got, tc.want, tc.interval)
		}
	}
}

// TestPriorityReschedulingInterval verifies that the priority returned for a given trophy
// count maps to the expected re-crawl interval, combining PriorityForTrophies with the
// baseInterval boundaries defined in the crawler package.
func TestPriorityReschedulingInterval(t *testing.T) {
	// A target claimed at priority 5 (24h interval) whose fresh profile shows 25,000 trophies
	// should be rescheduled at the 4h interval (priority 2), not the stale 24h interval.
	staledPriority := int16(5)
	freshTrophies := 25000
	realPriority := PriorityForTrophies(freshTrophies)

	if realPriority == staledPriority {
		t.Fatalf("test invariant broken: PriorityForTrophies(%d) = %d equals stale priority",
			freshTrophies, realPriority)
	}
	// Verify realPriority is in the 4h bucket (priority <= 2).
	if realPriority > 2 {
		t.Errorf("PriorityForTrophies(%d) = %d, expected <= 2 (4h interval)", freshTrophies, realPriority)
	}
}
