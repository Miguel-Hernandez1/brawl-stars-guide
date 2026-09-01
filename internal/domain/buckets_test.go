package domain

import "testing"

func TestBucketForTrophies(t *testing.T) {
	tests := []struct {
		trophies int
		want     TrophyBucket
	}{
		{0, Bucket1},
		{499, Bucket1},
		{500, Bucket2},
		{1999, Bucket2},
		{2000, Bucket3},
		{9999, Bucket3},
		{10000, Bucket4},
		{19999, Bucket4},
		{20000, Bucket5},
		{39999, Bucket5},
		{40000, Bucket6},
		{100000, Bucket6},
	}
	for _, tc := range tests {
		got := BucketForTrophies(tc.trophies)
		if got != tc.want {
			t.Errorf("BucketForTrophies(%d) = %d, want %d", tc.trophies, got, tc.want)
		}
	}
}
