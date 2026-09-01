// collector is the Brawl Stars Playbook data collection binary.
//
// Usage:
//
//	collector collect player <tag>   - fetch and ingest one player's profile + battle log
//	collector seed brawlers          - populate the brawlers reference table from the API
//	collector migrate up             - apply all pending database migrations
//	collector migrate down           - roll back the last migration
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/ingestion"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(1)
	}

	cmd, sub := os.Args[1], os.Args[2]

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
  migrate up             Apply pending database migrations
  migrate down           Roll back the last migration

Environment:
  BRAWLSTARS_API_TOKEN   Required for API calls
  DATABASE_URL           PostgreSQL DSN (default: postgres://playbook:playbook@localhost:5432/playbook?sslmode=disable)
  MIGRATIONS_PATH        Path to SQL migration files (default: db/migrations)`)
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
	ingestor := ingestion.NewBattleIngestor(pool, tag)
	newBattles, skipped, totalDiscoveries := 0, 0, 0
	for i, entry := range battleLog.Items {
		result, err := ingestor.IngestBattle(ctx, entry)
		if err != nil {
			// Log and continue - a single bad entry shouldn't abort the entire log.
			log.Printf("warning: battle %d (%s): %v", i, entry.BattleTime, err)
			continue
		}
		if result.IsNew {
			newBattles++
		} else {
			skipped++
		}
		totalDiscoveries += len(result.NewDiscoveries)
	}

	log.Printf("battles: %d new, %d already existed, %d new players discovered",
		newBattles, skipped, totalDiscoveries)
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

// mustEnv returns the env var value or the provided default.
func mustEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
