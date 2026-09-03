package queries

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClaimResult holds the details of a successfully claimed crawl target.
type ClaimResult struct {
	PlayerTag           string
	Generation          int32
	Priority            int16
	ConsecutiveFailures int32
}

// ClaimNextTarget claims one idle crawl target using a short transaction.
// The transaction commits immediately after incrementing crawl_generation and
// setting leased_until; no lock or connection is held afterward.
// Returns nil (no error) when the queue has no claimable rows.
func ClaimNextTarget(ctx context.Context, pool *pgxpool.Pool) (*ClaimResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var tag string
	var gen, failures int32
	var priority int16
	err = tx.QueryRow(ctx, `
		SELECT player_tag, crawl_generation, priority, consecutive_failures
		FROM crawl_targets
		WHERE is_active = TRUE
		  AND next_crawl_at <= NOW()
		  AND (leased_until IS NULL OR leased_until < NOW())
		ORDER BY priority ASC, next_crawl_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&tag, &gen, &priority, &failures)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	gen++
	_, err = tx.Exec(ctx, `
		UPDATE crawl_targets
		SET crawl_generation = $1,
		    leased_until      = NOW() + INTERVAL '10 minutes',
		    updated_at        = NOW()
		WHERE player_tag = $2
	`, gen, tag)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &ClaimResult{
		PlayerTag:           tag,
		Generation:          gen,
		Priority:            priority,
		ConsecutiveFailures: failures,
	}, nil
}

// FinalizeParams holds the outcome of a completed crawl for persisting.
type FinalizeParams struct {
	PlayerTag           string
	Generation          int32
	Status              string // 'success', 'not_found', 'rate_limited', 'error'
	NextCrawlAt         time.Time
	ConsecutiveFailures int32
	IsActive            bool
}

// FinalizeCrawl persists the outcome of a crawl using compare-and-update on
// crawl_generation. Returns true when the row was updated (generation matched),
// false when another worker has already claimed or finalized this row.
func FinalizeCrawl(ctx context.Context, pool *pgxpool.Pool, p FinalizeParams) (bool, error) {
	result, err := pool.Exec(ctx, `
		UPDATE crawl_targets
		SET last_crawl_status    = $1,
		    next_crawl_at        = $2,
		    consecutive_failures = $3,
		    is_active            = $4,
		    crawl_count          = crawl_count + 1,
		    last_crawled_at      = NOW(),
		    leased_until         = NULL,
		    updated_at           = NOW()
		WHERE player_tag      = $5
		  AND crawl_generation = $6
	`, p.Status, p.NextCrawlAt, p.ConsecutiveFailures, p.IsActive, p.PlayerTag, p.Generation)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

// OwnershipValid returns true if the worker still owns the lease for tag:
// generation matches and leased_until has not passed.
// Returns false (not an error) when ownership has been lost.
func OwnershipValid(ctx context.Context, pool *pgxpool.Pool, tag string, generation int32) (bool, error) {
	var gen int32
	var leasedUntil *time.Time
	err := pool.QueryRow(ctx, `
		SELECT crawl_generation, leased_until
		FROM crawl_targets
		WHERE player_tag = $1
	`, tag).Scan(&gen, &leasedUntil)
	if err != nil {
		return false, err
	}
	if gen != generation {
		return false, nil
	}
	if leasedUntil == nil || leasedUntil.Before(time.Now()) {
		return false, nil
	}
	return true, nil
}

// QueueDepth returns the number of currently claimable targets: active, due,
// and not actively leased by another worker.
func QueueDepth(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM crawl_targets
		WHERE is_active = TRUE
		  AND next_crawl_at <= NOW()
		  AND (leased_until IS NULL OR leased_until < NOW())
	`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}
