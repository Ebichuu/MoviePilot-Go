package database

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestNormalizeOrganizationPlan(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	plan, err := organizer.BuildPlan(targetRoot, []organizer.Operation{{
		Source:         filepath.Join(sourceRoot, "movie.mkv"),
		Target:         filepath.Join(targetRoot, "Movie", "movie.mkv"),
		Mode:           organizer.Copy,
		ExpectedSize:   5,
		ExpectedSHA256: organizationTestSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	intentID := uuid.MustParse("11111111-2222-4333-8444-555555555555")

	tests := []struct {
		name    string
		owner   string
		source  string
		intent  uuid.UUID
		input   organizer.Plan
		wantErr error
	}{
		{name: "valid plan is canonicalized", owner: " alice ", source: sourceRoot, intent: intentID, input: plan},
		{name: "empty owner", owner: " ", source: sourceRoot, intent: intentID, input: plan, wantErr: ErrInvalidOrganizationInput},
		{name: "nil intent", owner: "alice", source: sourceRoot, input: plan, wantErr: ErrInvalidOrganizationInput},
		{name: "relative source root rejected by source containment", owner: "alice", source: filepath.Join(root, "other"), intent: intentID, input: plan, wantErr: ErrInvalidOrganizationInput},
		{name: "plan id is immutable", owner: "alice", source: sourceRoot, intent: intentID, input: organizer.Plan{ID: "different", TargetRoot: plan.TargetRoot, Operations: plan.Operations}, wantErr: ErrInvalidOrganizationInput},
		{name: "duplicate targets rejected", owner: "alice", source: sourceRoot, intent: intentID, input: organizer.Plan{ID: plan.ID, TargetRoot: targetRoot, Operations: []organizer.Operation{
			organizationTestOperation(filepath.Join(sourceRoot, "a.mkv"), filepath.Join(targetRoot, "same.mkv"), organizer.Copy),
			organizationTestOperation(filepath.Join(sourceRoot, "b.mkv"), filepath.Join(targetRoot, "same.mkv"), organizer.Copy),
		}}, wantErr: ErrInvalidOrganizationInput},
		{name: "invalid fingerprint rejected", owner: "alice", source: sourceRoot, intent: intentID, input: organizationPlanWithFingerprint(plan, 5, strings.Repeat("A", 64)), wantErr: ErrInvalidOrganizationInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, _, _, gotErr := normalizeOrganizationPlan(tt.owner, tt.intent, tt.source, tt.input)
			if !errors.Is(gotErr, tt.wantErr) {
				t.Fatalf("normalizeOrganizationPlan() error = %v, want %v", gotErr, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			gotOwner, gotRoot, gotPlan, encoded, gotBatchID, err := normalizeOrganizationPlan(tt.owner, tt.intent, tt.source, tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if gotOwner != "alice" || gotRoot != sourceRoot || gotPlan.ID != plan.ID || len(encoded) == 0 || gotBatchID == uuid.Nil {
				t.Fatalf("规范化计划 = owner %q root %q plan %#v bytes %d batch %s", gotOwner, gotRoot, gotPlan, len(encoded), gotBatchID)
			}
			_, _, _, _, repeatedID, err := normalizeOrganizationPlan(tt.owner, tt.intent, tt.source, tt.input)
			if err != nil || repeatedID != gotBatchID {
				t.Fatalf("计划 ID 不稳定: %s / %s / %v", gotBatchID, repeatedID, err)
			}
		})
	}
}

func TestOrganizationValidationAndErrorBounds(t *testing.T) {
	if got := sanitizeOrganizationError("  failed\r\n\x00 "); got != "failed" {
		t.Fatalf("sanitizeOrganizationError() = %q, want failed", got)
	}
	if got := sanitizeOrganizationError("copy failed: token=private"); got != "organization operation failed" {
		t.Fatalf("secret error = %q, want generic error", got)
	}
	longUTF8 := strings.Repeat("错", maxOrganizationErrorLength)
	if got := sanitizeOrganizationError(longUTF8); len(got) > maxOrganizationErrorLength || !strings.HasSuffix(got, "错") {
		t.Fatalf("UTF-8 错误长度 = %d, want <= %d", len(got), maxOrganizationErrorLength)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var nilStore *Store
	if err := validateOrganizationStore(nilStore, context.Background()); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := nilStore.GetOrganizationBatchByIntent(context.Background(), "owner", uuid.New()); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("nil store get by intent error = %v", err)
	}
	if _, err := nilStore.GetOrganizationBatchByIntent(context.Background(), "owner", uuid.Nil); !errors.Is(err, ErrInvalidOrganizationInput) {
		t.Fatalf("nil intent get error = %v", err)
	}
	if _, err := nilStore.ListDueOrganizationBatchIDs(context.Background(), time.Now(), 1); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("nil store due list error = %v", err)
	}
	if err := validateOrganizationStore(&Store{}, cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v", err)
	}
	if err := organizationPersistenceError(nil); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("nil context persistence error = %v", err)
	}
	if err := organizationPersistenceError(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context persistence error = %v", err)
	}

	for _, tt := range []struct {
		name string
		call func() error
	}{
		{name: "renew invalid duration", call: func() error {
			return nilStore.RenewOrganizationBatchLease(context.Background(), uuid.New(), uuid.New(), time.Now(), 0)
		}},
		{name: "complete invalid file", call: func() error {
			return nilStore.CompleteOrganizationFile(context.Background(), uuid.New(), uuid.New(), 0)
		}},
		{name: "recover invalid page", call: func() error {
			_, err := nilStore.RecoverExpiredOrganizationLeases(context.Background(), time.Now(), maxOrganizationClaimSize+1)
			return err
		}},
		{name: "claim one invalid batch", call: func() error {
			_, _, err := nilStore.ClaimOrganizationBatch(context.Background(), uuid.Nil, time.Now(), time.Minute)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("error = %v, want ErrInvalidOrganizationInput", err)
			}
		})
	}
}

func TestSameOrganizationPlan(t *testing.T) {
	t.Parallel()

	sourceRoot := filepath.Join(t.TempDir(), "downloads")
	targetRoot := filepath.Join(t.TempDir(), "library")
	operation := organizationTestOperation(
		filepath.Join(sourceRoot, "movie.mkv"),
		filepath.Join(targetRoot, "Movie", "movie.mkv"),
		organizer.Copy,
	)
	plan := mustOrganizationPlan(t, targetRoot, []organizer.Operation{operation})
	batch := OrganizationBatch{SourceRoot: sourceRoot, Plan: plan}

	tests := []struct {
		name       string
		sourceRoot string
		plan       organizer.Plan
		want       bool
	}{
		{name: "identical", sourceRoot: sourceRoot, plan: plan, want: true},
		{name: "different source root", sourceRoot: sourceRoot + "-other", plan: plan},
		{name: "different plan id", sourceRoot: sourceRoot, plan: organizer.Plan{ID: "other", TargetRoot: plan.TargetRoot, Operations: plan.Operations}},
		{name: "different target root", sourceRoot: sourceRoot, plan: organizer.Plan{ID: plan.ID, TargetRoot: plan.TargetRoot + "-other", Operations: plan.Operations}},
		{name: "different operation count", sourceRoot: sourceRoot, plan: organizer.Plan{ID: plan.ID, TargetRoot: plan.TargetRoot}},
		{name: "different operation", sourceRoot: sourceRoot, plan: organizationPlanWithFingerprint(plan, operation.ExpectedSize+1, operation.ExpectedSHA256)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sameOrganizationPlan(batch, tt.sourceRoot, tt.plan); got != tt.want {
				t.Fatalf("sameOrganizationPlan() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrganizationStoredValueValidators(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		got  bool
		want bool
	}{
		{name: "planned batch state", got: validOrganizationBatchState(OrganizationBatchPlanned), want: true},
		{name: "organizing batch state", got: validOrganizationBatchState(OrganizationBatchOrganizing), want: true},
		{name: "failed batch state", got: validOrganizationBatchState(OrganizationBatchFailed), want: true},
		{name: "completed batch state", got: validOrganizationBatchState(OrganizationBatchCompleted), want: true},
		{name: "unknown batch state", got: validOrganizationBatchState(OrganizationBatchState("unknown"))},
		{name: "planned file state", got: validOrganizationFileState(OrganizationFilePlanned), want: true},
		{name: "organizing file state", got: validOrganizationFileState(OrganizationFileOrganizing), want: true},
		{name: "failed file state", got: validOrganizationFileState(OrganizationFileFailed), want: true},
		{name: "completed file state", got: validOrganizationFileState(OrganizationFileCompleted), want: true},
		{name: "unknown file state", got: validOrganizationFileState(OrganizationFileState("unknown"))},
		{name: "copy mode", got: validOrganizationMode(organizer.Copy), want: true},
		{name: "move mode", got: validOrganizationMode(organizer.Move), want: true},
		{name: "hardlink mode", got: validOrganizationMode(organizer.Hardlink), want: true},
		{name: "symlink mode", got: validOrganizationMode(organizer.Symlink), want: true},
		{name: "unknown mode", got: validOrganizationMode(organizer.Mode("unknown"))},
		{name: "lowercase SHA-256", got: validOrganizationSHA256(organizationTestSHA256), want: true},
		{name: "uppercase SHA-256", got: validOrganizationSHA256(strings.ToUpper(organizationTestSHA256))},
		{name: "short SHA-256", got: validOrganizationSHA256(organizationTestSHA256[:63])},
		{name: "non hexadecimal SHA-256", got: validOrganizationSHA256(strings.Repeat("g", 64))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Fatalf("validator result = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestOrganizationSQLAndMigrationContracts(t *testing.T) {
	var nilStore *Store
	for _, fragment := range []string{
		"ON CONFLICT (intent_id) WHERE intent_id IS NOT NULL DO NOTHING",
		"di.username=$3",
		"di.state='completed'",
		"di.subscription_id IS NULL OR s.id IS NOT NULL",
	} {
		if !strings.Contains(strings.Join(strings.Fields(createOrganizationBatchSQL), " "), fragment) {
			t.Fatalf("创建 SQL 缺少 %q", fragment)
		}
	}
	getByIntent := strings.Join(strings.Fields(getOrganizationBatchByIntentSQL), " ")
	for _, fragment := range []string{"b.intent_id=$1", "di.username=$2"} {
		if !strings.Contains(getByIntent, fragment) {
			t.Fatalf("按意图读取 SQL 缺少 %q", fragment)
		}
	}
	due := strings.Join(strings.Fields(listDueOrganizationBatchIDsSQL), " ")
	for _, fragment := range []string{
		"attempt_count < $3",
		"state IN ('planned', 'failed') AND next_attempt_at <= $1",
		"state='organizing' AND lease_expires_at <= $1",
		"ORDER BY next_attempt_at, updated_at, id",
		"LIMIT $2",
	} {
		if !strings.Contains(due, fragment) {
			t.Fatalf("due 批次 SQL 缺少 %q", fragment)
		}
	}
	if _, err := nilStore.ListDueOrganizationBatchIDs(context.Background(), time.Now(), maxOrganizationClaimSize+1); !errors.Is(err, ErrInvalidOrganizationInput) {
		t.Fatalf("越界 due limit error = %v", err)
	}
	claim := strings.Join(strings.Fields(claimOrganizationBatchesSQL), " ")
	for _, fragment := range []string{
		"FOR UPDATE OF b SKIP LOCKED",
		"locked_parents AS MATERIALIZED",
		"FOR UPDATE OF s",
		"CROSS JOIN parent_gate",
		"state='organizing'",
		"state='pending_organization'",
		"state='organizing'",
	} {
		if !strings.Contains(claim, fragment) {
			t.Fatalf("领取 SQL 缺少 %q", fragment)
		}
	}
	claimOne := strings.Join(strings.Fields(claimOrganizationBatchSQL), " ")
	for _, fragment := range []string{
		"WHERE b.id=$1",
		"FOR UPDATE OF b SKIP LOCKED",
		"locked_parents AS MATERIALIZED",
		"FOR UPDATE OF s",
		"CROSS JOIN parent_gate",
		"b.state='organizing' AND b.lease_expires_at <= $2",
		"SET state='failed', last_error='lease expired'",
		"SET state='organizing', attempt_count=b.attempt_count+1",
		"se.state='pending_organization'",
		"s.internal_state='pending_organization'",
	} {
		if !strings.Contains(claimOne, fragment) {
			t.Fatalf("单批领取 SQL 缺少 %q", fragment)
		}
	}
	if _, found, err := nilStore.ClaimOrganizationBatch(context.Background(), uuid.New(), time.Now(), time.Minute); found || !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("未初始化单批领取 = found %v, error %v", found, err)
	}
	content, err := migrationFS.ReadFile("migrations/020_organization_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := string(content)
	for _, fragment := range []string{
		"organization_batches_intent_idx",
		"organization_batches_plan_check",
		"organization_batches_root_length_check",
		"organization_batches_lease_check",
		"organization_batches_completed_at_check",
		"organization_files_claim_idx",
		"organization_files_completed_at_check",
		"organization_batch_plan_immutable",
		"organization_file_plan_immutable",
		"lease_token UUID",
		"attempt_count BETWEEN 0 AND 8",
	} {
		if !strings.Contains(migration, fragment) {
			t.Fatalf("迁移缺少 %q", fragment)
		}
	}
	fingerprintContent, err := migrationFS.ReadFile("migrations/021_organization_file_fingerprints.sql")
	if err != nil {
		t.Fatal(err)
	}
	fingerprintMigration := string(fingerprintContent)
	for _, fragment := range []string{
		"expected_size BIGINT",
		"expected_sha256 TEXT",
		"organization_files_expected_size_check",
		"organization_files_expected_sha256_check",
		"expected_size >= 0",
		"^[0-9a-f]{64}$",
		"NEW.expected_size IS DISTINCT FROM OLD.expected_size",
		"NEW.expected_sha256 IS DISTINCT FROM OLD.expected_sha256",
	} {
		if !strings.Contains(fingerprintMigration, fragment) {
			t.Fatalf("指纹迁移缺少 %q", fragment)
		}
	}
}

func TestScanOrganizationFileRejectsInvalidStoredData(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		expectedSize   int64
		expectedSHA256 string
		state          OrganizationFileState
	}{
		{name: "negative expected size", expectedSize: -1, expectedSHA256: organizationTestSHA256, state: OrganizationFilePlanned},
		{name: "uppercase SHA-256", expectedSize: 5, expectedSHA256: strings.Repeat("A", 64), state: OrganizationFilePlanned},
		{name: "short SHA-256", expectedSize: 5, expectedSHA256: strings.Repeat("a", 63), state: OrganizationFilePlanned},
		{name: "non hexadecimal SHA-256", expectedSize: 5, expectedSHA256: strings.Repeat("g", 64), state: OrganizationFilePlanned},
		{name: "corrupt state", expectedSize: 5, expectedSHA256: organizationTestSHA256, state: OrganizationFileState("corrupt")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := organizationRowFunc(func(dest ...any) error {
				*dest[0].(*int64) = 1
				*dest[1].(*uuid.UUID) = uuid.MustParse("11111111-2222-4333-8444-555555555555")
				*dest[2].(*int) = 0
				*dest[3].(*string) = "/downloads/a.mkv"
				*dest[4].(*string) = "/library/a.mkv"
				*dest[5].(*organizer.Mode) = organizer.Copy
				*dest[6].(*int64) = tt.expectedSize
				*dest[7].(*string) = tt.expectedSHA256
				*dest[8].(*OrganizationFileState) = tt.state
				*dest[9].(*int) = 0
				*dest[10].(*string) = ""
				*dest[11].(**time.Time) = nil
				*dest[12].(*time.Time) = time.Now()
				return nil
			})
			if _, err := scanOrganizationFile(row); !errors.Is(err, ErrOrganizationPersistence) {
				t.Fatalf("scanOrganizationFile() error = %v, want ErrOrganizationPersistence", err)
			}
		})
	}
}

func TestScanOrganizationBatchRejectsInvalidStoredFingerprint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	encoded, err := json.Marshal([]organizer.Operation{{
		Source: filepath.Join(sourceRoot, "a.mkv"),
		Target: filepath.Join(targetRoot, "a.mkv"),
		Mode:   organizer.Copy, ExpectedSize: 5, ExpectedSHA256: strings.Repeat("A", 64),
	}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	row := organizationRowFunc(func(dest ...any) error {
		*dest[0].(*uuid.UUID) = uuid.MustParse("11111111-2222-4333-8444-555555555555")
		*dest[1].(*uuid.UUID) = uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
		*dest[2].(*OrganizationBatchState) = OrganizationBatchPlanned
		*dest[3].(*string) = sourceRoot
		*dest[4].(*string) = targetRoot
		*dest[5].(*[]byte) = encoded
		*dest[6].(*string) = ""
		*dest[7].(*int) = 0
		*dest[8].(*time.Time) = now
		*dest[9].(**uuid.UUID) = nil
		*dest[10].(**time.Time) = nil
		*dest[11].(**time.Time) = nil
		*dest[12].(*time.Time) = now
		*dest[13].(*time.Time) = now
		return nil
	})
	if _, err := scanOrganizationBatch(row); !errors.Is(err, ErrOrganizationPersistence) {
		t.Fatalf("scanOrganizationBatch() error = %v, want ErrOrganizationPersistence", err)
	}
}

const organizationTestSHA256 = "0cab1c9617404faf2b24e221e189ca5945813e14d3f766345b09ca13bbe28ffc"

func organizationTestOperation(source, target string, mode organizer.Mode) organizer.Operation {
	return organizer.Operation{
		Source:         source,
		Target:         target,
		Mode:           mode,
		ExpectedSize:   5,
		ExpectedSHA256: organizationTestSHA256,
	}
}

func organizationPlanWithFingerprint(plan organizer.Plan, size int64, digest string) organizer.Plan {
	operations := append([]organizer.Operation(nil), plan.Operations...)
	operations[0].ExpectedSize = size
	operations[0].ExpectedSHA256 = digest
	return organizer.Plan{ID: plan.ID, TargetRoot: plan.TargetRoot, Operations: operations}
}

func mustOrganizationPlan(t *testing.T, targetRoot string, operations []organizer.Operation) organizer.Plan {
	t.Helper()
	plan, err := organizer.BuildPlan(targetRoot, operations)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

type organizationRowFunc func(...any) error

func (scan organizationRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}
