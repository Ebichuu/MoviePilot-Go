package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type asyncAcceptanceSettlementTx struct {
	pgx.Tx
	missing int64
	query   string
	args    []any
	rows    int64
	err     error
}

func (tx *asyncAcceptanceSettlementTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return downloadIntentPlanRowFunc(func(dest ...any) error {
		*dest[0].(*int64) = tx.missing
		return nil
	})
}

func (tx *asyncAcceptanceSettlementTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	tx.query, tx.args = query, args
	if tx.rows == 0 {
		return pgconn.NewCommandTag("UPDATE 0"), tx.err
	}
	return pgconn.NewCommandTag("UPDATE 1"), tx.err
}

func TestAsyncDownloadAcceptanceSettlementKeepsProgressAndFinishesOrdinaryTV(t *testing.T) {
	t.Parallel()
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}
	for _, tc := range []struct {
		name      string
		finish    bool
		recovered bool
		best      bool
		complete  bool
		pending   bool
		want      subscription.State
	}{
		{name: "ordinary batch partial stays N", want: subscription.PendingSearch},
		{name: "ordinary async partial ends R", finish: true, want: subscription.Active},
		{name: "ordinary async complete", finish: true, complete: true, want: subscription.Completed},
		{name: "ordinary P stays P", finish: true, pending: true, want: subscription.PendingIdentification},
		{name: "ordinary P full stays P", finish: true, pending: true, complete: true, want: subscription.PendingIdentification},
		{name: "recovered batch partial stays N", recovered: true, want: subscription.PendingSearch},
		{name: "recovered async partial ends R", finish: true, recovered: true, want: subscription.Active},
		{name: "recovered async complete", finish: true, recovered: true, complete: true, want: subscription.Completed},
		{name: "recovered P stays P", finish: true, recovered: true, pending: true, want: subscription.PendingIdentification},
		{name: "best version partial keeps existing R", finish: true, best: true, want: subscription.Active},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coverage := episodes[:1]
			missing := int64(2)
			if tc.complete {
				coverage, missing = episodes, 0
			}
			config := map[string]any{"total_episode": 3, "note": []int{}, "best_version": tc.best}
			facts := downloadIntentTVFacts{priority: r08MoviePriority(90), coverage: coverage}
			wantPatch := map[string]any{"lack_episode": float64(missing), "note": []any{float64(1)}, "episode_priority": map[string]any{"1": float64(90)}}
			if tc.complete {
				wantPatch["note"] = []any{float64(1), float64(2), float64(3)}
				wantPatch["episode_priority"] = map[string]any{"1": float64(90), "2": float64(90), "3": float64(90)}
			}
			if tc.best {
				wantPatch["current_priority"] = float64(0)
			}
			if tc.recovered {
				config["total_episode"] = 0
				config["note"] = []int{9}
				config["episode_priority"] = map[string]int{"9": 70}
				config["current_priority"] = 70
				facts.recovered = recoveredTargetFixture(1)
				if tc.complete {
					facts.recovered = recoveredTargetFixture(1, 2, 3)
				}
				wantPatch = map[string]any{"lack_episode": float64(missing)}
			}
			before, _ := json.Marshal(config)
			intentID := uuid.New()
			parent := downloadIntentAcceptanceSubscription{mediaType: "tv", state: subscription.Acquired, config: config, currentIntent: &intentID}
			tx := &asyncAcceptanceSettlementTx{missing: missing, rows: 1}
			var err error
			wantQuery := continueDownloadIntentSubscriptionSQL
			if tc.pending {
				parent.state = subscription.PendingIdentification
				err = preservePendingIdentificationDownloadIntentAcceptance(t.Context(), tx, "owner", 42, intentID, parent, coverage, nil, facts)
				wantQuery = preservePendingIdentificationDownloadIntentSubscriptionSQL
			} else {
				err = settleDownloadIntentAcceptanceSubscription(t.Context(), tx, "owner", 42, intentID, parent, coverage, nil, facts, tc.finish)
				if tc.want == subscription.Completed {
					wantQuery = completeDownloadIntentSubscriptionSQL
				}
			}
			if err != nil || tx.query != wantQuery || len(tx.args) != 6 {
				t.Fatalf("query=%s args=%v err=%v", tx.query, tx.args, err)
			}
			projected, _ := subscription.V2State(tc.want)
			var patch map[string]any
			if err := json.Unmarshal(tx.args[5].([]byte), &patch); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(config)
			if tx.args[0] != int64(42) || tx.args[1] != "owner" || tx.args[2] != intentID ||
				tx.args[3] != tc.want || tx.args[4] != projected || !reflect.DeepEqual(patch, wantPatch) || string(before) != string(after) {
				t.Fatalf("args=%v patch=%v config=%v wantPatch=%v", tx.args, patch, config, wantPatch)
			}
		})
	}
}

func TestAsyncDownloadAcceptanceSettlementRequiresParentWrite(t *testing.T) {
	t.Parallel()
	parent := downloadIntentAcceptanceSubscription{mediaType: "tv", config: map[string]any{"total_episode": 0}}
	facts := downloadIntentTVFacts{coverage: []subscription.Episode{{Season: 1, Episode: 1}}, recovered: recoveredTargetFixture(1)}
	for _, tc := range []struct {
		name string
		err  error
		rows int64
	}{
		{name: "parent ownership changed"},
		{name: "database rejected write", rows: 1, err: errors.New("write failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := &asyncAcceptanceSettlementTx{rows: tc.rows, err: tc.err}
			err := settleDownloadIntentAcceptanceSubscription(t.Context(), tx, "owner", 42, uuid.New(), parent, nil, nil, facts, true)
			if !errors.Is(err, ErrDownloadIntentPersistence) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAcceptDownloadIntentAndFinishSearchValidationAndCancellation(t *testing.T) {
	t.Parallel()
	var store *Store
	intentID := uuid.New()
	if _, _, err := store.AcceptDownloadIntentAndFinishSearch(t.Context(), "", intentID, databaseIntentV1Hash); !errors.Is(err, ErrInvalidDownloadIntent) {
		t.Fatalf("invalid input=%v", err)
	}
	if _, _, err := store.AcceptDownloadIntentAndFinishSearch(nil, "owner", intentID, databaseIntentV1Hash); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("nil context=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := store.AcceptDownloadIntentAndFinishSearch(ctx, "owner", intentID, databaseIntentV1Hash); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context=%v", err)
	}
}
