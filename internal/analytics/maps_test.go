package analytics

import (
	"context"
	"os"
	"testing"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
)

// TestMapList_IneligibleMode verifies that calling MapList with a showdown mode
// returns an error immediately without touching the DB.
func TestMapList_IneligibleMode(t *testing.T) {
	_, err := MapList(context.Background(), nil, MapListParams{Mode: "soloShowdown"})
	if err == nil {
		t.Fatal("expected error for soloShowdown mode, got nil")
	}
}

// TestMapList_Integration inserts two synthetic battles on different maps and verifies
// that MapList returns them ordered by battle count, with correct qualifying_brawlers.
//
// Test data: two battles in mode "testMapList99":
//   - Battle 1 on "Map Alpha": brawlers TESTMLA, TESTMLB (3 per team)
//   - Battle 2 on "Map Beta":  brawlers TESTMLA only (3 per team)
//
// Expected (ranked filter, min qualifying=10):
//   - "Map Alpha": battles=1, qualifying_brawlers=0 (neither reaches 10 battles)
//   - "Map Beta":  battles=1, qualifying_brawlers=0
//
// Ordering is by battles DESC; both have 1 battle so order may vary -- test checks
// that both maps appear and no empty map names are returned.
func TestMapList_Integration(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping map list integration test")
	}

	ctx := context.Background()
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		testMode      = "testMapList99"
		testEventSIDA = 99999830
		testEventSIDB = 99999831
		testBrawlerA  = 99999832
		testBrawlerB  = 99999833
		testFP1       = "TESTMLFP001"
		testFP2       = "TESTMLFP002"
	)
	players := []string{
		"TESTMLP1", "TESTMLP2", "TESTMLP3", "TESTMLP4", "TESTMLP5", "TESTMLP6",
		"TESTMLP7", "TESTMLP8", "TESTMLP9", "TESTMLP10", "TESTMLP11", "TESTMLP12",
	}

	pool.Exec(ctx, `DELETE FROM battle_participants WHERE player_tag = ANY($1)`, players)
	pool.Exec(ctx, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = $1)`, testMode)
	pool.Exec(ctx, `DELETE FROM battles WHERE event_mode = $1`, testMode)
	pool.Exec(ctx, `DELETE FROM events WHERE supercell_id IN ($1, $2)`, testEventSIDA, testEventSIDB)
	pool.Exec(ctx, `DELETE FROM brawlers WHERE id IN ($1, $2)`, testBrawlerA, testBrawlerB)
	pool.Exec(ctx, `DELETE FROM players WHERE tag = ANY($1)`, players)

	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM battle_participants WHERE player_tag = ANY($1)`, players)
		pool.Exec(bg, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = $1)`, testMode)
		pool.Exec(bg, `DELETE FROM battles WHERE event_mode = $1`, testMode)
		pool.Exec(bg, `DELETE FROM events WHERE supercell_id IN ($1, $2)`, testEventSIDA, testEventSIDB)
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
		`INSERT INTO brawlers (id, name, is_active) VALUES ($1, 'TESTMLA', true), ($2, 'TESTMLB', true)`,
		testBrawlerA, testBrawlerB,
	); err != nil {
		t.Fatalf("insert brawlers: %v", err)
	}

	insertBattle := func(eventSID int, mapName, fp string, playerSet []string, brawlerIDs []int) int64 {
		t.Helper()
		var evID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO events (supercell_id, mode, map_name) VALUES ($1, $2, $3)
			 ON CONFLICT (supercell_id, mode, map_name) DO UPDATE SET mode=EXCLUDED.mode
			 RETURNING id`,
			eventSID, testMode, mapName,
		).Scan(&evID); err != nil {
			t.Fatalf("insert event %s: %v", mapName, err)
		}

		var battleID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO battles
			   (fingerprint, battle_time, event_id, event_mode, event_map, battle_type, raw_battle_data)
			 VALUES ($1, NOW(), $2, $3, $4, 'ranked', '{}')
			 RETURNING id`,
			fp, evID, testMode, mapName,
		).Scan(&battleID); err != nil {
			t.Fatalf("insert battle %s: %v", mapName, err)
		}

		var teamAID, teamBID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO battle_teams (battle_id, team_index, result) VALUES ($1, 0, 'victory') RETURNING id`,
			battleID,
		).Scan(&teamAID); err != nil {
			t.Fatalf("insert team A for %s: %v", mapName, err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO battle_teams (battle_id, team_index, result) VALUES ($1, 1, 'defeat') RETURNING id`,
			battleID,
		).Scan(&teamBID); err != nil {
			t.Fatalf("insert team B for %s: %v", mapName, err)
		}

		for i, tag := range playerSet {
			brawlerID := brawlerIDs[i%len(brawlerIDs)]
			teamID := teamAID
			if i >= 3 {
				teamID = teamBID
			}
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
		return battleID
	}

	// Battle 1: Map Alpha - mix of both brawlers
	insertBattle(testEventSIDA, "Map Alpha", testFP1,
		players[:6], []int{testBrawlerA, testBrawlerB, testBrawlerA, testBrawlerB, testBrawlerA, testBrawlerB})
	// Battle 2: Map Beta - only brawler A
	insertBattle(testEventSIDB, "Map Beta", testFP2,
		players[6:], []int{testBrawlerA, testBrawlerA, testBrawlerA, testBrawlerA, testBrawlerA, testBrawlerA})

	result, err := MapList(ctx, pool, MapListParams{Mode: testMode})
	if err != nil {
		t.Fatalf("MapList: %v", err)
	}

	if len(result.Maps) != 2 {
		t.Fatalf("len(Maps) = %d, want 2", len(result.Maps))
	}

	// Neither map should have an empty name.
	for _, m := range result.Maps {
		if m.Map == "" {
			t.Errorf("empty map name in result: %+v", m)
		}
		if m.Battles < 1 {
			t.Errorf("map %q has battles=%d, want >= 1", m.Map, m.Battles)
		}
	}

	// Verify both expected maps are present.
	mapSet := make(map[string]MapEntry)
	for _, m := range result.Maps {
		mapSet[m.Map] = m
	}
	if _, ok := mapSet["Map Alpha"]; !ok {
		t.Errorf("Map Alpha not in result; got maps: %v", result.Maps)
	}
	if _, ok := mapSet["Map Beta"]; !ok {
		t.Errorf("Map Beta not in result; got maps: %v", result.Maps)
	}

	// qualifying_brawlers should be 0 for both: no brawler has >= 10 battles.
	for _, m := range result.Maps {
		if m.QualifyingBrawlers != 0 {
			t.Errorf("map %q: QualifyingBrawlers = %d, want 0 (only 1 battle each)", m.Map, m.QualifyingBrawlers)
		}
	}

	// BattleType=soloRanked should exclude ranked data: result should be empty.
	resultSR, err := MapList(ctx, pool, MapListParams{
		Mode:       testMode,
		BattleType: BattleTypeSoloRanked,
	})
	if err != nil {
		t.Fatalf("MapList soloRanked: %v", err)
	}
	if len(resultSR.Maps) != 0 {
		t.Errorf("soloRanked: expected 0 maps (data is ranked), got %d", len(resultSR.Maps))
	}
}
