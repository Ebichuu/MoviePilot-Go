package database

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestNormalizeDownloadIntentSuccessInputCreate(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	base := organization.SuccessInput{
		IntentID:   intentID,
		Version:    organization.CurrentSuccessVersion,
		Username:   " alice ",
		Manual:     true,
		Background: true,
		Media: organization.MediaContext{
			Source: " tmdb ", MediaID: " 42 ", TitleYear: " Example (2026) ",
			MediaType: " 电视剧 ", Category: " 剧情 ", VoteAverage: 8.5,
			PosterURL: " https://image.example/original/poster.jpg ",
		},
		ResourceTerm: " 2160p WEB-DL ",
		Episodes: []organization.Episode{
			{Season: 1, Episode: 2}, {Season: 1, Episode: 1}, {Season: 1, Episode: 2},
		},
	}
	want := base.Clone()
	want.Username = "alice"
	want.Media.Source = "tmdb"
	want.Media.MediaID = "42"
	want.Media.TitleYear = "Example (2026)"
	want.Media.MediaType = "电视剧"
	want.Media.Category = "剧情"
	want.Media.PosterURL = "https://image.example/original/poster.jpg"
	want.ResourceTerm = "2160p WEB-DL"
	want.Episodes = []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}

	tests := []struct {
		name     string
		username string
		mutate   func(*organization.SuccessInput)
		wantErr  error
	}{
		{name: "规范化新任务输入", username: " alice "},
		{name: "拒绝空 owner", username: " ", wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "拒绝 owner 与快照用户名不一致", username: "bob", wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "拒绝 legacy 创建", username: "alice", mutate: func(input *organization.SuccessInput) {
			input.Legacy = true
			input.Version = 0
		}, wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "拒绝空意图", username: "alice", mutate: func(input *organization.SuccessInput) {
			input.IntentID = uuid.Nil
		}, wantErr: ErrInvalidDownloadIntentSuccessInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := base.Clone()
			if tt.mutate != nil {
				tt.mutate(&input)
			}
			owner, got, encoded, err := normalizeDownloadIntentSuccessInputCreate(tt.username, input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("normalizeDownloadIntentSuccessInputCreate() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if owner != "alice" || !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized = owner %q input %#v, want %#v", owner, got, want)
			}
			var episodes []organization.Episode
			if json.Unmarshal(encoded, &episodes) != nil || !reflect.DeepEqual(episodes, want.Episodes) {
				t.Fatalf("encoded episodes = %s", encoded)
			}
		})
	}
}

func TestDownloadIntentSuccessInputSQLIsOwnerScopedAndImmutable(t *testing.T) {
	t.Parallel()

	createQuery := compactDatabaseIntentSQL(createDownloadIntentSuccessInputSQL)
	for _, required := range []string{
		"FROM download_intent_plans AS p JOIN download_intents AS di ON di.id=p.intent_id",
		"WHERE p.intent_id=$1 AND di.username=$2",
		"ON CONFLICT (intent_id) DO NOTHING",
	} {
		if !strings.Contains(createQuery, required) {
			t.Fatalf("success input create SQL missing %q: %s", required, createQuery)
		}
	}
	if strings.Contains(strings.ToUpper(createQuery), "DO UPDATE") {
		t.Fatalf("immutable success input SQL contains update: %s", createQuery)
	}

	getQuery := compactDatabaseIntentSQL(getDownloadIntentSuccessInputSQL)
	if !strings.Contains(getQuery, "JOIN download_intents AS di ON di.id=s.intent_id") ||
		!strings.Contains(getQuery, "WHERE s.intent_id=$1 AND di.username=$2") {
		t.Fatalf("success input get SQL is not owner scoped: %s", getQuery)
	}
}

func TestNormalizeDownloadIntentSuccessInputCreateEncodesEmptyEpisodesAsArray(t *testing.T) {
	t.Parallel()

	input := organization.SuccessInput{
		IntentID: uuid.New(), Version: organization.CurrentSuccessVersion, Username: "alice",
	}
	_, normalized, encoded, err := normalizeDownloadIntentSuccessInputCreate("alice", input)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Episodes == nil || string(encoded) != "[]" {
		t.Fatalf("empty episodes = %#v encoded=%s, want non-nil []", normalized.Episodes, encoded)
	}
}

func TestScanDownloadIntentSuccessInputRejectsCorruptEpisodes(t *testing.T) {
	t.Parallel()

	row := scannerFunc(func(dest ...any) error {
		*dest[0].(*uuid.UUID) = uuid.New()
		*dest[1].(*int) = organization.CurrentSuccessVersion
		*dest[2].(*bool) = false
		*dest[3].(*string) = "alice"
		*dest[4].(*bool) = false
		*dest[5].(*bool) = false
		for index := 6; index <= 10; index++ {
			*dest[index].(*string) = ""
		}
		*dest[11].(*float64) = 0
		for index := 12; index <= 14; index++ {
			*dest[index].(*string) = ""
		}
		*dest[15].(*[]byte) = []byte(`[{`)
		return nil
	})
	if _, err := scanDownloadIntentSuccessInput(row); !errors.Is(err, errStoredDownloadIntentSuccessInputInvalid) {
		t.Fatalf("scanDownloadIntentSuccessInput() error = %v", err)
	}
}
