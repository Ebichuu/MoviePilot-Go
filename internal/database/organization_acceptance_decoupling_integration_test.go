//go:build integration

package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	organizationAcceptanceCurrentNil = iota
	organizationAcceptanceCurrentIntent
	organizationAcceptanceCurrentSwitched
)

type organizationAcceptanceSubscriptionState struct {
	parentState subscription.State
	v2State     string
	current     int
	completed   bool
	episode     subscription.SubscriptionEpisodeState
}

type organizationAcceptanceFixture struct {
	username       string
	subscriptionID int64
	intentID       uuid.UUID
	sourceRoot     string
	plan           organizer.Plan
}

type organizationAcceptanceSubscriptionSnapshot struct {
	parentState      string
	v2State          string
	currentIntent    *uuid.UUID
	completed        bool
	episodeState     string
	parentUpdatedAt  time.Time
	episodeUpdatedAt time.Time
}

func TestOrganizationAcceptanceDecouplingPostgresAcceptedIntentLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		state organizationAcceptanceSubscriptionState
	}{
		{
			name: "completed parent with cleared current",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.Completed,
				v2State:     "",
				current:     organizationAcceptanceCurrentNil,
				completed:   true,
				episode:     subscription.SubscriptionEpisodeIndexed,
			},
		},
		{
			name: "pending search parent with cleared current",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.PendingSearch,
				v2State:     "N",
				current:     organizationAcceptanceCurrentNil,
				episode:     subscription.SubscriptionEpisodeIndexed,
			},
		},
		{
			name: "parent switched to another current intent",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.PendingOrganization,
				v2State:     "R",
				current:     organizationAcceptanceCurrentSwitched,
				episode:     subscription.SubscriptionEpisodePendingOrganization,
			},
		},
		{
			name: "stale current intent on acquired parent",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.Acquired,
				v2State:     "R",
				current:     organizationAcceptanceCurrentIntent,
				episode:     subscription.SubscriptionEpisodePendingOrganization,
			},
		},
		{
			name: "stale current intent on pending search parent",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.PendingSearch,
				v2State:     "N",
				current:     organizationAcceptanceCurrentIntent,
				episode:     subscription.SubscriptionEpisodePendingOrganization,
			},
		},
		{
			name: "stale current intent on paused parent",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.Paused,
				v2State:     "S",
				current:     organizationAcceptanceCurrentIntent,
				episode:     subscription.SubscriptionEpisodePendingOrganization,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSiteStatisticsIntegrationStore(t)
			username := "organization-accepted-" + uuid.NewString()
			insertSubscriptionIntegrationUsers(t, store, username)
			fixture := prepareOrganizationAcceptanceFixture(t, store, username, tt.name)
			setOrganizationAcceptanceSubscriptionState(t, store, fixture, tt.state)
			before := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, fixture)

			batch := createOrganizationAcceptanceBatch(t, store, fixture)
			claimed, file := claimOrganizationAcceptanceBatchAndFile(t, store, batch)
			if err := store.CompleteOrganizationFile(
				t.Context(), claimed.ID, claimed.LeaseToken, file.ID,
			); err != nil {
				t.Fatalf("complete organization file: %v", err)
			}
			if err := store.CompleteOrganizationBatch(
				t.Context(), claimed.ID, claimed.LeaseToken,
			); err != nil {
				t.Fatalf("complete organization batch: %v", err)
			}

			after := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, fixture)
			assertOrganizationAcceptanceSubscriptionUnchanged(t, before, after)
			assertOrganizationAcceptanceBatchAndFileState(
				t, store, batch.ID, OrganizationBatchCompleted, OrganizationFileCompleted,
			)
		})
	}
}

func TestOrganizationAcceptanceDecouplingPostgresMixedBatchClaim(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	username := "organization-mixed-" + uuid.NewString()
	insertSubscriptionIntegrationUsers(t, store, username)

	detached := prepareOrganizationAcceptanceFixture(t, store, username, "detached")
	setOrganizationAcceptanceSubscriptionState(t, store, detached, organizationAcceptanceSubscriptionState{
		parentState: subscription.PendingSearch,
		v2State:     "N",
		current:     organizationAcceptanceCurrentNil,
		episode:     subscription.SubscriptionEpisodePendingOrganization,
	})
	detachedBefore := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, detached)
	detachedBatch := createOrganizationAcceptanceBatch(t, store, detached)

	legacy := prepareOrganizationAcceptanceFixture(t, store, username, "legacy")
	legacyBatch := createOrganizationAcceptanceBatch(t, store, legacy)

	claimed, err := store.ClaimOrganizationBatches(t.Context(), time.Now().UTC(), 2, time.Minute)
	if err != nil {
		t.Fatalf("claim mixed organization batches: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed mixed organization batches = %d, want 2", len(claimed))
	}
	claimedIDs := map[uuid.UUID]bool{claimed[0].ID: true, claimed[1].ID: true}
	if !claimedIDs[detachedBatch.ID] || !claimedIDs[legacyBatch.ID] {
		t.Fatalf("claimed mixed organization batches = %#v", claimedIDs)
	}

	detachedAfter := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, detached)
	assertOrganizationAcceptanceSubscriptionUnchanged(t, detachedBefore, detachedAfter)
	assertOrganizationAcceptanceSubscriptionState(
		t, store, legacy,
		subscription.Organizing,
		subscription.SubscriptionEpisodeOrganizing,
		organizationAcceptanceCurrentIntent,
		false,
	)
}

func TestOrganizationAcceptanceDecouplingPostgresFailureRecoveryGate(t *testing.T) {
	operations := []struct {
		name string
		run  func(*testing.T, *Store, OrganizationBatch)
	}{
		{
			name: "fail batch",
			run: func(t *testing.T, store *Store, batch OrganizationBatch) {
				t.Helper()
				if err := store.FailOrganizationBatch(
					t.Context(), batch.ID, batch.LeaseToken,
					time.Now().UTC().Add(time.Minute), "worker failed",
				); err != nil {
					t.Fatalf("fail organization batch: %v", err)
				}
			},
		},
		{
			name: "recover expired batch",
			run: func(t *testing.T, store *Store, batch OrganizationBatch) {
				t.Helper()
				now := time.Now().UTC()
				if _, err := store.db.Pool.Exec(t.Context(), `
					UPDATE organization_batches SET lease_expires_at=$2 WHERE id=$1`,
					batch.ID, now.Add(-time.Minute),
				); err != nil {
					t.Fatalf("expire organization batch: %v", err)
				}
				recovered, err := store.RecoverExpiredOrganizationLeases(t.Context(), now, 1)
				if err != nil || recovered != 1 {
					t.Fatalf("recover organization batch = %d, %v; want 1, nil", recovered, err)
				}
			},
		},
	}
	gates := []struct {
		name     string
		state    organizationAcceptanceSubscriptionState
		rollback bool
	}{
		{
			name: "completed parent",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.Completed,
				v2State:     "",
				current:     organizationAcceptanceCurrentNil,
				completed:   true,
				episode:     subscription.SubscriptionEpisodeOrganizing,
			},
		},
		{
			name: "cleared current",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.PendingSearch,
				v2State:     "N",
				current:     organizationAcceptanceCurrentNil,
				episode:     subscription.SubscriptionEpisodeOrganizing,
			},
		},
		{
			name: "switched current",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.Organizing,
				v2State:     "R",
				current:     organizationAcceptanceCurrentSwitched,
				episode:     subscription.SubscriptionEpisodeOrganizing,
			},
		},
		{
			name: "legacy organizing parent",
			state: organizationAcceptanceSubscriptionState{
				parentState: subscription.Organizing,
				v2State:     "R",
				current:     organizationAcceptanceCurrentIntent,
				episode:     subscription.SubscriptionEpisodeOrganizing,
			},
			rollback: true,
		},
	}
	for _, operation := range operations {
		for _, gate := range gates {
			t.Run(operation.name+"/"+gate.name, func(t *testing.T) {
				store := newSiteStatisticsIntegrationStore(t)
				username := "organization-recovery-gate-" + uuid.NewString()
				insertSubscriptionIntegrationUsers(t, store, username)
				fixture := prepareOrganizationAcceptanceFixture(t, store, username, gate.name)
				batch := createOrganizationAcceptanceBatch(t, store, fixture)
				claimed, _ := claimOrganizationAcceptanceBatchAndFile(t, store, batch)
				setOrganizationAcceptanceSubscriptionState(t, store, fixture, gate.state)
				before := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, fixture)

				operation.run(t, store, claimed)

				assertOrganizationAcceptanceBatchAndFileState(
					t, store, batch.ID, OrganizationBatchFailed, OrganizationFileFailed,
				)
				if gate.rollback {
					assertOrganizationAcceptanceSubscriptionState(
						t, store, fixture,
						subscription.PendingOrganization,
						subscription.SubscriptionEpisodePendingOrganization,
						organizationAcceptanceCurrentIntent,
						false,
					)
					return
				}
				after := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, fixture)
				assertOrganizationAcceptanceSubscriptionUnchanged(t, before, after)
			})
		}
	}
}

func TestOrganizationAcceptanceDecouplingPostgresLegacyPendingIndex(t *testing.T) {
	tests := []struct {
		name         string
		episodeState subscription.SubscriptionEpisodeState
	}{
		{name: "pending index episode", episodeState: subscription.SubscriptionEpisodePendingIndex},
		{name: "organizing episode", episodeState: subscription.SubscriptionEpisodeOrganizing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newSiteStatisticsIntegrationStore(t)
			username := "organization-pending-index-" + uuid.NewString()
			insertSubscriptionIntegrationUsers(t, store, username)
			fixture := prepareOrganizationAcceptanceFixture(t, store, username, tt.name)
			batch := createOrganizationAcceptanceBatch(t, store, fixture)
			claimed, file := claimOrganizationAcceptanceBatchAndFile(t, store, batch)
			if err := store.CompleteOrganizationFile(
				t.Context(), claimed.ID, claimed.LeaseToken, file.ID,
			); err != nil {
				t.Fatalf("complete organization file: %v", err)
			}
			setOrganizationAcceptanceSubscriptionState(t, store, fixture, organizationAcceptanceSubscriptionState{
				parentState: subscription.PendingIndex,
				v2State:     "R",
				current:     organizationAcceptanceCurrentIntent,
				episode:     tt.episodeState,
			})

			if err := store.CompleteOrganizationBatch(
				t.Context(), claimed.ID, claimed.LeaseToken,
			); err != nil {
				t.Fatalf("complete legacy pending_index batch: %v", err)
			}

			assertOrganizationAcceptanceSubscriptionState(
				t, store, fixture,
				subscription.Completed,
				subscription.SubscriptionEpisodeIndexed,
				organizationAcceptanceCurrentNil,
				true,
			)
			assertOrganizationAcceptanceBatchAndFileState(
				t, store, batch.ID, OrganizationBatchCompleted, OrganizationFileCompleted,
			)
		})
	}
}

func prepareOrganizationAcceptanceFixture(
	t *testing.T,
	store *Store,
	username string,
	key string,
) organizationAcceptanceFixture {
	t.Helper()

	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, username, "organization-acceptance-"+key+"-"+uuid.NewString(),
	)
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (
			id, username, subscription_id, candidate_key, label, state
		) VALUES ($1, $2, $3, $4, $5, 'completed')`,
		intentID, username, subscriptionID,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString(),
	); err != nil {
		t.Fatalf("create completed organization intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$2, internal_state='pending_organization',
			v2_state='R', completed_at=NULL
		WHERE id=$1`, subscriptionID, intentID); err != nil {
		t.Fatalf("bind organization intent to subscription: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (
			subscription_id, season, episode, state, download_intent_id
		) VALUES ($1, 1, 1, 'pending_organization', $2)`,
		subscriptionID, intentID,
	); err != nil {
		t.Fatalf("create organization intent episode: %v", err)
	}

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(
			filepath.Join(sourceRoot, "one.mkv"),
			filepath.Join(root, "library", "one.mkv"),
			organizer.Copy,
		),
	})
	if err != nil {
		t.Fatalf("build organization plan: %v", err)
	}
	return organizationAcceptanceFixture{
		username:       username,
		subscriptionID: subscriptionID,
		intentID:       intentID,
		sourceRoot:     sourceRoot,
		plan:           plan,
	}
}

func createOrganizationAcceptanceBatch(
	t *testing.T,
	store *Store,
	fixture organizationAcceptanceFixture,
) OrganizationBatch {
	t.Helper()

	snapshots := organizationLegacySuccessSnapshotsIntegration(
		t, store, fixture.username, fixture.intentID, fixture.plan,
	)
	batch, err := store.CreateOrganizationBatch(
		t.Context(), fixture.username, fixture.intentID,
		fixture.sourceRoot, fixture.plan, snapshots,
	)
	if err != nil {
		t.Fatalf("create organization batch: %v", err)
	}
	return batch
}

func claimOrganizationAcceptanceBatchAndFile(
	t *testing.T,
	store *Store,
	batch OrganizationBatch,
) (OrganizationBatch, OrganizationFile) {
	t.Helper()

	claimed, found, err := store.ClaimOrganizationBatch(
		t.Context(), batch.ID, time.Now().UTC(), time.Minute,
	)
	if err != nil || !found {
		t.Fatalf("claim organization batch = %#v, found %v, %v", claimed, found, err)
	}
	files, err := store.ClaimOrganizationFiles(
		t.Context(), claimed.ID, claimed.LeaseToken, 1,
	)
	if err != nil || len(files) != 1 {
		t.Fatalf("claim organization file = %#v, %v", files, err)
	}
	return claimed, files[0]
}

func setOrganizationAcceptanceSubscriptionState(
	t *testing.T,
	store *Store,
	fixture organizationAcceptanceFixture,
	state organizationAcceptanceSubscriptionState,
) {
	t.Helper()

	var currentIntent *uuid.UUID
	switch state.current {
	case organizationAcceptanceCurrentNil:
	case organizationAcceptanceCurrentIntent:
		intentID := fixture.intentID
		currentIntent = &intentID
	case organizationAcceptanceCurrentSwitched:
		intentID := uuid.New()
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO download_intents (
				id, username, subscription_id, candidate_key, label, state
			) VALUES ($1, $2, $3, $4, $5, 'downloading')`,
			intentID, fixture.username, fixture.subscriptionID,
			"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString(),
		); err != nil {
			t.Fatalf("create switched organization intent: %v", err)
		}
		currentIntent = &intentID
	default:
		t.Fatalf("unknown organization current intent mode %d", state.current)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$2, v2_state=$3, current_download_intent_id=$4,
			completed_at=CASE WHEN $5 THEN now() ELSE NULL END,
			updated_at=clock_timestamp()
		WHERE id=$1`,
		fixture.subscriptionID, state.parentState, state.v2State,
		currentIntent, state.completed,
	); err != nil {
		t.Fatalf("set organization subscription state: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes
		SET state=$3, updated_at=clock_timestamp()
		WHERE subscription_id=$1 AND download_intent_id=$2`,
		fixture.subscriptionID, fixture.intentID, state.episode,
	); err != nil {
		t.Fatalf("set organization episode state: %v", err)
	}
}

func loadOrganizationAcceptanceSubscriptionSnapshot(
	t *testing.T,
	store *Store,
	fixture organizationAcceptanceFixture,
) organizationAcceptanceSubscriptionSnapshot {
	t.Helper()

	var snapshot organizationAcceptanceSubscriptionSnapshot
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, current_download_intent_id,
			completed_at IS NOT NULL, updated_at
		FROM subscriptions WHERE id=$1`, fixture.subscriptionID).Scan(
		&snapshot.parentState,
		&snapshot.v2State,
		&snapshot.currentIntent,
		&snapshot.completed,
		&snapshot.parentUpdatedAt,
	); err != nil {
		t.Fatalf("load organization subscription state: %v", err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state, updated_at
		FROM subscription_episodes
		WHERE subscription_id=$1 AND download_intent_id=$2`,
		fixture.subscriptionID, fixture.intentID,
	).Scan(&snapshot.episodeState, &snapshot.episodeUpdatedAt); err != nil {
		t.Fatalf("load organization episode state: %v", err)
	}
	return snapshot
}

func assertOrganizationAcceptanceSubscriptionUnchanged(
	t *testing.T,
	want organizationAcceptanceSubscriptionSnapshot,
	got organizationAcceptanceSubscriptionSnapshot,
) {
	t.Helper()

	if want.parentState != got.parentState ||
		want.v2State != got.v2State ||
		!sameOrganizationAcceptanceIntent(want.currentIntent, got.currentIntent) ||
		want.completed != got.completed ||
		want.episodeState != got.episodeState ||
		!want.parentUpdatedAt.Equal(got.parentUpdatedAt) ||
		!want.episodeUpdatedAt.Equal(got.episodeUpdatedAt) {
		t.Fatalf("organization subscription changed\n got: %#v\nwant: %#v", got, want)
	}
}

func sameOrganizationAcceptanceIntent(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func assertOrganizationAcceptanceSubscriptionState(
	t *testing.T,
	store *Store,
	fixture organizationAcceptanceFixture,
	wantParent subscription.State,
	wantEpisode subscription.SubscriptionEpisodeState,
	wantCurrent int,
	wantCompleted bool,
) {
	t.Helper()

	got := loadOrganizationAcceptanceSubscriptionSnapshot(t, store, fixture)
	if got.parentState != string(wantParent) ||
		got.episodeState != string(wantEpisode) ||
		got.completed != wantCompleted {
		t.Fatalf(
			"organization subscription = parent %q episode %q completed %v, want %q %q %v",
			got.parentState, got.episodeState, got.completed,
			wantParent, wantEpisode, wantCompleted,
		)
	}
	switch wantCurrent {
	case organizationAcceptanceCurrentNil:
		if got.currentIntent != nil {
			t.Fatalf("organization current intent = %v, want nil", got.currentIntent)
		}
	case organizationAcceptanceCurrentIntent:
		if got.currentIntent == nil || *got.currentIntent != fixture.intentID {
			t.Fatalf("organization current intent = %v, want %s", got.currentIntent, fixture.intentID)
		}
	default:
		t.Fatalf("unsupported expected organization current intent mode %d", wantCurrent)
	}
}

func assertOrganizationAcceptanceBatchAndFileState(
	t *testing.T,
	store *Store,
	batchID uuid.UUID,
	wantBatch OrganizationBatchState,
	wantFile OrganizationFileState,
) {
	t.Helper()

	var batchState OrganizationBatchState
	var fileState OrganizationFileState
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT b.state, f.state
		FROM organization_batches AS b
		JOIN organization_files AS f ON f.batch_id=b.id
		WHERE b.id=$1`, batchID).Scan(&batchState, &fileState); err != nil {
		t.Fatalf("load organization batch and file state: %v", err)
	}
	if batchState != wantBatch || fileState != wantFile {
		t.Fatalf(
			"organization batch/file state = %q/%q, want %q/%q",
			batchState, fileState, wantBatch, wantFile,
		)
	}
}
