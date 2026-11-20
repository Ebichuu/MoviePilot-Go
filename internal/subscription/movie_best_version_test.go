package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMovieBestVersionMatchesFrozenV2(t *testing.T) {
	t.Parallel()
	_, currentFile, _, _ := runtime.Caller(0)
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "..", "..", "testdata", "contracts", "fixtures", "r08", "movie-quality-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name  string `json:"name"`
			Input struct {
				Best      bool `json:"best_version"`
				Current   *int `json:"current_priority"`
				Candidate int  `json:"candidate_priority"`
			} `json:"input"`
			Expected struct {
				Allowed        bool `json:"allowed"`
				BeforeComplete bool `json:"before_complete"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 18 {
		t.Fatalf("movie fixture cases=%d", len(fixture.Cases))
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			if got := MovieUpgradeAllowed(test.Input.Best, test.Input.Current, test.Input.Candidate); got != test.Expected.Allowed {
				t.Fatalf("allowed=%v want=%v", got, test.Expected.Allowed)
			}
			if got := MovieBestVersionComplete(test.Input.Best, test.Input.Current); got != test.Expected.BeforeComplete {
				t.Fatalf("complete=%v want=%v", got, test.Expected.BeforeComplete)
			}
		})
	}
}
