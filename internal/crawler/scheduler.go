package crawler

import (
	"context"
	"errors"
	"log/slog"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// Run launches workerCount crawl goroutines and blocks until the context is cancelled
// or SIGINT/SIGTERM is received. Logs a stats summary every 60 seconds.
// Returns ErrGlobalHalt if the API signals a key-level failure (401 or 403).
func Run(ctx context.Context, w *Worker, workerCount int) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		haltErr  error
		haltOnce sync.Once
	)

	var (
		statsCrawled     int64
		statsNotFound    int64
		statsRateLimited int64
		statsErrors      int64
		statsDiscoveries int64
	)

	// Periodic stats goroutine.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				depth, _ := queries.QueueDepth(ctx, w.pool)
				slog.Info("crawler stats",
					"crawled", atomic.LoadInt64(&statsCrawled),
					"not_found", atomic.LoadInt64(&statsNotFound),
					"rate_limited", atomic.LoadInt64(&statsRateLimited),
					"errors", atomic.LoadInt64(&statsErrors),
					"discoveries", atomic.LoadInt64(&statsDiscoveries),
					"queue_depth", depth,
				)
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				result, err := w.RunOnce(ctx)
				if err != nil {
					if errors.Is(err, ErrGlobalHalt) {
						haltOnce.Do(func() {
							haltErr = err
							cancel()
						})
					}
					return
				}
				if result.Status == "empty" {
					select {
					case <-time.After(30 * time.Second):
					case <-ctx.Done():
						return
					}
					continue
				}
				switch result.Status {
				case "success":
					atomic.AddInt64(&statsCrawled, 1)
					atomic.AddInt64(&statsDiscoveries, int64(result.Discoveries))
				case "not_found":
					atomic.AddInt64(&statsNotFound, 1)
				case "rate_limited":
					atomic.AddInt64(&statsRateLimited, 1)
				default:
					atomic.AddInt64(&statsErrors, 1)
				}
			}
		}()
	}

	wg.Wait()
	return haltErr
}

// RunN processes exactly total crawl targets across workerCount goroutines, then returns.
// Each non-empty RunOnce result (regardless of per-player outcome) counts toward total.
// When the queue is empty, workers sleep 30 seconds before retrying.
// Returns ErrGlobalHalt if the API signals a key-level failure (401 or 403).
func RunN(ctx context.Context, w *Worker, workerCount int, total int) error {
	remaining := int64(total)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		haltErr  error
		haltOnce sync.Once
	)

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				result, err := w.RunOnce(ctx)
				if err != nil {
					if errors.Is(err, ErrGlobalHalt) {
						haltOnce.Do(func() {
							haltErr = err
							cancel()
						})
					}
					return
				}
				if result.Status == "empty" {
					select {
					case <-time.After(30 * time.Second):
					case <-ctx.Done():
						return
					}
					continue
				}
				// A target was claimed and fully processed (any non-empty outcome).
				if atomic.AddInt64(&remaining, -1) <= 0 {
					cancel()
					return
				}
			}
		}()
	}

	wg.Wait()
	return haltErr
}
