package ingestion

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BattleKey contains the fields needed to compute a stable battle fingerprint.
// The same battle appears in up to 6 players' logs in a 3v3 - the fingerprint
// ensures we store it exactly once regardless of discovery order.
type BattleKey struct {
	BattleTime      time.Time
	SupcellEventID  int
	ParticipantTags []string // raw tags from the API response (may include '#')
}

// ComputeFingerprint returns a 64-character lowercase hex SHA-256 fingerprint
// for the given battle. The fingerprint is:
//
//	SHA-256("YYYYMMDDTHHMMSS.000Z|eventID|TAG1|TAG2|...TAGn")
//
// Tags are normalized (no '#', uppercase) and sorted lexicographically so the
// result is invariant to team order and discovery-player perspective.
func ComputeFingerprint(key BattleKey) string {
	tags := make([]string, len(key.ParticipantTags))
	for i, t := range key.ParticipantTags {
		tags[i] = strings.ToUpper(strings.TrimPrefix(t, "#"))
	}
	sort.Strings(tags)

	raw := fmt.Sprintf("%s|%d|%s",
		key.BattleTime.UTC().Format("20060102T150405.000Z"),
		key.SupcellEventID,
		strings.Join(tags, "|"),
	)

	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h)
}
