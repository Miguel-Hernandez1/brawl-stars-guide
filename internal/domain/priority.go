package domain

// PriorityForTrophies returns the crawl priority (1 = highest, crawled most often)
// for a player's total trophies. Higher-trophy players are re-crawled more frequently.
//
// Priority maps to re-crawl intervals via crawler.baseInterval:
//
//	1-2 → 4h, 3-4 → 8h, 5-6 → 24h, 7+ → 72h
func PriorityForTrophies(trophies int) int16 {
	switch {
	case trophies >= 40000:
		return 1
	case trophies >= 20000:
		return 2
	case trophies >= 10000:
		return 3
	case trophies >= 2000:
		return 4
	case trophies >= 500:
		return 6
	default:
		return 8
	}
}
