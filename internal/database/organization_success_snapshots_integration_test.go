//go:build integration

package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestOrganizationSuccessSnapshotsPostgresImmutableReplay(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "success-snapshot-owner"
	insertDownloadIntentIntegrationUsers(t, store, username, "success-snapshot-other")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "success-snapshot-site")

	intent := mustDatabaseDownloadIntent(t, username, "success-snapshot-resource")
	if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
		t.Fatalf("create download intent: %v", err)
	}
	plan := downloadIntentPlanIntegrationFixture(intent.ID, siteID, "success-snapshot-resource")
	if _, err := store.CreateDownloadIntentPlan(t.Context(), username, plan); err != nil {
		t.Fatalf("create download plan: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET state='completed', torrent_hash=$2 WHERE id=$1`,
		intent.ID, strings.Repeat("a", 40)); err != nil {
		t.Fatalf("complete download intent fixture: %v", err)
	}

	input := organization.SuccessInput{
		IntentID: intent.ID, Version: organization.CurrentSuccessVersion,
		Username: username, Background: true,
		Media: organization.MediaContext{
			Source: "tmdb", MediaID: "42", TitleYear: "Example (2026)",
			MediaType: "电视剧", Category: "剧情", VoteAverage: 8.5,
			PosterURL:   "https://image.example/original/poster.jpg",
			BackdropURL: "https://image.example/original/backdrop.jpg",
		},
		ResourceTerm: "2160p WEB-DL",
		Episodes:     []organization.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 1}},
	}
	createDownloadSuccessInputIntegration(t, store, username, input)
	gotInput, err := store.GetDownloadIntentSuccessInput(t.Context(), " "+username+" ", intent.ID)
	if err != nil || !sameOrganizationSuccessInput(gotInput, input) {
		t.Fatalf("get success input = %#v, %v", gotInput, err)
	}
	if _, err := store.GetDownloadIntentSuccessInput(t.Context(), "success-snapshot-other", intent.ID); !errors.Is(err, ErrDownloadIntentSuccessInputNotFound) {
		t.Fatalf("other owner get error = %v", err)
	}
	createDownloadSuccessInputIntegration(t, store, username, input)

	conflictingInput := input.Clone()
	conflictingInput.ResourceTerm = "1080p"
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDownloadIntentSuccessInputTx(t.Context(), tx, username, conflictingInput); !errors.Is(err, ErrDownloadIntentSuccessInputConflict) {
		_ = tx.Rollback(context.Background())
		t.Fatalf("conflicting input error = %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	organizationPlan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(filepath.Join(root, "downloads", "s01e01.mkv"), filepath.Join(root, "library", "s01e01.mkv"), organizer.Copy),
		organizationTestOperation(filepath.Join(root, "downloads", "s02e01.mkv"), filepath.Join(root, "library", "s02e01.mkv"), organizer.Copy),
	})
	if err != nil {
		t.Fatal(err)
	}
	groupOne, err := organization.BuildGroupKey(input.Media, 1)
	if err != nil {
		t.Fatal(err)
	}
	groupTwo, err := organization.BuildGroupKey(input.Media, 2)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []organization.SuccessSnapshot{
		{
			BatchID: uuid.NewSHA1(intent.ID, []byte(organizationPlan.ID)), IntentID: intent.ID, GroupKey: groupTwo,
			Version: input.Version, Username: username, NeedNotify: true,
			Background: input.Background, Media: input.Media,
			Files: []organization.FileFact{{
				Ordinal: 1, Role: "video", SizeBytes: organizationPlan.Operations[1].ExpectedSize,
				Season: 2, Episodes: []int{1}, ResourceTerm: input.ResourceTerm,
			}},
		},
		{
			BatchID: uuid.NewSHA1(intent.ID, []byte(organizationPlan.ID)), IntentID: intent.ID, GroupKey: groupOne,
			Version: input.Version, Username: username, NeedNotify: true,
			Background: input.Background, Media: input.Media,
			Files: []organization.FileFact{{
				Ordinal: 0, Role: "video", SizeBytes: organizationPlan.Operations[0].ExpectedSize,
				Season: 1, Episodes: []int{1}, ResourceTerm: input.ResourceTerm,
			}},
		},
	}
	batch, err := store.CreateOrganizationBatch(
		t.Context(), username, intent.ID, filepath.Join(root, "downloads"), organizationPlan, snapshots,
	)
	if err != nil {
		t.Fatalf("create organization batch: %v", err)
	}
	stored := createOrganizationSuccessSnapshotsIntegration(t, store, username, snapshots)
	normalizedOwner, normalized, _, err := normalizeOrganizationSuccessSnapshots(username, snapshots)
	if err != nil || normalizedOwner != username || !sameOrganizationSuccessSnapshots(stored, normalized) {
		t.Fatalf("stored snapshots = %#v, normalize error=%v", stored, err)
	}
	listed, err := store.ListOrganizationSuccessSnapshots(t.Context(), username, batch.ID)
	if err != nil || !sameOrganizationSuccessSnapshots(listed, normalized) {
		t.Fatalf("listed snapshots = %#v, %v", listed, err)
	}
	createOrganizationSuccessSnapshotsIntegration(t, store, username, snapshots)
	if _, err := store.ListOrganizationSuccessSnapshots(t.Context(), "success-snapshot-other", batch.ID); !errors.Is(err, ErrOrganizationSuccessSnapshotNotFound) {
		t.Fatalf("other owner list error = %v", err)
	}

	conflictingSnapshots := cloneOrganizationSnapshots(snapshots)
	conflictingSnapshots[0].NeedNotify = false
	tx, err = store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrganizationSuccessSnapshotsTx(t.Context(), tx, username, conflictingSnapshots); !errors.Is(err, ErrOrganizationSuccessSnapshotConflict) {
		_ = tx.Rollback(context.Background())
		t.Fatalf("conflicting snapshots error = %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intent_success_inputs SET resource_term='changed' WHERE intent_id=$1`, intent.ID); !postgresTriggerError(err) {
		t.Fatalf("success input update error = %v, want immutable trigger", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		DELETE FROM organization_success_snapshots WHERE batch_id=$1`, batch.ID); !postgresTriggerError(err) {
		t.Fatalf("success snapshot delete error = %v, want immutable trigger", err)
	}
}

func createDownloadSuccessInputIntegration(
	t *testing.T,
	store *Store,
	username string,
	input organization.SuccessInput,
) {
	t.Helper()
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	stored, err := store.CreateDownloadIntentSuccessInputTx(t.Context(), tx, username, input)
	if err != nil || !sameOrganizationSuccessInput(stored, input) {
		t.Fatalf("create success input = %#v, %v", stored, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func createOrganizationSuccessSnapshotsIntegration(
	t *testing.T,
	store *Store,
	username string,
	snapshots []organization.SuccessSnapshot,
) []organization.SuccessSnapshot {
	t.Helper()
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	stored, err := store.CreateOrganizationSuccessSnapshotsTx(t.Context(), tx, username, snapshots)
	if err != nil {
		t.Fatalf("create success snapshots: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return stored
}

func sameOrganizationSuccessInput(left, right organization.SuccessInput) bool {
	leftNormalized, leftErr := organization.NormalizeSuccessInput(left)
	rightNormalized, rightErr := organization.NormalizeSuccessInput(right)
	return leftErr == nil && rightErr == nil &&
		leftNormalized.IntentID == rightNormalized.IntentID &&
		leftNormalized.Version == rightNormalized.Version &&
		leftNormalized.Legacy == rightNormalized.Legacy &&
		leftNormalized.Username == rightNormalized.Username &&
		leftNormalized.Manual == rightNormalized.Manual &&
		leftNormalized.Background == rightNormalized.Background &&
		leftNormalized.Media == rightNormalized.Media &&
		leftNormalized.ResourceTerm == rightNormalized.ResourceTerm &&
		sameOrganizationEpisodes(leftNormalized.Episodes, rightNormalized.Episodes)
}

func sameOrganizationEpisodes(left, right []organization.Episode) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func postgresTriggerError(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "P0001"
}
