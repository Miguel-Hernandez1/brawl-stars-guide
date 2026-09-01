package domain

// TrophyBucket classifies a trophy count into a population segment.
// Used on battle_participants and player_snapshots to enable
// stratified analytics (e.g. win rate for bucket 4 players on map X).
//
// Boundaries are estimates based on the game's trophy distribution.
// Recalibrate after inspecting the first 10k ingested players.
type TrophyBucket int16

const (
	Bucket1 TrophyBucket = 1 // 0 – 500
	Bucket2 TrophyBucket = 2 // 500 – 2,000
	Bucket3 TrophyBucket = 3 // 2,000 – 10,000
	Bucket4 TrophyBucket = 4 // 10,000 – 20,000
	Bucket5 TrophyBucket = 5 // 20,000 – 40,000
	Bucket6 TrophyBucket = 6 // 40,000+
)

// BucketForTrophies returns the TrophyBucket for a given trophy count.
// trophies may be player total trophies or brawler trophies - the same
// scale is used for both in M1. This may be split into two functions later
// if brawler-specific buckets need different boundaries.
func BucketForTrophies(trophies int) TrophyBucket {
	switch {
	case trophies < 500:
		return Bucket1
	case trophies < 2000:
		return Bucket2
	case trophies < 10000:
		return Bucket3
	case trophies < 20000:
		return Bucket4
	case trophies < 40000:
		return Bucket5
	default:
		return Bucket6
	}
}
