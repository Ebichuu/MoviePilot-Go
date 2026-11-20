//go:build integration

package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestOrganizationTerminalFailurePostgresPartialSuccess(t *testing.T) {
	store, username, batch, files := createOrganizationTerminalFailureFixture(t, 2)

	if err := store.CompleteOrganizationFile(t.Context(), batch.ID, batch.LeaseToken, files[0].ID); err != nil {
		t.Fatalf("complete successful file: %v", err)
	}
	if err := store.FinalizeOrganizationFileFailure(
		t.Context(), batch.ID, batch.LeaseToken, files[1].ID, " disk timeout\r\n",
	); err != nil {
		t.Fatalf("finalize failed file: %v", err)
	}
	if err := store.FinalizeOrganizationFileFailure(
		t.Context(), batch.ID, batch.LeaseToken, files[1].ID, "disk timeout",
	); err != nil {
		t.Fatalf("replay failed file: %v", err)
	}
	if err := store.FinalizeOrganizationFileFailure(
		t.Context(), batch.ID, batch.LeaseToken, files[1].ID, "different error",
	); !errors.Is(err, ErrOrganizationFileStateConflict) {
		t.Fatalf("different terminal error = %v, want ErrOrganizationFileStateConflict", err)
	}
	if err := store.CompleteOrganizationFile(
		t.Context(), batch.ID, batch.LeaseToken, files[1].ID,
	); !errors.Is(err, ErrOrganizationFileStateConflict) {
		t.Fatalf("complete terminal failure = %v, want ErrOrganizationFileStateConflict", err)
	}

	remaining, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, batch.LeaseToken, 10)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("claim after terminal results = %#v, %v; want empty", remaining, err)
	}

	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	succeeded, err := loadCompletedOrganizationFilesForIndex(t.Context(), tx, batch.ID)
	if err != nil {
		t.Fatalf("load terminal result set: %v", err)
	}
	if len(succeeded) != 1 || succeeded[0].Ordinal != files[0].Ordinal {
		t.Fatalf("successful files = %#v, want ordinal %d", succeeded, files[0].Ordinal)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := store.CompleteOrganizationBatch(t.Context(), batch.ID, batch.LeaseToken); err != nil {
		t.Fatalf("complete partial-success batch: %v", err)
	}
	if err := store.FinalizeOrganizationFileFailure(
		t.Context(), batch.ID, batch.LeaseToken, files[1].ID, "disk timeout",
	); err != nil {
		t.Fatalf("replay terminal failure after batch completion: %v", err)
	}
	if err := store.FinalizeOrganizationFileFailure(
		t.Context(), batch.ID, batch.LeaseToken, files[1].ID, "different error",
	); !errors.Is(err, ErrOrganizationFileStateConflict) {
		t.Fatalf("different replay after completion = %v, want ErrOrganizationFileStateConflict", err)
	}

	stored, err := store.ListOrganizationFiles(t.Context(), username, batch.ID, 0, 10)
	if err != nil || len(stored) != 2 {
		t.Fatalf("list terminal files = %#v, %v", stored, err)
	}
	if stored[0].State != OrganizationFileCompleted || stored[0].CompletedAt == nil ||
		stored[1].State != OrganizationFileTerminalFailed || stored[1].CompletedAt == nil ||
		stored[1].LastError != "disk timeout" {
		t.Fatalf("stored terminal files = %#v", stored)
	}
}

func TestOrganizationTerminalFailurePostgresCompletionGate(t *testing.T) {
	tests := []struct {
		name       string
		finalize   func(*testing.T, *Store, OrganizationBatch, []OrganizationFile)
		wantCount  int
		wantErr    error
		wantClosed bool
	}{
		{
			name: "all terminal failures close with empty success set",
			finalize: func(t *testing.T, store *Store, batch OrganizationBatch, files []OrganizationFile) {
				t.Helper()
				for _, file := range files {
					if err := store.FinalizeOrganizationFileFailure(t.Context(), batch.ID, batch.LeaseToken, file.ID, "copy failed"); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantClosed: true,
		},
		{
			name: "retryable failure remains incomplete",
			finalize: func(t *testing.T, store *Store, batch OrganizationBatch, files []OrganizationFile) {
				t.Helper()
				if err := store.CompleteOrganizationFile(t.Context(), batch.ID, batch.LeaseToken, files[0].ID); err != nil {
					t.Fatal(err)
				}
				if err := store.FailOrganizationFile(t.Context(), batch.ID, batch.LeaseToken, files[1].ID, "retry later"); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrOrganizationFilesIncomplete,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _, batch, files := createOrganizationTerminalFailureFixture(t, 2)
			tt.finalize(t, store, batch, files)

			tx, err := store.db.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			succeeded, gotErr := loadCompletedOrganizationFilesForIndex(t.Context(), tx, batch.ID)
			if !errors.Is(gotErr, tt.wantErr) || len(succeeded) != tt.wantCount {
				t.Fatalf("completion gate = %#v, %v; want count %d error %v", succeeded, gotErr, tt.wantCount, tt.wantErr)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}

			completeErr := store.CompleteOrganizationBatch(t.Context(), batch.ID, batch.LeaseToken)
			if !errors.Is(completeErr, tt.wantErr) {
				t.Fatalf("CompleteOrganizationBatch() error = %v, want %v", completeErr, tt.wantErr)
			}
			var state OrganizationBatchState
			if err := store.db.Pool.QueryRow(t.Context(), `SELECT state FROM organization_batches WHERE id=$1`, batch.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if (state == OrganizationBatchCompleted) != tt.wantClosed {
				t.Fatalf("batch state = %q, want closed %v", state, tt.wantClosed)
			}
		})
	}
}

func TestOrganizationTerminalFailurePostgresSettlesAllCurrentIntentEpisodes(t *testing.T) {
	tests := []struct {
		name               string
		successfulOrdinals map[int]bool
	}{
		{name: "partial success", successfulOrdinals: map[int]bool{0: true}},
		{name: "all terminal failures", successfulOrdinals: map[int]bool{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, username, batch, files := createOrganizationTerminalFailureFixture(t, 2)
			subscriptionID := bindOrganizationTerminalFailureSubscription(
				t, store, username, batch.IntentID, len(files),
			)
			for _, file := range files {
				if tt.successfulOrdinals[file.Ordinal] {
					if err := store.CompleteOrganizationFile(t.Context(), batch.ID, batch.LeaseToken, file.ID); err != nil {
						t.Fatalf("complete file %d: %v", file.Ordinal, err)
					}
					continue
				}
				if err := store.FinalizeOrganizationFileFailure(
					t.Context(), batch.ID, batch.LeaseToken, file.ID, "copy failed",
				); err != nil {
					t.Fatalf("finalize file %d: %v", file.Ordinal, err)
				}
			}

			if err := store.CompleteOrganizationBatch(t.Context(), batch.ID, batch.LeaseToken); err != nil {
				t.Fatalf("complete terminal batch: %v", err)
			}
			assertMediaIndexSubscriptionState(
				t, store, subscriptionID, batch.IntentID,
				subscription.Completed, "indexed", false, true,
			)
			var total, indexed int
			if err := store.db.Pool.QueryRow(t.Context(), `
				SELECT count(*), count(*) FILTER (WHERE state='indexed')
				FROM subscription_episodes
				WHERE subscription_id=$1 AND download_intent_id=$2`,
				subscriptionID, batch.IntentID,
			).Scan(&total, &indexed); err != nil {
				t.Fatal(err)
			}
			if total != len(files) || indexed != total {
				t.Fatalf("current intent episode settlement = %d/%d indexed, want %d/%d",
					indexed, total, len(files), len(files))
			}
		})
	}
}

func TestOrganizationTerminalFailureMigrationDownPostgres(t *testing.T) {
	store, _, batch, files := createOrganizationTerminalFailureFixture(t, 1)
	if err := store.FinalizeOrganizationFileFailure(
		t.Context(), batch.ID, batch.LeaseToken, files[0].ID, "copy failed",
	); err != nil {
		t.Fatal(err)
	}

	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 27); err != nil {
		t.Fatalf("down migration 028: %v", err)
	}

	var state OrganizationFileState
	var completedAt *time.Time
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT state, completed_at FROM organization_files WHERE id=$1`, files[0].ID,
	).Scan(&state, &completedAt); err != nil {
		t.Fatal(err)
	}
	if state != OrganizationFileFailed || completedAt != nil {
		t.Fatalf("downgraded terminal file = state %q completed %v", state, completedAt)
	}
	_, err := store.db.Pool.Exec(ctx, `
		UPDATE organization_files
		SET state='terminal_failed'
		WHERE id=$1`, files[0].ID)
	var postgresErr *pgconn.PgError
	if !errors.As(err, &postgresErr) || postgresErr.Code != "23514" ||
		postgresErr.ConstraintName != "organization_files_state_check" {
		t.Fatalf("old state constraint error = %v", err)
	}

	if err := goose.UpToContext(ctx, migrationDB, "migrations", 28); err != nil {
		t.Fatalf("up migration 028: %v", err)
	}
}

func createOrganizationTerminalFailureFixture(
	t *testing.T,
	fileCount int,
) (*Store, string, OrganizationBatch, []OrganizationFile) {
	t.Helper()
	store := newSiteStatisticsIntegrationStore(t)
	username := "organization-terminal-" + uuid.NewString()
	insertSubscriptionIntegrationUsers(t, store, username)
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, 'completed')`, intentID, username,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	operations := make([]organizer.Operation, fileCount)
	for ordinal := range operations {
		name := uuid.NewString() + ".mkv"
		operations[ordinal] = organizationTestOperation(
			filepath.Join(sourceRoot, name), filepath.Join(targetRoot, name), organizer.Copy,
		)
	}
	plan, err := organizer.BuildPlan(targetRoot, operations)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)
	created, err := store.CreateOrganizationBatch(t.Context(), username, intentID, sourceRoot, plan, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	batch, found, err := store.ClaimOrganizationBatch(t.Context(), created.ID, time.Now().UTC(), time.Minute)
	if err != nil || !found {
		t.Fatalf("claim batch = %#v, %v, %v", batch, found, err)
	}
	files, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, batch.LeaseToken, fileCount)
	if err != nil || len(files) != fileCount {
		t.Fatalf("claim files = %#v, %v", files, err)
	}
	return store, username, batch, files
}

func bindOrganizationTerminalFailureSubscription(
	t *testing.T,
	store *Store,
	username string,
	intentID uuid.UUID,
	episodeCount int,
) int64 {
	t.Helper()
	subscriptionID := createSubscriptionIntegrationFixture(
		t, store, username, "organization-terminal-settlement-"+uuid.NewString(),
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents SET subscription_id=$2
		WHERE id=$1 AND username=$3`, intentID, subscriptionID, username); err != nil {
		t.Fatalf("bind terminal intent to subscription: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$2, internal_state='pending_organization', v2_state='R'
		WHERE id=$1`, subscriptionID, intentID); err != nil {
		t.Fatalf("prepare terminal subscription: %v", err)
	}
	for episode := 1; episode <= episodeCount; episode++ {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO subscription_episodes (
				subscription_id, season, episode, state, download_intent_id
			) VALUES ($1, 1, $2, 'pending_organization', $3)`,
			subscriptionID, episode, intentID,
		); err != nil {
			t.Fatalf("create terminal episode %d: %v", episode, err)
		}
	}
	return subscriptionID
}
