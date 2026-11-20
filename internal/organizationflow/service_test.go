package organizationflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestPlanningServiceCreatesAndEnqueuesExactBatch(t *testing.T) {
	t.Parallel()
	fixture := newPlanningFixture(t)
	extra := filepath.Join(fixture.content.Task.ContentPath, "sample.mkv")
	writePlannerFile(t, extra, "not-selected")
	batchID := uuid.New()
	store := &planningStoreFake{
		intent:      fixture.intent,
		directories: fixture.directories,
		createBatch: database.OrganizationBatch{ID: batchID, IntentID: fixture.intent.ID},
	}
	submitter := &planningSubmitterFake{content: fixture.content, found: true}
	enqueuer := &planningEnqueuerFake{}
	service := newPlanningServiceForTest(t, store, fixture.downloaderID, submitter, enqueuer)

	if err := service.HandleDownloadCompleted(t.Context(), fixture.target); err != nil {
		t.Fatal(err)
	}
	if submitter.label != fixture.intent.Label || submitter.hash != fixture.intent.TorrentHash {
		t.Fatalf("inspect identity = %q/%q", submitter.label, submitter.hash)
	}
	if store.createdSourceRoot != fixture.content.Task.ContentPath || len(store.createdPlan.Operations) != 1 {
		t.Fatalf("created plan = %#v, source root = %q", store.createdPlan, store.createdSourceRoot)
	}
	if len(store.createdSnapshots) != 1 || store.createdSnapshots[0].IntentID != fixture.intent.ID ||
		store.createdSnapshots[0].Username != fixture.target.Username {
		t.Fatalf("created snapshots = %#v", store.createdSnapshots)
	}
	if samePath(store.createdPlan.Operations[0].Source, extra) {
		t.Fatal("unselected file entered persisted plan")
	}
	if len(enqueuer.args) != 1 || enqueuer.args[0].BatchID != batchID {
		t.Fatalf("enqueued args = %#v", enqueuer.args)
	}
}

func TestPlanningServiceReenqueuesExistingImmutableBatchOnConflict(t *testing.T) {
	t.Parallel()
	fixture := newPlanningFixture(t)
	existingID := uuid.New()
	store := &planningStoreFake{
		intent:                fixture.intent,
		directories:           fixture.directories,
		createErr:             database.ErrOrganizationPlanConflict,
		existingNotFoundCalls: 1,
		existingBatch: database.OrganizationBatch{
			ID: existingID, IntentID: fixture.intent.ID,
		},
	}
	enqueuer := &planningEnqueuerFake{}
	service := newPlanningServiceForTest(
		t,
		store,
		fixture.downloaderID,
		&planningSubmitterFake{content: fixture.content, found: true},
		enqueuer,
	)

	if err := service.HandleDownloadCompleted(t.Context(), fixture.target); err != nil {
		t.Fatal(err)
	}
	if store.getExistingCalls != 2 || len(enqueuer.args) != 1 || enqueuer.args[0].BatchID != existingID {
		t.Fatalf("existing calls/enqueue = %d/%#v", store.getExistingCalls, enqueuer.args)
	}
}

func TestPlanningServiceReplaysExistingBatchWithoutDownloaderSnapshot(t *testing.T) {
	t.Parallel()
	fixture := newPlanningFixture(t)
	existingID := uuid.New()
	store := &planningStoreFake{
		intent: fixture.intent,
		existingBatch: database.OrganizationBatch{
			ID: existingID, IntentID: fixture.intent.ID,
		},
	}
	submitter := &planningSubmitterFake{err: errors.New("downloader should not be called")}
	enqueuer := &planningEnqueuerFake{}
	service := newPlanningServiceForTest(t, store, fixture.downloaderID, submitter, enqueuer)

	if err := service.HandleDownloadCompleted(t.Context(), fixture.target); err != nil {
		t.Fatal(err)
	}
	if submitter.label != "" || submitter.hash != "" {
		t.Fatalf("existing batch replay queried downloader with %q/%q", submitter.label, submitter.hash)
	}
	if len(enqueuer.args) != 1 || enqueuer.args[0].BatchID != existingID {
		t.Fatalf("enqueued args = %#v", enqueuer.args)
	}
}

func TestPlanningServiceRejectsUnconfirmedOrUnavailableContent(t *testing.T) {
	fixture := newPlanningFixture(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		mutate    func(*planningFixture, *planningStoreFake, *planningSubmitterFake, *planningEnqueuerFake)
		provider  downloadflow.SubmitterProvider
		wantError error
	}{
		{name: "nil context", wantError: ErrInvalidOrganizationPlanningService},
		{name: "canceled context", ctx: canceled, wantError: context.Canceled},
		{
			name: "intent is no longer completed", ctx: t.Context(), wantError: ErrOrganizationTaskContentUnavailable,
			mutate: func(_ *planningFixture, store *planningStoreFake, _ *planningSubmitterFake, _ *planningEnqueuerFake) {
				store.intent.State = downloader.IntentDownloading
			},
		},
		{
			name: "task missing", ctx: t.Context(), wantError: ErrOrganizationTaskContentUnavailable,
			mutate: func(_ *planningFixture, _ *planningStoreFake, submitter *planningSubmitterFake, _ *planningEnqueuerFake) {
				submitter.found = false
			},
		},
		{
			name: "task hash changed", ctx: t.Context(), wantError: ErrOrganizationTaskContentUnavailable,
			mutate: func(_ *planningFixture, _ *planningStoreFake, submitter *planningSubmitterFake, _ *planningEnqueuerFake) {
				submitter.content.Task.Hash = strings.Repeat("b", 40)
			},
		},
		{
			name: "settings unavailable", ctx: t.Context(), wantError: ErrOrganizationPlanningUnavailable,
			mutate: func(_ *planningFixture, store *planningStoreFake, _ *planningSubmitterFake, _ *planningEnqueuerFake) {
				store.settingErr = errors.New("sensitive storage detail")
			},
		},
		{
			name: "enqueue unavailable", ctx: t.Context(), wantError: ErrOrganizationPlanningUnavailable,
			mutate: func(_ *planningFixture, _ *planningStoreFake, _ *planningSubmitterFake, enqueuer *planningEnqueuerFake) {
				enqueuer.err = errors.New("river detail")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := fixture.clone()
			store := &planningStoreFake{
				intent:      current.intent,
				directories: current.directories,
				createBatch: database.OrganizationBatch{ID: uuid.New(), IntentID: current.intent.ID},
			}
			submitter := &planningSubmitterFake{content: current.content, found: true}
			enqueuer := &planningEnqueuerFake{}
			if tt.mutate != nil {
				tt.mutate(&current, store, submitter, enqueuer)
			}
			provider := tt.provider
			if provider == nil {
				provider = planningSubmitterProvider{downloaderID: current.downloaderID, submitter: submitter}
			}
			service, err := NewPlanningService(store, provider, enqueuer)
			if err != nil {
				t.Fatal(err)
			}
			err = service.HandleDownloadCompleted(tt.ctx, current.target)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("HandleDownloadCompleted() error = %v, want %v", err, tt.wantError)
			}
		})
	}
}

type planningFixture struct {
	downloaderID string
	intent       downloader.Intent
	target       downloader.ReconciliationTarget
	content      downloader.TaskContent
	directories  []downloadconfig.Directory
}

func newPlanningFixture(t *testing.T) planningFixture {
	t.Helper()
	root := t.TempDir()
	downloadRoot := filepath.Join(root, "downloads")
	libraryRoot := filepath.Join(root, "library")
	contentRoot := filepath.Join(downloadRoot, "Fixture")
	source := filepath.Join(contentRoot, "Fixture.mkv")
	writePlannerFile(t, source, "video")
	intentID := uuid.New()
	downloaderID := uuid.NewString()
	hash := strings.Repeat("a", 40)
	label := "mp-" + strings.Repeat("c", 32)
	intent := downloader.Intent{ID: intentID, Label: label, State: downloader.IntentCompleted, TorrentHash: hash}
	return planningFixture{
		downloaderID: downloaderID,
		intent:       intent,
		target: downloader.ReconciliationTarget{
			ID: intentID, Username: "alice", DownloaderID: downloaderID,
			State: downloader.IntentCompleted, TorrentHash: hash,
		},
		content: downloader.TaskContent{
			Task: downloader.Task{
				Hash: hash, Label: label, State: downloader.TaskStateCompleted,
				SavePath: downloadRoot, ContentPath: contentRoot,
			},
			Files: []downloader.TaskContentFile{{
				RelativePath: "Fixture/Fixture.mkv", SizeBytes: int64(len("video")),
			}},
		},
		directories: []downloadconfig.Directory{localDirectory(downloadRoot, libraryRoot, "copy", 0)},
	}
}

func (f planningFixture) clone() planningFixture {
	f.content.Files = append([]downloader.TaskContentFile(nil), f.content.Files...)
	f.directories = append([]downloadconfig.Directory(nil), f.directories...)
	return f
}

type planningStoreFake struct {
	intent                downloader.Intent
	directories           []downloadconfig.Directory
	settingErr            error
	createBatch           database.OrganizationBatch
	createErr             error
	existingBatch         database.OrganizationBatch
	getExistingErr        error
	getExistingCalls      int
	existingNotFoundCalls int
	createdSourceRoot     string
	createdPlan           organizer.Plan
	createdSnapshots      []organization.SuccessSnapshot
	successInput          organization.SuccessInput
	successInputErr       error
}

func (s *planningStoreFake) GetDownloadIntent(context.Context, string, uuid.UUID) (downloader.Intent, error) {
	return s.intent, nil
}

func (s *planningStoreFake) GetDownloadIntentSuccessInput(
	_ context.Context,
	username string,
	intentID uuid.UUID,
) (organization.SuccessInput, error) {
	if s.successInputErr != nil {
		return organization.SuccessInput{}, s.successInputErr
	}
	if s.successInput.IntentID == uuid.Nil {
		return organization.SuccessInput{
			IntentID: intentID, Version: organization.CurrentSuccessVersion, Username: username,
		}, nil
	}
	return s.successInput.Clone(), nil
}

func (s *planningStoreFake) GetSetting(_ context.Context, _ string, target any) (bool, error) {
	if s.settingErr != nil {
		return false, s.settingErr
	}
	directories, ok := target.(*[]downloadconfig.Directory)
	if !ok {
		return false, errors.New("unexpected setting target")
	}
	*directories = append([]downloadconfig.Directory(nil), s.directories...)
	return true, nil
}

func (s *planningStoreFake) CreateOrganizationBatch(
	_ context.Context,
	_ string,
	_ uuid.UUID,
	sourceRoot string,
	plan organizer.Plan,
	snapshots []organization.SuccessSnapshot,
) (database.OrganizationBatch, error) {
	s.createdSourceRoot = sourceRoot
	s.createdPlan = plan
	s.createdSnapshots = make([]organization.SuccessSnapshot, len(snapshots))
	for index := range snapshots {
		s.createdSnapshots[index] = snapshots[index].Clone()
	}
	return s.createBatch, s.createErr
}

func (s *planningStoreFake) GetOrganizationBatchByIntent(context.Context, string, uuid.UUID) (database.OrganizationBatch, error) {
	s.getExistingCalls++
	if s.getExistingCalls <= s.existingNotFoundCalls {
		return database.OrganizationBatch{}, database.ErrOrganizationBatchNotFound
	}
	if s.getExistingErr == nil && s.existingBatch.ID == uuid.Nil {
		return database.OrganizationBatch{}, database.ErrOrganizationBatchNotFound
	}
	return s.existingBatch, s.getExistingErr
}

type planningSubmitterProvider struct {
	downloaderID string
	submitter    downloadflow.TorrentSubmitter
}

func (p planningSubmitterProvider) ResolveSubmitter(downloaderID string) (downloadflow.TorrentSubmitter, error) {
	if downloaderID != p.downloaderID || p.submitter == nil {
		return nil, downloadflow.ErrDownloaderUnavailable
	}
	return p.submitter, nil
}

type planningSubmitterFake struct {
	content downloader.TaskContent
	found   bool
	err     error
	label   string
	hash    string
}

func (s *planningSubmitterFake) InspectTaskContent(
	_ context.Context,
	label string,
	hash string,
) (downloader.TaskContent, bool, error) {
	s.label, s.hash = label, hash
	return s.content, s.found, s.err
}

func (*planningSubmitterFake) ObserveTasks(context.Context, []string) (map[string]downloader.TaskObservation, error) {
	return nil, nil
}

func (*planningSubmitterFake) Reconcile(_ context.Context, intent downloader.Intent) (downloader.Intent, bool, error) {
	return intent, false, nil
}

func (*planningSubmitterFake) AddOnce(
	_ context.Context,
	intent downloader.Intent,
	_ downloader.Candidate,
	_ []byte,
) (downloader.Intent, error) {
	return intent, nil
}

type planningEnqueuerFake struct {
	args []jobs.OrganizationArgs
	err  error
}

func (e *planningEnqueuerFake) EnqueueOrganization(_ context.Context, args jobs.OrganizationArgs) error {
	e.args = append(e.args, args)
	return e.err
}

func newPlanningServiceForTest(
	t *testing.T,
	store PlanningStore,
	downloaderID string,
	submitter downloadflow.TorrentSubmitter,
	enqueuer OrganizationEnqueuer,
) *PlanningService {
	t.Helper()
	service, err := NewPlanningService(
		store,
		planningSubmitterProvider{downloaderID: downloaderID, submitter: submitter},
		enqueuer,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

var _ downloader.TaskContentInspector = (*planningSubmitterFake)(nil)
var _ downloadflow.TorrentSubmitter = (*planningSubmitterFake)(nil)
