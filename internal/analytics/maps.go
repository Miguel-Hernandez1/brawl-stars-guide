package analytics

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MapEntry is one row in a MapListResult.
type MapEntry struct {
	Map                string
	Battles            int // distinct eligible battles on this map
	QualifyingBrawlers int // brawlers with >= 10 distinct battles on this map (matches win-rate default)
}

// MapListParams configures a MapList query.
type MapListParams struct {
	Mode       string
	BattleType BattleTypeFilter // default BattleTypeRanked
}

// MapListResult is the output of MapList.
type MapListResult struct {
	Maps []MapEntry // ordered by battles DESC
}

// MapList returns all maps for the given mode that have at least one eligible battle,
// sorted by battle count descending. Empty map names (from early collection before
// event_map was captured reliably) are excluded.
//
// QualifyingBrawlers reflects the count at min_battles=10, matching the default for
// GET /v1/meta/winrates so callers can predict whether a win-rate query will return data.
func MapList(ctx context.Context, pool *pgxpool.Pool, p MapListParams) (MapListResult, error) {
	if IsIneligibleMode(p.Mode) {
		return MapListResult{}, fmt.Errorf(
			"mode %q uses rank-based outcomes or has no brawler attribution; map list not available",
			p.Mode,
		)
	}
	if p.BattleType == "" {
		p.BattleType = BattleTypeRanked
	}

	btClause := battleTypeSQL(p.BattleType)

	// Two CTEs:
	// 1. brawler_map: per-brawler battle count on each map, filtered to eligible battles.
	//    Used to derive qualifying_brawlers (brawlers with >= 10 distinct battles).
	// 2. qualifying: maps to qualifying brawler count.
	//
	// Main query: distinct battles per map, joined to qualifying brawler count.
	query := `
        WITH brawler_map AS (
          SELECT bp.brawler_id, b.event_map, COUNT(DISTINCT b.id) AS cnt
          FROM battle_participants bp
          JOIN battle_teams bt ON bt.id = bp.team_id
          JOIN battles b       ON b.id  = bp.battle_id
          WHERE b.event_mode = $1
            AND b.event_map  != ''
            AND bp.brawler_id IS NOT NULL
            AND bt.result IN ('victory', 'defeat', 'draw')` +
		btClause + `
          GROUP BY bp.brawler_id, b.event_map
        ),
        qualifying AS (
          SELECT event_map, COUNT(*) AS qualifying_brawlers
          FROM brawler_map
          WHERE cnt >= 10
          GROUP BY event_map
        )
        SELECT
          b.event_map,
          COUNT(DISTINCT b.id)                          AS battles,
          COALESCE(q.qualifying_brawlers, 0)            AS qualifying_brawlers
        FROM battles b
        JOIN battle_teams bt ON bt.battle_id = b.id
        LEFT JOIN qualifying q ON q.event_map = b.event_map
        WHERE b.event_mode = $1
          AND b.event_map  != ''
          AND bt.result IN ('victory', 'defeat', 'draw')` +
		btClause + `
        GROUP BY b.event_map, q.qualifying_brawlers
        ORDER BY COUNT(DISTINCT b.id) DESC`

	rows, err := pool.Query(ctx, query, p.Mode)
	if err != nil {
		return MapListResult{}, fmt.Errorf("map list query: %w", err)
	}
	defer rows.Close()

	maps := make([]MapEntry, 0)
	for rows.Next() {
		var e MapEntry
		if err := rows.Scan(&e.Map, &e.Battles, &e.QualifyingBrawlers); err != nil {
			return MapListResult{}, fmt.Errorf("scan map row: %w", err)
		}
		maps = append(maps, e)
	}
	if err := rows.Err(); err != nil {
		return MapListResult{}, fmt.Errorf("iterate map rows: %w", err)
	}

	return MapListResult{Maps: maps}, nil
}
