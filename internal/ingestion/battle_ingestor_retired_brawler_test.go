package ingestion

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
)

// TestRetiredBrawlerIngestion verifies that a 3v3 battle containing a nonzero
// brawler_id absent from the brawlers table is ingested without error. The
// invariant under test:
//
//   - brawler_id != 0 with no matching brawlers row must not produce a FK
//     violation. EnsureRetiredBrawler inserts a placeholder row (is_active=false)
//     before the participant INSERT.
//   - The participant row keeps a non-null brawler_id equal to the historical ID.
//   - Re-ingesting the same battle creates no duplicate brawler or participant rows.
//   - An existing active brawler is never flipped to is_active=false.
//
// This is the regression guard for the presentPlunder/trophyThieves FK failure
// where brawler 16000088 (retired between Dec 2024 and Sep 2026) appeared in a
// historical battle log.
func TestRetiredBrawlerIngestion(t *testing.T) {
	dsn := os.Getenv("INTEGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL not set; skipping retired brawler integration test")
	}

	ctx := context.Background()
	pool, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const (
		discovererTag   = "TESTRT01"
		teammateTag     = "TESTRT02"
		opponentATag    = "TESTRT03"
		opponentBTag    = "TESTRT04"
		opponentCTag    = "TESTRT05"
		teammateB       = "TESTRT06"
		retiredBrawler  = 99999801 // synthetic ID guaranteed absent from brawlers table
		activeBrawlerID = 16000005 // SPIKE -- known active brawler; must stay active after test
	)

	// Ensure the retired brawler is not already in the table (clean state).
	if _, err := pool.Exec(ctx, `DELETE FROM brawlers WHERE id = $1`, retiredBrawler); err != nil {
		t.Fatalf("pre-clean retired brawler: %v", err)
	}

	// Confirm the active brawler exists and is active before we start.
	var activeBefore bool
	if err := pool.QueryRow(ctx,
		`SELECT is_active FROM brawlers WHERE id = $1`, activeBrawlerID,
	).Scan(&activeBefore); err != nil {
		t.Fatalf("lookup active brawler before test: %v", err)
	}
	if !activeBefore {
		t.Fatalf("precondition: brawler %d must be active before test", activeBrawlerID)
	}

	// Insert player rows so FK on battle_participants.player_tag is satisfied.
	for _, tag := range []string{discovererTag, teammateTag, opponentATag, opponentBTag, opponentCTag, teammateB} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (tag, name, first_seen_at) VALUES ($1, 'Test', NOW())
			 ON CONFLICT (tag) DO NOTHING`, tag); err != nil {
			t.Fatalf("insert player %s: %v", tag, err)
		}
	}

	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM battle_participants WHERE player_tag IN ('TESTRT01','TESTRT02','TESTRT03','TESTRT04','TESTRT05','TESTRT06')`)
		pool.Exec(bg, `DELETE FROM battle_teams WHERE battle_id IN (SELECT id FROM battles WHERE event_mode = 'testRetiredMode')`)
		pool.Exec(bg, `DELETE FROM battles WHERE event_mode = 'testRetiredMode'`)
		pool.Exec(bg, `DELETE FROM events WHERE supercell_id = 99999801`)
		pool.Exec(bg, `DELETE FROM crawl_targets WHERE player_tag IN ('TESTRT01','TESTRT02','TESTRT03','TESTRT04','TESTRT05','TESTRT06')`)
		pool.Exec(bg, `DELETE FROM players WHERE tag IN ('TESTRT01','TESTRT02','TESTRT03','TESTRT04','TESTRT05','TESTRT06')`)
		pool.Exec(bg, `DELETE FROM brawlers WHERE id = $1`, retiredBrawler)
	})

	// Synthetic 3v3 battle: discoverer's team uses the retired brawler for one
	// slot. The other slots use the known active brawler (SPIKE, 16000005).
	entry := apiclient.BattleEntry{
		BattleTime: time.Now().UTC().Format("20060102T150405.000Z"),
		Event: apiclient.BattleEvent{
			ID:   99999801,
			Mode: "testRetiredMode",
			Map:  "Test Arena",
		},
		Battle: apiclient.BattleData{
			Mode:     "gemGrab",
			Type:     "ranked",
			Result:   "victory",
			Duration: 120,
			Teams: [][]apiclient.BattleParticipant{
				{
					{
						Tag:  "#" + discovererTag,
						Name: "Discoverer",
						Brawler: apiclient.BrawlerInBattle{
							ID: activeBrawlerID, Name: "SPIKE", Power: 10, Trophies: 500,
						},
					},
					{
						Tag:  "#" + teammateTag,
						Name: "Teammate",
						// The retired brawler -- nonzero ID, empty name (as API returns for retired)
						Brawler: apiclient.BrawlerInBattle{
							ID: retiredBrawler, Name: "", Power: 11, Trophies: 300,
						},
					},
					{
						Tag:  "#" + teammateB,
						Name: "TeammatB",
						Brawler: apiclient.BrawlerInBattle{
							ID: activeBrawlerID, Name: "SPIKE", Power: 9, Trophies: 400,
						},
					},
				},
				{
					{
						Tag:  "#" + opponentATag,
						Name: "OppA",
						Brawler: apiclient.BrawlerInBattle{
							ID: activeBrawlerID, Name: "SPIKE", Power: 10, Trophies: 600,
						},
					},
					{
						Tag:  "#" + opponentBTag,
						Name: "OppB",
						Brawler: apiclient.BrawlerInBattle{
							ID: activeBrawlerID, Name: "SPIKE", Power: 8, Trophies: 250,
						},
					},
					{
						Tag:  "#" + opponentCTag,
						Name: "OppC",
						Brawler: apiclient.BrawlerInBattle{
							ID: activeBrawlerID, Name: "SPIKE", Power: 11, Trophies: 450,
						},
					},
				},
			},
		},
	}

	ingestor := NewBattleIngestor(pool, discovererTag, BattleIngestorConfig{})
	result, err := ingestor.IngestBattle(ctx, entry)
	if err != nil {
		t.Fatalf("IngestBattle returned error (regression: retired brawler_id must not violate FK): %v", err)
	}
	if !result.IsNew {
		t.Fatal("expected IsNew=true for first ingestion of this battle")
	}
	if !result.HasParticipantRows {
		t.Fatal("HasParticipantRows must be true for a 3v3 battle")
	}

	// 1. Retired brawler placeholder must now exist with is_active=false.
	var retiredIsActive bool
	var retiredName string
	if err := pool.QueryRow(ctx,
		`SELECT is_active, name FROM brawlers WHERE id = $1`, retiredBrawler,
	).Scan(&retiredIsActive, &retiredName); err != nil {
		t.Fatalf("retired brawler row not found after ingestion: %v", err)
	}
	if retiredIsActive {
		t.Errorf("retired brawler %d: is_active=true, want false", retiredBrawler)
	}
	expectedName := "BRAWLER_99999801"
	if retiredName != expectedName {
		t.Errorf("retired brawler %d: name=%q, want %q (empty API name should produce deterministic fallback)",
			retiredBrawler, retiredName, expectedName)
	}

	// 2. Find the inserted battle.
	var battleID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM battles WHERE event_mode = 'testRetiredMode'`,
	).Scan(&battleID); err != nil {
		t.Fatalf("lookup battle id: %v", err)
	}

	// 3. All 6 participant rows must be present, teammate's brawler_id non-null.
	rows, err := pool.Query(ctx,
		`SELECT player_tag, brawler_id FROM battle_participants
		 WHERE battle_id = $1 ORDER BY player_tag`, battleID)
	if err != nil {
		t.Fatalf("query participants: %v", err)
	}
	defer rows.Close()

	type prow struct {
		tag       string
		brawlerID *int
	}
	var participants []prow
	for rows.Next() {
		var pr prow
		if err := rows.Scan(&pr.tag, &pr.brawlerID); err != nil {
			t.Fatalf("scan participant: %v", err)
		}
		participants = append(participants, pr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("participants rows error: %v", err)
	}
	rows.Close()

	if len(participants) != 6 {
		t.Fatalf("expected 6 participant rows, got %d", len(participants))
	}
	for _, pr := range participants {
		if pr.brawlerID == nil {
			t.Errorf("participant %s: brawler_id is NULL, want non-null (retired brawler must keep its ID)", pr.tag)
		} else if pr.tag == teammateTag && *pr.brawlerID != retiredBrawler {
			t.Errorf("participant %s: brawler_id=%d, want %d", pr.tag, *pr.brawlerID, retiredBrawler)
		}
	}

	// 4. Active brawler must still be active -- EnsureRetiredBrawler must not
	//    flip existing active rows.
	var activeAfter bool
	if err := pool.QueryRow(ctx,
		`SELECT is_active FROM brawlers WHERE id = $1`, activeBrawlerID,
	).Scan(&activeAfter); err != nil {
		t.Fatalf("lookup active brawler after test: %v", err)
	}
	if !activeAfter {
		t.Errorf("brawler %d (SPIKE) was flipped to is_active=false by EnsureRetiredBrawler; ON CONFLICT DO NOTHING must preserve existing rows", activeBrawlerID)
	}

	// 5. Idempotency: re-ingesting must not create duplicate brawler or participant rows.
	result2, err := ingestor.IngestBattle(ctx, entry)
	if err != nil {
		t.Fatalf("second IngestBattle returned error (idempotency regression): %v", err)
	}
	if result2.IsNew {
		t.Error("IsNew=true on second call; battle already existed")
	}

	var dupBrawlers int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM brawlers WHERE id = $1`, retiredBrawler,
	).Scan(&dupBrawlers); err != nil {
		t.Fatalf("count retired brawler rows: %v", err)
	}
	if dupBrawlers != 1 {
		t.Errorf("expected exactly 1 brawler row for id=%d, got %d", retiredBrawler, dupBrawlers)
	}

	var dupParticipants int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM battle_participants WHERE battle_id = $1`, battleID,
	).Scan(&dupParticipants); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if dupParticipants != 6 {
		t.Errorf("expected 6 participant rows after re-ingestion, got %d (possible duplicates)", dupParticipants)
	}
}
