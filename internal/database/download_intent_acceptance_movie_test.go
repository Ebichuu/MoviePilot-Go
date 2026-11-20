package database

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestMovieAcceptanceQualityAndStateMatchFrozenV2(t *testing.T) {
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
				Best    bool `json:"best_version"`
				Current *int `json:"current_priority"`
			} `json:"input"`
			Expected struct {
				Current  *int `json:"current_priority"`
				Written  *int `json:"written_priority"`
				Complete bool `json:"complete"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 18 {
		t.Fatalf("cases=%d", len(fixture.Cases))
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			config := map[string]any{"best_version": test.Input.Best, "note": []int{2}, "lack_episode": 3}
			if test.Input.Current != nil {
				config["current_priority"] = *test.Input.Current
			}
			state, err := downloadIntentAcceptanceMovieState(config, test.Expected.Written)
			want := subscription.Active
			if !test.Input.Best || test.Expected.Complete {
				want = subscription.Completed
			}
			if err != nil || state != want {
				t.Fatalf("state=%s err=%v want=%s", state, err, want)
			}
			base, err := buildDownloadIntentAcceptanceProgressPatch("电影", config, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			patch, err := addDownloadIntentAcceptanceMoviePriority("电影", base, test.Expected.Written)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(patch, &decoded); err != nil {
				t.Fatal(err)
			}
			var current *int
			if test.Input.Current != nil {
				value := *test.Input.Current
				current = &value
			}
			if value, present := decoded["current_priority"]; present {
				parsed, ok := subscriptionInteger(value)
				if !ok {
					t.Fatal(value)
				}
				current = &parsed
			}
			if !reflect.DeepEqual(current, test.Expected.Current) {
				t.Fatalf("priority=%v want=%v", current, test.Expected.Current)
			}
			if test.Input.Best {
				if _, present := decoded["note"]; present {
					t.Fatal("best-version changed note")
				}
				if _, present := decoded["lack_episode"]; present {
					t.Fatal("best-version changed lack")
				}
			} else if decoded["lack_episode"] != float64(0) || !reflect.DeepEqual(decoded["note"], []any{float64(1), float64(2)}) {
				t.Fatalf("normal movie patch=%s", patch)
			}
		})
	}
}

func TestMovieAcceptanceIgnoresSnapshotForTVAndRejectsInvalidLegacyQuality(t *testing.T) {
	t.Parallel()
	priority := 100
	patch, err := addDownloadIntentAcceptanceMoviePriority("电视剧", []byte(`{"note":[1]}`), &priority)
	if err != nil || string(patch) != `{"note":[1]}` {
		t.Fatalf("TV patch=%s err=%v", patch, err)
	}
	config := map[string]any{"best_version": true, "current_priority": "invalid"}
	if _, err := downloadIntentAcceptanceMovieState(config, nil); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatal(err)
	}
	if state, err := downloadIntentAcceptanceMovieState(config, &priority); err != nil || state != subscription.Completed {
		t.Fatalf("recorded fact state=%s err=%v", state, err)
	}
}
