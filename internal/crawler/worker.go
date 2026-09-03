package crawler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ingestion"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ratelimit"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// ErrGlobalHalt is returned by RunOnce when the API signals a key-level failure
// (401 Unauthorized or 403 Forbidden). The caller must cancel all workers.
var ErrGlobalHalt = errors.New("crawler: API key failure (401 or 403); halt all workers")

// WorkResult summarizes the outcome of a single crawl cycle.
type WorkResult struct {
	Tag         string
	Status      string // "success", "not_found", "rate_limited", "error", "empty", "ownership_lost"
	BattlesNew  int
	Discoveries int
}

// Worker holds the shared resources used across crawl goroutines.
type Worker struct {
	pool    *pgxpool.Pool
	client  *apiclient.Client
	limiter *ratelimit.Limiter
}

// NewWorker creates a Worker with the given shared pool, API client, and rate limiter.
func NewWorker(pool *pgxpool.Pool, client *apiclient.Client, limiter *ratelimit.Limiter) *Worker {
	return &Worker{pool: pool, client: client, limiter: limiter}
}

// RunOnce claims one crawl target, fetches both the player profile and battle log,
// verifies the lease, ingests all data, and finalizes the claim.
//
//   - Returns WorkResult{Status: "empty"} when no claimable rows exist in the queue.
//   - Returns ErrGlobalHalt on 401 or 403; the caller must cancel the shared context.
//   - All other per-player errors are handled internally via FinalizeCrawl.
func (w *Worker) RunOnce(ctx context.Context) (WorkResult, error) {
	// --- Claim ---
	claim, err := queries.ClaimNextTarget(ctx, w.pool)
	if err != nil {
		return WorkResult{}, fmt.Errorf("claim: %w", err)
	}
	if claim == nil {
		return WorkResult{Status: "empty"}, nil
	}
	tag := claim.PlayerTag

	// --- Fetch player profile ---
	if err := w.limiter.Wait(ctx); err != nil {
		// Context cancelled before the first API call. Lease expires naturally.
		return WorkResult{Tag: tag, Status: "error"}, nil
	}
	profile, err := w.client.GetPlayer(ctx, tag)
	if err != nil {
		if apiclient.IsUnauthorized(err) || apiclient.IsForbidden(err) {
			return WorkResult{}, ErrGlobalHalt
		}
		return WorkResult{Tag: tag, Status: w.finalizeAPIError(ctx, claim, err)}, nil
	}

	// --- Fetch battle log ---
	if err := w.limiter.Wait(ctx); err != nil {
		return WorkResult{Tag: tag, Status: "error"}, nil
	}
	battleLog, err := w.client.GetBattleLog(ctx, tag)
	if err != nil {
		if apiclient.IsUnauthorized(err) || apiclient.IsForbidden(err) {
			return WorkResult{}, ErrGlobalHalt
		}
		return WorkResult{Tag: tag, Status: w.finalizeAPIError(ctx, claim, err)}, nil
	}

	// --- Pre-ingestion ownership check ---
	// Both API calls succeeded. Before writing any rows, confirm the lease is still valid.
	owned, err := queries.OwnershipValid(ctx, w.pool, tag, claim.Generation)
	if err != nil {
		return WorkResult{Tag: tag, Status: "error"}, fmt.Errorf("ownership check %s: %w", tag, err)
	}
	if !owned {
		slog.Warn("stale crawl abandoned: lease was lost while fetching API responses",
			"tag", tag, "generation", claim.Generation)
		return WorkResult{Tag: tag, Status: "ownership_lost"}, nil
	}

	// --- Ingest ---
	start := time.Now()
	var battlesNew, discoveries int

	if _, err = ingestion.IngestPlayer(ctx, w.pool, profile); err != nil {
		return WorkResult{Tag: tag, Status: w.finalizeTransientError(ctx, claim, err)}, nil
	}

	ingestor := ingestion.NewBattleIngestor(w.pool, apiclient.NormalizeTag(tag))
	for _, entry := range battleLog.Items {
		result, err := ingestor.IngestBattle(ctx, entry)
		if err != nil {
			return WorkResult{Tag: tag, Status: w.finalizeTransientError(ctx, claim, err)}, nil
		}
		if result.IsNew {
			battlesNew++
		}
		discoveries += len(result.NewDiscoveries)
	}

	// --- Finalize success ---
	ok, err := queries.FinalizeCrawl(ctx, w.pool, queries.FinalizeParams{
		PlayerTag:           tag,
		Generation:          claim.Generation,
		Status:              "success",
		NextCrawlAt:         time.Now().Add(baseInterval(claim.Priority)),
		ConsecutiveFailures: 0,
		IsActive:            true,
	})
	if err != nil {
		slog.Warn("finalize success failed; ingestion already committed",
			"tag", tag, "err", err)
	} else if !ok {
		slog.Warn("finalize generation mismatch; ingestion already committed",
			"tag", tag, "generation", claim.Generation)
	}

	slog.Info("player crawled",
		"tag", tag,
		"battles_new", battlesNew,
		"discoveries", discoveries,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return WorkResult{Tag: tag, Status: "success", BattlesNew: battlesNew, Discoveries: discoveries}, nil
}

// finalizeAPIError classifies an API error and calls FinalizeCrawl with the appropriate
// outcome. Returns the status string.
//
//   - 404 Not Found: deactivates the player (is_active=false), failures unchanged
//   - 429 Too Many Requests: requeues with 5-minute delay, failures unchanged
//   - other: increments consecutive_failures and applies exponential backoff
func (w *Worker) finalizeAPIError(ctx context.Context, claim *queries.ClaimResult, apiErr error) string {
	var status string
	var nextAt time.Time
	var failures int32
	var isActive bool

	switch {
	case apiclient.IsNotFound(apiErr):
		status = "not_found"
		nextAt = time.Now().Add(24 * time.Hour) // row is inactive; value is irrelevant
		failures = claim.ConsecutiveFailures    // unchanged: 404 is not a transient failure
		isActive = false
		slog.Info("player not_found", "tag", claim.PlayerTag)

	case apiclient.IsTooManyRequests(apiErr):
		status = "rate_limited"
		nextAt = time.Now().Add(5 * time.Minute)
		failures = claim.ConsecutiveFailures // unchanged: rate limiting is not a player failure
		isActive = true
		slog.Info("player rate_limited",
			"tag", claim.PlayerTag,
			"next_crawl_at", nextAt.Format(time.RFC3339),
		)

	default:
		failures = claim.ConsecutiveFailures + 1
		nextAt = time.Now().Add(backoffDuration(baseInterval(claim.Priority), claim.ConsecutiveFailures))
		status = "error"
		isActive = true
		slog.Info("player error",
			"tag", claim.PlayerTag,
			"failures", failures,
			"next_crawl_at", nextAt.Format(time.RFC3339),
			"err", apiErr,
		)
	}

	w.finalize(ctx, claim, status, nextAt, failures, isActive)
	return status
}

// finalizeTransientError handles ingestion-layer errors, which are always transient.
// Increments consecutive_failures and applies exponential backoff.
func (w *Worker) finalizeTransientError(ctx context.Context, claim *queries.ClaimResult, ingestErr error) string {
	failures := claim.ConsecutiveFailures + 1
	nextAt := time.Now().Add(backoffDuration(baseInterval(claim.Priority), claim.ConsecutiveFailures))
	slog.Info("player error",
		"tag", claim.PlayerTag,
		"failures", failures,
		"next_crawl_at", nextAt.Format(time.RFC3339),
		"err", ingestErr,
	)
	w.finalize(ctx, claim, "error", nextAt, failures, true)
	return "error"
}

// finalize calls FinalizeCrawl and logs a warning if it fails. A finalize failure is
// non-fatal: the lease expires naturally after 10 minutes, making the row re-claimable.
func (w *Worker) finalize(ctx context.Context, claim *queries.ClaimResult, status string, nextAt time.Time, failures int32, isActive bool) {
	_, err := queries.FinalizeCrawl(ctx, w.pool, queries.FinalizeParams{
		PlayerTag:           claim.PlayerTag,
		Generation:          claim.Generation,
		Status:              status,
		NextCrawlAt:         nextAt,
		ConsecutiveFailures: failures,
		IsActive:            isActive,
	})
	if err != nil {
		slog.Warn("finalize failed; lease will expire naturally",
			"tag", claim.PlayerTag, "status", status, "err", err)
	}
}

// baseInterval returns the re-crawl interval for a given priority level.
//
//	Priority 1-2: 4 hours
//	Priority 3-4: 8 hours
//	Priority 5-6: 24 hours
//	Priority 7+:  72 hours
func baseInterval(priority int16) time.Duration {
	switch {
	case priority <= 2:
		return 4 * time.Hour
	case priority <= 4:
		return 8 * time.Hour
	case priority <= 6:
		return 24 * time.Hour
	default:
		return 72 * time.Hour
	}
}

// backoffDuration computes the retry delay for a transient failure.
// n is consecutive_failures BEFORE the current failure is recorded.
// Formula: min(2^n * base, 7 days).
func backoffDuration(base time.Duration, n int32) time.Duration {
	const maxBackoff = 7 * 24 * time.Hour
	d := time.Duration(float64(base) * math.Pow(2, float64(n)))
	if d > maxBackoff || d < 0 { // d < 0 guards int64 overflow for very large n
		return maxBackoff
	}
	return d
}
