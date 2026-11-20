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

func TestNormalizeOrganizationSuccessSnapshots(t *testing.T) {
	t.Parallel()

	batchID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	intentID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	media := organization.MediaContext{Source: "tmdb", MediaID: "42", TitleYear: "Example (2026)", MediaType: "电视剧"}
	groupOne, err := organization.BuildGroupKey(media, 1)
	if err != nil {
		t.Fatal(err)
	}
	groupTwo, err := organization.BuildGroupKey(media, 2)
	if err != nil {
		t.Fatal(err)
	}
	base := []organization.SuccessSnapshot{
		{
			BatchID: batchID, IntentID: intentID, GroupKey: groupTwo,
			Version: organization.CurrentSuccessVersion, Username: "alice", NeedNotify: true,
			Media: media, Files: []organization.FileFact{{Ordinal: 1, Role: "video", SizeBytes: 20, Season: 2, Episodes: []int{1}}},
		},
		{
			BatchID: batchID, IntentID: intentID, GroupKey: groupOne,
			Version: organization.CurrentSuccessVersion, Username: "alice", NeedNotify: true,
			Media: media, Files: []organization.FileFact{{Ordinal: 0, Role: "video", SizeBytes: 10, Season: 1, Episodes: []int{2, 1}}},
		},
	}

	tests := []struct {
		name     string
		username string
		mutate   func([]organization.SuccessSnapshot) []organization.SuccessSnapshot
		wantErr  error
	}{
		{name: "按 group key 排序并规范化文件", username: " alice "},
		{name: "拒绝空集合", username: "alice", mutate: func([]organization.SuccessSnapshot) []organization.SuccessSnapshot { return nil }, wantErr: ErrInvalidOrganizationSuccessSnapshot},
		{name: "拒绝不同 owner", username: "bob", wantErr: ErrInvalidOrganizationSuccessSnapshot},
		{name: "拒绝不同批次", username: "alice", mutate: func(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
			items[1].BatchID = uuid.New()
			return items
		}, wantErr: ErrInvalidOrganizationSuccessSnapshot},
		{name: "拒绝不同意图", username: "alice", mutate: func(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
			items[1].IntentID = uuid.New()
			return items
		}, wantErr: ErrInvalidOrganizationSuccessSnapshot},
		{name: "拒绝重复 group", username: "alice", mutate: func(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
			items[1] = items[0].Clone()
			return items
		}, wantErr: ErrInvalidOrganizationSuccessSnapshot},
		{name: "拒绝重复 ordinal", username: "alice", mutate: func(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
			items[0].Files[0].Ordinal = 0
			return items
		}, wantErr: ErrInvalidOrganizationSuccessSnapshot},
		{name: "拒绝 ordinal 缺口", username: "alice", mutate: func(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
			items[0].Files[0].Ordinal = 2
			return items
		}, wantErr: ErrInvalidOrganizationSuccessSnapshot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items := cloneOrganizationSuccessSnapshots(base)
			if tt.mutate != nil {
				items = tt.mutate(items)
			}
			owner, got, encoded, err := normalizeOrganizationSuccessSnapshots(tt.username, items)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("normalizeOrganizationSuccessSnapshots() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if owner != "alice" || len(got) != 2 || got[0].GroupKey != groupOne || got[1].GroupKey != groupTwo {
				t.Fatalf("normalized snapshots = owner %q %#v", owner, got)
			}
			if !reflect.DeepEqual(got[0].Files[0].Episodes, []int{1, 2}) {
				t.Fatalf("normalized episodes = %v", got[0].Files[0].Episodes)
			}
			var files []organization.FileFact
			if json.Unmarshal([]byte(encoded[0]), &files) != nil || !reflect.DeepEqual(files, got[0].Files) {
				t.Fatalf("encoded file facts = %q", encoded[0])
			}
		})
	}
}

func TestOrganizationSuccessSnapshotSQLIsOwnerScopedAndImmutable(t *testing.T) {
	t.Parallel()

	createQuery := compactDatabaseIntentSQL(createOrganizationSuccessSnapshotsSQL)
	if !strings.Contains(createQuery, "ON CONFLICT (batch_id, group_key) DO NOTHING") ||
		strings.Contains(strings.ToUpper(createQuery), "DO UPDATE") {
		t.Fatalf("snapshot create SQL is mutable: %s", createQuery)
	}
	listQuery := compactDatabaseIntentSQL(listOrganizationSuccessSnapshotsSQL)
	for _, required := range []string{
		"JOIN organization_batches AS b ON b.id=s.batch_id AND b.intent_id=s.intent_id",
		"JOIN download_intents AS di ON di.id=s.intent_id",
		"WHERE s.batch_id=$1 AND di.username=$2",
		"ORDER BY s.group_key",
	} {
		if !strings.Contains(listQuery, required) {
			t.Fatalf("snapshot list SQL missing %q: %s", required, listQuery)
		}
	}
}

func TestScanOrganizationSuccessSnapshotRejectsCorruptFiles(t *testing.T) {
	t.Parallel()

	row := scannerFunc(func(dest ...any) error {
		*dest[0].(*uuid.UUID) = uuid.New()
		*dest[1].(*string) = "sha256:" + strings.Repeat("a", 64)
		*dest[2].(*uuid.UUID) = uuid.New()
		*dest[3].(*int) = organization.CurrentSuccessVersion
		*dest[4].(*bool) = false
		*dest[5].(*string) = "alice"
		*dest[6].(*bool) = true
		*dest[7].(*bool) = false
		*dest[8].(*bool) = false
		for index := 9; index <= 13; index++ {
			*dest[index].(*string) = ""
		}
		*dest[14].(*float64) = 0
		*dest[15].(*string) = ""
		*dest[16].(*string) = ""
		*dest[17].(*[]byte) = []byte(`[{`)
		return nil
	})
	if _, err := scanOrganizationSuccessSnapshot(row); !errors.Is(err, errStoredOrganizationSuccessSnapshotInvalid) {
		t.Fatalf("scanOrganizationSuccessSnapshot() error = %v", err)
	}
}

func cloneOrganizationSuccessSnapshots(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
	cloned := make([]organization.SuccessSnapshot, len(items))
	for index := range items {
		cloned[index] = items[index].Clone()
	}
	return cloned
}
