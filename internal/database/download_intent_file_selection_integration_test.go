//go:build integration

package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestDownloadIntentFileSelectionPostgresPlanSemantics(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "file-selection-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "file-selection-site")
	publicStore := NewStore(store.db, nil)
	selected := []downloader.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	tests := []struct {
		name          string
		knownTV       bool
		paused        bool
		emptyInput    bool
		emptyCoverage bool
		want          []downloader.Episode
	}{
		{name: "known-full-season", knownTV: true},
		{name: "known-unpack", knownTV: true, paused: true, want: selected},
		{name: "unknown-unpaused", want: selected},
		{name: "unknown-paused", paused: true, want: selected},
		{name: "known-empty-input-unpaused", knownTV: true, emptyInput: true},
		{name: "known-empty-input-paused", knownTV: true, paused: true, emptyInput: true},
		{name: "unknown-empty-input-unpaused", emptyInput: true},
		{name: "unknown-empty-input-paused", paused: true, emptyInput: true},
		{name: "known-empty-coverage-unpaused", knownTV: true, emptyCoverage: true},
		{name: "known-empty-coverage-paused", knownTV: true, paused: true, emptyCoverage: true, want: selected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := organization.SuccessInput{
				Version: organization.CurrentSuccessVersion, Username: username,
				Media: organization.MediaContext{MediaType: "电视剧"}, ResourceTerm: "2160p WEB-DL",
				Episodes: []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
			}
			if tt.emptyInput {
				input.Episodes = []organization.Episode{}
			}
			plan := DownloadIntentPlan{Paused: tt.paused}
			if tt.knownTV {
				priority := 90
				plan.TVPriority = &priority
				plan.TVCoverage = []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
				plan.TVFullCoverage = true
				if tt.emptyCoverage {
					plan.TVCoverage = []subscription.Episode{}
				}
			}
			input, plan = createFileSelectionPostgresFixture(t, store, username, siteID, tt.name, input, plan)
			got, err := publicStore.GetDownloadIntentFileSelection(t.Context(), " \t"+username+" ", input.IntentID)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("读取文件选择 = %v, %v; want %v", got, err, tt.want)
			}
			if len(got) > 0 {
				got[0].Episode = 99
				replayed, err := publicStore.GetDownloadIntentFileSelection(t.Context(), username, input.IntentID)
				if err != nil || !reflect.DeepEqual(replayed, tt.want) {
					t.Fatalf("重读文件选择受调用方修改影响 = %v, %v", replayed, err)
				}
			}
			storedInput, err := store.GetDownloadIntentSuccessInput(t.Context(), username, input.IntentID)
			if err != nil || !reflect.DeepEqual(storedInput, input) {
				t.Fatalf("读取文件选择改写质量或通知事实 = %#v, %v", storedInput, err)
			}
			storedPlan, err := store.GetDownloadIntentPlan(t.Context(), username, input.IntentID)
			if err != nil || !sameDownloadIntentPlan(storedPlan, plan) {
				t.Fatalf("读取文件选择改写不可变计划 = %#v, %v", storedPlan, err)
			}
		})
	}
}

func TestDownloadIntentFileSelectionPostgresOwnerMissingAndCancellation(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "file-selection-errors-owner"
	insertDownloadIntentIntegrationUsers(t, store, username, "file-selection-other")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "file-selection-errors-site")
	input := organization.SuccessInput{Version: organization.CurrentSuccessVersion, Username: username, Episodes: []organization.Episode{}}
	input, _ = createFileSelectionPostgresFixture(t, store, username, siteID, "owner-scope", input, DownloadIntentPlan{})
	missingInput := mustDatabaseDownloadIntent(t, username, "file-selection-missing-input")
	if _, err := store.CreateDownloadIntent(t.Context(), username, missingInput); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDownloadIntentPlan(t.Context(), username, downloadIntentPlanIntegrationFixture(missingInput.ID, siteID, "missing-input")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name     string
		ctx      context.Context
		username string
		intentID uuid.UUID
		wantErr  error
	}{
		{name: "其他 owner", ctx: t.Context(), username: "file-selection-other", intentID: input.IntentID, wantErr: ErrDownloadIntentSuccessInputNotFound},
		{name: "缺成功输入", ctx: t.Context(), username: username, intentID: missingInput.ID, wantErr: ErrDownloadIntentSuccessInputNotFound},
		{name: "不存在意图", ctx: t.Context(), username: username, intentID: uuid.New(), wantErr: ErrDownloadIntentSuccessInputNotFound},
		{name: "非法 owner", ctx: t.Context(), username: " \t", intentID: input.IntentID, wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "非法意图", ctx: t.Context(), username: username, wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "取消", ctx: ctx, username: username, intentID: input.IntentID, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.GetDownloadIntentFileSelection(tt.ctx, tt.username, tt.intentID)
			if got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("文件选择错误 = %v, %v; want nil, %v", got, err, tt.wantErr)
			}
		})
	}
}

func TestDownloadIntentFileSelectionPostgresCorruptionAndDatabaseErrors(t *testing.T) {
	tests := []struct {
		name    string
		fault   func(*testing.T, *Store, uuid.UUID)
		wantErr error
	}{
		{name: "快照 owner 损坏", fault: func(t *testing.T, store *Store, intentID uuid.UUID) {
			fileSelectionExecFault(t, store, `ALTER TABLE download_intent_success_inputs DISABLE TRIGGER download_intent_success_inputs_immutable`)
			fileSelectionExecFault(t, store, `UPDATE download_intent_success_inputs SET username='file-selection-corrupt-owner' WHERE intent_id=$1`, intentID)
		}, wantErr: ErrDownloadIntentSuccessInputPersistence},
		{name: "快照集数损坏", fault: func(t *testing.T, store *Store, intentID uuid.UUID) {
			fileSelectionExecFault(t, store, `ALTER TABLE download_intent_success_inputs DISABLE TRIGGER download_intent_success_inputs_immutable`)
			fileSelectionExecFault(t, store, `UPDATE download_intent_success_inputs SET episodes='[{"season":1,"episode":0}]'::jsonb WHERE intent_id=$1`, intentID)
		}, wantErr: ErrDownloadIntentSuccessInputPersistence},
		{name: "成功输入数据库失败", fault: func(t *testing.T, store *Store, _ uuid.UUID) {
			fileSelectionExecFault(t, store, `ALTER TABLE download_intent_success_inputs RENAME TO download_intent_success_inputs_fault`)
		}, wantErr: ErrDownloadIntentSuccessInputPersistence},
		{name: "计划数据库失败", fault: func(t *testing.T, store *Store, _ uuid.UUID) {
			fileSelectionExecFault(t, store, `ALTER TABLE download_intent_plans RENAME TO download_intent_plans_fault`)
		}, wantErr: ErrDownloadIntentPlanPersistence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newDownloadIntentPlanIntegrationStore(t)
			const username = "file-selection-fault-owner"
			insertDownloadIntentIntegrationUsers(t, store, username)
			siteID := insertDownloadIntentPlanIntegrationSite(t, store, "file-selection-fault-site")
			input := organization.SuccessInput{
				Version: organization.CurrentSuccessVersion, Username: username,
				Episodes: []organization.Episode{{Season: 1, Episode: 1}},
			}
			plan := DownloadIntentPlan{TVCoverage: []subscription.Episode{{Season: 1, Episode: 1}}}
			input, plan = createFileSelectionPostgresFixture(t, store, username, siteID, "fault", input, plan)
			tt.fault(t, store, input.IntentID)
			got, err := store.GetDownloadIntentFileSelection(t.Context(), username, input.IntentID)
			if got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("数据库损坏后的文件选择 = %v, %v; want nil, %v", got, err, tt.wantErr)
			}
			for _, forbidden := range []string{username, input.IntentID.String(), plan.ResourceSecret, "SELECT", "download_intent", "file-selection-corrupt-owner"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("文件选择错误泄露字段 %q: %v", forbidden, err)
				}
			}
		})
	}
}

func createFileSelectionPostgresFixture(
	t *testing.T,
	store *Store,
	username string,
	siteID int64,
	resourceID string,
	input organization.SuccessInput,
	flags DownloadIntentPlan,
) (organization.SuccessInput, DownloadIntentPlan) {
	t.Helper()
	intent := mustDatabaseDownloadIntent(t, username, "file-selection-"+resourceID)
	if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
		t.Fatalf("创建文件选择意图: %v", err)
	}
	plan := downloadIntentPlanIntegrationFixture(intent.ID, siteID, resourceID)
	plan.Paused, plan.TVPriority, plan.TVCoverage, plan.TVFullCoverage = flags.Paused, flags.TVPriority, flags.TVCoverage, flags.TVFullCoverage
	if _, err := store.CreateDownloadIntentPlan(t.Context(), username, plan); err != nil {
		t.Fatalf("创建文件选择计划: %v", err)
	}
	input.IntentID = intent.ID
	createDownloadSuccessInputIntegration(t, store, username, input)
	return input, plan
}

func fileSelectionExecFault(t *testing.T, store *Store, sql string, args ...any) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("准备文件选择数据库故障: %v", err)
	}
}
