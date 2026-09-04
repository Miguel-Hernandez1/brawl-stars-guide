package queries

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/domain"
)

// ClassifyAndSampleTarget classifies a crawl target into its trophy bucket and applies
// reservoir sampling to keep each bucket at or below cap active players.
//
// Call this only when claim.IsClassified is false (player_trophy_bucket IS NULL).
// The WHERE player_trophy_bucket IS NULL guard in the UPDATE is defensive -- given lease
// semantics only one worker ever processes a given tag, so this should always match.
//
// Transaction ordering (classify-first to include current player in the denominator):
//  1. Acquire per-bucket advisory lock (pg_advisory_xact_lock) -- serialises concurrent
//     classifiers for the same bucket without blocking other buckets.
//  2. UPDATE crawl_targets: set player_trophy_bucket, priority, trophy_estimate.
//  3. COUNT all rows WHERE player_trophy_bucket = b (seen_b, now includes current player).
//  4. If seen_b <= cap: commit, return (false, nil) -- under cap, keep active.
//  5. Else: accept = randFn() < cap/seen_b.
//     - Accept: if active-others >= cap, evict one random non-leased active player.
//       Commit, return (false, nil).
//     - Reject: commit, return (true, nil). Caller passes sampledOut=true to FinalizeCrawl,
//       which sets is_active=FALSE and last_crawl_status='sampled_out'.
//
// cap <= 0 disables reservoir sampling (no evictions, all players kept).
// randFn is injectable so tests can use a deterministic source.
func ClassifyAndSampleTarget(
	ctx context.Context,
	pool *pgxpool.Pool,
	tag string,
	bucket domain.TrophyBucket,
	priority int16,
	trophies int,
	cap int,
	randFn func() float64,
) (sampledOut bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Serialise classification for this bucket. Lock is released at transaction end.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(bucket)); err != nil {
		return false, fmt.Errorf("advisory lock bucket %d: %w", bucket, err)
	}

	// Step 1: classify (set bucket, priority, trophy_estimate from the fresh profile).
	if _, err = tx.Exec(ctx, `
		UPDATE crawl_targets
		SET player_trophy_bucket = $1,
		    priority             = $2,
		    trophy_estimate      = $3,
		    updated_at           = NOW()
		WHERE player_tag = $4
		  AND player_trophy_bucket IS NULL
	`, int16(bucket), priority, trophies, tag); err != nil {
		return false, fmt.Errorf("classify %s: %w", tag, err)
	}

	// Step 2: count all classified players in this bucket, including the current player.
	// This is the reservoir denominator; using (seen_b + 1) before classification would
	// be equivalent, but classifying first is cleaner.
	var seenB int
	if err = tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM crawl_targets WHERE player_trophy_bucket = $1`,
		int16(bucket),
	).Scan(&seenB); err != nil {
		return false, fmt.Errorf("count bucket %d: %w", bucket, err)
	}

	if cap <= 0 || seenB <= cap {
		return false, tx.Commit(ctx)
	}

	// Step 3: reservoir decision -- accept with probability cap/seenB.
	if randFn() < float64(cap)/float64(seenB) {
		// Accepted: check if the active count (excluding current player) is already at cap.
		var activeB int
		if err = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM crawl_targets
			WHERE player_trophy_bucket = $1
			  AND is_active = TRUE
			  AND player_tag != $2
		`, int16(bucket), tag).Scan(&activeB); err != nil {
			return false, fmt.Errorf("count active bucket %d: %w", bucket, err)
		}
		if activeB >= cap {
			// Evict one random active player in this bucket (not current, not actively leased
			// to avoid a race with FinalizeCrawl overwriting our eviction).
			if _, err = tx.Exec(ctx, `
				UPDATE crawl_targets
				SET is_active         = FALSE,
				    last_crawl_status = 'sampled_out',
				    updated_at        = NOW()
				WHERE player_tag = (
				    SELECT player_tag FROM crawl_targets
				    WHERE player_trophy_bucket = $1
				      AND is_active = TRUE
				      AND player_tag != $2
				      AND (leased_until IS NULL OR leased_until < NOW())
				    ORDER BY random() LIMIT 1
				)
			`, int16(bucket), tag); err != nil {
				return false, fmt.Errorf("evict from bucket %d: %w", bucket, err)
			}
		}
		return false, tx.Commit(ctx)
	}

	// Rejected: caller will finalize with is_active=FALSE, last_crawl_status='sampled_out'.
	return true, tx.Commit(ctx)
}

// UpdateCrawlProfile updates priority and trophy_estimate for an already-classified target.
// Call this on re-crawls (claim.IsClassified == true) instead of ClassifyAndSampleTarget.
// player_trophy_bucket is intentionally not changed -- bucket migration is deferred to a
// future milestone to avoid instability from near-boundary trophy oscillation.
func UpdateCrawlProfile(ctx context.Context, pool *pgxpool.Pool, tag string, priority int16, trophies int) error {
	_, err := pool.Exec(ctx, `
		UPDATE crawl_targets
		SET priority        = $1,
		    trophy_estimate = $2,
		    updated_at      = NOW()
		WHERE player_tag = $3
	`, priority, trophies, tag)
	if err != nil {
		return fmt.Errorf("update crawl profile %s: %w", tag, err)
	}
	return nil
}

// ActiveTargetCount returns the number of currently active crawl targets.
// Used by the worker to enforce the global soft ceiling before enabling discovery.
func ActiveTargetCount(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM crawl_targets WHERE is_active = TRUE`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("active target count: %w", err)
	}
	return n, nil
}
