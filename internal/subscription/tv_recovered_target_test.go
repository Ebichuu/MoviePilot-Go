package subscription

import (
	"errors"
	"testing"
)

func TestNormalizeTVRecoveredTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		input   *TVRecoveredTarget
		invalid bool
	}{
		{name: "legacy nil"},
		{name: "valid season zero", input: &TVRecoveredTarget{TotalEpisode: 3, MissingEpisodes: []Episode{{0, 3}, {0, 1}, {0, 3}}, ExpectedCoverage: []Episode{{0, 1}, {0, 1}}}},
		{name: "unknown total", input: &TVRecoveredTarget{}, invalid: true},
		{name: "empty missing", input: &TVRecoveredTarget{TotalEpisode: 3, ExpectedCoverage: []Episode{{1, 1}}}, invalid: true},
		{name: "empty expected", input: &TVRecoveredTarget{TotalEpisode: 3, MissingEpisodes: []Episode{{1, 1}}}, invalid: true},
		{name: "different season", input: &TVRecoveredTarget{TotalEpisode: 3, MissingEpisodes: []Episode{{1, 1}}, ExpectedCoverage: []Episode{{2, 1}}}, invalid: true},
		{name: "outside total", input: &TVRecoveredTarget{TotalEpisode: 3, MissingEpisodes: []Episode{{1, 4}}, ExpectedCoverage: []Episode{{1, 4}}}, invalid: true},
		{name: "outside missing", input: &TVRecoveredTarget{TotalEpisode: 3, MissingEpisodes: []Episode{{1, 1}}, ExpectedCoverage: []Episode{{1, 2}}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeTVRecoveredTarget(tc.input)
			if tc.invalid {
				if !errors.Is(err, ErrInvalidTVTarget) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.input == nil {
				if got != nil {
					t.Fatal("nil became facts")
				}
				return
			}
			if len(got.MissingEpisodes) != 2 || len(got.ExpectedCoverage) != 1 || got.MissingEpisodes[0].Episode != 1 {
				t.Fatalf("not canonical=%+v", got)
			}
			tc.input.MissingEpisodes[0].Episode = 99
			tc.input.ExpectedCoverage[0].Episode = 99
			if got.MissingEpisodes[1].Episode != 3 || got.ExpectedCoverage[0].Episode != 1 {
				t.Fatal("aliased caller facts")
			}
		})
	}
}
