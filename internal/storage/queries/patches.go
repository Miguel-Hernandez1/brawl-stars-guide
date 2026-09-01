package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PatchIDForTime returns the ID of the patch whose released_at is on or before t,
// i.e. the patch that was active when a battle or snapshot occurred.
// Returns nil (no error) if no patch predates t - callers should store NULL rather
// than attributing data to the wrong patch.
func PatchIDForTime(ctx context.Context, pool *pgxpool.Pool, t time.Time) (*int, error) {
	var id int
	err := pool.QueryRow(ctx,
		`SELECT id FROM patches WHERE released_at <= $1 ORDER BY released_at DESC LIMIT 1`, t,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query patch for time %s: %w", t.Format(time.RFC3339), err)
	}
	return &id, nil
}
