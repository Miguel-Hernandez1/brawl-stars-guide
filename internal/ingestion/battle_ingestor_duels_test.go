package ingestion

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
)

// TestDuelsBattleIngestion verifies that a Duels (tagTeam) battle with brawler.id=0
// is ingested without error and produces participant rows with brawler_id IS NULL.
// This is the regression guard for the FK violation that occurred when brawler_id=0
// was passed directly to InsertBattleParticipant (which references brawlers.id).
func TestDuelsBattleIngestion(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping Duels ingestion integration test")
	}

	ctx := context.Background()
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const discovererTag = "TESTDL01"
	const opponentTag = "TESTDL02"

	for _, tag := range []string{discovererTag, opponentTag} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (tag, name, first_seen_at) VALUES ($1, 'Test', NOW())
			 ON CONFLICT (tag) DO NOTHING`, tag); err != nil {
			t.Fatalf("insert player %s: %v", tag, err)
		}
	}

	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM battle_participants WHERE player_tag IN ('TESTDL01','TESTDL02')`)
		pool.Exec(bg, `DELETE FROM battles WHERE discovered_via_player_tag = 'TESTDL01' AND event_mode = 'tagTeam'`)
		pool.Exec(bg, `DELETE FROM events WHERE supercell_id = 99999901`)
		pool.Exec(bg, `DELETE FROM crawl_targets WHERE player_tag IN ('TESTDL01','TESTDL02')`)
		pool.Exec(bg, `DELETE FROM players WHERE tag IN ('TESTDL01','TESTDL02')`)
	})

	// Synthetic Duels battle payload mirroring real API responses:
	// - event.mode = "tagTeam" (Supercell's event category for Duels)
	// - battle.mode = "duels"
	// - battle.teams = nil, battle.players = 2 entries
	// - brawler.id=0 is the API sentinel for "multiple brawlers used, none attributable"
	entry := apiclient.BattleEntry{
		BattleTime: time.Now().UTC().Format("20060102T150405.000Z"),
		Event: apiclient.BattleEvent{
			ID:   99999901,
			Mode: "tagTeam",
			Map:  "No Surrender",
		},
		Battle: apiclient.BattleData{
			Mode:     "duels",
			Type:     "ranked",
			Result:   "victory",
			Duration: 97,
			Players: []apiclient.BattleParticipant{
				{
					Tag:  "#" + discovererTag,
					Name: "TestDuelist",
					Brawler: apiclient.BrawlerInBattle{
						ID: 0, Name: "", Power: 0, Trophies: 0,
					},
				},
				{
					Tag:  "#" + opponentTag,
					Name: "TestOpponent",
					Brawler: apiclient.BrawlerInBattle{
						ID: 0, Name: "", Power: 0, Trophies: 0,
					},
				},
			},
		},
	}

	ingestor := NewBattleIngestor(pool, discovererTag, BattleIngestorConfig{})
	result, err := ingestor.IngestBattle(ctx, entry)
	if err != nil {
		t.Fatalf("IngestBattle returned error (regression: brawler_id=0 must not violate FK): %v", err)
	}
	if !result.IsNew {
		t.Fatal("expected IsNew=true for first ingestion of this battle")
	}
	if !result.HasParticipantRows {
		t.Fatal("HasParticipantRows must be true: Duels participants (NULL brawler) should be written")
	}

	// Locate the inserted battle.
	var battleID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM battles WHERE discovered_via_player_tag = $1 AND event_mode = 'tagTeam'`,
		discovererTag,
	).Scan(&battleID); err != nil {
		t.Fatalf("lookup battle id: %v", err)
	}

	// Verify participants: brawler columns must be NULL, not 0.
	type participantRow struct {
		playerTag       string
		brawlerID       *int
		brawlerPower    *int
		brawlerTrophies *int
		trophyBucket    *int16
	}

	dbRows, err := pool.Query(ctx,
		`SELECT player_tag, brawler_id, brawler_power, brawler_trophies, trophy_bucket
		 FROM battle_participants WHERE battle_id = $1 ORDER BY player_tag`, battleID)
	if err != nil {
		t.Fatalf("query participants: %v", err)
	}
	defer dbRows.Close()

	var participants []participantRow
	for dbRows.Next() {
		var pr participantRow
		if err := dbRows.Scan(&pr.playerTag, &pr.brawlerID, &pr.brawlerPower, &pr.brawlerTrophies, &pr.trophyBucket); err != nil {
			t.Fatalf("scan: %v", err)
		}
		participants = append(participants, pr)
	}
	if err := dbRows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}
	dbRows.Close()

	if len(participants) != 2 {
		t.Fatalf("expected 2 participant rows, got %d", len(participants))
	}
	for _, pr := range participants {
		if pr.brawlerID != nil {
			t.Errorf("participant %s: brawler_id=%d, want NULL (sentinel 0 must not be stored as FK value)",
				pr.playerTag, *pr.brawlerID)
		}
		if pr.brawlerPower != nil {
			t.Errorf("participant %s: brawler_power=%d, want NULL", pr.playerTag, *pr.brawlerPower)
		}
		if pr.brawlerTrophies != nil {
			t.Errorf("participant %s: brawler_trophies=%d, want NULL", pr.playerTag, *pr.brawlerTrophies)
		}
		if pr.trophyBucket != nil {
			t.Errorf("participant %s: trophy_bucket=%d, want NULL", pr.playerTag, *pr.trophyBucket)
		}
	}

	// Idempotency: re-ingesting the same battle must not error.
	result2, err := ingestor.IngestBattle(ctx, entry)
	if err != nil {
		t.Fatalf("second IngestBattle returned error (idempotency regression): %v", err)
	}
	if result2.IsNew {
		t.Error("IsNew=true on second call; battle already existed")
	}
}
