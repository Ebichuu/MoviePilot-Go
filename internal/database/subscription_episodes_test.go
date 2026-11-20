package database

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestValidateSubscriptionEpisodeTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current SubscriptionEpisodeState
		next    SubscriptionEpisodeState
		wantErr bool
	}{
		{name: "missing to acquired", current: SubscriptionEpisodeMissing, next: SubscriptionEpisodeAcquired},
		{name: "acquired to downloading", current: SubscriptionEpisodeAcquired, next: SubscriptionEpisodeDownloading},
		{name: "acquired retry missing", current: SubscriptionEpisodeAcquired, next: SubscriptionEpisodeMissing},
		{name: "downloading to organization", current: SubscriptionEpisodeDownloading, next: SubscriptionEpisodePendingOrganization},
		{name: "downloading retry missing", current: SubscriptionEpisodeDownloading, next: SubscriptionEpisodeMissing},
		{name: "pending organization to organizing", current: SubscriptionEpisodePendingOrganization, next: SubscriptionEpisodeOrganizing},
		{name: "organizing to pending index", current: SubscriptionEpisodeOrganizing, next: SubscriptionEpisodePendingIndex},
		{name: "organizing to indexed", current: SubscriptionEpisodeOrganizing, next: SubscriptionEpisodeIndexed},
		{name: "organizing retry", current: SubscriptionEpisodeOrganizing, next: SubscriptionEpisodePendingOrganization},
		{name: "pending index to indexed", current: SubscriptionEpisodePendingIndex, next: SubscriptionEpisodeIndexed},
		{name: "pending index replay", current: SubscriptionEpisodePendingIndex, next: SubscriptionEpisodePendingIndex},
		{name: "pending index cannot retry organization", current: SubscriptionEpisodePendingIndex, next: SubscriptionEpisodePendingOrganization, wantErr: true},
		{name: "same state replay", current: SubscriptionEpisodeIndexed, next: SubscriptionEpisodeIndexed},
		{name: "indexed cannot regress", current: SubscriptionEpisodeIndexed, next: SubscriptionEpisodeMissing, wantErr: true},
		{name: "missing cannot skip", current: SubscriptionEpisodeMissing, next: SubscriptionEpisodeDownloading, wantErr: true},
		{name: "unknown current", current: SubscriptionEpisodeState("unknown"), next: SubscriptionEpisodeMissing, wantErr: true},
		{name: "unknown next", current: SubscriptionEpisodeMissing, next: SubscriptionEpisodeState("unknown"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSubscriptionEpisodeTransition(tt.current, tt.next)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSubscriptionEpisodeTransition(%q, %q) error = %v, wantErr %v", tt.current, tt.next, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidSubscriptionEpisode) {
				t.Fatalf("error = %v, want ErrInvalidSubscriptionEpisode", err)
			}
		})
	}
}

func TestNormalizeSubscriptionEpisodeStatesSupportsAllStates(t *testing.T) {
	t.Parallel()

	states := []SubscriptionEpisodeState{
		SubscriptionEpisodeMissing,
		SubscriptionEpisodeAcquired,
		SubscriptionEpisodeDownloading,
		SubscriptionEpisodePendingOrganization,
		SubscriptionEpisodeOrganizing,
		SubscriptionEpisodePendingIndex,
		SubscriptionEpisodeIndexed,
	}
	want := []string{
		"missing",
		"acquired",
		"downloading",
		"pending_organization",
		"organizing",
		"pending_index",
		"indexed",
	}
	got, err := normalizeSubscriptionEpisodeStates(states)
	if err != nil {
		t.Fatalf("normalizeSubscriptionEpisodeStates() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeSubscriptionEpisodeStates() = %#v, want %#v", got, want)
	}
	if _, err := normalizeSubscriptionEpisodeStates(append(states, SubscriptionEpisodeMissing)); !errors.Is(err, ErrInvalidSubscriptionEpisode) {
		t.Fatalf("normalizeSubscriptionEpisodeStates(eight states) error = %v, want ErrInvalidSubscriptionEpisode", err)
	}
}

func TestBuildUpsertSubscriptionEpisodes(t *testing.T) {
	t.Parallel()

	query, args, err := buildUpsertSubscriptionEpisodes(" alice ", 41, []subscription.Episode{
		{Season: 1, Episode: 2},
		{Season: 0, Episode: 1},
		{Season: 1, Episode: 2},
	})
	if err != nil {
		t.Fatalf("buildUpsertSubscriptionEpisodes() error = %v", err)
	}
	wantArgs := []any{int64(41), "alice", []int32{0, 1}, []int32{1, 2}, maxSubscriptionEpisodes}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("upsert args = %#v, want %#v", args, wantArgs)
	}
	query = compactSubscriptionEpisodeSQL(query)
	for _, fragment := range []string{
		"id=$1 AND username=$2 AND completed_at IS NULL",
		"FOR UPDATE",
		"ON CONFLICT (subscription_id, season, episode) DO NOTHING",
		"<= $5 AS allowed",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("upsert SQL 缺少 %q: %s", fragment, query)
		}
	}

	tests := []struct {
		name     string
		username string
		id       int64
		episodes []subscription.Episode
		wantErr  error
	}{
		{name: "空 owner", id: 41, episodes: []subscription.Episode{{Season: 1, Episode: 1}}, wantErr: ErrInvalidSubscriptionEpisode},
		{name: "空订阅 ID", username: "alice", episodes: []subscription.Episode{{Season: 1, Episode: 1}}, wantErr: ErrInvalidSubscriptionEpisode},
		{name: "空集列表", username: "alice", id: 41, wantErr: ErrInvalidSubscriptionEpisode},
		{name: "季号越界", username: "alice", id: 41, episodes: []subscription.Episode{{Season: 1000, Episode: 1}}, wantErr: ErrInvalidSubscriptionEpisode},
		{name: "集号越界", username: "alice", id: 41, episodes: []subscription.Episode{{Season: 1, Episode: 5001}}, wantErr: ErrInvalidSubscriptionEpisode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildUpsertSubscriptionEpisodes(tt.username, tt.id, tt.episodes)
			if !errors.Is(err, tt.wantErr) || query != "" || args != nil {
				t.Fatalf("非法 upsert = query %q args %#v err %v", query, args, err)
			}
		})
	}
}

func TestBuildListSubscriptionEpisodes(t *testing.T) {
	t.Parallel()

	states := []SubscriptionEpisodeState{SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired, SubscriptionEpisodeMissing}
	query, args, err := buildListSubscriptionEpisodes(" alice ", 42, states, 10_000)
	if err != nil {
		t.Fatalf("buildListSubscriptionEpisodes() error = %v", err)
	}
	if !reflect.DeepEqual(args, []any{int64(42), "alice", []string{"missing", "acquired"}, 10_000}) {
		t.Fatalf("list args = %#v", args)
	}
	query = compactSubscriptionEpisodeSQL(query)
	for _, fragment := range []string{
		"s.id=$1 AND s.username=$2",
		"se.state=ANY($3::text[])",
		"ORDER BY se.season, se.episode",
		"LIMIT $4",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("list SQL 缺少 %q: %s", fragment, query)
		}
	}

	tests := []struct {
		name   string
		states []SubscriptionEpisodeState
		limit  int
	}{
		{name: "空状态", limit: 1},
		{name: "未知状态", states: []SubscriptionEpisodeState{"unknown"}, limit: 1},
		{name: "零上限", states: []SubscriptionEpisodeState{SubscriptionEpisodeMissing}},
		{name: "超过上限", states: []SubscriptionEpisodeState{SubscriptionEpisodeMissing}, limit: maxSubscriptionEpisodes + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildListSubscriptionEpisodes("alice", 42, tt.states, tt.limit)
			if !errors.Is(err, ErrInvalidSubscriptionEpisode) || query != "" || args != nil {
				t.Fatalf("非法 list = query %q args %#v err %v", query, args, err)
			}
		})
	}
}

func TestBuildCompareAndSwapSubscriptionEpisodeState(t *testing.T) {
	t.Parallel()

	query, args, err := buildCompareAndSwapSubscriptionEpisodeState(
		" alice ", 43, subscription.Episode{Season: 1, Episode: 2},
		SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	)
	if err != nil {
		t.Fatalf("build CAS error = %v", err)
	}
	if !reflect.DeepEqual(args, []any{int64(43), "alice", 1, 2, SubscriptionEpisodeAcquired, SubscriptionEpisodeMissing}) {
		t.Fatalf("CAS args = %#v", args)
	}
	query = compactSubscriptionEpisodeSQL(query)
	for _, fragment := range []string{
		"WITH target AS MATERIALIZED",
		"WHERE id=$1 AND username=$2 AND completed_at IS NULL",
		"FOR UPDATE",
		"se.subscription_id=target.id",
		"se.download_intent_id IS NULL OR target.current_download_intent_id=se.download_intent_id",
		"se.state=$6",
		"SELECT id FROM updated",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("CAS SQL 缺少 %q: %s", fragment, query)
		}
	}

	if _, _, err := buildCompareAndSwapSubscriptionEpisodeState(
		"alice", 43, subscription.Episode{Season: 1, Episode: 2},
		SubscriptionEpisodeIndexed, SubscriptionEpisodeMissing,
	); !errors.Is(err, ErrInvalidSubscriptionEpisode) {
		t.Fatalf("非法回退 CAS error = %v", err)
	}
}

func TestBuildBindSubscriptionEpisodes(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	query, args, expected, err := buildBindSubscriptionEpisodes(
		" alice ", 44, intentID,
		[]subscription.Episode{{Season: 1, Episode: 2}, {Season: 1, Episode: 1}},
	)
	if err != nil {
		t.Fatalf("build bind error = %v", err)
	}
	if expected != 2 || !reflect.DeepEqual(args, []any{int64(44), "alice", intentID, []int32{1, 1}, []int32{1, 2}}) {
		t.Fatalf("bind = args %#v expected %d", args, expected)
	}
	query = compactSubscriptionEpisodeSQL(query)
	for _, fragment := range []string{
		"di.id=$3 AND di.username=$2",
		"current_download_intent_id=$3",
		"EXISTS(SELECT 1 FROM intent)",
		"counts.requested_count=counts.matched_count",
		"counts.requested_count=counts.eligible_count",
		"SET state=CASE WHEN se.state='missing' THEN 'acquired' ELSE se.state END",
		"download_intent_id=$3",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("bind SQL 缺少 %q: %s", fragment, query)
		}
	}

	if _, _, _, err := buildBindSubscriptionEpisodes("alice", 44, uuid.Nil, []subscription.Episode{{Season: 1, Episode: 1}}); !errors.Is(err, ErrInvalidSubscriptionEpisode) {
		t.Fatalf("空 intent bind error = %v", err)
	}
}

func TestBuildVerifySubscriptionEpisodesBoundToIntent(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("12121212-3434-4567-8abc-909090909090")
	query, args, expected, err := buildVerifySubscriptionEpisodesBoundToIntent(
		" alice ", 45, intentID,
		[]subscription.Episode{{Season: 2, Episode: 2}, {Season: 2, Episode: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if expected != 2 || !reflect.DeepEqual(args, []any{int64(45), "alice", intentID, []int32{2, 2}, []int32{1, 2}}) {
		t.Fatalf("verify replay = args %#v expected %d", args, expected)
	}
	query = compactSubscriptionEpisodeSQL(query)
	for _, fragment := range []string{
		"di.subscription_id=s.id AND di.id=$3 AND di.username=$2",
		"s.current_download_intent_id=di.id",
		"s.completed_at IS NULL",
		"s.internal_state IN ( 'acquired', 'downloading', 'pending_organization', 'organizing', 'pending_index', 'indexed' )",
		"se.download_intent_id=$3",
		"SELECT count FROM bound",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("verify replay SQL missing %q: %s", fragment, query)
		}
	}
}

func TestBuildTransitionSubscriptionEpisodesByIntent(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("22222222-3333-4444-5555-666666666666")
	query, args, err := buildTransitionSubscriptionEpisodesByIntent(
		"alice", intentID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired, SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	)
	if err != nil {
		t.Fatalf("build intent transition error = %v", err)
	}
	if !reflect.DeepEqual(args, []any{
		intentID,
		"alice",
		[]string{"acquired"},
		SubscriptionEpisodeDownloading,
		[]string{
			"acquired",
			"downloading",
			"pending_organization",
			"organizing",
			"pending_index",
			"indexed",
		},
	}) {
		t.Fatalf("intent transition args = %#v", args)
	}
	query = compactSubscriptionEpisodeSQL(query)
	for _, fragment := range []string{
		"locked_intent AS MATERIALIZED",
		"FROM download_intents WHERE id=$1 AND username=$2 FOR UPDATE",
		"FROM locked_intent AS di",
		"s.current_download_intent_id=di.id",
		"FOR UPDATE OF s",
		"se.download_intent_id=target.id",
		"FOR UPDATE OF se",
		"bound.state=ANY($3::text[])",
		"bound.state<>ALL($5::text[])",
		"se.state=ANY($3::text[])",
		"state_set.matched_count=0 OR state_set.incompatible_count=0",
		"SET state=$4",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("intent transition SQL 缺少 %q: %s", fragment, query)
		}
	}

	if _, _, err := buildTransitionSubscriptionEpisodesByIntent(
		"alice", intentID, []SubscriptionEpisodeState{SubscriptionEpisodeMissing}, SubscriptionEpisodeIndexed,
	); !errors.Is(err, ErrInvalidSubscriptionEpisode) {
		t.Fatalf("非法 intent transition error = %v", err)
	}
}

func TestCompatibleSubscriptionEpisodeStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from []string
		next SubscriptionEpisodeState
		want []string
	}{
		{
			name: "download replay accepts later workflow states",
			from: []string{"acquired"},
			next: SubscriptionEpisodeDownloading,
			want: []string{
				"acquired", "downloading", "pending_organization", "organizing", "pending_index", "indexed",
			},
		},
		{
			name: "recovery only accepts source and missing",
			from: []string{"acquired", "downloading"},
			next: SubscriptionEpisodeMissing,
			want: []string{"acquired", "downloading", "missing"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := compatibleSubscriptionEpisodeStates(tt.from, tt.next); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("compatible states = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBuildSummarizeSubscriptionEpisodes(t *testing.T) {
	t.Parallel()

	query, args, err := buildSummarizeSubscriptionEpisodes(" alice ", 45)
	if err != nil {
		t.Fatalf("build summary error = %v", err)
	}
	if !reflect.DeepEqual(args, []any{int64(45), "alice"}) {
		t.Fatalf("summary args = %#v", args)
	}
	query = compactSubscriptionEpisodeSQL(query)
	if !strings.Contains(query, "s.id=$1 AND s.username=$2") || !strings.Contains(query, "GROUP BY s.id") {
		t.Fatalf("summary SQL 未限定 owner: %s", query)
	}
}

func TestScanSubscriptionEpisodeSummaryIncludesPendingIndex(t *testing.T) {
	t.Parallel()

	row := subscriptionEpisodeSummaryRowFunc(func(dest ...any) error {
		if len(dest) != 8 {
			return errors.New("unexpected summary column count")
		}
		values := []int64{8, 2, 1, 1, 1, 1, 1, 1}
		for index, value := range values {
			*dest[index].(*int64) = value
		}
		return nil
	})
	summary, err := scanSubscriptionEpisodeSummary(row)
	if err != nil {
		t.Fatalf("scanSubscriptionEpisodeSummary() error = %v", err)
	}
	if summary.Total != 8 || summary.PendingIndex != 1 || summary.Indexed != 1 {
		t.Fatalf("scanSubscriptionEpisodeSummary() = %#v, want pending index count", summary)
	}

	for _, tt := range []struct {
		name   string
		values []int64
	}{
		{name: "count mismatch", values: []int64{8, 1, 1, 1, 1, 1, 1, 0}},
		{name: "total over limit", values: []int64{maxSubscriptionEpisodes + 1, 0, 0, 0, 0, 0, 0, 0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := subscriptionEpisodeSummaryRowFunc(func(dest ...any) error {
				for index, value := range tt.values {
					*dest[index].(*int64) = value
				}
				return nil
			})
			if _, scanErr := scanSubscriptionEpisodeSummary(row); !errors.Is(scanErr, ErrSubscriptionEpisodePersistence) {
				t.Fatalf("scanSubscriptionEpisodeSummary() error = %v, want persistence error", scanErr)
			}
		})
	}

	wantErr := errors.New("summary scan failure")
	if _, err := scanSubscriptionEpisodeSummary(subscriptionEpisodeSummaryRowFunc(func(...any) error { return wantErr })); !errors.Is(err, wantErr) {
		t.Fatalf("scanSubscriptionEpisodeSummary() error = %v, want %v", err, wantErr)
	}
}

type subscriptionEpisodeSummaryRowFunc func(...any) error

func (scan subscriptionEpisodeSummaryRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}

func TestSubscriptionEpisodePersistenceErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	var store *Store
	if err := store.UpsertSubscriptionEpisodes(t.Context(), "sensitive-owner", 46, []subscription.Episode{{Season: 1, Episode: 1}}); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("未初始化 Store upsert error = %v", err)
	}
	if _, err := store.ListSubscriptionEpisodes(t.Context(), "sensitive-owner", 46, []SubscriptionEpisodeState{SubscriptionEpisodeMissing}, 1); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("未初始化 Store list error = %v", err)
	}
	if err := store.CompareAndSwapSubscriptionEpisodeState(
		t.Context(), "sensitive-owner", 46, subscription.Episode{Season: 1, Episode: 1}, SubscriptionEpisodeMissing, SubscriptionEpisodeAcquired,
	); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("未初始化 Store CAS error = %v", err)
	}
	if _, err := store.TransitionSubscriptionEpisodesByIntent(t.Context(), "sensitive-owner", uuid.MustParse("33333333-4444-5555-6666-777777777777"), []SubscriptionEpisodeState{SubscriptionEpisodeAcquired}, SubscriptionEpisodeDownloading); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("未初始化 Store intent transition error = %v", err)
	}
	if _, err := store.SummarizeSubscriptionEpisodes(t.Context(), "sensitive-owner", 46); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("未初始化 Store summary error = %v", err)
	}

	var tx pgx.Tx
	initializedStore := &Store{}
	if err := initializedStore.BindSubscriptionEpisodesTx(t.Context(), tx, "sensitive-owner", 46, uuid.MustParse("44444444-5555-6666-7777-888888888888"), []subscription.Episode{{Season: 1, Episode: 1}}); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("空事务 bind error = %v", err)
	}
	if err := initializedStore.VerifySubscriptionEpisodesBoundToIntentTx(t.Context(), tx, "sensitive-owner", 46, uuid.MustParse("44444444-5555-6666-7777-888888888888"), []subscription.Episode{{Season: 1, Episode: 1}}); err != ErrSubscriptionEpisodePersistence {
		t.Fatalf("空事务 verify replay error = %v", err)
	}
	for _, err := range []error{ErrSubscriptionEpisodePersistence, ErrSubscriptionEpisodeStateConflict, ErrSubscriptionEpisodeNotFound, ErrSubscriptionEpisodeBindingConflict} {
		for _, sensitive := range []string{"sensitive-owner", "46", "subscription_episodes", "SELECT", "UPDATE"} {
			if strings.Contains(err.Error(), sensitive) {
				t.Fatalf("错误 %q 泄露 %q", err, sensitive)
			}
		}
	}
}

func TestSubscriptionEpisodeMigrationDefinesConstraintsAndIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/014_subscription_episode_state.sql")
	if err != nil {
		t.Fatalf("读取逐集迁移: %v", err)
	}
	query := compactSubscriptionEpisodeSQL(string(content))
	for _, fragment := range []string{
		"subscription_episodes_coordinate_check",
		"subscription_episodes_state_check",
		"subscription_episodes_download_intent_fk",
		"subscription_episodes_subscription_state_idx",
		"subscription_episodes_download_intent_idx",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("逐集迁移缺少 %q: %s", fragment, query)
		}
	}
}

func compactSubscriptionEpisodeSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}
