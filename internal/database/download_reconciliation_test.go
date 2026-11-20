package database

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

func TestListDownloadReconciliationTargetsValidatesBoundary(t *testing.T) {
	t.Parallel()

	var store *Store
	now := time.Now().UTC()
	cursorID := uuid.MustParse("3aba5f38-3821-4e7a-96ea-064e1c23256d")
	tests := []struct {
		name      string
		ctx       bool
		cutoff    time.Time
		afterTime time.Time
		afterID   uuid.UUID
		limit     int
		wantErr   error
	}{
		{name: "空上下文", cutoff: now, limit: 1, wantErr: ErrInvalidDownloadReconciliationQuery},
		{name: "空截止时间", ctx: true, limit: 1, wantErr: ErrInvalidDownloadReconciliationQuery},
		{name: "零页大小", ctx: true, cutoff: now, wantErr: ErrInvalidDownloadReconciliationQuery},
		{name: "超过页上限", ctx: true, cutoff: now, limit: maxDownloadReconciliationPageSize + 1, wantErr: ErrInvalidDownloadReconciliationQuery},
		{name: "游标时间缺少 UUID", ctx: true, cutoff: now, afterTime: now, limit: 1, wantErr: ErrInvalidDownloadReconciliationQuery},
		{name: "游标 UUID 缺少时间", ctx: true, cutoff: now, afterID: cursorID, limit: 1, wantErr: ErrInvalidDownloadReconciliationQuery},
		{name: "依赖不可用", ctx: true, cutoff: now, limit: 1, wantErr: ErrDownloadReconciliationQuery},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var ctx = t.Context()
			if !tt.ctx {
				ctx = nil
			}
			_, err := store.ListDownloadReconciliationTargets(ctx, tt.cutoff, tt.afterTime, tt.afterID, tt.limit)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ListDownloadReconciliationTargets() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeDownloadReconciliationTarget(t *testing.T) {
	t.Parallel()

	now := time.Now()
	id := uuid.MustParse("ef624e54-49ff-4e8a-8a8e-41637a5a744e")
	downloaderID := "5b94c889-572f-437b-93ce-3036b6d69122"
	hash := strings.Repeat("a", 40)
	tests := []struct {
		name     string
		target   downloader.ReconciliationTarget
		wantHash string
		wantErr  error
	}{
		{
			name: "规范化 submitting",
			target: downloader.ReconciliationTarget{
				ID: id, Username: " alice ", DownloaderID: strings.ToUpper(downloaderID),
				State: downloader.IntentSubmitting, TorrentHash: hash, UpdatedAt: now,
			},
			wantHash: strings.ToUpper(hash),
		},
		{
			name: "规范化 downloading",
			target: downloader.ReconciliationTarget{
				ID: id, Username: "alice", DownloaderID: downloaderID,
				State: downloader.IntentDownloading, TorrentHash: hash, UpdatedAt: now,
			},
			wantHash: strings.ToUpper(hash),
		},
		{
			name: "规范化 completed episode recovery",
			target: downloader.ReconciliationTarget{
				ID: id, Username: "alice", DownloaderID: downloaderID,
				State: downloader.IntentCompleted, TorrentHash: hash, UpdatedAt: now,
			},
			wantHash: strings.ToUpper(hash),
		},
		{
			name: "规范化 failed subscription recovery",
			target: downloader.ReconciliationTarget{
				ID: id, Username: "alice", DownloaderID: downloaderID,
				State: downloader.IntentFailed, TorrentHash: hash, UpdatedAt: now,
			},
			wantHash: strings.ToUpper(hash),
		},
		{
			name: "允许 failed 空 Hash",
			target: downloader.ReconciliationTarget{
				ID: id, Username: "alice", DownloaderID: downloaderID,
				State: downloader.IntentFailed, UpdatedAt: now,
			},
		},
		{
			name: "允许 planned 空 Hash",
			target: downloader.ReconciliationTarget{
				ID: id, Username: "alice", DownloaderID: downloaderID,
				State: downloader.IntentPlanned, UpdatedAt: now,
			},
		},
		{
			name: "规范化 failed 空白 Hash",
			target: downloader.ReconciliationTarget{
				ID: id, Username: "alice", DownloaderID: downloaderID,
				State: downloader.IntentFailed, TorrentHash: " \t\n", UpdatedAt: now,
			},
		},
		{name: "拒绝 submitting 空 Hash", target: downloader.ReconciliationTarget{ID: id, Username: "alice", DownloaderID: downloaderID, State: downloader.IntentSubmitting, UpdatedAt: now}, wantErr: ErrDownloadReconciliationQuery},
		{name: "拒绝 downloading 空 Hash", target: downloader.ReconciliationTarget{ID: id, Username: "alice", DownloaderID: downloaderID, State: downloader.IntentDownloading, UpdatedAt: now}, wantErr: ErrDownloadReconciliationQuery},
		{name: "拒绝 completed 空 Hash", target: downloader.ReconciliationTarget{ID: id, Username: "alice", DownloaderID: downloaderID, State: downloader.IntentCompleted, UpdatedAt: now}, wantErr: ErrDownloadReconciliationQuery},
		{name: "拒绝 failed 损坏非空 Hash", target: downloader.ReconciliationTarget{ID: id, Username: "alice", DownloaderID: downloaderID, State: downloader.IntentFailed, TorrentHash: "not-a-hash", UpdatedAt: now}, wantErr: ErrDownloadReconciliationQuery},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeDownloadReconciliationTarget(tt.target)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("normalizeDownloadReconciliationTarget() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Username != "alice" || got.DownloaderID != downloaderID || got.TorrentHash != tt.wantHash || got.UpdatedAt.Location() != time.UTC {
				t.Fatalf("normalized target = %#v", got)
			}
		})
	}
}

func TestDownloadReconciliationQueryIsSecretFreeAndKeysetPaginated(t *testing.T) {
	t.Parallel()

	query := strings.Join(strings.Fields(listDownloadReconciliationTargetsSQL), " ")
	for _, required := range []string{
		"JOIN download_intent_plans AS p ON p.intent_id=di.id",
		"LEFT JOIN subscriptions AS s ON s.id=di.subscription_id AND s.username=di.username",
		"di.state IN ('submitting', 'downloading')",
		"(di.subscription_id IS NULL OR s.id IS NOT NULL)",
		"di.state='planned'",
		"s.internal_state IN ('acquired', 'pending_identification')",
		"di.state='failed'",
		"di.state='completed'",
		"di.organization_reconciliation_pending",
		"s.current_download_intent_id=di.id",
		"NOT EXISTS ( SELECT 1 FROM organization_batches AS b WHERE b.intent_id=di.id OFFSET 0 )",
		"di.updated_at <= $1",
		"(di.updated_at, di.id) > ($2, $3)",
		"ORDER BY di.updated_at, di.id",
		"LIMIT $4",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("对账查询缺少 %q: %s", required, query)
		}
	}
	for _, forbidden := range []string{"resource_id", "resource_secret", "save_path", "payload", "last_error"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("对账查询读取敏感或无关字段 %q: %s", forbidden, query)
		}
	}
}

func TestDownloadReconciliationPendingMigrationDefinesDerivedMarker(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/029_download_reconciliation_pending.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, required := range []string{
		"ADD COLUMN organization_reconciliation_pending BOOLEAN NOT NULL DEFAULT TRUE",
		"UPDATE download_intents AS di SET organization_reconciliation_pending=FALSE",
		"WHERE EXISTS ( SELECT 1 FROM organization_batches AS b WHERE b.intent_id=di.id )",
		"IF TG_OP='INSERT' THEN",
		"SET organization_reconciliation_pending=FALSE WHERE id=NEW.intent_id",
		"ELSIF TG_OP='DELETE' THEN",
		"SET organization_reconciliation_pending=TRUE WHERE id=OLD.intent_id",
		"AFTER INSERT OR DELETE ON organization_batches",
		"DROP COLUMN organization_reconciliation_pending",
	} {
		if !strings.Contains(migration, required) {
			t.Fatalf("download reconciliation pending migration missing %q: %s", required, migration)
		}
	}
}

func TestDownloadReconciliationCandidateIndexMigrationIsConcurrentAndRecoverable(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/030_download_reconciliation_candidates_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, required := range []string{
		"-- +goose NO TRANSACTION",
		"DROP INDEX CONCURRENTLY IF EXISTS download_intents_reconciliation_candidates_idx",
		"CREATE INDEX CONCURRENTLY download_intents_reconciliation_candidates_idx",
		"ON download_intents (updated_at, id)",
		"WHERE state IN ('submitting', 'downloading', 'failed') OR ( state='completed' AND organization_reconciliation_pending )",
	} {
		if !strings.Contains(migration, required) {
			t.Fatalf("download reconciliation candidate index migration missing %q: %s", required, migration)
		}
	}
	for _, retained := range []string{
		"download_intents_reconciliation_idx",
		"download_intents_completed_episode_recovery_idx",
		"download_intents_failed_recovery_idx",
	} {
		if strings.Contains(migration, "DROP INDEX CONCURRENTLY IF EXISTS "+retained) ||
			strings.Contains(migration, "DROP INDEX IF EXISTS "+retained) {
			t.Fatalf("candidate index migration drops rollback index %q", retained)
		}
	}
}

func TestDownloadReconciliationPlannedMigrationReplacesAndRestoresCandidatePredicate(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/031_download_reconciliation_planned.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(content), "-- +goose Down", 2)
	if len(parts) != 2 {
		t.Fatalf("planned reconciliation migration missing Down section: %s", content)
	}
	up := strings.Join(strings.Fields(parts[0]), " ")
	down := strings.Join(strings.Fields(parts[1]), " ")
	for _, section := range []struct {
		name      string
		migration string
		predicate string
	}{
		{
			name:      "up",
			migration: up,
			predicate: "WHERE state IN ('planned', 'submitting', 'downloading', 'failed') OR ( state='completed' AND organization_reconciliation_pending )",
		},
		{
			name:      "down",
			migration: down,
			predicate: "WHERE state IN ('submitting', 'downloading', 'failed') OR ( state='completed' AND organization_reconciliation_pending )",
		},
	} {
		t.Run(section.name, func(t *testing.T) {
			t.Parallel()
			for _, required := range []string{
				"DROP INDEX CONCURRENTLY IF EXISTS download_intents_reconciliation_candidates_idx",
				"CREATE INDEX CONCURRENTLY download_intents_reconciliation_candidates_idx",
				"ON download_intents (updated_at, id)",
				section.predicate,
			} {
				if !strings.Contains(section.migration, required) {
					t.Fatalf("planned reconciliation %s migration missing %q: %s", section.name, required, section.migration)
				}
			}
		})
	}
	if !strings.Contains(up, "-- +goose NO TRANSACTION") {
		t.Fatalf("planned reconciliation migration must run without a transaction: %s", up)
	}
	if strings.Contains(down, "'planned'") {
		t.Fatalf("planned reconciliation rollback retained planned predicate: %s", down)
	}
}

func TestDownloadCompletedEpisodeRecoveryMigrationDefinesPartialIndex(t *testing.T) {
	t.Parallel()
	content, err := migrationFS.ReadFile("migrations/015_download_completed_episode_recovery.sql")
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Join(strings.Fields(string(content)), " ")
	for _, required := range []string{
		"ON download_intents (updated_at, id)",
		"WHERE state = 'completed'",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("completed episode recovery migration missing %q: %s", required, query)
		}
	}
}

func TestDownloadFailedRecoveryMigrationDefinesPartialIndex(t *testing.T) {
	t.Parallel()
	content, err := migrationFS.ReadFile("migrations/016_download_failed_recovery_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Join(strings.Fields(string(content)), " ")
	for _, required := range []string{
		"ON download_intents (updated_at, id)",
		"WHERE state = 'failed'",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("failed recovery migration missing %q: %s", required, query)
		}
	}
}

func TestSubscriptionCurrentDownloadIntentMigrationDefinesOwnership(t *testing.T) {
	t.Parallel()
	content, err := migrationFS.ReadFile("migrations/017_subscription_current_download_intent.sql")
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Join(strings.Fields(string(content)), " ")
	for _, required := range []string{
		"ADD COLUMN IF NOT EXISTS current_download_intent_id UUID",
		"FOREIGN KEY (current_download_intent_id) REFERENCES download_intents(id) ON DELETE SET NULL",
		"CREATE UNIQUE INDEX IF NOT EXISTS subscriptions_current_download_intent_idx",
		"WHERE current_download_intent_id IS NOT NULL",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("current intent migration missing %q: %s", required, query)
		}
	}
}
