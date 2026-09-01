package queries

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentPatchID returns the ID of the patch marked is_current = TRUE.
// Returns 0 and a descriptive error if no current patch is set.
func CurrentPatchID(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var id int
	err := pool.QueryRow(ctx,
		`SELECT id FROM patches WHERE is_current = TRUE LIMIT 1`,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("query current patch: %w", err)
	}
	return id, nil
}
