package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertEvent ensures an event row exists and returns its surrogate ID.
// Uses ON CONFLICT on (supercell_id, mode, map_name) for idempotency.
func UpsertEvent(ctx context.Context, pool *pgxpool.Pool, supercellID int, mode, mapName string) (int, error) {
	var id int
	err := pool.QueryRow(ctx, `
		INSERT INTO events (supercell_id, mode, map_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (supercell_id, mode, map_name) DO UPDATE SET
			updated_at = NOW()
		RETURNING id
	`, supercellID, mode, mapName).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert event %d %s %q: %w", supercellID, mode, mapName, err)
	}
	return id, nil
}

// InsertBattleResult is returned by InsertBattle.
type InsertBattleResult struct {
	BattleID int64
	IsNew    bool // false if fingerprint already existed
}

// InsertBattle writes a battle record using the fingerprint for deduplication.
// Returns IsNew=false if this fingerprint was already in the database - in that case
// BattleID is the existing row's ID.
func InsertBattle(ctx context.Context, pool *pgxpool.Pool, p BattleParams) (InsertBattleResult, error) {
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO battles (
			fingerprint, battle_time, event_id, event_mode, event_map,
			battle_type, duration_seconds, star_player_tag, patch_id,
			trophy_change, trophy_change_player_tag,
			discovered_via_player_tag, raw_battle_data
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11,
			$12, $13
		)
		ON CONFLICT (fingerprint) DO NOTHING
		RETURNING id
	`,
		p.Fingerprint, p.BattleTime, p.EventID, p.EventMode, p.EventMap,
		p.BattleType, p.DurationSeconds, p.StarPlayerTag, p.PatchID,
		p.TrophyChange, p.TrophyChangePlayerTag,
		p.DiscoveredViaPlayerTag, p.RawBattleData,
	).Scan(&id)

	if err != nil {
		// pgx returns ErrNoRows when ON CONFLICT DO NOTHING fires with RETURNING clause
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err2 := battleIDByFingerprint(ctx, pool, p.Fingerprint)
			if err2 != nil {
				return InsertBattleResult{}, fmt.Errorf("lookup existing battle %s: %w", p.Fingerprint, err2)
			}
			return InsertBattleResult{BattleID: existing, IsNew: false}, nil
		}
		return InsertBattleResult{}, fmt.Errorf("insert battle %s: %w", p.Fingerprint, err)
	}
	return InsertBattleResult{BattleID: id, IsNew: true}, nil
}

func battleIDByFingerprint(ctx context.Context, pool *pgxpool.Pool, fp string) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, `SELECT id FROM battles WHERE fingerprint = $1`, fp).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

type BattleParams struct {
	Fingerprint            string
	BattleTime             time.Time
	EventID                *int
	EventMode              string
	EventMap               string
	BattleType             string
	DurationSeconds        *int
	StarPlayerTag          *string
	PatchID                *int
	TrophyChange           *int
	TrophyChangePlayerTag  *string
	DiscoveredViaPlayerTag *string
	RawBattleData          []byte
}

// InsertBattleTeam inserts one team record for a battle.
// Returns the team's surrogate ID.
func InsertBattleTeam(ctx context.Context, pool *pgxpool.Pool, battleID int64, teamIndex int, result string) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO battle_teams (battle_id, team_index, result)
		VALUES ($1, $2, $3)
		ON CONFLICT (battle_id, team_index) DO UPDATE SET result = EXCLUDED.result
		RETURNING id
	`, battleID, teamIndex, result).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert battle team %d/%d: %w", battleID, teamIndex, err)
	}
	return id, nil
}

// InsertBattleParticipant inserts one participant record.
// ON CONFLICT DO NOTHING because the same battle from a different player's log
// has identical participant data.
func InsertBattleParticipant(ctx context.Context, pool *pgxpool.Pool, p ParticipantParams) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO battle_participants (
			battle_id, team_id, player_tag, player_name,
			brawler_id, brawler_power, brawler_trophies,
			is_star_player, trophy_bucket
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (battle_id, player_tag) DO NOTHING
	`,
		p.BattleID, p.TeamID, p.PlayerTag, p.PlayerName,
		p.BrawlerID, p.BrawlerPower, p.BrawlerTrophies,
		p.IsStarPlayer, p.TrophyBucket,
	)
	if err != nil {
		return fmt.Errorf("insert participant %s in battle %d: %w", p.PlayerTag, p.BattleID, err)
	}
	return nil
}

type ParticipantParams struct {
	BattleID        int64
	TeamID          *int64 // nil for Solo Showdown (no team structure)
	PlayerTag       string
	PlayerName     string
	BrawlerID      int
	BrawlerPower   int
	BrawlerTrophies int
	IsStarPlayer   bool
	TrophyBucket   int16
}

// CountBattles returns the total number of battles in the database.
// Used for acceptance testing.
func CountBattles(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM battles`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count battles: %w", err)
	}
	return n, nil
}
