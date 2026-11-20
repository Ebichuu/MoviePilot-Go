package organizationflow

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

func TestHandlerRequiresCompleteTerminalCapabilityBundle(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	intentID := uuid.New()
	token := uuid.New()
	root := t.TempDir()
	plan := organizer.Plan{
		ID:         "sha256:" + strings.Repeat("a", 64),
		TargetRoot: filepath.Join(root, "library"),
		Operations: []organizer.Operation{{
			Source: filepath.Join(root, "downloads", "movie.mkv"),
			Target: filepath.Join(root, "library", "movie.mkv"),
			Mode:   organizer.Copy, ExpectedSize: 1, ExpectedSHA256: handlerTestSHA256,
		}},
	}
	store := &terminalCapabilityStoreFake{
		durableCompletionStoreFake: &durableCompletionStoreFake{
			executionStoreFake: &executionStoreFake{
				batch: database.OrganizationBatch{
					ID: batchID, IntentID: intentID, State: database.OrganizationBatchOrganizing,
					LeaseToken: token, SourceRoot: filepath.Join(root, "downloads"), Plan: plan,
				},
				claimed: true,
				filePages: [][]database.OrganizationFile{{{
					ID: 1, BatchID: batchID, State: database.OrganizationFileOrganizing,
					SourcePath: plan.Operations[0].Source, TargetPath: plan.Operations[0].Target,
					Mode: organizer.Copy, ExpectedSize: 1, ExpectedSHA256: handlerTestSHA256,
				}}},
			},
		},
		outcomes: []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted}},
	}
	handler, err := NewHandler(
		store,
		&organizationEnqueuerFake{},
		&mediaIndexCompletionPlannerFake{},
		&mediaIndexEnqueuerFake{},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.execute = func(context.Context, string, organizer.Plan) error {
		return errors.New("copy failed")
	}

	err = handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
	if !errors.Is(err, ErrOrganizationFileOperation) {
		t.Fatalf("HandleOrganization() error = %v, want ErrOrganizationFileOperation", err)
	}
	if store.finalizeCalls != 0 || store.outcomeCalls != 0 {
		t.Fatalf("terminal capability used without durable bundle: finalize=%d outcomes=%d",
			store.finalizeCalls, store.outcomeCalls)
	}
	if store.failedFileError != organizationStableFileError || store.failBatchCalls != 1 {
		t.Fatalf("legacy retry path = file error %q, batch failures %d",
			store.failedFileError, store.failBatchCalls)
	}
}

func TestHandlerSnapshotlessLegacyFallbackRequiresAllSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		outcomes   []organization.FileOutcome
		input      organization.SuccessInput
		wantErr    bool
		wantLegacy bool
	}{
		{
			name: "explicit legacy all-success",
			outcomes: []organization.FileOutcome{
				{Ordinal: 1, State: organization.FileOutcomeCompleted},
				{Ordinal: 0, State: organization.FileOutcomeCompleted},
			},
			input: organization.SuccessInput{Version: 0, Legacy: true}, wantLegacy: true,
		},
		{
			name: "legacy terminal failure",
			outcomes: []organization.FileOutcome{
				{Ordinal: 0, State: organization.FileOutcomeCompleted},
				{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "copy failed"},
			},
			input: organization.SuccessInput{Version: 0, Legacy: true}, wantErr: true,
		},
		{
			name: "legacy incomplete coverage",
			outcomes: []organization.FileOutcome{
				{Ordinal: 0, State: organization.FileOutcomeCompleted},
			},
			input: organization.SuccessInput{Version: 0, Legacy: true}, wantErr: true,
		},
		{
			name: "new input cannot fallback",
			outcomes: []organization.FileOutcome{
				{Ordinal: 0, State: organization.FileOutcomeCompleted},
				{Ordinal: 1, State: organization.FileOutcomeCompleted},
			},
			input: organization.SuccessInput{Version: organization.CurrentSuccessVersion}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			batchID := uuid.New()
			intentID := uuid.New()
			token := uuid.New()
			root := t.TempDir()
			plan := organizer.Plan{
				ID:         "sha256:" + strings.Repeat("b", 64),
				TargetRoot: filepath.Join(root, "library"),
				Operations: []organizer.Operation{
					{Source: filepath.Join(root, "downloads", "one.mkv"), Target: filepath.Join(root, "library", "one.mkv"), Mode: organizer.Copy, ExpectedSize: 1, ExpectedSHA256: handlerTestSHA256},
					{Source: filepath.Join(root, "downloads", "two.mkv"), Target: filepath.Join(root, "library", "two.mkv"), Mode: organizer.Copy, ExpectedSize: 2, ExpectedSHA256: handlerTestSHA256},
				},
			}
			tt.input.IntentID = intentID
			if !tt.input.Legacy {
				tt.input.Username = "alice"
			}
			store := &terminalCapabilityStoreFake{
				durableCompletionStoreFake: &durableCompletionStoreFake{
					executionStoreFake: &executionStoreFake{
						batch: database.OrganizationBatch{
							ID: batchID, IntentID: intentID, State: database.OrganizationBatchOrganizing,
							LeaseToken: token, SourceRoot: filepath.Join(root, "downloads"), Plan: plan,
						},
						claimed: true, filePages: [][]database.OrganizationFile{nil},
					},
					snapshotErr:  database.ErrOrganizationSuccessSnapshotNotFound,
					successInput: tt.input,
				},
				outcomes: tt.outcomes,
			}
			legacyPlan, err := notificationflow.BuildPlan(batchID, "alice", len(plan.Operations), nil)
			if err != nil {
				t.Fatal(err)
			}
			notificationPlanner := &terminalCapabilityNotificationPlannerFake{
				durableNotificationPlannerFake: &durableNotificationPlannerFake{plan: legacyPlan},
			}
			handler, err := NewHandler(
				store,
				&organizationEnqueuerFake{},
				&mediaIndexCompletionPlannerFake{},
				&mediaIndexEnqueuerFake{},
			)
			if err != nil {
				t.Fatal(err)
			}
			handler.execute = func(context.Context, string, organizer.Plan) error { return nil }
			handler.SetDurableSideEffects(
				store,
				&durableScrapePlannerFake{plan: scrapeflow.Plan{
					BatchID: batchID, IntentID: intentID, Username: "alice",
				}},
				notificationPlanner,
				&durableScrapeEnqueuerFake{},
				&durableNotificationEnqueuerFake{},
			)

			err = handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID})
			if (err != nil) != tt.wantErr {
				t.Fatalf("HandleOrganization() error = %v, wantErr %t", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrOrganizationIndexPlanning) {
				t.Fatalf("HandleOrganization() error = %v, want ErrOrganizationIndexPlanning", err)
			}
			if got := notificationPlanner.legacyCalls == 1; got != tt.wantLegacy {
				t.Fatalf("legacy planner called = %t, want %t", got, tt.wantLegacy)
			}
			wantCompletionCalls := 0
			if tt.wantLegacy {
				wantCompletionCalls = 1
			}
			if store.completionCalls != wantCompletionCalls || notificationPlanner.resultsCalls != 0 {
				t.Fatalf("completion/results calls = %d/%d, want %d/0",
					store.completionCalls, notificationPlanner.resultsCalls, wantCompletionCalls)
			}
			if tt.wantLegacy && !reflect.DeepEqual(
				store.gotNotificationPlan, legacyPlan,
			) {
				t.Fatalf("persisted legacy notification plan = %#v", store.gotNotificationPlan)
			}
		})
	}
}

func TestLegacyOrganizationOutcomesComplete(t *testing.T) {
	t.Parallel()
	plan := organizer.Plan{Operations: make([]organizer.Operation, 2)}
	tests := []struct {
		name     string
		outcomes []organization.FileOutcome
		want     bool
	}{
		{name: "complete", outcomes: []organization.FileOutcome{{Ordinal: 1, State: organization.FileOutcomeCompleted}, {Ordinal: 0, State: organization.FileOutcomeCompleted}}, want: true},
		{name: "duplicate", outcomes: []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted}, {Ordinal: 0, State: organization.FileOutcomeCompleted}}},
		{name: "out of range", outcomes: []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted}, {Ordinal: 2, State: organization.FileOutcomeCompleted}}},
		{name: "success with error", outcomes: []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted}, {Ordinal: 1, State: organization.FileOutcomeCompleted, Error: "unexpected"}}},
		{name: "terminal failure", outcomes: []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted}, {Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "failed"}}},
		{name: "missing", outcomes: []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := legacyOrganizationOutcomesComplete(plan, tt.outcomes); got != tt.want {
				t.Fatalf("legacyOrganizationOutcomesComplete() = %t, want %t", got, tt.want)
			}
		})
	}
}

type terminalCapabilityStoreFake struct {
	*durableCompletionStoreFake
	outcomes      []organization.FileOutcome
	outcomeErr    error
	outcomeCalls  int
	finalizeCalls int
}

func (s *terminalCapabilityStoreFake) ListOrganizationFileOutcomes(
	context.Context,
	string,
	uuid.UUID,
) ([]organization.FileOutcome, error) {
	s.outcomeCalls++
	return append([]organization.FileOutcome(nil), s.outcomes...), s.outcomeErr
}

func (s *terminalCapabilityStoreFake) FinalizeOrganizationFileFailure(
	context.Context,
	uuid.UUID,
	uuid.UUID,
	int64,
	string,
) error {
	s.finalizeCalls++
	return nil
}

type terminalCapabilityNotificationPlannerFake struct {
	*durableNotificationPlannerFake
	resultsCalls int
}

func (p *terminalCapabilityNotificationPlannerFake) PlanResults(
	uuid.UUID,
	string,
	[]organization.ResultSnapshot,
	[]organization.FileFailure,
	string,
) (notificationflow.Plan, error) {
	p.resultsCalls++
	return notificationflow.Plan{}, nil
}

var _ TerminalOrganizationFileStore = (*terminalCapabilityStoreFake)(nil)
var _ OrganizationOutcomeStore = (*terminalCapabilityStoreFake)(nil)
var _ TerminalNotificationPlanBuilder = (*terminalCapabilityNotificationPlannerFake)(nil)
