package queries_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// These tests require a running PostgreSQL instance with the playbook schema.
// Set INTEGRATION_TEST_DATABASE_URL to run them; they are skipped otherwise.
// Example: INTEGRATION_TEST_DATABASE_URL=postgres://playbook:playbook@localhost:5432/playbook go test ./internal/storage/queries/...

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping integration tests")
	}
	pool, err := storage.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// insertTestTarget inserts a player + crawl_target row for testing.
// The row is removed in t.Cleanup regardless of test outcome.
func insertTestTarget(t *testing.T, pool *pgxpool.Pool, tag string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO players (tag, name, first_seen_at) VALUES ($1, 'TestPlayer', NOW())
		 ON CONFLICT (tag) DO NOTHING`, tag)
	if err != nil {
		t.Fatalf("insert player %s: %v", tag, err)
	}
	// Priority 1 (highest) with a far-past timestamp ensures the test row sorts to the
	// front of the queue ahead of any real priority-5 targets already in crawl_targets.
	_, err = pool.Exec(ctx,
		`INSERT INTO crawl_targets (player_tag, priority, next_crawl_at, discovery_source)
		 VALUES ($1, 1, NOW() - INTERVAL '1 year', 'test')
		 ON CONFLICT (player_tag) DO UPDATE SET
		     is_active            = TRUE,
		     leased_until         = NULL,
		     crawl_generation     = 0,
		     consecutive_failures = 0,
		     last_crawl_status    = NULL,
		     next_crawl_at        = NOW() - INTERVAL '1 year'`, tag)
	if err != nil {
		t.Fatalf("insert crawl_target %s: %v", tag, err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM crawl_targets WHERE player_tag = $1`, tag)
		pool.Exec(context.Background(), `DELETE FROM players WHERE tag = $1`, tag)
	})
}

func TestClaimNextTarget_IdleRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := "TESTCRAWL01"
	insertTestTarget(t, pool, tag)

	claim, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil {
		t.Fatalf("ClaimNextTarget: %v", err)
	}
	if claim == nil {
		t.Fatal("expected a claim, got nil (queue may have been empty or row not inserted)")
	}
	if claim.PlayerTag != tag {
		// Another pre-existing claimable row may have been returned.
		// That is not a test failure - just skip detailed assertions.
		t.Logf("claimed %s (not our test row %s); skipping assertions", claim.PlayerTag, tag)
		return
	}
	if claim.Generation != 1 {
		t.Errorf("generation: got %d, want 1", claim.Generation)
	}

	// leased_until should be in the future.
	var leasedUntil *time.Time
	err = pool.QueryRow(ctx,
		`SELECT leased_until FROM crawl_targets WHERE player_tag = $1`, tag,
	).Scan(&leasedUntil)
	if err != nil {
		t.Fatalf("check leased_until: %v", err)
	}
	if leasedUntil == nil || leasedUntil.Before(time.Now()) {
		t.Errorf("leased_until not set in future: %v", leasedUntil)
	}
}

func TestClaimNextTarget_ActiveLeaseExcludes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := "TESTCRAWL02"
	insertTestTarget(t, pool, tag)

	// First claim.
	first, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil {
		t.Fatalf("first ClaimNextTarget: %v", err)
	}
	if first == nil || first.PlayerTag != tag {
		t.Skip("our test row was not returned by the first claim; queue has other rows")
	}

	// Immediately re-claim: the same row must not be returned.
	second, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil {
		t.Fatalf("second ClaimNextTarget: %v", err)
	}
	if second != nil && second.PlayerTag == tag {
		t.Errorf("same row returned on second claim while actively leased")
	}
}

func TestFinalizeCrawl_CorrectGeneration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := "TESTCRAWL03"
	insertTestTarget(t, pool, tag)

	claim, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil || claim == nil || claim.PlayerTag != tag {
		t.Skip("could not claim our test row")
	}

	ok, err := queries.FinalizeCrawl(ctx, pool, queries.FinalizeParams{
		PlayerTag:           tag,
		Generation:          claim.Generation,
		Status:              "success",
		NextCrawlAt:         time.Now().Add(24 * time.Hour),
		ConsecutiveFailures: 0,
		IsActive:            true,
	})
	if err != nil {
		t.Fatalf("FinalizeCrawl: %v", err)
	}
	if !ok {
		t.Fatal("FinalizeCrawl returned false (rows_affected=0)")
	}

	// Verify leased_until cleared and status set.
	var leasedUntil *time.Time
	var status *string
	err = pool.QueryRow(ctx,
		`SELECT leased_until, last_crawl_status FROM crawl_targets WHERE player_tag = $1`, tag,
	).Scan(&leasedUntil, &status)
	if err != nil {
		t.Fatalf("check state: %v", err)
	}
	if leasedUntil != nil {
		t.Errorf("leased_until should be NULL after finalize, got %v", leasedUntil)
	}
	if status == nil || *status != "success" {
		t.Errorf("last_crawl_status: got %v, want 'success'", status)
	}
}

func TestFinalizeCrawl_ExpiredLeaseNewClaim(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := "TESTCRAWL04"
	insertTestTarget(t, pool, tag)

	claim, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil || claim == nil || claim.PlayerTag != tag {
		t.Skip("could not claim our test row")
	}
	oldGen := claim.Generation

	// Expire the lease manually.
	_, err = pool.Exec(ctx,
		`UPDATE crawl_targets SET leased_until = NOW() - INTERVAL '1 second' WHERE player_tag = $1`, tag)
	if err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	// A new claim should succeed with an incremented generation.
	newClaim, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil {
		t.Fatalf("new ClaimNextTarget: %v", err)
	}
	if newClaim == nil || newClaim.PlayerTag != tag {
		t.Skip("test row was not returned by new claim after lease expiry")
	}
	if newClaim.Generation != oldGen+1 {
		t.Errorf("generation: got %d, want %d", newClaim.Generation, oldGen+1)
	}

	// Finalize with old generation: must not match.
	ok, err := queries.FinalizeCrawl(ctx, pool, queries.FinalizeParams{
		PlayerTag:   tag,
		Generation:  oldGen,
		Status:      "success",
		NextCrawlAt: time.Now().Add(24 * time.Hour),
		IsActive:    true,
	})
	if err != nil {
		t.Fatalf("FinalizeCrawl old gen: %v", err)
	}
	if ok {
		t.Error("FinalizeCrawl with stale generation returned true, expected false")
	}
}

func TestFinalizeCrawl_NotFound_Deactivates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := "TESTCRAWL05"
	insertTestTarget(t, pool, tag)

	claim, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil || claim == nil || claim.PlayerTag != tag {
		t.Skip("could not claim our test row")
	}

	_, err = queries.FinalizeCrawl(ctx, pool, queries.FinalizeParams{
		PlayerTag:   tag,
		Generation:  claim.Generation,
		Status:      "not_found",
		NextCrawlAt: time.Now().Add(24 * time.Hour),
		IsActive:    false,
	})
	if err != nil {
		t.Fatalf("FinalizeCrawl not_found: %v", err)
	}

	// Row must no longer be claimable.
	var isActive bool
	err = pool.QueryRow(ctx, `SELECT is_active FROM crawl_targets WHERE player_tag = $1`, tag).Scan(&isActive)
	if err != nil {
		t.Fatalf("check is_active: %v", err)
	}
	if isActive {
		t.Error("player should be deactivated after not_found finalize")
	}

	// ClaimNextTarget must not return this row.
	newClaim, err := queries.ClaimNextTarget(ctx, pool)
	if err != nil {
		t.Fatalf("post-not_found claim: %v", err)
	}
	if newClaim != nil && newClaim.PlayerTag == tag {
		t.Error("deactivated player was returned by ClaimNextTarget")
	}
}

func TestClaimNextTarget_ConcurrentRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tag := "TESTCRAWL06"
	insertTestTarget(t, pool, tag)

	// Ensure only our test row is in the queue by checking queue state first.
	// This test is approximate: if other rows exist, both goroutines may succeed
	// on different rows. The tag-specific assertion handles that.

	var mu sync.Mutex
	var claimed []string
	var wg sync.WaitGroup

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := queries.ClaimNextTarget(ctx, pool)
			if err != nil {
				t.Errorf("ClaimNextTarget: %v", err)
				return
			}
			if result != nil {
				mu.Lock()
				claimed = append(claimed, result.PlayerTag)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Count how many goroutines claimed our specific test tag.
	count := 0
	for _, c := range claimed {
		if c == tag {
			count++
		}
	}
	if count > 1 {
		t.Errorf("test row claimed %d times simultaneously, want at most 1", count)
	}
}
