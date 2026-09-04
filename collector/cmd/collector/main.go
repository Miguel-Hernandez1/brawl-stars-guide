// collector is the Brawl Stars Playbook data collection binary.
//
// Usage:
//
//	collector collect player <tag>   - fetch and ingest one player's profile + battle log
//	collector seed brawlers          - populate the brawlers reference table from the API
//	collector repair showdown        - write participant rows for deferred Solo Showdown battles
//	collector migrate up             - apply all pending database migrations
//	collector migrate down           - roll back the last migration
//	collector crawl-once <N>         - process exactly N crawl targets, then exit
//	collector crawl                  - continuous crawl (requires explicit operator approval)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/crawler"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/domain"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ingestion"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ratelimit"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]

	// crawl-once and crawl are top-level commands (no sub-argument required for dispatch).
	switch cmd {
	case "crawl-once":
		n := 5
		if len(os.Args) >= 3 {
			v, err := strconv.Atoi(os.Args[2])
			if err != nil || v < 1 {
				log.Fatalf("crawl-once: N must be a positive integer, got %q", os.Args[2])
			}
			n = v
		}
		runCrawlOnce(n)
		return

	case "crawl":
		log.Fatal("continuous crawl is not yet enabled; run 'crawl-once N=5' and verify results first")
	}

	// All other commands require a subcommand.
	if len(os.Args) < 3 {
		usage()
		os.Exit(1)
	}
	sub := os.Args[2]

	switch cmd {
	case "collect":
		switch sub {
		case "player":
			if len(os.Args) < 4 {
				log.Fatal("usage: collector collect player <tag>")
			}
			runCollectPlayer(os.Args[3])
		default:
			log.Fatalf("unknown collect subcommand: %s", sub)
		}

	case "seed":
		switch sub {
		case "brawlers":
			runSeedBrawlers()
		default:
			log.Fatalf("unknown seed subcommand: %s", sub)
		}

	case "repair":
		switch sub {
		case "showdown":
			runRepairShowdown()
		default:
			log.Fatalf("unknown repair subcommand: %s", sub)
		}

	case "migrate":
		switch sub {
		case "up":
			runMigrate("up")
		case "down":
			runMigrate("down")
		default:
			log.Fatalf("unknown migrate subcommand: %s (use 'up' or 'down')", sub)
		}

	default:
		log.Fatalf("unknown command: %s", cmd)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Brawl Stars Playbook collector

Commands:
  collect player <tag>   Ingest a player's profile and battle log
  seed brawlers          Populate the brawlers reference table
  repair showdown        Write participant rows for deferred Solo Showdown battles
  migrate up             Apply pending database migrations
  migrate down           Roll back the last migration
  crawl-once [N]         Process exactly N crawl targets (default 5), then exit
  crawl                  Continuous crawl until SIGINT/SIGTERM (requires gate approval)

Environment:
  BRAWLSTARS_API_TOKEN   Required for API calls
  DATABASE_URL           PostgreSQL DSN (default: postgres://playbook:playbook@localhost:5432/playbook?sslmode=disable)
  MIGRATIONS_PATH        Path to SQL migration files (default: db/migrations)
  CRAWLER_WORKERS        Worker goroutine count (default: 3)
  CRAWLER_RATE_LIMIT     API requests per minute (default: 80)`)
}

// runCollectPlayer fetches and ingests one player's profile + battle log.
func runCollectPlayer(rawTag string) {
	ctx := context.Background()
	client := mustAPIClient()
	pool := mustDBPool(ctx)
	defer pool.Close()

	tag := apiclient.NormalizeTag(rawTag)
	log.Printf("collecting player %s", tag)

	// Fetch profile.
	profile, err := client.GetPlayer(ctx, tag)
	if err != nil {
		if apiclient.IsNotFound(err) {
			log.Fatalf("player %s not found", tag)
		}
		log.Fatalf("get player: %v", err)
	}

	// Ingest profile snapshot.
	snapshotID, err := ingestion.IngestPlayer(ctx, pool, profile)
	if err != nil {
		log.Fatalf("ingest player: %v", err)
	}
	if snapshotID > 0 {
		log.Printf("player snapshot created: id=%d trophies=%d brawlers=%d",
			snapshotID, profile.Trophies, len(profile.Brawlers))
	} else {
		log.Printf("player snapshot already existed for this second; skipped")
	}

	// Fetch battle log.
	battleLog, err := client.GetBattleLog(ctx, tag)
	if err != nil {
		log.Fatalf("get battle log: %v", err)
	}
	log.Printf("battle log: %d entries", len(battleLog.Items))

	// Ingest battles.
	ingestor := ingestion.NewBattleIngestor(pool, tag, ingestion.BattleIngestorConfig{})
	newBattles, skipped, errored, totalDiscoveries := 0, 0, 0, 0
	for i, entry := range battleLog.Items {
		result, err := ingestor.IngestBattle(ctx, entry)
		if err != nil {
			// Log and continue so one bad entry doesn't abort the whole log.
			log.Printf("warning: battle %d (%s): %v", i, entry.BattleTime, err)
			errored++
			continue
		}
		if result.IsNew {
			newBattles++
		} else {
			skipped++
		}
		totalDiscoveries += len(result.NewDiscoveries)
	}

	log.Printf("battles: %d new, %d already existed, %d unrecognized, %d new players discovered",
		newBattles, skipped, errored, totalDiscoveries)
}

// runSeedBrawlers fetches all brawlers from the API and upserts them into the reference table.
func runSeedBrawlers() {
	ctx := context.Background()
	client := mustAPIClient()
	pool := mustDBPool(ctx)
	defer pool.Close()

	log.Println("fetching brawler list from API...")
	resp, err := client.GetBrawlers(ctx)
	if err != nil {
		log.Fatalf("get brawlers: %v", err)
	}

	for _, b := range resp.Items {
		if err := queries.UpsertBrawler(ctx, pool, b.ID, b.Name, "", "", true, b); err != nil {
			log.Printf("warning: upsert brawler %d %s: %v", b.ID, b.Name, err)
			continue
		}
	}
	log.Printf("seeded %d brawlers", len(resp.Items))
}

// runMigrate applies or rolls back migrations.
// Expects to be run from the project root (where db/migrations lives).
func runMigrate(direction string) {
	dsn := mustEnv("DATABASE_URL", "postgres://playbook:playbook@localhost:5432/playbook?sslmode=disable")
	migrationsPath := mustEnv("MIGRATIONS_PATH", "db/migrations")

	// golang-migrate pgx/v5 driver DSN uses the pgx:// scheme.
	pgxDSN := strings.Replace(dsn, "postgres://", "pgx5://", 1)
	pgxDSN = strings.Replace(pgxDSN, "postgresql://", "pgx5://", 1)

	m, err := migrate.New("file://"+migrationsPath, pgxDSN)
	if err != nil {
		log.Fatalf("create migrator: %v", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			log.Printf("migration source close: %v", srcErr)
		}
		if dbErr != nil {
			log.Printf("migration db close: %v", dbErr)
		}
	}()

	switch direction {
	case "up":
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			log.Fatalf("migrate up: %v", err)
		}
		log.Println("migrations applied")
	case "down":
		if err := m.Steps(-1); err != nil {
			log.Fatalf("migrate down: %v", err)
		}
		log.Println("last migration rolled back")
	}
}

// mustAPIClient creates an API client from env, fataling if token is missing.
func mustAPIClient() *apiclient.Client {
	token := os.Getenv("BRAWLSTARS_API_TOKEN")
	if token == "" {
		log.Fatal("BRAWLSTARS_API_TOKEN is not set - see .env.example")
	}
	return apiclient.New(token)
}

// mustDBPool creates a pgxpool connection, fataling on error.
func mustDBPool(ctx context.Context) *pgxpool.Pool {
	dsn := mustEnv("DATABASE_URL", "postgres://playbook:playbook@localhost:5432/playbook?sslmode=disable")
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	return pool
}

// runRepairShowdown writes participant rows for Solo Showdown battles that have none.
// Uses raw_battle_data already stored in the battles table - no API calls needed.
// Safe to run multiple times: InsertBattleParticipant uses ON CONFLICT DO NOTHING.
func runRepairShowdown() {
	ctx := context.Background()
	pool := mustDBPool(ctx)
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT b.id, b.raw_battle_data
		FROM battles b
		WHERE b.event_mode = 'soloShowdown'
		  AND NOT EXISTS (SELECT 1 FROM battle_participants bp WHERE bp.battle_id = b.id)
		ORDER BY b.id
	`)
	if err != nil {
		log.Fatalf("query deferred showdown battles: %v", err)
	}
	defer rows.Close()

	type deferred struct {
		battleID int64
		rawJSON  []byte
	}
	var battles []deferred
	for rows.Next() {
		var d deferred
		if err := rows.Scan(&d.battleID, &d.rawJSON); err != nil {
			log.Fatalf("scan row: %v", err)
		}
		battles = append(battles, d)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("iterate rows: %v", err)
	}

	log.Printf("found %d Solo Showdown battles with no participant rows", len(battles))

	repaired, inserted := 0, 0
	for _, b := range battles {
		var entry apiclient.BattleEntry
		if err := json.Unmarshal(b.rawJSON, &entry); err != nil {
			log.Printf("warning: battle %d: unmarshal: %v", b.battleID, err)
			continue
		}
		for _, p := range entry.Battle.Players {
			normalTag := apiclient.NormalizeTag(p.Tag)
			bucket := domain.BucketForTrophies(p.Brawler.Trophies)
			if err := queries.InsertBattleParticipant(ctx, pool, queries.ParticipantParams{
				BattleID:        b.battleID,
				TeamID:          nil,
				PlayerTag:       normalTag,
				PlayerName:      p.Name,
				BrawlerID:       p.Brawler.ID,
				BrawlerPower:    p.Brawler.Power,
				BrawlerTrophies: p.Brawler.Trophies,
				IsStarPlayer:    false,
				TrophyBucket:    int16(bucket),
			}); err != nil {
				log.Printf("warning: battle %d participant %s: %v", b.battleID, normalTag, err)
				continue
			}
			inserted++
		}
		repaired++
	}

	log.Printf("repaired %d battles, inserted %d participant rows", repaired, inserted)
}

// runCrawlOnce processes exactly n crawl targets across the configured worker pool, then exits.
func runCrawlOnce(n int) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := mustAPIClient()
	pool := mustDBPool(ctx)
	defer pool.Close()

	workers := mustEnvInt("CRAWLER_WORKERS", 3)
	rateLimit := mustEnvInt("CRAWLER_RATE_LIMIT", 80)
	limiter := ratelimit.New(rateLimit)
	w := crawler.NewWorker(pool, client, limiter)

	log.Printf("crawl-once: processing %d targets with %d workers at %d req/min", n, workers, rateLimit)
	if err := crawler.RunN(ctx, w, workers, n); err != nil {
		log.Fatalf("crawl-once: %v", err)
	}
	log.Printf("crawl-once: done")
}

// mustEnv returns the env var value or the provided default.
func mustEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// mustEnvInt returns the env var parsed as an int, or defaultVal if unset or unparseable.
func mustEnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Printf("warning: %s=%q is not a positive integer; using default %d", key, v, defaultVal)
		return defaultVal
	}
	return n
}
