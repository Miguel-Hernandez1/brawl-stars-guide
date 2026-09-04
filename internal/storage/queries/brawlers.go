package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertBrawler inserts or updates a brawler in the reference table.
// Keyed on Supercell's numeric brawler ID.
func UpsertBrawler(ctx context.Context, pool *pgxpool.Pool, id int, name, rarity, class string, isActive bool, rawData any) error {
	raw, err := json.Marshal(rawData)
	if err != nil {
		return fmt.Errorf("marshal brawler raw data: %w", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO brawlers (id, name, rarity, class, is_active, raw_data, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			name       = EXCLUDED.name,
			rarity     = EXCLUDED.rarity,
			class      = EXCLUDED.class,
			is_active  = EXCLUDED.is_active,
			raw_data   = EXCLUDED.raw_data,
			updated_at = EXCLUDED.updated_at
	`, id, name, rarity, class, isActive, raw, time.Now().UTC())

	if err != nil {
		return fmt.Errorf("upsert brawler %d: %w", id, err)
	}
	return nil
}

// BrawlerExists reports whether a brawler with the given ID exists in the table.
// Used during ingestion to validate participant brawler IDs.
func BrawlerExists(ctx context.Context, pool *pgxpool.Pool, id int) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM brawlers WHERE id = $1)`, id,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check brawler %d: %w", id, err)
	}
	return exists, nil
}

// EnsureRetiredBrawler guarantees a brawler row exists before a participant
// insert that would otherwise violate the brawler_id FK. Call this for any
// nonzero brawler_id that originated from a historical battle log.
//
// If the brawler is already in the table (active or retired), this is a
// no-op that preserves its existing is_active value. If it is absent, a
// placeholder row is inserted with is_active=false and name taken from the
// API response (or "BRAWLER_<id>" when the API returned an empty name for
// a retired brawler).
func EnsureRetiredBrawler(ctx context.Context, pool *pgxpool.Pool, id int, name string) error {
	n := name
	if n == "" {
		n = fmt.Sprintf("BRAWLER_%d", id)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO brawlers (id, name, is_active)
		VALUES ($1, $2, false)
		ON CONFLICT (id) DO NOTHING
	`, id, n)
	if err != nil {
		return fmt.Errorf("ensure retired brawler %d: %w", id, err)
	}
	return nil
}
