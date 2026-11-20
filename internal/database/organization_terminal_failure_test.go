package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestOrganizationTerminalFailureMigrationContract(t *testing.T) {
	content, err := migrationFS.ReadFile("migrations/028_organization_file_terminal_failure.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"state IN ('planned', 'organizing', 'failed', 'terminal_failed', 'completed')",
		"state IN ('completed', 'terminal_failed')",
		"state <> 'terminal_failed' OR octet_length(last_error) BETWEEN 1 AND 2048",
		"UPDATE organization_files SET state='failed', completed_at=NULL WHERE state='terminal_failed'",
		"state IN ('planned', 'organizing', 'failed', 'completed')",
		"(state='completed') = (completed_at IS NOT NULL)",
	} {
		if !strings.Contains(migration, fragment) {
			t.Fatalf("terminal failure migration missing %q", fragment)
		}
	}

	downgrade := strings.Index(migration, "UPDATE organization_files SET state='failed', completed_at=NULL WHERE state='terminal_failed'")
	restoreStateConstraint := strings.LastIndex(migration, "state IN ('planned', 'organizing', 'failed', 'completed')")
	if downgrade < 0 || restoreStateConstraint < 0 || downgrade >= restoreStateConstraint {
		t.Fatal("terminal failure migration must downgrade rows before restoring the old state constraint")
	}
}

func TestOrganizationTerminalFailureStoredState(t *testing.T) {
	t.Parallel()

	if !validOrganizationFileState(OrganizationFileTerminalFailed) {
		t.Fatal("terminal_failed must be a valid stored organization file state")
	}

	now := time.Now().UTC()
	tests := []struct {
		name        string
		lastError   string
		completedAt *time.Time
		wantErr     error
	}{
		{
			name:        "terminal failure with completion time and stable error",
			lastError:   "copy failed",
			completedAt: &now,
		},
		{
			name:      "terminal failure without completion time",
			lastError: "copy failed",
			wantErr:   ErrOrganizationPersistence,
		},
		{
			name:        "terminal failure without stable error",
			completedAt: &now,
			wantErr:     ErrOrganizationPersistence,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			row := organizationRowFunc(func(dest ...any) error {
				*dest[0].(*int64) = 1
				*dest[1].(*uuid.UUID) = uuid.MustParse("11111111-2222-4333-8444-555555555555")
				*dest[2].(*int) = 0
				*dest[3].(*string) = filepath.Join(root, "downloads", "a.mkv")
				*dest[4].(*string) = filepath.Join(root, "library", "a.mkv")
				*dest[5].(*organizer.Mode) = organizer.Copy
				*dest[6].(*int64) = 5
				*dest[7].(*string) = organizationTestSHA256
				*dest[8].(*OrganizationFileState) = OrganizationFileTerminalFailed
				*dest[9].(*int) = 1
				*dest[10].(*string) = tt.lastError
				*dest[11].(**time.Time) = tt.completedAt
				*dest[12].(*time.Time) = now
				return nil
			})

			file, err := scanOrganizationFile(row)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("scanOrganizationFile() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (file.State != OrganizationFileTerminalFailed || file.LastError != tt.lastError || file.CompletedAt == nil) {
				t.Fatalf("scanOrganizationFile() = %+v, want terminal failure with stored error and completion time", file)
			}
		})
	}
}

func TestFinalizeOrganizationFileFailureRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	var store *Store
	batchID := uuid.New()
	token := uuid.New()
	tests := []struct {
		name    string
		batchID uuid.UUID
		token   uuid.UUID
		fileID  int64
	}{
		{name: "nil batch ID", token: token, fileID: 1},
		{name: "nil lease token", batchID: batchID, fileID: 1},
		{name: "non-positive file ID", batchID: batchID, token: token},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := store.FinalizeOrganizationFileFailure(
				context.Background(), tt.batchID, tt.token, tt.fileID, "copy failed",
			)
			if !errors.Is(err, ErrInvalidOrganizationInput) {
				t.Fatalf("FinalizeOrganizationFileFailure() error = %v, want ErrInvalidOrganizationInput", err)
			}
		})
	}
}
