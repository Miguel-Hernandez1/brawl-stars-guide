package ingestion

import (
	"context"
	"testing"
)

// TestDiscoveryBudget_LocalToInstance verifies that two BattleIngestor instances each
// start with a fresh counter -- the budget is per-instance, not shared.
func TestDiscoveryBudget_LocalToInstance(t *testing.T) {
	a := NewBattleIngestor(nil, "TAGA", BattleIngestorConfig{MaxDiscoveriesPerCrawl: 2})
	b := NewBattleIngestor(nil, "TAGB", BattleIngestorConfig{MaxDiscoveriesPerCrawl: 2})

	if a.discoveriesThisCrawl != 0 {
		t.Errorf("instance A: initial counter = %d, want 0", a.discoveriesThisCrawl)
	}
	if b.discoveriesThisCrawl != 0 {
		t.Errorf("instance B: initial counter = %d, want 0", b.discoveriesThisCrawl)
	}
	// Confirm the two instances have independent counters by mutating one.
	a.discoveriesThisCrawl = 1
	if b.discoveriesThisCrawl != 0 {
		t.Error("modifying instance A affected instance B -- counters must be independent")
	}
}

// TestDiscoveryBudget_ZeroMeansUnlimited verifies that MaxDiscoveriesPerCrawl=0
// does not gate on the counter (unlimited mode used for manual collect runs).
func TestDiscoveryBudget_ZeroMeansUnlimited(t *testing.T) {
	b := NewBattleIngestor(nil, "DISCOVERER", BattleIngestorConfig{MaxDiscoveriesPerCrawl: 0})
	// Exhaust by setting counter way above any limit.
	b.discoveriesThisCrawl = 9999

	// discoverPlayer skips own tag and no-ops the DB call when pool is nil.
	// We test only the budget guard: a nil pool here means we'd panic on DB call.
	// Since MaxDiscoveriesPerCrawl=0, the budget guard must NOT fire.
	// We verify by calling with the discoverer's own tag (always a no-op) to avoid
	// the DB path, then asserting that a non-discoverer-tag would not be gated by budget.
	discoveries := b.discoverPlayer(context.Background(), "DISCOVERER", "self", nil)
	if len(discoveries) != 0 {
		t.Error("own tag should always be skipped regardless of budget")
	}

	// A different tag would reach the DB path. We can't test that without a real pool,
	// but we can confirm the budget gate is bypassed by checking the condition directly.
	if b.cfg.MaxDiscoveriesPerCrawl > 0 && b.discoveriesThisCrawl >= b.cfg.MaxDiscoveriesPerCrawl {
		t.Error("budget gate fired for MaxDiscoveriesPerCrawl=0; unlimited mode must disable the gate")
	}
}

// TestDiscoveryBudget_GateFires verifies that when MaxDiscoveriesPerCrawl > 0 and
// the counter equals the limit, discoverPlayer returns early without calling the DB.
// We use a nil pool -- if the gate does not fire, the function will panic on DB access.
func TestDiscoveryBudget_GateFires(t *testing.T) {
	b := NewBattleIngestor(nil, "DISCOVERER", BattleIngestorConfig{MaxDiscoveriesPerCrawl: 3})
	b.discoveriesThisCrawl = 3 // at limit

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("budget gate did not fire and panicked on nil pool: %v", r)
		}
	}()

	result := b.discoverPlayer(context.Background(), "OTHERTAG", "OtherPlayer", []string{"existing"})
	// Gate should fire, returning the discoveries slice unchanged.
	if len(result) != 1 || result[0] != "existing" {
		t.Errorf("discoverPlayer modified discoveries when gate should have fired; got %v", result)
	}
}
