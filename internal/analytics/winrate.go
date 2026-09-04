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

// BattleTypeFilter controls which battle types are included in a win-rate query.
// Friendly and challenge battles are excluded by default because they are non-competitive
// and produce structurally incomplete participant sets (some have fewer than 3 players
// per team due to forfeits). Using them in meta analysis biases win rates.
type BattleTypeFilter string

const (
	// BattleTypeRanked includes only battle_type='ranked' (team Ranked queue).
	// This is the default for all win-rate queries.
	BattleTypeRanked BattleTypeFilter = "ranked"

	// BattleTypeSoloRanked includes only battle_type='soloRanked' (solo queue Ranked).
	// Produces different win-rate distributions than BattleTypeRanked because team
	// coordination and pick patterns differ between the two queues.
	BattleTypeSoloRanked BattleTypeFilter = "soloRanked"

	// BattleTypeCompetitive includes both 'ranked' and 'soloRanked'. Use when you want
	// the union of competitive play without distinguishing queue type.
	BattleTypeCompetitive BattleTypeFilter = "competitive"

	// BattleTypeAny applies no battle_type filter. Includes friendly and challenge battles.
	// Intended for diagnostics only; friendly battles have incomplete participant sets.
	BattleTypeAny BattleTypeFilter = "any"
)

// ValidBattleType reports whether bt is a recognized BattleTypeFilter value.
func ValidBattleType(bt BattleTypeFilter) bool {
	switch bt {
	case BattleTypeRanked, BattleTypeSoloRanked, BattleTypeCompetitive, BattleTypeAny:
		return true
	}
	return false
}

// battleTypeSQL returns a SQL WHERE fragment for the given filter.
// Values are SQL literals (not parameters) because battle_type values are a fixed
// enum validated before this function is called -- no user input reaches the SQL directly.
func battleTypeSQL(bt BattleTypeFilter) string {
	switch bt {
	case BattleTypeRanked:
		return " AND b.battle_type = 'ranked'"
	case BattleTypeSoloRanked:
		return " AND b.battle_type = 'soloRanked'"
	case BattleTypeCompetitive:
		return " AND b.battle_type IN ('ranked', 'soloRanked')"
	default: // BattleTypeAny
		return ""
	}
}

// BrawlerWinRate holds per-brawler stats for a single mode query.
//
// Field labels:
//   - DIRECT: raw COUNT or SUM from the database with no further computation.
//   - DERIVED: a ratio computed from DIRECT fields.
type BrawlerWinRate struct {
	BrawlerID int
	Name      string
	Battles   int      // DIRECT: COUNT(DISTINCT battle_id) where this brawler appeared
	Slots     int      // DIRECT: COUNT(*) of eligible participant rows (wins+losses+draws); numerator for PickRate
	Wins      int      // DIRECT: participant rows on a 'victory' team
	Losses    int      // DIRECT: participant rows on a 'defeat' team
	Draws     int      // DIRECT: participant rows on a 'draw' team
	WinPct    *float64 // DERIVED: wins/(wins+losses), rounded to 3 decimal places; nil when wins+losses==0
	PickRate  *float64 // DERIVED: slots/total_slots, rounded to 4 decimal places; nil when total_slots==0
}

// WinRateParams configures a BrawlerWinRates query.
type WinRateParams struct {
	Mode                string
	Map                 *string          // nil means all maps combined
	BattleType          BattleTypeFilter // default BattleTypeRanked; never empty after validation
	BrawlerTrophyBucket *int16           // nil means no bucket filter (all tiers combined)
	MinBattles          int              // brawler must appear in at least this many distinct battles
}

// WinRateResult is the output of BrawlerWinRates.
type WinRateResult struct {
	SampleBattles int              // DIRECT: COUNT(DISTINCT b.id) of eligible battles in the filtered population
	TotalSlots    int              // DIRECT: COUNT(*) of eligible participant rows in the filtered population; denominator for PickRate
	Brawlers      []BrawlerWinRate // ordered by win_pct DESC NULLS LAST, then battles DESC
}

// BrawlerWinRates returns per-brawler win rates and pick rates for a mode.
//
// Eligible rows: bt.result IN ('victory','defeat','draw'), non-null brawler_id,
// and battle_type matching p.BattleType (default: 'ranked' only).
//
// Draws are reported separately and excluded from the WinPct denominator:
// WinPct = wins / (wins + losses).
//
// PickRate = slots / TotalSlots where TotalSlots is COUNT(*) of all eligible
// participant rows in the population. Slots can exceed Battles when the same
// brawler appears on both teams in a single battle.
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
	if p.BattleType == "" {
		p.BattleType = BattleTypeRanked
	}
	minBattles := p.MinBattles
	if minBattles < 1 {
		minBattles = 1
	}

	// Build dynamic WHERE fragments.
	// battle_type clause is a safe SQL literal (controlled enum, validated before call).
	// bucket and map clauses use positional parameters ($N) for user-supplied values.
	btClause := battleTypeSQL(p.BattleType)
	baseArgs := []any{p.Mode}

	bucketClause := ""
	if p.BrawlerTrophyBucket != nil {
		baseArgs = append(baseArgs, *p.BrawlerTrophyBucket)
		bucketClause = fmt.Sprintf(" AND bp.trophy_bucket = $%d", len(baseArgs))
	}

	mapClause := ""
	if p.Map != nil {
		baseArgs = append(baseArgs, *p.Map)
		mapClause = fmt.Sprintf(" AND b.event_map = $%d", len(baseArgs))
	}

	baseWhere := `
        WHERE b.event_mode = $1
          AND bp.brawler_id IS NOT NULL
          AND bt.result IN ('victory', 'defeat', 'draw')` +
		btClause + bucketClause + mapClause

	const baseJoins = `
        FROM battle_participants bp
        JOIN battle_teams bt ON bt.id = bp.team_id
        JOIN battles b       ON b.id  = bp.battle_id`

	// Count eligible battles (DIRECT) and total eligible participant slots (DIRECT)
	// in one scan. TotalSlots is the authoritative denominator for PickRate; the
	// relationship total_slots = sample_battles * players_per_battle is an integrity
	// check, not the definition.
	var sampleBattles, totalSlots int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT b.id), COUNT(*)`+baseJoins+baseWhere,
		baseArgs...,
	).Scan(&sampleBattles, &totalSlots); err != nil {
		return WinRateResult{}, fmt.Errorf("count eligible battles: %w", err)
	}

	// Per-brawler aggregation. minBattles is appended last.
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
			baseWhere+`
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
		// Slots = wins + losses + draws by identity (all result types sum to COUNT(*)).
		// This is algebraically equivalent to COUNT(*) for this brawler in the same WHERE clause.
		r.Slots = r.Wins + r.Losses + r.Draws
		if r.Wins+r.Losses > 0 {
			pct := math.Round(float64(r.Wins)/float64(r.Wins+r.Losses)*1000) / 1000
			r.WinPct = &pct
		}
		if totalSlots > 0 {
			rate := math.Round(float64(r.Slots)/float64(totalSlots)*10000) / 10000
			r.PickRate = &rate
		}
		brawlers = append(brawlers, r)
	}
	if err := rows.Err(); err != nil {
		return WinRateResult{}, fmt.Errorf("iterate brawler rows: %w", err)
	}

	return WinRateResult{SampleBattles: sampleBattles, TotalSlots: totalSlots, Brawlers: brawlers}, nil
}
