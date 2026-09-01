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
