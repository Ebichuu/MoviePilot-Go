package database

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeRSSRoundSiteIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		siteIDs []int64
		want    []int64
		wantErr error
	}{
		{name: "empty round", siteIDs: nil, want: []int64{}},
		{name: "sorts and deduplicates", siteIDs: []int64{7, 2, 7, 4}, want: []int64{2, 4, 7}},
		{name: "rejects zero site", siteIDs: []int64{1, 0}, wantErr: ErrInvalidRSSInput},
		{name: "rejects negative site", siteIDs: []int64{-1}, wantErr: ErrInvalidRSSInput},
		{name: "rejects unbounded round", siteIDs: make([]int64, maxRSSRoundSites+1), wantErr: ErrInvalidRSSInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeRSSRoundSiteIDs(tt.siteIDs)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("normalizeRSSRoundSiteIDs() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeRSSRoundSiteIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}
