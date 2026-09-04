package analytics

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
)

// TestIsIneligibleMode verifies the denylist of rank-based and attribution-free modes.
func TestIsIneligibleMode(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"soloShowdown", true},
		{"duoShowdown", true},
		{"trioShowdown", true},
		{"tagTeam", true},
		{"gemGrab", false},
		{"knockout", false},
		{"brawlBall", false},
		{"heist", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsIneligibleMode(tc.mode); got != tc.want {
			t.Errorf("IsIneligibleMode(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestBrawlerWinRates_IneligibleMode verifies that calling BrawlerWinRates with a
// showdown mode returns an error immediately without touching the DB.
func TestBrawlerWinRates_IneligibleMode(t *testing.T) {
	_, err := BrawlerWinRates(context.Background(), nil, WinRateParams{Mode: "soloShowdown"})
	if err == nil {
		t.Fatal("expected error for soloShowdown mode, got nil")
	}
}

// TestBrawlerWinRates_Integration inserts controlled test data into the DB and verifies
// that BrawlerWinRates returns correct counts, win_pct, and sample_battles.
//
// Test data: one 3v3 battle in mode "testWinRate99" with two synthetic brawlers.
//   - Team A (victory): TESTWRP1-TESTA, TESTWRP2-TESTB, TESTWRP3-TESTA
//   - Team B (defeat):  TESTWRP4-TESTB, TESTWRP5-TESTA, TESTWRP6-TESTB
//
// Expected after querying with min_battles=1:
//
//	TESTA: battles=1, wins=2 (WRP1+WRP3), losses=1 (WRP5), win_pct=0.667
//	TESTB: battles=1, wins=1 (WRP2), losses=2 (WRP4+WRP6), win_pct=0.333
func TestBrawlerWinRates_Integration(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping win-rate integration test")
	}

	ctx := context.Background()
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		testMode     = "testWinRate99"
		testEventSID = 99999810 // synthetic supercell event ID, guaranteed unused
		testBrawlerA = 99999811 // synthetic brawler A
		testBrawlerB = 99999812 // synthetic brawler B
		testFP       = "TESTWRFP001"
	)
	players := []string{"TESTWRP1", "TESTWRP2", "TESTWRP3", "TESTWRP4", "TESTWRP5", "TESTWRP6"}

	// Pre-clean any leftover state from a previous failed run.
	pool.Exec(ctx, `DELETE FROM battle_participants WHERE player_tag = ANY($1)`, players)
	pool.Exec(ctx, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = $1)`, testMode)
	pool.Exec(ctx, `DELETE FROM battles WHERE event_mode = $1`, testMode)
	pool.Exec(ctx, `DELETE FROM events WHERE supercell_id = $1`, testEventSID)
	pool.Exec(ctx, `DELETE FROM brawlers WHERE id IN ($1, $2)`, testBrawlerA, testBrawlerB)
	pool.Exec(ctx, `DELETE FROM players WHERE tag = ANY($1)`, players)

	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM battle_participants WHERE player_tag = ANY($1)`, players)
		pool.Exec(bg, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = $1)`, testMode)
		pool.Exec(bg, `DELETE FROM battles WHERE event_mode = $1`, testMode)
		pool.Exec(bg, `DELETE FROM events WHERE supercell_id = $1`, testEventSID)
		pool.Exec(bg, `DELETE FROM brawlers WHERE id IN ($1, $2)`, testBrawlerA, testBrawlerB)
		pool.Exec(bg, `DELETE FROM players WHERE tag = ANY($1)`, players)
	})

	// Insert players.
	for _, tag := range players {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (tag, name) VALUES ($1, 'Test') ON CONFLICT (tag) DO NOTHING`, tag,
		); err != nil {
			t.Fatalf("insert player %s: %v", tag, err)
		}
	}

	// Insert synthetic brawlers.
	if _, err := pool.Exec(ctx,
		`INSERT INTO brawlers (id, name, is_active) VALUES ($1, 'TESTA', true), ($2, 'TESTB', true)`,
		testBrawlerA, testBrawlerB,
	); err != nil {
		t.Fatalf("insert brawlers: %v", err)
	}

	// Insert event.
	var eventID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (supercell_id, mode, map_name) VALUES ($1, $2, 'Test Map') RETURNING id`,
		testEventSID, testMode,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Insert battle.
	var battleID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO battles
		   (fingerprint, battle_time, event_id, event_mode, event_map, battle_type, raw_battle_data)
		 VALUES ($1, NOW(), $2, $3, 'Test Map', 'ranked', '{}')
		 RETURNING id`,
		testFP, eventID, testMode,
	).Scan(&battleID); err != nil {
		t.Fatalf("insert battle: %v", err)
	}

	// Insert teams: team 0 = victory, team 1 = defeat.
	var teamAID, teamBID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO battle_teams (battle_id, team_index, result) VALUES ($1, 0, 'victory') RETURNING id`,
		battleID,
	).Scan(&teamAID); err != nil {
		t.Fatalf("insert team A: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO battle_teams (battle_id, team_index, result) VALUES ($1, 1, 'defeat') RETURNING id`,
		battleID,
	).Scan(&teamBID); err != nil {
		t.Fatalf("insert team B: %v", err)
	}

	// trophy_bucket 2 = 500-2000 trophies.
	insertP := func(teamID int64, tag string, brawlerID int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO battle_participants
			   (battle_id, team_id, player_tag, player_name,
			    brawler_id, brawler_power, brawler_trophies, is_star_player, trophy_bucket)
			 VALUES ($1, $2, $3, 'Test', $4, 10, 500, false, 2)`,
			battleID, teamID, tag, brawlerID,
		); err != nil {
			t.Fatalf("insert participant %s: %v", tag, err)
		}
	}

	// Team A (victory): WRP1=TESTA, WRP2=TESTB, WRP3=TESTA
	insertP(teamAID, "TESTWRP1", testBrawlerA)
	insertP(teamAID, "TESTWRP2", testBrawlerB)
	insertP(teamAID, "TESTWRP3", testBrawlerA)
	// Team B (defeat): WRP4=TESTB, WRP5=TESTA, WRP6=TESTB
	insertP(teamBID, "TESTWRP4", testBrawlerB)
	insertP(teamBID, "TESTWRP5", testBrawlerA)
	insertP(teamBID, "TESTWRP6", testBrawlerB)

	// Run the query with min_battles=1 to include both brawlers.
	result, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates: %v", err)
	}

	if result.SampleBattles != 1 {
		t.Errorf("SampleBattles = %d, want 1", result.SampleBattles)
	}
	// One 3v3 battle = 6 eligible participant rows. TotalSlots is COUNT(*), not sample_battles*6.
	if result.TotalSlots != 6 {
		t.Errorf("TotalSlots = %d, want 6", result.TotalSlots)
	}
	if len(result.Brawlers) != 2 {
		t.Fatalf("len(Brawlers) = %d, want 2", len(result.Brawlers))
	}

	// TESTA should be first (higher win_pct).
	a, b := result.Brawlers[0], result.Brawlers[1]
	if a.BrawlerID != testBrawlerA {
		t.Errorf("Brawlers[0] = %d (%s), want TESTA (%d)", a.BrawlerID, a.Name, testBrawlerA)
	}
	if b.BrawlerID != testBrawlerB {
		t.Errorf("Brawlers[1] = %d (%s), want TESTB (%d)", b.BrawlerID, b.Name, testBrawlerB)
	}

	// TESTA: 1 distinct battle, 2 wins (WRP1+WRP3), 1 loss (WRP5), 3 slots total.
	if a.Battles != 1 {
		t.Errorf("TESTA.Battles = %d, want 1", a.Battles)
	}
	if a.Wins != 2 {
		t.Errorf("TESTA.Wins = %d, want 2", a.Wins)
	}
	if a.Losses != 1 {
		t.Errorf("TESTA.Losses = %d, want 1", a.Losses)
	}
	if a.Draws != 0 {
		t.Errorf("TESTA.Draws = %d, want 0", a.Draws)
	}
	// Slots = wins + losses + draws (DIRECT count identity).
	if a.Slots != 3 {
		t.Errorf("TESTA.Slots = %d, want 3 (wins+losses+draws)", a.Slots)
	}
	if a.WinPct == nil {
		t.Fatal("TESTA.WinPct is nil")
	}
	wantA := math.Round(float64(2)/float64(3)*1000) / 1000
	if *a.WinPct != wantA {
		t.Errorf("TESTA.WinPct = %.4f, want %.4f", *a.WinPct, wantA)
	}
	// TESTA pick_rate = 3 slots / 6 total_slots = 0.5000.
	if a.PickRate == nil {
		t.Fatal("TESTA.PickRate is nil")
	}
	wantPickA := math.Round(float64(3)/float64(6)*10000) / 10000
	if *a.PickRate != wantPickA {
		t.Errorf("TESTA.PickRate = %.4f, want %.4f", *a.PickRate, wantPickA)
	}

	// TESTB: 1 distinct battle, 1 win (WRP2), 2 losses (WRP4+WRP6), 3 slots total.
	if b.Battles != 1 {
		t.Errorf("TESTB.Battles = %d, want 1", b.Battles)
	}
	if b.Wins != 1 {
		t.Errorf("TESTB.Wins = %d, want 1", b.Wins)
	}
	if b.Losses != 2 {
		t.Errorf("TESTB.Losses = %d, want 2", b.Losses)
	}
	if b.Slots != 3 {
		t.Errorf("TESTB.Slots = %d, want 3", b.Slots)
	}
	if b.WinPct == nil {
		t.Fatal("TESTB.WinPct is nil")
	}
	wantB := math.Round(float64(1)/float64(3)*1000) / 1000
	if *b.WinPct != wantB {
		t.Errorf("TESTB.WinPct = %.4f, want %.4f", *b.WinPct, wantB)
	}
	// TESTB pick_rate = 3/6 = 0.5000 (same as TESTA; both brawlers appear equally in the one battle).
	if b.PickRate == nil {
		t.Fatal("TESTB.PickRate is nil")
	}
	wantPickB := math.Round(float64(3)/float64(6)*10000) / 10000
	if *b.PickRate != wantPickB {
		t.Errorf("TESTB.PickRate = %.4f, want %.4f", *b.PickRate, wantPickB)
	}

	// Verify that min_battles=2 excludes both brawlers (each only appears in 1 distinct battle).
	result2, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		MinBattles: 2,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates min_battles=2: %v", err)
	}
	if len(result2.Brawlers) != 0 {
		t.Errorf("min_battles=2: expected 0 brawlers, got %d", len(result2.Brawlers))
	}
	// SampleBattles and TotalSlots reflect the eligible population, not the HAVING-filtered brawlers.
	if result2.SampleBattles != 1 {
		t.Errorf("min_battles=2: SampleBattles = %d, want 1", result2.SampleBattles)
	}
	if result2.TotalSlots != 6 {
		t.Errorf("min_battles=2: TotalSlots = %d, want 6", result2.TotalSlots)
	}

	// Verify bucket filter: bucket=2 should return same result; bucket=3 should return empty.
	b2 := int16(2)
	resultBucket, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:                testMode,
		BrawlerTrophyBucket: &b2,
		MinBattles:          1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates bucket=2: %v", err)
	}
	if len(resultBucket.Brawlers) != 2 {
		t.Errorf("bucket=2: expected 2 brawlers, got %d", len(resultBucket.Brawlers))
	}

	b3 := int16(3)
	resultBucket3, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:                testMode,
		BrawlerTrophyBucket: &b3,
		MinBattles:          1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates bucket=3: %v", err)
	}
	if len(resultBucket3.Brawlers) != 0 {
		t.Errorf("bucket=3: expected 0 brawlers, got %d", len(resultBucket3.Brawlers))
	}

	// Verify map filter: matching map returns both brawlers; wrong map returns none.
	matchMap := "Test Map"
	resultMap, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		Map:        &matchMap,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates map=Test Map: %v", err)
	}
	if len(resultMap.Brawlers) != 2 {
		t.Errorf("map=Test Map: expected 2 brawlers, got %d", len(resultMap.Brawlers))
	}

	wrongMap := "No Such Map"
	resultWrongMap, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		Map:        &wrongMap,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates map=No Such Map: %v", err)
	}
	if len(resultWrongMap.Brawlers) != 0 {
		t.Errorf("map=No Such Map: expected 0 brawlers, got %d", len(resultWrongMap.Brawlers))
	}

	// Verify battle_type filter: test data is 'ranked', so soloRanked returns empty.
	resultSR, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		BattleType: BattleTypeSoloRanked,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates battle_type=soloRanked: %v", err)
	}
	if len(resultSR.Brawlers) != 0 {
		t.Errorf("battle_type=soloRanked: expected 0 brawlers (data is ranked), got %d", len(resultSR.Brawlers))
	}

	// BattleTypeCompetitive includes ranked, so returns same 2 brawlers.
	resultComp, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		BattleType: BattleTypeCompetitive,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates battle_type=competitive: %v", err)
	}
	if len(resultComp.Brawlers) != 2 {
		t.Errorf("battle_type=competitive: expected 2 brawlers, got %d", len(resultComp.Brawlers))
	}
}

// TestBrawlerWinRates_FriendlyExclusion verifies that battles with battle_type='friendly'
// are excluded by the default (ranked) filter and included only with BattleTypeAny.
func TestBrawlerWinRates_FriendlyExclusion(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping friendly exclusion test")
	}

	ctx := context.Background()
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		testMode     = "testFriendly99"
		testEventSID = 99999820
		testBrawlerA = 99999821
		testBrawlerB = 99999822
		testFP       = "TESTWRFP002"
	)
	players := []string{"TESTWRFR1", "TESTWRFR2", "TESTWRFR3", "TESTWRFR4", "TESTWRFR5", "TESTWRFR6"}

	pool.Exec(ctx, `DELETE FROM battle_participants WHERE player_tag = ANY($1)`, players)
	pool.Exec(ctx, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = $1)`, testMode)
	pool.Exec(ctx, `DELETE FROM battles WHERE event_mode = $1`, testMode)
	pool.Exec(ctx, `DELETE FROM events WHERE supercell_id = $1`, testEventSID)
	pool.Exec(ctx, `DELETE FROM brawlers WHERE id IN ($1, $2)`, testBrawlerA, testBrawlerB)
	pool.Exec(ctx, `DELETE FROM players WHERE tag = ANY($1)`, players)

	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM battle_participants WHERE player_tag = ANY($1)`, players)
		pool.Exec(bg, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = $1)`, testMode)
		pool.Exec(bg, `DELETE FROM battles WHERE event_mode = $1`, testMode)
		pool.Exec(bg, `DELETE FROM events WHERE supercell_id = $1`, testEventSID)
		pool.Exec(bg, `DELETE FROM brawlers WHERE id IN ($1, $2)`, testBrawlerA, testBrawlerB)
		pool.Exec(bg, `DELETE FROM players WHERE tag = ANY($1)`, players)
	})

	for _, tag := range players {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (tag, name) VALUES ($1, 'Test') ON CONFLICT (tag) DO NOTHING`, tag,
		); err != nil {
			t.Fatalf("insert player %s: %v", tag, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO brawlers (id, name, is_active) VALUES ($1, 'TESTA', true), ($2, 'TESTB', true)`,
		testBrawlerA, testBrawlerB,
	); err != nil {
		t.Fatalf("insert brawlers: %v", err)
	}

	var eventID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (supercell_id, mode, map_name) VALUES ($1, $2, 'Friendly Map') RETURNING id`,
		testEventSID, testMode,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Insert a battle with battle_type='friendly' (not 'ranked').
	var battleID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO battles
		   (fingerprint, battle_time, event_id, event_mode, event_map, battle_type, raw_battle_data)
		 VALUES ($1, NOW(), $2, $3, 'Friendly Map', 'friendly', '{}')
		 RETURNING id`,
		testFP, eventID, testMode,
	).Scan(&battleID); err != nil {
		t.Fatalf("insert battle: %v", err)
	}

	var teamAID, teamBID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO battle_teams (battle_id, team_index, result) VALUES ($1, 0, 'victory') RETURNING id`,
		battleID,
	).Scan(&teamAID); err != nil {
		t.Fatalf("insert team A: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO battle_teams (battle_id, team_index, result) VALUES ($1, 1, 'defeat') RETURNING id`,
		battleID,
	).Scan(&teamBID); err != nil {
		t.Fatalf("insert team B: %v", err)
	}

	insertP := func(teamID int64, tag string, brawlerID int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO battle_participants
			   (battle_id, team_id, player_tag, player_name,
			    brawler_id, brawler_power, brawler_trophies, is_star_player, trophy_bucket)
			 VALUES ($1, $2, $3, 'Test', $4, 10, 500, false, 2)`,
			battleID, teamID, tag, brawlerID,
		); err != nil {
			t.Fatalf("insert participant %s: %v", tag, err)
		}
	}
	insertP(teamAID, "TESTWRFR1", testBrawlerA)
	insertP(teamAID, "TESTWRFR2", testBrawlerB)
	insertP(teamAID, "TESTWRFR3", testBrawlerA)
	insertP(teamBID, "TESTWRFR4", testBrawlerB)
	insertP(teamBID, "TESTWRFR5", testBrawlerA)
	insertP(teamBID, "TESTWRFR6", testBrawlerB)

	// Default (ranked): friendly battle is excluded -- SampleBattles=0, TotalSlots=0.
	resultRanked, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates ranked (default): %v", err)
	}
	if resultRanked.SampleBattles != 0 {
		t.Errorf("ranked: SampleBattles = %d, want 0 (friendly battle excluded)", resultRanked.SampleBattles)
	}
	if resultRanked.TotalSlots != 0 {
		t.Errorf("ranked: TotalSlots = %d, want 0 (friendly battle excluded)", resultRanked.TotalSlots)
	}
	if len(resultRanked.Brawlers) != 0 {
		t.Errorf("ranked: expected 0 brawlers, got %d", len(resultRanked.Brawlers))
	}

	// BattleTypeAny: friendly battle is included -- SampleBattles=1, TotalSlots=6.
	resultAny, err := BrawlerWinRates(ctx, pool, WinRateParams{
		Mode:       testMode,
		BattleType: BattleTypeAny,
		MinBattles: 1,
	})
	if err != nil {
		t.Fatalf("BrawlerWinRates battle_type=any: %v", err)
	}
	if resultAny.SampleBattles != 1 {
		t.Errorf("any: SampleBattles = %d, want 1", resultAny.SampleBattles)
	}
	if resultAny.TotalSlots != 6 {
		t.Errorf("any: TotalSlots = %d, want 6 (6 participant rows in the friendly battle)", resultAny.TotalSlots)
	}
	if len(resultAny.Brawlers) != 2 {
		t.Errorf("any: expected 2 brawlers, got %d", len(resultAny.Brawlers))
	}
	// Each brawler has 3 slots out of 6 total => pick_rate = 0.5000.
	for _, br := range resultAny.Brawlers {
		if br.Slots != 3 {
			t.Errorf("any: brawler %s Slots = %d, want 3", br.Name, br.Slots)
		}
		if br.PickRate == nil {
			t.Errorf("any: brawler %s PickRate is nil", br.Name)
		} else if *br.PickRate != 0.5 {
			t.Errorf("any: brawler %s PickRate = %.4f, want 0.5000", br.Name, *br.PickRate)
		}
	}
}
