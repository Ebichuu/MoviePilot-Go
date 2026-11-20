package database

import (
	"testing"

	"github.com/google/uuid"
)

func r08MoviePriority(value int) *int { return &value }

func TestDownloadIntentMoviePriorityImmutableNormalizationAndEquality(t *testing.T) {
	t.Parallel()
	base := DownloadIntentPlan{IntentID: uuid.New(), SiteID: 1, DownloaderID: downloadIntentPlanTestDownloaderID,
		ResolverKind: "haidan", ResolverVersion: 1, ResourceID: "123", MoviePriority: r08MoviePriority(90)}
	normalized, err := normalizeDownloadIntentPlanFields(base)
	if err != nil {
		t.Fatal(err)
	}
	*base.MoviePriority = 100
	if *normalized.MoviePriority != 90 {
		t.Fatal("caller mutation changed normalized snapshot")
	}
	for _, tc := range []struct {
		name        string
		left, right *int
		equal       bool
	}{
		{"unknown equal", nil, nil, true}, {"zero is known", nil, r08MoviePriority(0), false},
		{"same value different pointers", r08MoviePriority(90), r08MoviePriority(90), true},
		{"quality differs", r08MoviePriority(90), r08MoviePriority(100), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := normalized, normalized
			left.MoviePriority, right.MoviePriority = tc.left, tc.right
			if got := sameDownloadIntentPlan(left, right); got != tc.equal {
				t.Fatalf("equal=%v want=%v", got, tc.equal)
			}
		})
	}
	for _, value := range []int{-2147483648, -1, 0, 90, 100, 101, 2147483647} {
		base.MoviePriority = r08MoviePriority(value)
		if _, err := normalizeDownloadIntentPlanFields(base); err != nil {
			t.Fatalf("valid priority %d: %v", value, err)
		}
	}
	for _, value := range []int{-2147483649, 2147483648} {
		base.MoviePriority = r08MoviePriority(value)
		if _, err := normalizeDownloadIntentPlanFields(base); err != ErrInvalidDownloadIntentPlan {
			t.Fatalf("overflow %d: %v", value, err)
		}
	}
}
