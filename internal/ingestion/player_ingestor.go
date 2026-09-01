package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/domain"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// IngestPlayer writes a player profile snapshot to the database.
// It upserts the player identity row, inserts an immutable snapshot,
// and inserts one brawler snapshot row per owned brawler.
//
// Returns the newly created snapshot ID.
func IngestPlayer(ctx context.Context, pool *pgxpool.Pool, resp *apiclient.PlayerResponse) (int64, error) {
	normalTag := apiclient.NormalizeTag(resp.Tag)

	// Ensure the player identity row exists.
	if err := queries.UpsertPlayer(ctx, pool, normalTag, resp.Name); err != nil {
		return 0, fmt.Errorf("upsert player: %w", err)
	}

	// Serialize the full profile for raw_data storage.
	rawData, err := json.Marshal(resp)
	if err != nil {
		return 0, fmt.Errorf("marshal player response: %w", err)
	}

	bucket := domain.BucketForTrophies(resp.Trophies)

	// Look up the patch active at snapshot time (best-effort; nil if none found).
	snapshotAt := time.Now().UTC()
	patchIDPtr, _ := queries.PatchIDForTime(ctx, pool, snapshotAt)

	var clubTag, clubName *string
	if resp.Club != nil {
		norm := apiclient.NormalizeTag(resp.Club.Tag)
		clubTag = &norm
		clubName = &resp.Club.Name
	}

	snapshotID, err := queries.InsertPlayerSnapshot(ctx, pool, queries.PlayerSnapshotParams{
		// NOTE: returns 0 if snapshot already exists at this exact timestamp (extremely rare)
		PlayerTag:       normalTag,
		SnapshotAt:      snapshotAt,
		PatchID:         patchIDPtr,
		Trophies:        resp.Trophies,
		HighestTrophies: resp.HighestTrophies,
		ExpLevel:        resp.ExpLevel,
		ThreeVThreeWins: resp.ThreeVThreeWins,
		SoloVictories:   resp.SoloVictories,
		DuoVictories:    resp.DuoVictories,
		ClubTag:         clubTag,
		ClubName:        clubName,
		TrophyBucket:    int16(bucket),
		RawData:         rawData,
	})
	if err != nil {
		return 0, fmt.Errorf("insert player snapshot: %w", err)
	}
	if snapshotID == 0 {
		// Snapshot already existed at this exact timestamp; skip brawler rows.
		return 0, nil
	}

	// Insert one row per owned brawler.
	for _, brawler := range resp.Brawlers {
		starPowerIDs := make([]int32, len(brawler.StarPowers))
		for i, sp := range brawler.StarPowers {
			starPowerIDs[i] = int32(sp.ID)
		}

		gadgetIDs := make([]int32, len(brawler.Gadgets))
		for i, g := range brawler.Gadgets {
			gadgetIDs[i] = int32(g.ID)
		}

		gearsJSON, err := json.Marshal(brawler.Gears)
		if err != nil {
			return 0, fmt.Errorf("marshal gears for brawler %d: %w", brawler.ID, err)
		}

		if err := queries.InsertPlayerBrawlerSnapshot(ctx, pool, queries.BrawlerSnapshotParams{
			SnapshotID:      snapshotID,
			BrawlerID:       brawler.ID,
			Power:           brawler.Power,
			Rank:            brawler.Rank,
			Trophies:        brawler.Trophies,
			HighestTrophies: brawler.HighestTrophies,
			StarPowerIDs:    starPowerIDs,
			GadgetIDs:       gadgetIDs,
			Gears:           gearsJSON,
		}); err != nil {
			return 0, fmt.Errorf("insert brawler %d snapshot: %w", brawler.ID, err)
		}
	}

	// Mark the player as crawled.
	if err := queries.UpdatePlayerCrawled(ctx, pool, normalTag); err != nil {
		return 0, fmt.Errorf("update player crawled: %w", err)
	}

	return snapshotID, nil
}
