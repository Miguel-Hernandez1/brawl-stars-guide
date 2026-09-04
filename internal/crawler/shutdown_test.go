package crawler

// TestShutdownRace is an integration test that reproduces the shutdown race:
// one worker receives a 403 (global halt trigger) while a sibling worker has an
// in-flight API request. The sibling's request is cancelled by the shared context
// and must NOT be recorded as a per-player transient error.
//
// Lease behavior: when RunOnce returns "shutdown", FinalizeCrawl is intentionally
// not called. The claimed lease expires after 10 minutes, making the row
// re-claimable. This is safe because ingestion uses ON CONFLICT DO NOTHING.
//
// Requires INTEGRATION_TEST_DATABASE_URL to be set.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ratelimit"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
)

// haltFakeClient implements apiClient for the shutdown race test.
//
// GetPlayer("TESTSD01") returns 403 immediately, causing the receiving worker to
// return ErrGlobalHalt and cancel the shared context.
// GetPlayer("TESTSD02") blocks until ctx.Done(), simulating an in-flight request
// that gets interrupted by the peer's global halt.
// All other tags return 404.
type haltFakeClient struct{}

func (c *haltFakeClient) GetPlayer(ctx context.Context, tag string) (*apiclient.PlayerResponse, error) {
	switch tag {
	case "TESTSD01":
		return nil, &apiclient.APIError{StatusCode: http.StatusForbidden}
	case "TESTSD02":
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		return nil, &apiclient.APIError{StatusCode: http.StatusNotFound}
	}
}

func (c *haltFakeClient) GetBattleLog(ctx context.Context, tag string) (*apiclient.BattleLogResponse, error) {
	return &apiclient.BattleLogResponse{}, nil
}

func TestShutdownRace(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping shutdown race integration test")
	}

	ctx := context.Background()
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Insert two players then their crawl targets. Priority 1 with a far-past
	// next_crawl_at ensures these rows sort to the front of the queue ahead of
	// any real production rows already present.
	for _, tag := range []string{"TESTSD01", "TESTSD02"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (tag, name, first_seen_at) VALUES ($1, 'TestPlayer', NOW())
			 ON CONFLICT (tag) DO NOTHING`, tag); err != nil {
			t.Fatalf("insert player %s: %v", tag, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO crawl_targets
				(player_tag, priority, next_crawl_at, discovery_source)
			VALUES ($1, 1, NOW() - INTERVAL '1 year', 'test')
			ON CONFLICT (player_tag) DO UPDATE
				SET priority             = 1,
				    next_crawl_at        = NOW() - INTERVAL '1 year',
				    is_active            = true,
				    consecutive_failures = 0,
				    last_crawl_status    = NULL,
				    crawl_generation     = 0,
				    leased_until         = NULL
		`, tag); err != nil {
			t.Fatalf("insert crawl_target %s: %v", tag, err)
		}
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM crawl_targets WHERE player_tag IN ('TESTSD01','TESTSD02')`)
		pool.Exec(context.Background(), `DELETE FROM players WHERE tag IN ('TESTSD01','TESTSD02')`)
	})

	limiter := ratelimit.New(10000) // no throttle in tests
	w := NewWorker(pool, &haltFakeClient{}, limiter)

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err = RunN(runCtx, w, 2, 2)
	if !errors.Is(err, ErrGlobalHalt) {
		t.Fatalf("RunN returned %v; want ErrGlobalHalt", err)
	}

	// TESTSD02 must have consecutive_failures == 0 regardless of which stage the
	// worker was at when the shared context was cancelled (claim, limiter.Wait, or
	// GetPlayer). The "shutdown" path never calls FinalizeCrawl, so the row's
	// failure counter is unchanged from its initial value.
	var failures int32
	err = pool.QueryRow(ctx,
		`SELECT consecutive_failures FROM crawl_targets WHERE player_tag = 'TESTSD02'`,
	).Scan(&failures)
	if err != nil {
		t.Fatalf("query TESTSD02 consecutive_failures: %v", err)
	}
	if failures != 0 {
		t.Errorf("TESTSD02 consecutive_failures = %d, want 0 (shutdown must not increment failures)", failures)
	}

	// TESTSD01 also must not have failures incremented: 403 is a key-level failure,
	// not a per-player error, so ErrGlobalHalt is returned without calling FinalizeCrawl.
	err = pool.QueryRow(ctx,
		`SELECT consecutive_failures FROM crawl_targets WHERE player_tag = 'TESTSD01'`,
	).Scan(&failures)
	if err != nil {
		t.Fatalf("query TESTSD01 consecutive_failures: %v", err)
	}
	if failures != 0 {
		t.Errorf("TESTSD01 consecutive_failures = %d, want 0 (403 must not increment failures)", failures)
	}
}
