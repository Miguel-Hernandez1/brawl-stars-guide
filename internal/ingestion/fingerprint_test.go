package ingestion

import (
	"testing"
	"time"
)

func mustParseUTC(s string) time.Time {
	t, err := time.ParseInLocation("20060102T150405.000", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestComputeFingerprint_Deterministic(t *testing.T) {
	key := BattleKey{
		BattleTime:     mustParseUTC("20240901T143022.000"),
		SupcellEventID: 15000123,
		ParticipantTags: []string{
			"#ABC123", "#DEF456", "#GHI789",
			"#JKL012", "#MNO345", "#PQR678",
		},
	}

	fp1 := ComputeFingerprint(key)
	fp2 := ComputeFingerprint(key)

	if fp1 != fp2 {
		t.Fatalf("fingerprint not deterministic: %q != %q", fp1, fp2)
	}
	if len(fp1) != 64 {
		t.Fatalf("expected 64-char hex fingerprint, got len %d: %q", len(fp1), fp1)
	}
}

func TestComputeFingerprint_TeamOrderInvariant(t *testing.T) {
	bt := mustParseUTC("20240901T143022.000")
	eventID := 15000123

	// Same battle discovered from team A's perspective (different tag order)
	keyA := BattleKey{
		BattleTime:     bt,
		SupcellEventID: eventID,
		ParticipantTags: []string{
			"#ABC123", "#DEF456", "#GHI789",
			"#JKL012", "#MNO345", "#PQR678",
		},
	}
	// Same battle discovered from team B's perspective (tags in different order)
	keyB := BattleKey{
		BattleTime:     bt,
		SupcellEventID: eventID,
		ParticipantTags: []string{
			"#JKL012", "#MNO345", "#PQR678",
			"#ABC123", "#DEF456", "#GHI789",
		},
	}

	if ComputeFingerprint(keyA) != ComputeFingerprint(keyB) {
		t.Fatal("fingerprint differs when team order is reversed")
	}
}

func TestComputeFingerprint_TagNormalization(t *testing.T) {
	bt := mustParseUTC("20240901T143022.000")
	eventID := 99

	withHash := BattleKey{
		BattleTime:     bt,
		SupcellEventID: eventID,
		ParticipantTags: []string{"#abc123", "#DEF456"},
	}
	withoutHash := BattleKey{
		BattleTime:     bt,
		SupcellEventID: eventID,
		ParticipantTags: []string{"ABC123", "def456"},
	}

	if ComputeFingerprint(withHash) != ComputeFingerprint(withoutHash) {
		t.Fatal("fingerprint differs between '#'-prefixed and normalized tags")
	}
}

func TestComputeFingerprint_DifferentBattlesDiffer(t *testing.T) {
	bt := mustParseUTC("20240901T143022.000")

	keyA := BattleKey{
		BattleTime:     bt,
		SupcellEventID: 111,
		ParticipantTags: []string{"#AAA", "#BBB"},
	}
	keyB := BattleKey{
		BattleTime:     bt,
		SupcellEventID: 222, // different event
		ParticipantTags: []string{"#AAA", "#BBB"},
	}
	keyC := BattleKey{
		BattleTime:     bt.Add(time.Second), // different time
		SupcellEventID: 111,
		ParticipantTags: []string{"#AAA", "#BBB"},
	}
	keyD := BattleKey{
		BattleTime:     bt,
		SupcellEventID: 111,
		ParticipantTags: []string{"#AAA", "#CCC"}, // different participant
	}

	fps := []string{
		ComputeFingerprint(keyA),
		ComputeFingerprint(keyB),
		ComputeFingerprint(keyC),
		ComputeFingerprint(keyD),
	}

	seen := map[string]bool{}
	for _, fp := range fps {
		if seen[fp] {
			t.Fatalf("fingerprint collision detected: %q appeared twice", fp)
		}
		seen[fp] = true
	}
}
