package crawler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/domain"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ingestion"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ratelimit"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// ErrGlobalHalt is returned by RunOnce when the API signals a key-level failure
// (401 Unauthorized or 403 Forbidden). The caller must cancel all workers.
var ErrGlobalHalt = errors.New("crawler: API key failure (401 or 403); halt all workers")

// apiClient is the API surface required by Worker. *apiclient.Client satisfies this
// interface, and tests may substitute a fake implementation.
type apiClient interface {
	GetPlayer(ctx context.Context, tag string) (*apiclient.PlayerResponse, error)
	GetBattleLog(ctx context.Context, tag string) (*apiclient.BattleLogResponse, error)
}

// WorkerConfig holds population-control settings for a Worker.
type WorkerConfig struct {
	// MaxDiscoveriesPerCrawl is the maximum number of new crawl_targets rows that may be
	// inserted during one RunOnce call. 0 means unlimited.
	MaxDiscoveriesPerCrawl int
	// MaxActiveTargets is the global soft ceiling on is_active=TRUE rows. When the live
	// count reaches this value, discovery is disabled for the current RunOnce call.
	// 0 means unlimited.
	MaxActiveTargets int
	// BucketCap is the maximum number of active players per trophy bucket after
	// classification. 0 disables reservoir sampling entirely.
	BucketCap int
}

// WorkResult summarizes the outcome of a single crawl cycle.
type WorkResult struct {
	Tag string
	// Status values:
	//   "success"        - crawl completed and finalized
	//   "sampled_out"    - crawl succeeded but player deactivated by reservoir sampling
	//   "not_found"      - player deactivated (authoritative 404)
	//   "rate_limited"   - requeued with 5-minute delay
	//   "error"          - transient failure; backoff applied
	//   "empty"          - queue had no claimable rows
	//   "ownership_lost" - lease expired before ingestion began
	//   "shutdown"       - context was cancelled (global halt or signal);
	//                      no per-player penalty applied, lease expires naturally
	Status      string
	BattlesNew  int
	Discoveries int
}

// Worker holds the shared resources used across crawl goroutines.
type Worker struct {
	pool    *pgxpool.Pool
	client  apiClient
	limiter *ratelimit.Limiter
	cfg     WorkerConfig
}

// NewWorker creates a Worker with the given shared pool, API client, rate limiter, and config.
// client must implement apiClient; *apiclient.Client satisfies this interface.
func NewWorker(pool *pgxpool.Pool, client apiClient, limiter *ratelimit.Limiter, cfg WorkerConfig) *Worker {
	return &Worker{pool: pool, client: client, limiter: limiter, cfg: cfg}
}

// RunOnce claims one crawl target, fetches both the player profile and battle log,
// verifies the lease, ingests all data, and finalizes the claim.
//
//   - Returns WorkResult{Status: "empty"} when no claimable rows exist in the queue.
//   - Returns WorkResult{Status: "shutdown"} when the shared context was cancelled
//     (by a peer's global halt or by SIGINT). No per-player penalty is applied and
//     no FinalizeCrawl is called; the claimed lease expires naturally after 10 minutes.
//   - Returns ErrGlobalHalt on 401 or 403; the caller must cancel the shared context.
//   - All other per-player errors are handled internally via FinalizeCrawl.
func (w *Worker) RunOnce(ctx context.Context) (WorkResult, error) {
	// --- Claim ---
	claim, err := queries.ClaimNextTarget(ctx, w.pool)
	if err != nil {
		if ctx.Err() != nil {
			return WorkResult{Status: "shutdown"}, nil
		}
		return WorkResult{}, fmt.Errorf("claim: %w", err)
	}
	if claim == nil {
		return WorkResult{Status: "empty"}, nil
	}
	tag := claim.PlayerTag

	// --- Fetch player profile ---
	if err := w.limiter.Wait(ctx); err != nil {
		return WorkResult{Tag: tag, Status: "shutdown"}, nil
	}
	profile, err := w.client.GetPlayer(ctx, tag)
	if err != nil {
		if apiclient.IsUnauthorized(err) || apiclient.IsForbidden(err) {
			return WorkResult{}, ErrGlobalHalt
		}
		// ctx.Err() specifically checks the shared crawler context, not any per-request
		// timeout. http.Client.Timeout creates an internal child context per request; when
		// that fires, the parent ctx remains live and ctx.Err() == nil, so genuine request
		// timeouts still reach finalizeAPIError as transient errors. Only a shared-context
		// cancellation (SIGINT or peer 401/403) sets ctx.Err(). In that case, leave the
		// lease to expire naturally; a cleanup context here risks writing incorrect backoff
		// state if another worker has already re-claimed the row.
		if ctx.Err() != nil {
			return WorkResult{Tag: tag, Status: "shutdown"}, nil
		}
		return WorkResult{Tag: tag, Status: w.finalizeAPIError(ctx, claim, err)}, nil
	}

	// --- Fetch battle log ---
	if err := w.limiter.Wait(ctx); err != nil {
		return WorkResult{Tag: tag, Status: "shutdown"}, nil
	}
	battleLog, err := w.client.GetBattleLog(ctx, tag)
	if err != nil {
		if apiclient.IsUnauthorized(err) || apiclient.IsForbidden(err) {
			return WorkResult{}, ErrGlobalHalt
		}
		if ctx.Err() != nil {
			return WorkResult{Tag: tag, Status: "shutdown"}, nil
		}
		return WorkResult{Tag: tag, Status: w.finalizeAPIError(ctx, claim, err)}, nil
	}

	// --- Pre-ingestion ownership check ---
	owned, err := queries.OwnershipValid(ctx, w.pool, tag, claim.Generation)
	if err != nil {
		if ctx.Err() != nil {
			return WorkResult{Tag: tag, Status: "shutdown"}, nil
		}
		return WorkResult{Tag: tag, Status: "error"}, fmt.Errorf("ownership check %s: %w", tag, err)
	}
	if !owned {
		slog.Warn("stale crawl abandoned: lease was lost while fetching API responses",
			"tag", tag, "generation", claim.Generation)
		return WorkResult{Tag: tag, Status: "ownership_lost"}, nil
	}

	// --- Ingest player profile ---
	start := time.Now()
	if _, err = ingestion.IngestPlayer(ctx, w.pool, profile); err != nil {
		if ctx.Err() != nil {
			return WorkResult{Tag: tag, Status: "shutdown"}, nil
		}
		return WorkResult{Tag: tag, Status: w.finalizeTransientError(ctx, claim, err)}, nil
	}

	// --- Classify and sample (first crawl) or update profile (re-crawl) ---
	// realPriority is derived from the fresh profile; used for both bucket sampling
	// and the success rescheduling interval (fixes stale claim.Priority bug).
	realPriority := domain.PriorityForTrophies(profile.Trophies)
	realBucket := domain.BucketForTrophies(profile.Trophies)

	var sampledOut bool
	if !claim.IsClassified {
		sampledOut, err = queries.ClassifyAndSampleTarget(
			ctx, w.pool, tag, realBucket, realPriority, profile.Trophies, w.cfg.BucketCap, rand.Float64,
		)
		if err != nil {
			if ctx.Err() != nil {
				return WorkResult{Tag: tag, Status: "shutdown"}, nil
			}
			// Classification failed (DB error). IngestPlayer already committed; keep player
			// active with old priority rather than incorrectly recording a transient failure.
			slog.Warn("bucket classification failed; player kept active with stale priority",
				"tag", tag, "err", err)
			realPriority = claim.Priority
		}
	} else {
		if err = queries.UpdateCrawlProfile(ctx, w.pool, tag, realPriority, profile.Trophies); err != nil {
			if ctx.Err() != nil {
				return WorkResult{Tag: tag, Status: "shutdown"}, nil
			}
			slog.Warn("update crawl profile failed; continuing with stale priority",
				"tag", tag, "err", err)
			realPriority = claim.Priority
		}
	}

	// --- Check global active target ceiling before discovery ---
	ingestorCfg := ingestion.BattleIngestorConfig{
		MaxDiscoveriesPerCrawl: w.cfg.MaxDiscoveriesPerCrawl,
	}
	if w.cfg.MaxActiveTargets > 0 {
		activeCount, countErr := queries.ActiveTargetCount(ctx, w.pool)
		if countErr == nil && int(activeCount) >= w.cfg.MaxActiveTargets {
			ingestorCfg.MaxDiscoveriesPerCrawl = 0
			slog.Info("discovery disabled: active target ceiling reached",
				"active_count", activeCount, "ceiling", w.cfg.MaxActiveTargets)
		}
	}

	// --- Ingest battle log ---
	var battlesNew, discoveries int

	ingestor := ingestion.NewBattleIngestor(w.pool, apiclient.NormalizeTag(tag), ingestorCfg)
	for _, entry := range battleLog.Items {
		result, err := ingestor.IngestBattle(ctx, entry)
		if err != nil {
			if ctx.Err() != nil {
				return WorkResult{Tag: tag, Status: "shutdown"}, nil
			}
			return WorkResult{Tag: tag, Status: w.finalizeTransientError(ctx, claim, err)}, nil
		}
		if result.IsNew {
			battlesNew++
		}
		discoveries += len(result.NewDiscoveries)
	}

	// --- Finalize ---
	// sampledOut=true means ingestion succeeded but reservoir sampling rejected this player.
	// Data is preserved; the player is deactivated for population control.
	finalStatus := "success"
	isActive := true
	if sampledOut {
		finalStatus = "sampled_out"
		isActive = false
	}

	ok, err := queries.FinalizeCrawl(ctx, w.pool, queries.FinalizeParams{
		PlayerTag:           tag,
		Generation:          claim.Generation,
		Status:              finalStatus,
		NextCrawlAt:         time.Now().Add(baseInterval(realPriority)),
		ConsecutiveFailures: 0,
		IsActive:            isActive,
	})
	if err != nil {
		slog.Warn("finalize failed; ingestion already committed",
			"tag", tag, "status", finalStatus, "err", err)
	} else if !ok {
		slog.Warn("finalize generation mismatch; ingestion already committed",
			"tag", tag, "generation", claim.Generation)
	}

	slog.Info("player crawled",
		"tag", tag,
		"status", finalStatus,
		"battles_new", battlesNew,
		"discoveries", discoveries,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return WorkResult{Tag: tag, Status: finalStatus, BattlesNew: battlesNew, Discoveries: discoveries}, nil
}

// finalizeAPIError classifies an API error and calls FinalizeCrawl with the appropriate
// outcome. Returns the status string. This must only be called when ctx.Err() is nil
// (i.e., the error is genuinely per-player, not a shutdown side-effect).
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
		nextAt = time.Now().Add(24 * time.Hour)
		failures = claim.ConsecutiveFailures
		isActive = false
		slog.Info("player not_found", "tag", claim.PlayerTag)

	case apiclient.IsTooManyRequests(apiErr):
		status = "rate_limited"
		nextAt = time.Now().Add(5 * time.Minute)
		failures = claim.ConsecutiveFailures
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
// Must only be called when ctx.Err() is nil.
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

// finalize calls FinalizeCrawl and logs a warning if it fails.
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
	if d > maxBackoff || d < 0 {
		return maxBackoff
	}
	return d
}
