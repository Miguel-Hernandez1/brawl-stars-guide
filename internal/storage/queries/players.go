package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertPlayer ensures a player row exists with the given tag and name.
// On conflict (duplicate tag), updates the name if it changed.
func UpsertPlayer(ctx context.Context, pool *pgxpool.Pool, tag, name string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO players (tag, name, first_seen_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (tag) DO UPDATE SET name = EXCLUDED.name
	`, tag, name)
	if err != nil {
		return fmt.Errorf("upsert player %s: %w", tag, err)
	}
	return nil
}

// UpdatePlayerCrawled marks a player as just-crawled and increments crawl_count.
func UpdatePlayerCrawled(ctx context.Context, pool *pgxpool.Pool, tag string) error {
	_, err := pool.Exec(ctx, `
		UPDATE players
		SET last_crawled_at = NOW(),
		    crawl_count     = crawl_count + 1
		WHERE tag = $1
	`, tag)
	if err != nil {
		return fmt.Errorf("update crawled %s: %w", tag, err)
	}
	return nil
}

// InsertPlayerSnapshot writes an immutable player snapshot.
// Returns the newly created snapshot ID.
func InsertPlayerSnapshot(ctx context.Context, pool *pgxpool.Pool, p PlayerSnapshotParams) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO player_snapshots (
			player_tag, snapshot_at, patch_id,
			trophies, highest_trophies, exp_level,
			three_v_three_wins, solo_victories, duo_victories,
			club_tag, club_name, trophy_bucket, raw_data
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7, $8, $9,
			$10, $11, $12, $13
		)
		ON CONFLICT (player_tag, snapshot_at) DO NOTHING
		RETURNING id
	`,
		p.PlayerTag, p.SnapshotAt, p.PatchID,
		p.Trophies, p.HighestTrophies, p.ExpLevel,
		p.ThreeVThreeWins, p.SoloVictories, p.DuoVictories,
		p.ClubTag, p.ClubName, p.TrophyBucket, p.RawData,
	).Scan(&id)

	if err != nil {
		// ON CONFLICT DO NOTHING fires when the same player is collected twice
		// within the same second - extremely rare. Return 0 to signal "already existed".
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("insert player snapshot for %s: %w", p.PlayerTag, err)
	}
	return id, nil
}

type PlayerSnapshotParams struct {
	PlayerTag       string
	SnapshotAt      time.Time
	PatchID         *int
	Trophies        int
	HighestTrophies int
	ExpLevel        int
	ThreeVThreeWins int
	SoloVictories   int
	DuoVictories    int
	ClubTag         *string
	ClubName        *string
	TrophyBucket    int16
	RawData         []byte
}

// InsertPlayerBrawlerSnapshot writes one brawler entry for a player snapshot.
// Multiple calls are expected per snapshot (one per brawler owned).
func InsertPlayerBrawlerSnapshot(ctx context.Context, pool *pgxpool.Pool, p BrawlerSnapshotParams) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO player_brawler_snapshots (
			snapshot_id, brawler_id, power, rank,
			trophies, highest_trophies, star_powers, gadgets, gears
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (snapshot_id, brawler_id) DO NOTHING
	`,
		p.SnapshotID, p.BrawlerID, p.Power, p.Rank,
		p.Trophies, p.HighestTrophies, p.StarPowerIDs, p.GadgetIDs, p.Gears,
	)
	if err != nil {
		return fmt.Errorf("insert brawler snapshot %d for snapshot %d: %w", p.BrawlerID, p.SnapshotID, err)
	}
	return nil
}

type BrawlerSnapshotParams struct {
	SnapshotID      int64
	BrawlerID       int
	Power           int
	Rank            int
	Trophies        int
	HighestTrophies int
	StarPowerIDs    []int32
	GadgetIDs       []int32
	Gears           []byte // JSONB
}

// EnqueueDiscoveredPlayer adds a newly discovered player tag to the crawl queue.
// No-ops if the player is already in the queue.
// trophyEstimate and trophyBucket should be nil for battle-discovered players:
// we only know brawler trophies at that point, not the player's total. The
// profile fetch will fill these in when the player is crawled.
func EnqueueDiscoveredPlayer(ctx context.Context, pool *pgxpool.Pool, tag, name, discoverySource, discoveryVia string, trophyEstimate *int, trophyBucket *int16) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO players (tag, name, first_seen_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (tag) DO NOTHING
	`, tag, name)
	if err != nil {
		return fmt.Errorf("upsert discovered player %s: %w", tag, err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO crawl_targets (
			player_tag, priority, discovery_source, discovery_via_player,
			trophy_estimate, trophy_bucket_at_discovery, next_crawl_at
		) VALUES ($1, 5, $2, $3, $4, $5, NOW())
		ON CONFLICT (player_tag) DO NOTHING
	`, tag, discoverySource, discoveryVia, trophyEstimate, trophyBucket)
	if err != nil {
		return fmt.Errorf("enqueue discovered player %s: %w", tag, err)
	}
	return nil
}
