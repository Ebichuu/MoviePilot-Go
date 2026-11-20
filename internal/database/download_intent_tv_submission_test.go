package database

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestVerifyTVBestVersionSubmissionCoverageAndConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*tvSubmissionRow, *[]subscription.Episode)
		best   bool
		err    error
	}{
		{name: "independent best coverage", best: true},
		{name: "V2 complete title retains extra metadata episodes", best: true, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) {
			row.plan = `[{"season":1,"episode":1},{"season":1,"episode":2}]`
			row.success = row.plan
		}},
		{name: "ordinary keeps binding", mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.config = `{"best_version":false}` }},
		{name: "integer best flag", best: true, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.config = `{"best_version":1}` }},
		{name: "known empty validates exact empty inputs", best: true, mutate: func(row *tvSubmissionRow, actual *[]subscription.Episode) {
			row.plan, row.success, *actual = `[]`, `[]`, nil
		}},
		{name: "order and duplicates normalize", best: true, mutate: func(row *tvSubmissionRow, actual *[]subscription.Episode) {
			row.plan = `[{"season":1,"episode":2},{"season":1,"episode":1}]`
			row.success = `[{"season":1,"episode":1},{"season":1,"episode":2},{"season":1,"episode":1}]`
			*actual = []subscription.Episode{{Season: 1, Episode: 2}, {Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
		}},
		{name: "plan mismatch rejects", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.plan = `[{"season":1,"episode":2}]` }},
		{name: "success mismatch rejects", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.success = `[]` }},
		{name: "unknown quality rejects", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.priority = nil }},
		{name: "unknown coverage rejects", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.plan = "" }},
		{name: "JSON null coverage rejects", err: ErrSubscriptionEpisodePersistence, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.plan = `null` }},
		{name: "corrupt success rejects", err: ErrSubscriptionEpisodePersistence, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.success = `{}` }},
		{name: "invalid stored coordinates reject", err: ErrSubscriptionEpisodePersistence, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.plan = `[{"season":1,"episode":0}]` }},
		{name: "movie parent rejects TV independent facts", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.mediaType = "电影" }},
		{name: "movie success rejects TV independent facts", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.successType = "电影" }},
		{name: "invalid config rejects", err: ErrSubscriptionEpisodePersistence, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.config = `[]` }},
		{name: "paused parent rejects", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.state, row.v2 = subscription.Paused, "S" }},
		{name: "inconsistent V2 state rejects", err: ErrSubscriptionEpisodeBindingConflict, mutate: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.v2 = "P" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := newTVSubmissionRow()
			actual := []subscription.Episode{{Season: 1, Episode: 1}}
			if tc.mutate != nil {
				tc.mutate(&row, &actual)
			}
			querier := &tvSubmissionTestQuery{row: row}
			id := uuid.New()
			best, err := verifyTVBestVersionSubmission(t.Context(), querier, " alice ", 42, id, actual, nil)
			if !errors.Is(err, tc.err) || best != tc.best {
				t.Fatalf("verified best=%v err=%v want best=%v err=%v", best, err, tc.best, tc.err)
			}
			if !slices.Equal(querier.args, []any{int64(42), "alice", id}) {
				t.Fatalf("query did not retain exact owner/parent/intent: %v", querier.args)
			}
		})
	}
}

func TestVerifyTVBestVersionSubmissionReplayRetainsStateGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    subscription.State
		expected subscription.State
		allowed  bool
	}{
		{"precise acquired replay", subscription.Acquired, subscription.PendingSearch, true},
		{"RSS downloading replay", subscription.Downloading, subscription.Active, true},
		{"in-flight organization replay", subscription.Organizing, subscription.Active, true},
		{"RSS P remains P", subscription.PendingIdentification, subscription.PendingIdentification, true},
		{"precise cannot replay P", subscription.PendingIdentification, subscription.PendingSearch, false},
		{"RSS R cannot replay P", subscription.PendingIdentification, subscription.Active, false},
		{"RSS P cannot replay R", subscription.Acquired, subscription.PendingIdentification, false},
		{"unclaimed precise state rejects", subscription.PendingSearch, subscription.PendingSearch, false},
		{"released active state rejects", subscription.Active, subscription.Active, false},
		{"paused rejects", subscription.Paused, subscription.Active, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := newTVSubmissionRow()
			row.state = tc.state
			row.v2, _ = subscription.V2State(tc.state)
			// Empty episodes still require every ownership and state check.
			row.plan, row.success = `[]`, `[]`
			best, err := verifyTVBestVersionSubmission(t.Context(), &tvSubmissionTestQuery{row: row}, "alice", 42, uuid.New(), nil, &tc.expected)
			if tc.allowed && (err != nil || !best) || !tc.allowed && !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) {
				t.Fatalf("replay state=%s expected=%s best=%v err=%v", tc.state, tc.expected, best, err)
			}
		})
	}
}

func TestTVBestVersionSubmissionOwnerAndContextFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"missing owner or changed current intent", pgx.ErrNoRows, ErrSubscriptionEpisodeBindingConflict},
		{"SQL failure sanitized", errors.New("sensitive database details"), ErrSubscriptionEpisodePersistence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := verifyTVBestVersionSubmission(t.Context(), &tvSubmissionTestQuery{scanErr: tc.err}, "alice", 42, uuid.New(), nil, nil)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unsanitized error: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	querier := &tvSubmissionTestQuery{}
	if _, err := verifyTVBestVersionSubmission(ctx, querier, "alice", 42, uuid.New(), nil, nil); !errors.Is(err, context.Canceled) || querier.queries != 0 {
		t.Fatalf("cancelled validation queried database: queries=%d err=%v", querier.queries, err)
	}
}

func TestTVBestVersionSubmissionSQLLocksExactOwnerBinding(t *testing.T) {
	t.Parallel()
	query := compactDatabaseIntentSQL(verifyTVBestVersionSubmissionSQL)
	for _, required := range []string{
		"di.subscription_id=s.id", "di.id=$3 AND di.username=$2", "s.id=$1 AND s.username=$2",
		"s.completed_at IS NULL", "s.current_download_intent_id=di.id", "success.username=$2",
		"success.version=1 AND NOT success.legacy", "FOR UPDATE OF s, di",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("ownership or parent lock missing %q: %s", required, query)
		}
	}
	if strings.Contains(query, "subscription_episodes") {
		t.Fatal("independent facts validation must not rebind earlier episode rows")
	}
}

type tvSubmissionRow struct {
	mediaType, successType, config, plan, success, v2 string
	recovered                                         string
	state                                             subscription.State
	priority                                          *int
}

func newTVSubmissionRow() tvSubmissionRow {
	priority := 100
	return tvSubmissionRow{mediaType: "电视剧", successType: "电视剧", config: `{"best_version":true}`,
		plan: `[{"season":1,"episode":1}]`, success: `[{"season":1,"episode":1}]`,
		state: subscription.PendingSearch, v2: "N", priority: &priority}
}

type tvSubmissionTestQuery struct {
	row     tvSubmissionRow
	scanErr error
	queries int
	args    []any
}

func (q *tvSubmissionTestQuery) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	q.queries++
	q.args = args
	return downloadIntentPlanRowFunc(func(dest ...any) error {
		if q.scanErr != nil {
			return q.scanErr
		}
		row := q.row
		*dest[0].(*string), *dest[1].(*[]byte) = row.mediaType, []byte(row.config)
		*dest[2].(*subscription.State), *dest[3].(*string), *dest[4].(**int) = row.state, row.v2, row.priority
		if row.plan != "" {
			*dest[5].(*[]byte) = []byte(row.plan)
		}
		*dest[6].(*[]byte), *dest[7].(*string) = []byte(row.success), row.successType
		if row.recovered != "" {
			*dest[8].(*[]byte) = []byte(row.recovered)
		}
		return nil
	})
}
