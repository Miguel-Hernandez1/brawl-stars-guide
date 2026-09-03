package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/apiclient"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/domain"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage/queries"
)

// BattleIngestor writes battle log entries to the database with deduplication.
type BattleIngestor struct {
	pool          *pgxpool.Pool
	discovererTag string // normalized tag of the player whose log we're processing
}

// NewBattleIngestor creates an ingestor. discovererTag is the player whose battle
// log is being ingested (needed to attribute trophy_change correctly).
func NewBattleIngestor(pool *pgxpool.Pool, discovererTag string) *BattleIngestor {
	return &BattleIngestor{pool: pool, discovererTag: discovererTag}
}

// IngestResult summarises the outcome of ingesting one battle.
type IngestResult struct {
	Fingerprint        string
	IsNew              bool
	HasParticipantRows bool     // false for Showdown-style battles where participant rows are deferred
	NewDiscoveries     []string // normalized player tags newly added to crawl queue
}

// IngestBattle processes one BattleEntry from the API and writes it to the database.
// If the battle already exists (same fingerprint), the function still ensures all
// participant rows are present (idempotent inserts).
func (b *BattleIngestor) IngestBattle(ctx context.Context, entry apiclient.BattleEntry) (IngestResult, error) {
	battleTime, err := apiclient.ParseBattleTime(entry.BattleTime)
	if err != nil {
		return IngestResult{}, fmt.Errorf("parse battle time: %w", err)
	}

	// Collect all participant tags for fingerprinting (checks both Teams and Players).
	allTags := collectParticipantTags(entry.Battle)
	if len(allTags) == 0 {
		return IngestResult{}, fmt.Errorf(
			"event_mode=%s type=%s at %s: no participants found in teams or players array",
			entry.Event.Mode, entry.Battle.Type, entry.BattleTime,
		)
	}

	fingerprint := ComputeFingerprint(BattleKey{
		BattleTime:      battleTime,
		SupcellEventID:  entry.Event.ID,
		ParticipantTags: allTags,
	})

	// Upsert the event (map/mode reference).
	eventID, err := queries.UpsertEvent(ctx, b.pool, entry.Event.ID, entry.Event.Mode, entry.Event.Map)
	if err != nil {
		return IngestResult{}, fmt.Errorf("upsert event: %w", err)
	}

	// Look up which patch was active when this battle occurred.
	// Returns nil (no error) if no patch predates the battle time - stored as NULL.
	patchID, err := queries.PatchIDForTime(ctx, b.pool, battleTime)
	if err != nil {
		// Non-fatal: proceed with nil patch ID. The battle will have patch_id = NULL.
		log.Printf("warning: could not look up patch for battle at %s: %v", battleTime.Format("2006-01-02"), err)
	}

	// Serialize the raw battle object for storage.
	rawData, err := json.Marshal(entry)
	if err != nil {
		return IngestResult{}, fmt.Errorf("marshal battle entry: %w", err)
	}

	// Determine result from the discoverer's perspective.
	result := entry.Battle.Result

	// Build the team result map: team_index → result string.
	// For 3v3: team 0 has one result, team 1 has the opposite.
	teamResults := resolveTeamResults(entry.Battle.Teams, result, b.discovererTag)

	// Determine star player tag.
	var starPlayerTag *string
	if sp := entry.Battle.StarPlayer; sp != nil {
		norm := apiclient.NormalizeTag(sp.Tag)
		starPlayerTag = &norm
	}

	// Insert the battle row.
	eventIDPtr := &eventID
	discovererNorm := b.discovererTag

	bResult, err := queries.InsertBattle(ctx, b.pool, queries.BattleParams{
		Fingerprint:            fingerprint,
		BattleTime:             battleTime,
		EventID:                eventIDPtr,
		EventMode:              entry.Event.Mode,
		EventMap:               entry.Event.Map,
		BattleType:             entry.Battle.Type,
		DurationSeconds:        nullableInt(entry.Battle.Duration),
		StarPlayerTag:          starPlayerTag,
		PatchID:                patchID,
		TrophyChange:           entry.Battle.TrophyChange,
		TrophyChangePlayerTag:  &discovererNorm,
		DiscoveredViaPlayerTag: &discovererNorm,
		RawBattleData:          rawData,
	})
	if err != nil {
		return IngestResult{}, fmt.Errorf("insert battle: %w", err)
	}

	// Insert teams, participants, and queue discoveries.
	// Three cases based on the battle structure returned by the API:
	//   1. Teams present (3v3 modes): full team + participant rows written on first encounter.
	//   2. Players present, no Teams (Showdown modes): battle row written; participant rows
	//      deferred because team_id NOT NULL and there is no team structure. Players are still
	//      discovered for the crawl queue.
	//   3. Neither present: this should not occur after adding the Players field. Treated as
	//      an error so the caller can log it with enough context to debug.
	var newDiscoveries []string
	var hasParticipantRows bool

	switch {
	case len(entry.Battle.Teams) > 0:
		hasParticipantRows = true
		for teamIdx, team := range entry.Battle.Teams {
			var teamID int64
			if bResult.IsNew {
				teamResult := teamResults[teamIdx]
				var err error
				teamID, err = queries.InsertBattleTeam(ctx, b.pool, bResult.BattleID, teamIdx, teamResult)
				if err != nil {
					return IngestResult{}, fmt.Errorf("insert team %d: %w", teamIdx, err)
				}
			}

			for _, p := range team {
				normalTag := apiclient.NormalizeTag(p.Tag)

				if bResult.IsNew {
					isStarPlayer := starPlayerTag != nil && *starPlayerTag == normalTag
					bucket := domain.BucketForTrophies(p.Brawler.Trophies)
					if err := queries.InsertBattleParticipant(ctx, b.pool, queries.ParticipantParams{
						BattleID:        bResult.BattleID,
						TeamID:          teamID,
						PlayerTag:       normalTag,
						PlayerName:      p.Name,
						BrawlerID:       p.Brawler.ID,
						BrawlerPower:    p.Brawler.Power,
						BrawlerTrophies: p.Brawler.Trophies,
						IsStarPlayer:    isStarPlayer,
						TrophyBucket:    int16(bucket),
					}); err != nil {
						return IngestResult{}, fmt.Errorf("insert participant %s: %w", normalTag, err)
					}
				}

				newDiscoveries = b.discoverPlayer(ctx, normalTag, p.Name, newDiscoveries)
			}
		}

	case len(entry.Battle.Players) > 0:
		// Showdown-style mode. Raw battle data is stored for the record.
		// Participant rows require a team_id and are deferred until the schema
		// supports team-less participants. Players are still discovered.
		hasParticipantRows = false
		for _, p := range entry.Battle.Players {
			normalTag := apiclient.NormalizeTag(p.Tag)
			newDiscoveries = b.discoverPlayer(ctx, normalTag, p.Name, newDiscoveries)
		}

	default:
		// collectParticipantTags returned tags so we should never reach here,
		// but guard defensively.
		return IngestResult{}, fmt.Errorf(
			"event_mode=%s type=%s at %s: participant tags found but neither teams nor players array is populated",
			entry.Event.Mode, entry.Battle.Type, entry.BattleTime,
		)
	}

	return IngestResult{
		Fingerprint:        fingerprint,
		IsNew:              bResult.IsNew,
		HasParticipantRows: hasParticipantRows,
		NewDiscoveries:     newDiscoveries,
	}, nil
}

// discoverPlayer enqueues a newly encountered player tag for crawling if not already
// present. It is a no-op for the discoverer's own tag. Returns the updated discoveries slice.
func (b *BattleIngestor) discoverPlayer(ctx context.Context, normalTag, name string, discoveries []string) []string {
	if normalTag == b.discovererTag {
		return discoveries
	}
	ok, err := b.shouldEnqueue(ctx, normalTag)
	if err != nil {
		log.Printf("warning: check enqueue %s: %v", normalTag, err)
		return discoveries
	}
	if !ok {
		return discoveries
	}
	if err := queries.EnqueueDiscoveredPlayer(ctx, b.pool,
		normalTag, name,
		"battle_discovery", b.discovererTag,
		nil, nil,
	); err != nil {
		log.Printf("warning: enqueue %s: %v", normalTag, err)
		return discoveries
	}
	return append(discoveries, normalTag)
}

// shouldEnqueue checks whether a discovered player tag should be added to the crawl queue.
// Currently: add if not already present. Future: bucket capacity checks.
func (b *BattleIngestor) shouldEnqueue(ctx context.Context, tag string) (bool, error) {
	var exists bool
	err := b.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM crawl_targets WHERE player_tag = $1)`, tag,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// collectParticipantTags returns all raw player tags from a battle entry.
// It checks battle.Teams (3v3 modes) first, then battle.Players (Showdown modes).
func collectParticipantTags(battle apiclient.BattleData) []string {
	var tags []string
	for _, team := range battle.Teams {
		for _, p := range team {
			tags = append(tags, p.Tag)
		}
	}
	for _, p := range battle.Players {
		tags = append(tags, p.Tag)
	}
	return tags
}

// resolveTeamResults determines the result string for each team index.
// In 3v3, the discoverer's team gets `discovererResult`; the other team gets the opposite.
// For modes with draws or more than 2 teams, this is best-effort.
func resolveTeamResults(teams [][]apiclient.BattleParticipant, discovererResult, discovererTag string) map[int]string {
	results := make(map[int]string, len(teams))

	// Find which team index the discoverer is on.
	discovererTeam := -1
	for i, team := range teams {
		for _, p := range team {
			if apiclient.NormalizeTag(p.Tag) == discovererTag {
				discovererTeam = i
				break
			}
		}
		if discovererTeam >= 0 {
			break
		}
	}

	for i := range teams {
		if discovererTeam < 0 {
			results[i] = "" // can't determine
			continue
		}
		if i == discovererTeam {
			results[i] = discovererResult
		} else {
			results[i] = oppositeResult(discovererResult)
		}
	}
	return results
}

func oppositeResult(result string) string {
	switch result {
	case "victory":
		return "defeat"
	case "defeat":
		return "victory"
	default:
		return result // "draw" stays draw; empty stays empty
	}
}

func nullableInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
