package analytics

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ineligibleModes are modes that do not store binary victory/defeat results.
// soloShowdown/duoShowdown/trioShowdown are rank-based; tagTeam (Duels) has no brawler
// attribution. Win-rate queries against these modes are rejected.
var ineligibleModes = map[string]bool{
	"soloShowdown": true,
	"duoShowdown":  true,
	"trioShowdown": true,
	"tagTeam":      true,
}

// IsIneligibleMode reports whether mode cannot support binary win-rate calculation.
func IsIneligibleMode(mode string) bool {
	return ineligibleModes[mode]
}

// BrawlerWinRate holds per-brawler win-rate data for a single mode query.
type BrawlerWinRate struct {
	BrawlerID int
	Name      string
	Battles   int      // COUNT(DISTINCT battle_id) where this brawler appeared
	Wins      int      // participant rows on a 'victory' team
	Losses    int      // participant rows on a 'defeat' team
	Draws     int      // participant rows on a 'draw' team
	WinPct    *float64 // wins/(wins+losses), rounded to 3 decimal places; nil when wins+losses==0
}

// WinRateParams configures a BrawlerWinRates query.
type WinRateParams struct {
	Mode                string
	BrawlerTrophyBucket *int16 // nil means no bucket filter (all tiers combined)
	MinBattles          int    // brawler must appear in at least this many distinct battles
}

// WinRateResult is the output of BrawlerWinRates.
type WinRateResult struct {
	SampleBattles int              // distinct eligible battles in the filtered population
	Brawlers      []BrawlerWinRate // ordered by win_pct DESC NULLS LAST, then battles DESC
}

// BrawlerWinRates returns per-brawler win rates for a 3v3 mode.
//
// Eligible rows are those with bt.result IN ('victory','defeat','draw') and a non-null
// brawler_id. Draws are reported separately and excluded from the win_pct denominator:
// win_pct = wins / (wins + losses).
//
// Returns an error for rank-based or attribution-free modes (soloShowdown, duoShowdown,
// trioShowdown, tagTeam).
func BrawlerWinRates(ctx context.Context, pool *pgxpool.Pool, p WinRateParams) (WinRateResult, error) {
	if IsIneligibleMode(p.Mode) {
		return WinRateResult{}, fmt.Errorf(
			"mode %q uses rank-based outcomes or has no brawler attribution; binary win-rate not available",
			p.Mode,
		)
	}
	minBattles := p.MinBattles
	if minBattles < 1 {
		minBattles = 1
	}

	// baseArgs and bucketClause are shared between the count and main queries.
	// $1 is always mode; $2 is brawler_trophy_bucket when provided.
	baseArgs := []any{p.Mode}
	bucketClause := ""
	if p.BrawlerTrophyBucket != nil {
		baseArgs = append(baseArgs, *p.BrawlerTrophyBucket)
		bucketClause = " AND bp.trophy_bucket = $2"
	}

	const baseWhere = `
        WHERE b.event_mode = $1
          AND bp.brawler_id IS NOT NULL
          AND bt.result IN ('victory', 'defeat', 'draw')`

	const baseJoins = `
        FROM battle_participants bp
        JOIN battle_teams bt ON bt.id = bp.team_id
        JOIN battles b       ON b.id  = bp.battle_id`

	// Count distinct eligible battles in the filtered population.
	// This is the denominator for sample_battles in the API response.
	var sampleBattles int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT b.id)`+baseJoins+baseWhere+bucketClause,
		baseArgs...,
	).Scan(&sampleBattles); err != nil {
		return WinRateResult{}, fmt.Errorf("count eligible battles: %w", err)
	}

	// Per-brawler aggregation. minBattles is the last argument.
	mainArgs := make([]any, len(baseArgs)+1)
	copy(mainArgs, baseArgs)
	mainArgs[len(baseArgs)] = minBattles
	havingN := fmt.Sprintf("$%d", len(mainArgs))

	rows, err := pool.Query(ctx,
		`SELECT
           br.id,
           br.name,
           COUNT(DISTINCT b.id)                                                    AS battles,
           SUM(CASE WHEN bt.result = 'victory' THEN 1 ELSE 0 END)                 AS wins,
           SUM(CASE WHEN bt.result = 'defeat'  THEN 1 ELSE 0 END)                 AS losses,
           SUM(CASE WHEN bt.result = 'draw'    THEN 1 ELSE 0 END)                 AS draws`+
			baseJoins+`
        JOIN brawlers br ON br.id = bp.brawler_id`+
			baseWhere+bucketClause+`
        GROUP BY br.id, br.name
        HAVING COUNT(DISTINCT b.id) >= `+havingN+`
        ORDER BY
          SUM(CASE WHEN bt.result = 'victory' THEN 1.0 ELSE 0 END)
          / NULLIF(SUM(CASE WHEN bt.result IN ('victory', 'defeat') THEN 1 ELSE 0 END), 0)
          DESC NULLS LAST,
          COUNT(DISTINCT b.id) DESC`,
		mainArgs...,
	)
	if err != nil {
		return WinRateResult{}, fmt.Errorf("brawler win rate query: %w", err)
	}
	defer rows.Close()

	brawlers := make([]BrawlerWinRate, 0)
	for rows.Next() {
		var r BrawlerWinRate
		if err := rows.Scan(&r.BrawlerID, &r.Name, &r.Battles, &r.Wins, &r.Losses, &r.Draws); err != nil {
			return WinRateResult{}, fmt.Errorf("scan brawler row: %w", err)
		}
		if r.Wins+r.Losses > 0 {
			pct := math.Round(float64(r.Wins)/float64(r.Wins+r.Losses)*1000) / 1000
			r.WinPct = &pct
		}
		brawlers = append(brawlers, r)
	}
	if err := rows.Err(); err != nil {
		return WinRateResult{}, fmt.Errorf("iterate brawler rows: %w", err)
	}

	return WinRateResult{SampleBattles: sampleBattles, Brawlers: brawlers}, nil
}
