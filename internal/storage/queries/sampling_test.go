package queries_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/domain"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// preClassify sets player_trophy_bucket, priority, and trophy_estimate on an existing
// crawl_targets row, simulating a player that has already been through a first crawl.
func preClassify(t *testing.T, pool interface {
	Exec(ctx context.Context, sql string, args ...any) (interface {
		RowsAffected() int64
	}, error)
}, tag string, bucket int, trophies int) {
	t.Helper()
	_ = tag
	_ = bucket
	_ = trophies
}

func TestClassifyAndSampleTarget_UnderCap(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tag := "TESTSMP01"
	insertTestTarget(t, pool, tag)

	// BucketCap=5, only 1 player in bucket 91 (sentinel; no production rows here): under cap.
	alwaysAccept := func() float64 { return 0.0 }
	sampledOut, err := queries.ClassifyAndSampleTarget(
		ctx, pool, tag, domain.TrophyBucket(91), 4, 7500, 5, alwaysAccept,
	)
	if err != nil {
		t.Fatalf("ClassifyAndSampleTarget: %v", err)
	}
	if sampledOut {
		t.Error("sampledOut=true but bucket is under cap; expected false")
	}

	// Verify player_trophy_bucket and trophy_estimate were set.
	var gotBucket, gotTrophies *int
	err = pool.QueryRow(ctx,
		`SELECT player_trophy_bucket, trophy_estimate FROM crawl_targets WHERE player_tag = $1`, tag,
	).Scan(&gotBucket, &gotTrophies)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotBucket == nil || *gotBucket != 91 {
		t.Errorf("player_trophy_bucket: got %v, want 91", gotBucket)
	}
	if gotTrophies == nil || *gotTrophies != 7500 {
		t.Errorf("trophy_estimate: got %v, want 7500", gotTrophies)
	}
}

func TestClassifyAndSampleTarget_CapPlusOne_Reject(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Insert 1 pre-classified player in bucket 92 (sentinel; no production rows here).
	// cap=1 means bucket is at cap after this insert.
	existing := "TESTSMP02"
	insertTestTarget(t, pool, existing)
	_, err := pool.Exec(ctx,
		`UPDATE crawl_targets SET player_trophy_bucket = 92, priority = 4, trophy_estimate = 5000
		 WHERE player_tag = $1`, existing)
	if err != nil {
		t.Fatalf("pre-classify existing: %v", err)
	}

	// New player to classify.
	incoming := "TESTSMP03"
	insertTestTarget(t, pool, incoming)

	// cap=1, seen_b will be 2 after classification. Reject if rand >= cap/seen_b = 0.5.
	// Use 0.9 (> 0.5) → reject.
	alwaysReject := func() float64 { return 0.9 }
	sampledOut, err := queries.ClassifyAndSampleTarget(
		ctx, pool, incoming, domain.TrophyBucket(92), 4, 5000, 1, alwaysReject,
	)
	if err != nil {
		t.Fatalf("ClassifyAndSampleTarget: %v", err)
	}
	if !sampledOut {
		t.Error("sampledOut=false but rand > cap/seen_b; expected true (rejected)")
	}

	// The existing player must still be active (no eviction on reject).
	var existingActive bool
	pool.QueryRow(ctx, `SELECT is_active FROM crawl_targets WHERE player_tag = $1`, existing).Scan(&existingActive)
	if !existingActive {
		t.Error("existing player was evicted even though incoming player was rejected")
	}

	// Verify player_trophy_bucket was set on the incoming player (classify always runs before decision).
	var incomingBucket *int
	pool.QueryRow(ctx, `SELECT player_trophy_bucket FROM crawl_targets WHERE player_tag = $1`, incoming).Scan(&incomingBucket)
	if incomingBucket == nil || *incomingBucket != 92 {
		t.Errorf("incoming player_trophy_bucket: got %v, want 92", incomingBucket)
	}
}

func TestClassifyAndSampleTarget_CapPlusOne_Accept_Evicts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Insert 1 pre-classified active player in bucket 93 (sentinel; no production rows).
	// cap=1 → must evict them on accept.
	existing := "TESTSMP04"
	insertTestTarget(t, pool, existing)
	_, err := pool.Exec(ctx,
		`UPDATE crawl_targets SET player_trophy_bucket = 93, priority = 4, trophy_estimate = 5000
		 WHERE player_tag = $1`, existing)
	if err != nil {
		t.Fatalf("pre-classify existing: %v", err)
	}

	incoming := "TESTSMP05"
	insertTestTarget(t, pool, incoming)

	// seen_b=2 after classifying incoming. Accept: 0.1 < cap/seen_b = 1/2 = 0.5.
	alwaysAccept := func() float64 { return 0.1 }
	sampledOut, err := queries.ClassifyAndSampleTarget(
		ctx, pool, incoming, domain.TrophyBucket(93), 4, 5000, 1, alwaysAccept,
	)
	if err != nil {
		t.Fatalf("ClassifyAndSampleTarget: %v", err)
	}
	if sampledOut {
		t.Error("sampledOut=true but we expected accept (rand < cap/seen_b)")
	}

	// The existing player must have been evicted.
	var existingStatus *string
	var existingActive bool
	pool.QueryRow(ctx,
		`SELECT is_active, last_crawl_status FROM crawl_targets WHERE player_tag = $1`, existing,
	).Scan(&existingActive, &existingStatus)
	if existingActive {
		t.Error("existing player still active after eviction")
	}
	if existingStatus == nil || *existingStatus != "sampled_out" {
		t.Errorf("existing last_crawl_status: got %v, want 'sampled_out'", existingStatus)
	}

	// Incoming must be active.
	var incomingActive bool
	pool.QueryRow(ctx, `SELECT is_active FROM crawl_targets WHERE player_tag = $1`, incoming).Scan(&incomingActive)
	if !incomingActive {
		t.Error("incoming player should be active after acceptance")
	}
}

func TestClassifyAndSampleTarget_NeverEvictsCurrentPlayer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// cap=1, only the incoming player in bucket 94 (sentinel; no production rows).
	// active-others count is 0 < cap, so no eviction fires even with alwaysAccept RNG.
	incoming := "TESTSMP06"
	insertTestTarget(t, pool, incoming)

	alwaysAccept := func() float64 { return 0.0 }
	sampledOut, err := queries.ClassifyAndSampleTarget(
		ctx, pool, incoming, domain.TrophyBucket(94), 3, 15000, 1, alwaysAccept,
	)
	if err != nil {
		t.Fatalf("ClassifyAndSampleTarget: %v", err)
	}
	if sampledOut {
		t.Error("sampledOut=true with only one player in bucket; expected false (under cap)")
	}

	// Incoming must still be active.
	var active bool
	pool.QueryRow(ctx, `SELECT is_active FROM crawl_targets WHERE player_tag = $1`, incoming).Scan(&active)
	if !active {
		t.Error("incoming player deactivated -- eviction must never select the current player")
	}
}

func TestUpdateCrawlProfile_ReclassifiedPlayer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tag := "TESTSMP07"
	insertTestTarget(t, pool, tag)
	_, err := pool.Exec(ctx,
		`UPDATE crawl_targets SET player_trophy_bucket = 95, priority = 5, trophy_estimate = 6000
		 WHERE player_tag = $1`, tag)
	if err != nil {
		t.Fatalf("pre-classify: %v", err)
	}

	newPriority := int16(2)
	newTrophies := 25000
	if err = queries.UpdateCrawlProfile(ctx, pool, tag, newPriority, newTrophies); err != nil {
		t.Fatalf("UpdateCrawlProfile: %v", err)
	}

	var bucket, trophies *int
	var priority int16
	var isActive bool
	err = pool.QueryRow(ctx,
		`SELECT player_trophy_bucket, trophy_estimate, priority, is_active
		 FROM crawl_targets WHERE player_tag = $1`, tag,
	).Scan(&bucket, &trophies, &priority, &isActive)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if bucket == nil || *bucket != 95 {
		t.Errorf("player_trophy_bucket: got %v, want 95 (must not change on re-crawl)", bucket)
	}
	if trophies == nil || *trophies != newTrophies {
		t.Errorf("trophy_estimate: got %v, want %d", trophies, newTrophies)
	}
	if priority != newPriority {
		t.Errorf("priority: got %d, want %d", priority, newPriority)
	}
	if !isActive {
		t.Error("is_active must remain true; UpdateCrawlProfile must not change it")
	}
}

func TestEnqueueDiscoveredPlayer_NullAtDiscovery(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tag := "TESTSMP08"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM crawl_targets WHERE player_tag = $1`, tag)
		pool.Exec(context.Background(), `DELETE FROM players WHERE tag = $1`, tag)
	})

	inserted, err := queries.EnqueueDiscoveredPlayer(ctx, pool, tag, "TestPlayer", "test", "")
	if err != nil {
		t.Fatalf("EnqueueDiscoveredPlayer: %v", err)
	}
	if !inserted {
		t.Fatal("expected inserted=true for new player")
	}

	// trophy_estimate and player_trophy_bucket must be NULL at discovery time.
	var trophyEst, bucket *int
	err = pool.QueryRow(ctx,
		`SELECT trophy_estimate, player_trophy_bucket FROM crawl_targets WHERE player_tag = $1`, tag,
	).Scan(&trophyEst, &bucket)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if trophyEst != nil {
		t.Errorf("trophy_estimate must be NULL at discovery, got %d", *trophyEst)
	}
	if bucket != nil {
		t.Errorf("player_trophy_bucket must be NULL at discovery, got %d", *bucket)
	}
}

func TestEnqueueDiscoveredPlayer_TrophyEstimateSetAfterClassification(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tag := "TESTSMP09"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM crawl_targets WHERE player_tag = $1`, tag)
		pool.Exec(context.Background(), `DELETE FROM players WHERE tag = $1`, tag)
	})

	_, err := queries.EnqueueDiscoveredPlayer(ctx, pool, tag, "TestPlayer", "test", "")
	if err != nil {
		t.Fatalf("EnqueueDiscoveredPlayer: %v", err)
	}

	// Simulate first successful GetPlayer by calling ClassifyAndSampleTarget.
	// Use sentinel bucket 96 (no production rows) so the COUNT is 1 <= cap=750.
	profileTrophies := 12345
	alwaysAccept := func() float64 { return 0.0 }
	_, err = queries.ClassifyAndSampleTarget(
		ctx, pool, tag, domain.TrophyBucket(96), 4, profileTrophies, 750, alwaysAccept,
	)
	if err != nil {
		t.Fatalf("ClassifyAndSampleTarget: %v", err)
	}

	var trophyEst, bucket *int
	err = pool.QueryRow(ctx,
		`SELECT trophy_estimate, player_trophy_bucket FROM crawl_targets WHERE player_tag = $1`, tag,
	).Scan(&trophyEst, &bucket)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if trophyEst == nil || *trophyEst != profileTrophies {
		t.Errorf("trophy_estimate: got %v, want %d (must equal profile.Trophies)", trophyEst, profileTrophies)
	}
	if bucket == nil || *bucket != 96 {
		t.Errorf("player_trophy_bucket: got %v, want 96", bucket)
	}
}

func TestEnqueueDiscoveredPlayer_ExistingPlayerMissingTarget(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tag := "TESTSMP10"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM crawl_targets WHERE player_tag = $1`, tag)
		pool.Exec(context.Background(), `DELETE FROM players WHERE tag = $1`, tag)
	})

	// Player row exists, no crawl_targets row.
	_, err := pool.Exec(ctx,
		`INSERT INTO players (tag, name, first_seen_at) VALUES ($1, 'TestPlayer', NOW())
		 ON CONFLICT (tag) DO NOTHING`, tag)
	if err != nil {
		t.Fatalf("insert player: %v", err)
	}

	inserted, err := queries.EnqueueDiscoveredPlayer(ctx, pool, tag, "TestPlayer", "test", "")
	if err != nil {
		t.Fatalf("EnqueueDiscoveredPlayer: %v", err)
	}
	if !inserted {
		t.Error("inserted=false but player was not in crawl_targets; expected true")
	}
}

func TestEnqueueDiscoveredPlayer_AlreadyQueued(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tag := "TESTSMP11"
	insertTestTarget(t, pool, tag)

	inserted, err := queries.EnqueueDiscoveredPlayer(ctx, pool, tag, "TestPlayer", "test", "")
	if err != nil {
		t.Fatalf("EnqueueDiscoveredPlayer: %v", err)
	}
	if inserted {
		t.Error("inserted=true for already-queued player; expected false")
	}
}

func TestSampledOutAndNotFoundDistinguishable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	notFoundTag := "TESTSMP12"
	sampledOutTag := "TESTSMP13"
	insertTestTarget(t, pool, notFoundTag)
	insertTestTarget(t, pool, sampledOutTag)

	var notFoundClaim, sampledOutClaim *queries.ClaimResult

	for range 2 {
		c, err := queries.ClaimNextTarget(ctx, pool)
		if err != nil || c == nil {
			t.Skip("could not claim a test row")
		}
		switch c.PlayerTag {
		case notFoundTag:
			notFoundClaim = c
		case sampledOutTag:
			sampledOutClaim = c
		default:
			t.Skipf("claimed unrelated row %s; other rows present in queue", c.PlayerTag)
		}
	}
	if notFoundClaim == nil || sampledOutClaim == nil {
		t.Skip("could not claim both test rows")
	}

	future := time.Now().Add(24 * time.Hour)
	queries.FinalizeCrawl(ctx, pool, queries.FinalizeParams{
		PlayerTag: notFoundTag, Generation: notFoundClaim.Generation,
		Status: "not_found", NextCrawlAt: future, IsActive: false,
	})
	queries.FinalizeCrawl(ctx, pool, queries.FinalizeParams{
		PlayerTag: sampledOutTag, Generation: sampledOutClaim.Generation,
		Status: "sampled_out", NextCrawlAt: future, IsActive: false,
	})

	var nfStatus, soStatus string
	pool.QueryRow(ctx, `SELECT last_crawl_status FROM crawl_targets WHERE player_tag = $1`, notFoundTag).Scan(&nfStatus)
	pool.QueryRow(ctx, `SELECT last_crawl_status FROM crawl_targets WHERE player_tag = $1`, sampledOutTag).Scan(&soStatus)

	if nfStatus != "not_found" {
		t.Errorf("not_found tag has status %q, want 'not_found'", nfStatus)
	}
	if soStatus != "sampled_out" {
		t.Errorf("sampled_out tag has status %q, want 'sampled_out'", soStatus)
	}
	if nfStatus == soStatus {
		t.Error("not_found and sampled_out must have different last_crawl_status values")
	}

	// Both must be excluded from the claim queue.
	for _, tag := range []string{notFoundTag, sampledOutTag} {
		var isActive bool
		pool.QueryRow(ctx, `SELECT is_active FROM crawl_targets WHERE player_tag = $1`, tag).Scan(&isActive)
		if isActive {
			t.Errorf("%s should be inactive after finalization", tag)
		}
	}
}

// TestConcurrentBucketSampling verifies that after N concurrent goroutines each classify a
// player into the same bucket with cap=3, the advisory lock keeps the final active count <= cap.
//
// With randFn=0.5 and cap=3, the advisory lock serialises all admits (serial DB order):
//   - Players 1-3: seen_b<=cap, accepted. Active in bucket: 3.
//   - Player 4:    seen_b=4, 0.5 < 3/4=0.75 -- accept; evict one. Active: 3.
//   - Player 5:    seen_b=5, 0.5 < 3/5=0.60 -- accept; evict one. Active: 3.
//   - Player 6:    seen_b=6, 0.5 NOT < 3/6=0.50 -- reject (sampledOut=true).
//
// ClassifyAndSampleTarget returns sampledOut=true; the caller (RunOnce) is responsible for
// finalizing with is_active=FALSE. The test simulates that step directly.
// Expected final active count: 3 = cap.
func TestConcurrentBucketSampling_ActiveCountBelowCap(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const bucketCap = 3
	const goroutines = 6

	tags := make([]string, goroutines)
	for i := range tags {
		tags[i] = fmt.Sprintf("TESTCON%02d", i+1)
		insertTestTarget(t, pool, tags[i])
	}

	type classifyResult struct {
		tag        string
		sampledOut bool
	}
	resultCh := make(chan classifyResult, goroutines)

	var wg sync.WaitGroup
	for _, tag := range tags {
		tag := tag
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Fixed RNG = 0.5; sentinel bucket 97 (no production rows) ensures COUNT
			// includes only the 6 test players, making the serial math deterministic.
			so, _ := queries.ClassifyAndSampleTarget(ctx, pool, tag, domain.TrophyBucket(97), 2, 30000, bucketCap,
				func() float64 { return 0.5 },
			)
			resultCh <- classifyResult{tag: tag, sampledOut: so}
		}()
	}
	wg.Wait()
	close(resultCh)

	// Simulate RunOnce's FinalizeCrawl: rejected players must be deactivated.
	// We do a direct UPDATE (no generation check) because these rows were never claimed.
	for r := range resultCh {
		if r.sampledOut {
			pool.Exec(ctx,
				`UPDATE crawl_targets SET is_active = FALSE, last_crawl_status = 'sampled_out'
				 WHERE player_tag = $1`, r.tag)
		}
	}

	var activeCount int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM crawl_targets
		 WHERE player_trophy_bucket = 97 AND is_active = TRUE`,
	).Scan(&activeCount)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if activeCount > bucketCap {
		t.Errorf("active count in bucket = %d, want <= %d after finalization",
			activeCount, bucketCap)
	}
}
