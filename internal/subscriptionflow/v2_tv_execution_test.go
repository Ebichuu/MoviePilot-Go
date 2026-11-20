package subscriptionflow

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zeebo/bencode"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestExecuteV2TVCandidatesSubmitsComplementaryTitleCandidatesSerially(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2, 3, 4))
	setV2TVComplementaryMetainfo(t, &fixture)
	fixture.execution.results = []downloadflow.IntentExecutionResult{
		{Outcome: downloadflow.IntentExecutionAccepted},
		{Outcome: downloadflow.IntentExecutionAccepted},
	}
	candidates := []subscription.Candidate{
		v2TVExecutionCandidate("first", 1, 1, 2),
		v2TVExecutionCandidate("second", 1, 3, 4),
	}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query, candidates, []indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"first", "second"}) ||
		len(got.RemainingEpisodes) != 0 || got.SubscriptionState != subscription.V2TVSubscriptionStateCompleted {
		t.Fatalf("selection result = %#v", got)
	}
	wantEpisodes := [][]subscription.Episode{
		v2TVTestEpisodes(1, 1, 2), v2TVTestEpisodes(1, 3, 4),
	}
	if gotEpisodes := fixture.submission.stageEpisodes(); !reflect.DeepEqual(gotEpisodes, wantEpisodes) {
		t.Fatalf("staged episodes = %#v, want %#v", gotEpisodes, wantEpisodes)
	}
	if fixture.execution.maxActive != 1 || fixture.execution.calls != 2 {
		t.Fatalf("execution calls/max active = %d/%d", fixture.execution.calls, fixture.execution.maxActive)
	}
}

func TestExecuteV2TVCandidatesFreezesV2TitleCoverage(t *testing.T) {
	t.Parallel()
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2))
	candidate := v2TVExecutionCandidate("title-overstates", 1, 1, 2)
	candidate.Resource.PriOrder = 70
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RemainingEpisodes) != 0 || len(fixture.submission.calls) != 1 {
		t.Fatalf("V2 title coverage = result:%#v stages:%#v", got, fixture.submission.calls)
	}
	call := fixture.submission.calls[0]
	if !reflect.DeepEqual(call.episodes, v2TVTestEpisodes(1, 1, 2)) ||
		!reflect.DeepEqual(call.plan.TVCoverage, v2TVTestEpisodes(1, 1, 2)) ||
		!reflect.DeepEqual(call.input.Episodes, []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}) ||
		call.plan.TVPriority == nil || *call.plan.TVPriority != 70 || call.plan.TVFullCoverage {
		t.Fatalf("immutable TV facts = %#v", call)
	}
}

func TestExecuteV2TVCandidatesDoesNotGateTitleOnFileEpisodeNames(t *testing.T) {
	t.Parallel()
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E02.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("false-title", 1, 1)},
		[]indexer.Adapter{fixture.adapter}, false)
	if err != nil || len(fixture.submission.calls) != 1 || fixture.execution.calls != 1 ||
		len(got.RemainingEpisodes) != 0 || fixture.submission.calls[0].plan.TVFullCoverage {
		t.Fatalf("false title staging = %#v / %v / stages:%#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVCandidatesRefreshesVersionAfterAcceptedProgress(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2, 3, 4))
	setV2TVComplementaryMetainfo(t, &fixture)
	initialVersion := time.Date(2026, 9, 14, 1, 2, 3, 0, time.UTC)
	acceptedVersion := initialVersion.Add(time.Second)
	fixture.target.UpdatedAt = initialVersion
	current := cloneV2TVTestTarget(fixture.target)
	current.Config = jsonRoundTripV2TVTestConfig(t, current.Config)
	current.UpdatedAt = acceptedVersion
	current.Config["note"] = []any{float64(1), float64(2)}
	current.Config["lack_episode"] = float64(2)
	fixture.store.activeTarget = &current
	fixture.execution.results = []downloadflow.IntentExecutionResult{
		{Outcome: downloadflow.IntentExecutionAccepted},
		{Outcome: downloadflow.IntentExecutionAccepted},
	}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{
			v2TVExecutionCandidate("first", 1, 1, 2),
			v2TVExecutionCandidate("second", 1, 3, 4),
		},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"first", "second"}) || len(fixture.submission.calls) != 2 {
		t.Fatalf("selection result/stages = %#v/%d", got, len(fixture.submission.calls))
	}
	if !fixture.submission.calls[0].expectedUpdatedAt.Equal(initialVersion) ||
		!fixture.submission.calls[1].expectedUpdatedAt.Equal(acceptedVersion) {
		t.Fatalf("stage versions = %v, %v; want %v, %v",
			fixture.submission.calls[0].expectedUpdatedAt,
			fixture.submission.calls[1].expectedUpdatedAt,
			initialVersion,
			acceptedVersion,
		)
	}
}

func TestExecuteV2TVCandidatesStopsWhenSameEpisodeTargetConfigurationChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "筛选条件变化", change: func(config map[string]any) { config["filter"] = "4K" }},
		{name: "洗版模式变化", change: func(config map[string]any) { config["best_version"] = true }},
		{name: "新增空配置字段", change: func(config map[string]any) { config["filter"] = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2, 3, 4))
			setV2TVComplementaryMetainfo(t, &fixture)
			fixture.target.UpdatedAt = time.Date(2026, 9, 14, 2, 3, 4, 0, time.UTC)
			current := cloneV2TVTestTarget(fixture.target)
			current.UpdatedAt = fixture.target.UpdatedAt.Add(time.Second)
			tt.change(current.Config)
			fixture.store.activeTarget = &current
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: downloadflow.IntentExecutionAccepted},
				{Outcome: downloadflow.IntentExecutionAccepted},
			}

			got, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{
					v2TVExecutionCandidate("first", 1, 1, 2),
					v2TVExecutionCandidate("second", 1, 3, 4),
				},
				[]indexer.Adapter{fixture.adapter}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Stopped || len(got.AcceptedCandidateKeys) != 0 || len(fixture.submission.calls) != 1 {
				t.Fatalf("selection result/stages = %#v/%d", got, len(fixture.submission.calls))
			}
		})
	}
}

func TestExecuteV2TVCandidatesRunsThreeStagesInOrder(t *testing.T) {
	t.Parallel()

	missing := append(v2TVTestEpisodes(1, 1), v2TVTestEpisodes(2, 1)...)
	missing = append(missing, v2TVTestEpisodes(3, 1)...)
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, missing)
	fixture.adapter.bodies = map[string][]byte{
		"resource-full":   v2TVTestMetainfo(t, "Show.S01E01.mkv"),
		"resource-title":  v2TVTestMetainfo(t, "Show.S02E01.mkv"),
		"resource-unpack": v2TVTestMetainfo(t, "Show.S03E01.mkv"),
	}
	fixture.execution.results = []downloadflow.IntentExecutionResult{
		{Outcome: downloadflow.IntentExecutionAccepted},
		{Outcome: downloadflow.IntentExecutionAccepted},
		{Outcome: downloadflow.IntentExecutionAccepted},
	}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{
			v2TVExecutionCandidate("full", 1),
			v2TVExecutionCandidate("title", 2, 1),
			v2TVExecutionCandidate("unpack", 3, 1, 2),
		},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"full", "title", "unpack"}) ||
		len(got.RemainingEpisodes) != 0 {
		t.Fatalf("selection result = %#v", got)
	}
	wantEpisodes := [][]subscription.Episode{
		v2TVTestEpisodes(1, 1),
		v2TVTestEpisodes(2, 1),
		v2TVTestEpisodes(3, 1),
	}
	if gotEpisodes := fixture.submission.stageEpisodes(); !reflect.DeepEqual(gotEpisodes, wantEpisodes) {
		t.Fatalf("staged episodes = %#v, want %#v", gotEpisodes, wantEpisodes)
	}
	if fixture.submission.calls[0].plan.Paused || fixture.submission.calls[1].plan.Paused ||
		!fixture.submission.calls[2].plan.Paused {
		t.Fatalf("stage paused flags = %v, %v, %v",
			fixture.submission.calls[0].plan.Paused,
			fixture.submission.calls[1].plan.Paused,
			fixture.submission.calls[2].plan.Paused,
		)
	}
	if fixture.execution.maxActive != 1 || fixture.execution.calls != 3 {
		t.Fatalf("execution calls/max active = %d/%d", fixture.execution.calls, fixture.execution.maxActive)
	}
}

func TestExecuteV2TVCandidatesContinuesOnlyAfterDefinitiveFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		first        downloadflow.IntentExecutionOutcome
		wantStages   int
		wantAccepted []string
		wantStopped  bool
	}{
		{
			name: "definitive failure continues", first: downloadflow.IntentExecutionDefinitiveFailureRecovered,
			wantStages: 2, wantAccepted: []string{"fallback"},
		},
		{
			name: "permanent candidate failure continues", first: downloadflow.IntentExecutionPermanentCandidateFailure,
			wantStages: 2, wantAccepted: []string{"fallback"},
		},
		{
			name: "unknown result stops", first: downloadflow.IntentExecutionPendingReconciliation,
			wantStages: 1, wantStopped: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
			if tt.first == downloadflow.IntentExecutionDefinitiveFailureRecovered ||
				tt.first == downloadflow.IntentExecutionPermanentCandidateFailure {
				prepareV2TVFailureVersionRefresh(t, &fixture)
			}
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: tt.first}, {Outcome: downloadflow.IntentExecutionAccepted},
			}
			got, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{
					v2TVExecutionCandidate("first", 1, 1),
					v2TVExecutionCandidate("fallback", 1, 1),
				},
				[]indexer.Adapter{fixture.adapter}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			wantAccepted := tt.wantAccepted
			if wantAccepted == nil {
				wantAccepted = []string{}
			}
			if len(fixture.submission.calls) != tt.wantStages || got.Stopped != tt.wantStopped ||
				!reflect.DeepEqual(got.AcceptedCandidateKeys, wantAccepted) {
				t.Fatalf("result/stages = %#v/%d", got, len(fixture.submission.calls))
			}
			if tt.wantStages == 2 && !fixture.submission.calls[1].expectedUpdatedAt.Equal(
				fixture.store.activeTarget.UpdatedAt,
			) {
				t.Fatalf("fallback stage version = %v, want %v",
					fixture.submission.calls[1].expectedUpdatedAt, fixture.store.activeTarget.UpdatedAt)
			}
		})
	}
}

func TestExecuteV2TVCandidatesStopsAfterRecoveredFailureTargetChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*database.Subscription)
	}{
		{name: "筛选变化", change: func(target *database.Subscription) { target.Config["filter"] = "4K" }},
		{name: "媒体来源变化", change: func(target *database.Subscription) { target.MediaSource = "douban" }},
		{name: "媒体身份变化", change: func(target *database.Subscription) { target.MediaID = "changed" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
			prepareV2TVFailureVersionRefresh(t, &fixture)
			tt.change(fixture.store.activeTarget)
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered},
				{Outcome: downloadflow.IntentExecutionAccepted},
			}

			got, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{
					v2TVExecutionCandidate("first", 1, 1),
					v2TVExecutionCandidate("fallback", 1, 1),
				},
				[]indexer.Adapter{fixture.adapter}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Stopped || len(got.AcceptedCandidateKeys) != 0 || len(fixture.submission.calls) != 1 {
				t.Fatalf("selection result/stages = %#v/%d", got, len(fixture.submission.calls))
			}
		})
	}
}

func TestExecuteV2TVCandidatesPropagatesRecoveredFailureRefreshError(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	fixture.store.activeTargetErr = errors.New("private storage detail")
	fixture.execution.results = []downloadflow.IntentExecutionResult{{
		Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered,
	}}

	_, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("first", 1, 1)},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if !errors.Is(err, ErrSubscriptionSearchStorage) {
		t.Fatalf("refresh error = %v, want %v", err, ErrSubscriptionSearchStorage)
	}
	if len(fixture.submission.calls) != 1 {
		t.Fatalf("stage calls = %d, want 1", len(fixture.submission.calls))
	}
}

func TestExecuteV2TVCandidatesAcceptsOnlyAfterStoredCoverageChanges(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	fixture.execution.retainEpisodes = true
	fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("accepted-without-coverage", 1, 1)},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if !errors.Is(err, ErrSubscriptionSubmission) {
		t.Fatalf("execute error = %v, want %v", err, ErrSubscriptionSubmission)
	}
	if len(got.AcceptedCandidateKeys) != 0 || fixture.store.episodeReadCalls != 1 {
		t.Fatalf("result/read calls = %#v/%d", got, fixture.store.episodeReadCalls)
	}
}

func TestExecuteV2TVCandidatesUsesRSSStageForFirstReadyCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		state      subscription.State
		wantStates []subscription.State
	}{
		{
			name: "R uses RSS stage once", state: subscription.Active,
			wantStates: []subscription.State{subscription.Active, ""},
		},
		{
			name: "P preserves pending on every stage", state: subscription.PendingIdentification,
			wantStates: []subscription.State{
				subscription.PendingIdentification,
				subscription.PendingIdentification,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, tt.state, v2TVTestEpisodes(1, 1))
			prepareV2TVFailureVersionRefresh(t, &fixture)
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered},
				{Outcome: downloadflow.IntentExecutionAccepted},
			}

			got, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{
					v2TVExecutionCandidate("first", 1, 1),
					v2TVExecutionCandidate("fallback", 1, 1),
				},
				[]indexer.Adapter{fixture.adapter}, true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"fallback"}) ||
				len(fixture.submission.calls) != len(tt.wantStates) {
				t.Fatalf("result/stages = %#v/%#v", got, fixture.submission.calls)
			}
			for index, wantState := range tt.wantStates {
				if gotState := fixture.submission.calls[index].rssState; gotState != wantState {
					t.Fatalf("stage %d RSS state = %q, want %q", index, gotState, wantState)
				}
			}
		})
	}
}

func TestExecuteV2TVCandidatesRestoresRAndPreservesP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		state        subscription.State
		wantRSSState subscription.State
		wantV2State  string
		wantCASCalls int
	}{
		{
			name: "R partial match returns to active", state: subscription.Active,
			wantRSSState: subscription.Active, wantV2State: subscription.V2TVSubscriptionStateReady,
			wantCASCalls: 1,
		},
		{
			name: "P partial match remains pending", state: subscription.PendingIdentification,
			wantRSSState: subscription.PendingIdentification, wantV2State: subscription.V2TVSubscriptionStatePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, tt.state, v2TVTestEpisodes(1, 1, 2))
			fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}
			got, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{v2TVExecutionCandidate("partial", 1, 1)},
				[]indexer.Adapter{fixture.adapter}, true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got.SubscriptionState != tt.wantV2State || len(got.RemainingEpisodes) != 1 {
				t.Fatalf("selection result = %#v", got)
			}
			if len(fixture.submission.calls) != 1 || fixture.submission.calls[0].rssState != tt.wantRSSState {
				t.Fatalf("RSS stage calls = %#v", fixture.submission.calls)
			}
			if fixture.store.casCalls != tt.wantCASCalls ||
				(tt.wantCASCalls == 1 && fixture.store.casNext != subscription.Active) {
				t.Fatalf("restore CAS = calls:%d next:%q", fixture.store.casCalls, fixture.store.casNext)
			}
		})
	}
}

func TestExecuteV2TVCandidatesReusesFullSeasonMetainfo(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2))
	fixture.target.Config["total_episode"] = 2
	fixture.query.MissingEpisodes = v2TVTestEpisodes(1, 1, 2)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")
	fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}
	candidate := v2TVExecutionCandidate("full", 1)
	candidate.Parsed = media.ParseResult{Type: media.TypeTV, BeginSeason: 1, EndSeason: 1}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query, []subscription.Candidate{candidate},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil || len(got.RemainingEpisodes) != 0 {
		t.Fatalf("selection = %#v, %v", got, err)
	}
	if fixture.adapter.resolveCalls != 1 || fixture.execution.withMetainfoCalls != 1 ||
		fixture.execution.plainCalls != 0 {
		t.Fatalf(
			"resolve/with/plain calls = %d/%d/%d",
			fixture.adapter.resolveCalls,
			fixture.execution.withMetainfoCalls,
			fixture.execution.plainCalls,
		)
	}
	if len(fixture.submission.calls) != 1 || fixture.submission.calls[0].plan.Paused {
		t.Fatalf("full season stage = %#v", fixture.submission.calls)
	}
}

func TestExecuteV2TVCandidatesOrdinaryFullSeasonKeepsCountOnlyProgress(t *testing.T) {
	t.Parallel()
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2))
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E02.mkv", "Show.S01E03.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("shifted-full", 1)}, []indexer.Adapter{fixture.adapter}, false)
	if err != nil || len(got.RemainingEpisodes) != 0 || len(fixture.submission.calls) != 1 {
		t.Fatalf("V2 ordinary count progress = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
	call := fixture.submission.calls[0]
	if !reflect.DeepEqual(call.episodes, v2TVTestEpisodes(1, 1, 2)) ||
		!reflect.DeepEqual(call.plan.TVCoverage, v2TVTestEpisodes(1, 2, 3)) || call.plan.TVFullCoverage {
		t.Fatalf("V2 progress and metadata facts = %#v", call)
	}
}

func TestExecuteV2TVCandidatesProjectsFileEpisodesIntoTitleSeason(t *testing.T) {
	t.Parallel()
	for _, fileName := range []string{"Show.E01.mkv", "Show.S01E01.mkv"} {
		t.Run(fileName, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(2, 1))
			season := 2
			fixture.target.Season = &season
			fixture.adapter.body = v2TVTestMetainfo(t, fileName)
			got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{v2TVExecutionCandidate("season-two", 2)}, []indexer.Adapter{fixture.adapter}, false)
			if err != nil || len(fixture.submission.calls) != 1 || len(got.RemainingEpisodes) != 0 ||
				!reflect.DeepEqual(fixture.submission.calls[0].plan.TVCoverage, v2TVTestEpisodes(2, 1)) {
				t.Fatalf("title season projection = %#v / %v / %#v", got, err, fixture.submission.calls)
			}
		})
	}
}

func TestExecuteV2TVCandidatesUnpackUsesActualSelectedFiles(t *testing.T) {
	t.Parallel()
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 2))
	fixture.target.Config["total_episode"] = 3
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("unpack", 1)}, []indexer.Adapter{fixture.adapter}, false)
	if err != nil || len(got.RemainingEpisodes) != 0 || len(fixture.submission.calls) != 1 ||
		!fixture.submission.calls[0].plan.Paused || fixture.submission.calls[0].plan.TVFullCoverage ||
		!reflect.DeepEqual(fixture.submission.calls[0].plan.TVCoverage, v2TVTestEpisodes(1, 2)) {
		t.Fatalf("actual unpack selection = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVCandidatesReusesProjectionMetainfoAndUpdatedTitle(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2))
	fixture.target.Config["total_episode"] = 2
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv")
	fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}
	candidate := v2TVExecutionCandidate("partial-pack", 1)
	candidate.Parsed = media.ParseResult{Type: media.TypeTV, BeginSeason: 1, EndSeason: 1}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query, []subscription.Candidate{candidate},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RemainingEpisodes, v2TVTestEpisodes(1, 2)) {
		t.Fatalf("remaining episodes = %#v", got.RemainingEpisodes)
	}
	if fixture.adapter.resolveCalls != 1 || fixture.execution.withMetainfoCalls != 1 ||
		fixture.execution.plainCalls != 0 {
		t.Fatalf(
			"resolve/with/plain calls = %d/%d/%d",
			fixture.adapter.resolveCalls,
			fixture.execution.withMetainfoCalls,
			fixture.execution.plainCalls,
		)
	}
	if len(fixture.submission.calls) != 1 || fixture.submission.calls[0].plan.Paused {
		t.Fatalf("updated title stage = %#v", fixture.submission.calls)
	}
}

func TestExecuteV2TVCandidatesAppliesV2FailureCooldownFailOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		listErr    error
		coolFirst  bool
		wantStages int
		wantKeys   []string
	}{
		{
			name: "active failure skips candidate", coolFirst: true,
			wantStages: 1, wantKeys: []string{"fallback"},
		},
		{
			name:    "storage error fails open",
			listErr: errors.New("private database detail"), wantStages: 1, wantKeys: []string{"first"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
			first := v2TVExecutionCandidate("first", 1, 1)
			fallback := v2TVExecutionCandidate("fallback", 1, 1)
			fixture.store.failureListErr = tt.listErr
			if tt.coolFirst {
				fixture.store.activeFailures = map[string]struct{}{
					v2TVTestFailureFingerprint(t, fixture.target, first): {},
				}
			}
			fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}

			got, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{first, fallback}, []indexer.Adapter{fixture.adapter}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.store.failureListCalls != 1 || len(fixture.submission.calls) != tt.wantStages ||
				!reflect.DeepEqual(got.AcceptedCandidateKeys, tt.wantKeys) {
				t.Fatalf("failure query/result/stages = %d/%#v/%d", fixture.store.failureListCalls, got, len(fixture.submission.calls))
			}
		})
	}
}

func TestExecuteV2TVCandidatesRecordsResourceFailuresAndSkipsSameFingerprint(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	first := v2TVExecutionCandidate("first", 1, 1)
	duplicate := v2TVExecutionCandidate("duplicate", 1, 1)
	duplicate.Resource.DownloadID = first.Resource.DownloadID
	duplicate.Resource.TorrentID = first.Resource.TorrentID
	fixture.adapter.resolveErrors = map[string]error{first.Resource.DownloadID: errors.New("private resolver response")}
	fixture.store.failureRecordErr = errors.New("private database detail")
	fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{first, duplicate, v2TVExecutionCandidate("fallback", 1, 1)},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"fallback"}) ||
		len(fixture.submission.calls) != 1 || fixture.adapter.resolveCalls != 2 {
		t.Fatalf("resource failure result/stages/resolves = %#v/%d/%d", got, len(fixture.submission.calls), fixture.adapter.resolveCalls)
	}
	if len(fixture.store.recordedFailures) != 1 ||
		fixture.store.failureTTLs[0] != subscription.V2DownloadFailureResourceTTL ||
		fixture.store.recordedFailures[0].ErrorMessage != "下载种子内容为空" {
		t.Fatalf("recorded resource failures = %#v / %#v", fixture.store.recordedFailures, fixture.store.failureTTLs)
	}
	if fixture.store.recordedFailures[0].Episodes != "E01" {
		t.Fatalf("recorded selected episodes = %q", fixture.store.recordedFailures[0].Episodes)
	}
}

func TestExecuteV2TVCandidatesRecordsInvalidMetainfoAsResourceFailure(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	invalid := v2TVExecutionCandidate("invalid", 1, 1)
	fixture.adapter.bodies = map[string][]byte{invalid.Resource.DownloadID: []byte("not metainfo")}
	fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionAccepted}}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{invalid, v2TVExecutionCandidate("fallback", 1, 1)},
		[]indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"fallback"}) ||
		len(fixture.store.recordedFailures) != 1 ||
		fixture.store.recordedFailures[0].ErrorMessage != "无法读取种子文件" ||
		fixture.store.failureTTLs[0] != subscription.V2DownloadFailureResourceTTL {
		t.Fatalf("invalid metainfo result/failures = %#v/%#v/%#v", got, fixture.store.recordedFailures, fixture.store.failureTTLs)
	}
}

func TestExecuteV2TVCandidatesDoesNotCoolInvalidMetainfoDuringProjection(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	fullSeason := v2TVExecutionCandidate("invalid-full-season", 1)
	fixture.adapter.bodies = map[string][]byte{fullSeason.Resource.DownloadID: []byte("not metainfo")}

	got, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{fullSeason}, []indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RemainingEpisodes) != 1 || len(fixture.submission.calls) != 0 ||
		len(fixture.store.recordedFailures) != 0 {
		t.Fatalf("projection invalid metainfo result/stages/failures = %#v/%d/%#v",
			got, len(fixture.submission.calls), fixture.store.recordedFailures)
	}
}

func TestExecuteV2TVCandidatesDoesNotCoolResolverContextError(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	candidate := v2TVExecutionCandidate("deadline", 1, 1)
	fixture.adapter.resolveErrors = map[string]error{
		candidate.Resource.DownloadID: errors.Join(errors.New("resolver deadline"), context.DeadlineExceeded),
	}

	_, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, false,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolver context error = %v, want deadline exceeded", err)
	}
	if len(fixture.store.recordedFailures) != 0 || len(fixture.submission.calls) != 0 {
		t.Fatalf("resolver context error recorded/staged = %#v/%d", fixture.store.recordedFailures, len(fixture.submission.calls))
	}
}

func TestExecuteV2TVCandidatesFullSeasonFailureUsesProjectedEpisodesInFingerprint(t *testing.T) {
	t.Parallel()

	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2))
	fixture.target.Config["total_episode"] = 2
	fixture.query.MissingEpisodes = v2TVTestEpisodes(1, 1, 2)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")
	fixture.execution.results = []downloadflow.IntentExecutionResult{{
		Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered,
	}}
	candidate := v2TVExecutionCandidate("full", 1)
	candidate.Parsed = media.ParseResult{Type: media.TypeTV, BeginSeason: 1, EndSeason: 1}

	_, err := fixture.handler.executeV2TVCandidates(
		t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.store.recordedFailures) != 1 ||
		fixture.store.recordedFailures[0].Episodes != "E01-E02" {
		t.Fatalf("full-season failure = %#v", fixture.store.recordedFailures)
	}
	withoutProjection, ok := subscription.NewV2DownloadFailure(
		subscription.V2DownloadFailureTarget{
			MediaType: fixture.target.MediaType, Title: fixture.target.Name, Year: fixture.target.Year,
			MediaSource: fixture.target.MediaSource, MediaID: fixture.target.MediaID, Season: fixture.target.Season,
		},
		candidate, nil, "qB", "", "添加下载任务失败",
	)
	if !ok || fixture.store.recordedFailures[0].Fingerprint == withoutProjection.Fingerprint {
		t.Fatalf("full-season projected fingerprint was not updated: got=%q title=%q",
			fixture.store.recordedFailures[0].Fingerprint, withoutProjection.Fingerprint)
	}
}

func TestExecuteV2TVCandidatesCoolsOnlyDefinitiveDownloaderFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		outcome     downloadflow.IntentExecutionOutcome
		executeErr  error
		wantErr     bool
		wantRecords int
		wantStages  int
	}{
		{
			name: "definitive failure", outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered,
			wantRecords: 1, wantStages: 2,
		},
		{name: "permanent candidate fact", outcome: downloadflow.IntentExecutionPermanentCandidateFailure, wantStages: 2},
		{name: "unknown result", outcome: downloadflow.IntentExecutionPendingReconciliation, wantStages: 1},
		{
			name: "retryable dependency error", outcome: downloadflow.IntentExecutionRetryableFailure,
			executeErr: errors.New("private dependency detail"), wantErr: true, wantStages: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
			if tt.outcome == downloadflow.IntentExecutionDefinitiveFailureRecovered ||
				tt.outcome == downloadflow.IntentExecutionPermanentCandidateFailure {
				prepareV2TVFailureVersionRefresh(t, &fixture)
			}
			fixture.store.failureRecordErr = errors.New("private database detail")
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: tt.outcome}, {Outcome: downloadflow.IntentExecutionAccepted},
			}
			fixture.execution.errors = []error{tt.executeErr}

			_, err := fixture.handler.executeV2TVCandidates(
				t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{
					v2TVExecutionCandidate("first", 1, 1),
					v2TVExecutionCandidate("fallback", 1, 1),
				},
				[]indexer.Adapter{fixture.adapter}, false,
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("execute error = %v, want error %t", err, tt.wantErr)
			}
			if len(fixture.submission.calls) != tt.wantStages ||
				len(fixture.store.recordedFailures) != tt.wantRecords {
				t.Fatalf("stages/records = %d/%d", len(fixture.submission.calls), len(fixture.store.recordedFailures))
			}
			if tt.wantRecords == 1 &&
				(fixture.store.failureTTLs[0] != subscription.V2DownloadFailureTransientTTL ||
					fixture.store.recordedFailures[0].Downloader != "qB") {
				t.Fatalf("definitive failure record = %#v / %s", fixture.store.recordedFailures[0], fixture.store.failureTTLs[0])
			}
		})
	}
}

type v2TVExecutionFixture struct {
	handler    *Handler
	store      *v2TVExecutionStore
	submission *v2TVTestSubmission
	execution  *v2TVTestExecution
	adapter    *v2TVTestAdapter
	target     database.Subscription
	query      subscription.Query
}

func newV2TVExecutionFixture(
	t *testing.T,
	state subscription.State,
	missing []subscription.Episode,
) v2TVExecutionFixture {
	t.Helper()
	baseStore := &subscriptionFlowStore{
		downloaders: []downloadconfig.Downloader{{
			ID: "11111111-2222-3333-4444-555555555555", Name: "qB",
			Type: "qbittorrent", Default: true, Enabled: true,
		}},
		episodes: v2TVTestEpisodeRows(missing),
	}
	store := &v2TVExecutionStore{subscriptionFlowStore: baseStore}
	submission := &v2TVTestSubmission{staged: make(map[uuid.UUID][]subscription.Episode)}
	execution := &v2TVTestExecution{store: baseStore, submission: submission}
	adapter := &v2TVTestAdapter{id: 7, body: v2TVTestMetainfo(t, "Show.S01E01.mkv")}
	handler, err := NewHandler(
		store,
		&subscriptionFlowSearcher{},
		subscriptionFlowAdapters{adapter},
		submission,
		execution,
	)
	if err != nil {
		t.Fatal(err)
	}
	season := 1
	target := database.Subscription{
		ID: 21, Username: "alice", Name: "Show", MediaType: string(media.TypeTV),
		Season: &season, InternalState: state, Config: map[string]any{"total_episode": len(missing)},
	}
	return v2TVExecutionFixture{
		handler: handler, store: store, submission: submission, execution: execution,
		adapter: adapter, target: target,
		query: subscription.Query{
			MediaType: media.TypeTV, MissingEpisodes: append([]subscription.Episode(nil), missing...),
			FullSeasonMissing: v2TVTestFullSeasonShape(missing),
		},
	}
}

// Fixtures created as fully absent native seasons carry V2's empty-list shape.
// Tests for explicit ranges clear this map instead of inferring it in production.
func v2TVTestFullSeasonShape(missing []subscription.Episode) map[int]bool {
	bySeason := make(map[int][]subscription.Episode)
	for _, episode := range missing {
		bySeason[episode.Season] = append(bySeason[episode.Season], episode)
	}
	result := make(map[int]bool)
	for season, episodes := range bySeason {
		if v2TVEpisodesCoverRange(episodes, season, 1, len(episodes)) {
			result[season] = true
		}
	}
	return result
}

type v2TVExecutionStore struct {
	*subscriptionFlowStore
	episodeReadCalls int
	failureListCalls int
	activeFailures   map[string]struct{}
	failureListErr   error
	recordedFailures []subscription.V2DownloadFailure
	failureTTLs      []time.Duration
	failureRecordErr error
	ownedTarget      *database.Subscription
	ownedTargetErr   error
}

func (s *v2TVExecutionStore) GetOwnedSubscription(
	_ context.Context, username string, id int64,
) (database.Subscription, error) {
	if s.ownedTargetErr != nil {
		return database.Subscription{}, s.ownedTargetErr
	}
	if s.ownedTarget == nil || s.ownedTarget.Username != username || s.ownedTarget.ID != id {
		return database.Subscription{}, database.ErrSubscriptionNotFound
	}
	return *s.ownedTarget, nil
}

func (s *v2TVExecutionStore) ListSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	states []subscription.SubscriptionEpisodeState,
	limit int,
) ([]subscription.SubscriptionEpisode, error) {
	s.episodeReadCalls++
	return s.subscriptionFlowStore.ListSubscriptionEpisodes(ctx, username, subscriptionID, states, limit)
}

func (s *v2TVExecutionStore) ListActiveV2DownloadFailureFingerprints(
	context.Context,
	[]string,
	time.Time,
) (map[string]struct{}, error) {
	s.failureListCalls++
	if s.failureListErr != nil {
		return nil, s.failureListErr
	}
	result := make(map[string]struct{}, len(s.activeFailures))
	for fingerprint := range s.activeFailures {
		result[fingerprint] = struct{}{}
	}
	return result, nil
}

func (s *v2TVExecutionStore) RecordV2DownloadFailure(
	_ context.Context,
	failure subscription.V2DownloadFailure,
	_ time.Time,
	ttl time.Duration,
) error {
	s.recordedFailures = append(s.recordedFailures, failure)
	s.failureTTLs = append(s.failureTTLs, ttl)
	return s.failureRecordErr
}

type v2TVTestStageCall struct {
	intent            downloader.Intent
	plan              database.DownloadIntentPlan
	episodes          []subscription.Episode
	rssState          subscription.State
	expectedUpdatedAt time.Time
	input             organization.SuccessInput
}

type v2TVTestSubmission struct {
	calls       []v2TVTestStageCall
	staged      map[uuid.UUID][]subscription.Episode
	stageErrors map[string]error
}

func (*v2TVTestSubmission) SubmitSubscriptionEpisodes(
	context.Context,
	string,
	int64,
	downloader.Intent,
	database.DownloadIntentPlan,
	organization.SuccessInput,
	[]subscription.Episode,
) (uuid.UUID, error) {
	return uuid.Nil, errors.New("unexpected async submission")
}

func (s *v2TVTestSubmission) SubmitSubscriptionTargetEpisodes(
	ctx context.Context, username string, subscriptionID int64, _ time.Time,
	intent downloader.Intent, plan database.DownloadIntentPlan,
	input organization.SuccessInput, episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.SubmitSubscriptionEpisodes(ctx, username, subscriptionID, intent, plan, input, episodes)
}

func (s *v2TVTestSubmission) StageSubscriptionEpisodes(
	_ context.Context,
	_ string,
	_ int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	input organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.record(intent, plan, input, episodes, "")
}

func (s *v2TVTestSubmission) StageSubscriptionTargetEpisodes(
	ctx context.Context, username string, subscriptionID int64, expectedUpdatedAt time.Time,
	intent downloader.Intent, plan database.DownloadIntentPlan,
	input organization.SuccessInput, episodes []subscription.Episode,
) (uuid.UUID, error) {
	storedID, err := s.StageSubscriptionEpisodes(ctx, username, subscriptionID, intent, plan, input, episodes)
	if err == nil && len(s.calls) > 0 {
		s.calls[len(s.calls)-1].expectedUpdatedAt = expectedUpdatedAt
	}
	return storedID, err
}

func (s *v2TVTestSubmission) StageRSSSubscriptionEpisodes(
	_ context.Context,
	_ string,
	_ int64,
	expected subscription.State,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	input organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.record(intent, plan, input, episodes, expected)
}

func (s *v2TVTestSubmission) StageRSSTargetEpisodes(
	ctx context.Context, username string, subscriptionID int64, expected subscription.State, expectedUpdatedAt time.Time,
	intent downloader.Intent, plan database.DownloadIntentPlan,
	input organization.SuccessInput, episodes []subscription.Episode,
) (uuid.UUID, error) {
	storedID, err := s.StageRSSSubscriptionEpisodes(ctx, username, subscriptionID, expected, intent, plan, input, episodes)
	if err == nil && len(s.calls) > 0 {
		s.calls[len(s.calls)-1].expectedUpdatedAt = expectedUpdatedAt
	}
	return storedID, err
}

func (s *v2TVTestSubmission) record(
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	input organization.SuccessInput,
	episodes []subscription.Episode,
	rssState subscription.State,
) (uuid.UUID, error) {
	if err := s.stageErrors[plan.ResourceID]; err != nil {
		return uuid.Nil, err
	}
	cloned := append([]subscription.Episode(nil), episodes...)
	s.calls = append(s.calls, v2TVTestStageCall{
		intent: intent, plan: plan, episodes: cloned, rssState: rssState, input: input,
	})
	s.staged[intent.ID] = cloned
	return intent.ID, nil
}

func (s *v2TVTestSubmission) stageEpisodes() [][]subscription.Episode {
	result := make([][]subscription.Episode, len(s.calls))
	for index, call := range s.calls {
		result[index] = append([]subscription.Episode(nil), call.episodes...)
	}
	return result
}

type v2TVTestExecution struct {
	store             *subscriptionFlowStore
	submission        *v2TVTestSubmission
	results           []downloadflow.IntentExecutionResult
	errors            []error
	calls             int
	active            int
	maxActive         int
	plainCalls        int
	withMetainfoCalls int
	retainEpisodes    bool
	afterExecution    func(uuid.UUID, downloadflow.IntentExecutionResult)
}

func (e *v2TVTestExecution) ExecuteIntent(
	_ context.Context,
	_ string,
	intentID uuid.UUID,
) (downloadflow.IntentExecutionResult, error) {
	e.plainCalls++
	return e.execute(intentID)
}

func (e *v2TVTestExecution) ExecuteIntentWithMetainfo(
	_ context.Context,
	_ string,
	intentID uuid.UUID,
	metainfo []byte,
) (downloadflow.IntentExecutionResult, error) {
	if len(metainfo) == 0 {
		return downloadflow.IntentExecutionResult{}, errors.New("empty metainfo")
	}
	e.withMetainfoCalls++
	return e.execute(intentID)
}

func (e *v2TVTestExecution) execute(intentID uuid.UUID) (downloadflow.IntentExecutionResult, error) {
	e.active++
	e.maxActive = max(e.maxActive, e.active)
	defer func() { e.active-- }()
	index := e.calls
	e.calls++
	result := downloadflow.IntentExecutionResult{Outcome: downloadflow.IntentExecutionAccepted}
	if index < len(e.results) {
		result = e.results[index]
	}
	var err error
	if index < len(e.errors) {
		err = e.errors[index]
	}
	if err == nil && result.Outcome == downloadflow.IntentExecutionAccepted && !e.retainEpisodes {
		e.store.episodes = removeV2TVTestEpisodes(e.store.episodes, e.submission.staged[intentID])
	}
	if err == nil && e.afterExecution != nil {
		e.afterExecution(intentID, result)
	}
	return result, err
}

type v2TVTestAdapter struct {
	id            int
	body          []byte
	bodies        map[string][]byte
	resolveErrors map[string]error
	resolveCalls  int
}

func (*v2TVTestAdapter) Name() string  { return "fixture" }
func (a *v2TVTestAdapter) SiteID() int { return a.id }
func (*v2TVTestAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}
func (a *v2TVTestAdapter) ResolveDownload(_ context.Context, downloadID string) (indexer.ResolvedDownload, error) {
	a.resolveCalls++
	if err := a.resolveErrors[downloadID]; err != nil {
		return indexer.ResolvedDownload{}, err
	}
	body := a.body
	if configured, exists := a.bodies[downloadID]; exists {
		body = configured
	}
	return indexer.ResolvedDownload{Body: append([]byte(nil), body...)}, nil
}

func v2TVTestFailureFingerprint(
	t *testing.T,
	target database.Subscription,
	candidate subscription.Candidate,
) string {
	t.Helper()
	failure, ok := subscription.NewV2DownloadFailure(
		subscription.V2DownloadFailureTarget{
			MediaType: target.MediaType, Title: target.Name, Year: target.Year,
			MediaSource: target.MediaSource, MediaID: target.MediaID, Season: target.Season,
		},
		candidate, nil, "", "", "",
	)
	if !ok {
		t.Fatal("failed to build V2 download failure fingerprint")
	}
	return failure.Fingerprint
}
func (*v2TVTestAdapter) ResolverKind() string   { return "fixture" }
func (*v2TVTestAdapter) ResolverVersion() int16 { return 1 }

func v2TVExecutionCandidate(key string, season int, episodes ...int) subscription.Candidate {
	parsed := media.ParseResult{Type: media.TypeTV, BeginSeason: season, EndSeason: season}
	if len(episodes) > 0 {
		parsed.BeginEpisode = episodes[0]
		parsed.EndEpisode = episodes[len(episodes)-1]
	}
	return subscription.Candidate{
		Key: key,
		Resource: indexer.Resource{
			SiteID: 7, Site: "Fixture", Title: "Show.S01",
			TorrentID: "torrent-" + key, DownloadID: "resource-" + key,
		},
		Parsed: parsed, CoveredEpisodes: v2TVTestEpisodes(season, episodes...),
	}
}

func v2TVTestEpisodes(season int, episodes ...int) []subscription.Episode {
	result := make([]subscription.Episode, len(episodes))
	for index, episode := range episodes {
		result[index] = subscription.Episode{Season: season, Episode: episode}
	}
	return result
}

func cloneV2TVTestTarget(target database.Subscription) database.Subscription {
	cloned := target
	cloned.Config = make(map[string]any, len(target.Config))
	for key, value := range target.Config {
		cloned.Config[key] = value
	}
	return cloned
}

func setV2TVComplementaryMetainfo(t *testing.T, fixture *v2TVExecutionFixture) {
	t.Helper()
	fixture.adapter.bodies = map[string][]byte{
		"resource-first":  v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv"),
		"resource-second": v2TVTestMetainfo(t, "Show.S01E03.mkv", "Show.S01E04.mkv"),
	}
}

func prepareV2TVFailureVersionRefresh(t *testing.T, fixture *v2TVExecutionFixture) {
	t.Helper()
	initial := time.Date(2026, 9, 14, 3, 4, 5, 0, time.UTC)
	fixture.target.UpdatedAt = initial
	current := cloneV2TVTestTarget(fixture.target)
	current.Config = jsonRoundTripV2TVTestConfig(t, current.Config)
	current.UpdatedAt = initial.Add(time.Second)
	fixture.store.activeTarget = &current
}

func jsonRoundTripV2TVTestConfig(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func v2TVTestEpisodeRows(episodes []subscription.Episode) []subscription.SubscriptionEpisode {
	result := make([]subscription.SubscriptionEpisode, len(episodes))
	for index, episode := range episodes {
		result[index] = subscription.SubscriptionEpisode{
			Season: episode.Season, Episode: episode.Episode,
			State: subscription.SubscriptionEpisodeMissing,
		}
	}
	return result
}

func removeV2TVTestEpisodes(
	rows []subscription.SubscriptionEpisode,
	removed []subscription.Episode,
) []subscription.SubscriptionEpisode {
	set := make(map[subscription.Episode]struct{}, len(removed))
	for _, episode := range removed {
		set[episode] = struct{}{}
	}
	result := make([]subscription.SubscriptionEpisode, 0, len(rows))
	for _, row := range rows {
		if _, exists := set[subscription.Episode{Season: row.Season, Episode: row.Episode}]; !exists {
			result = append(result, row)
		}
	}
	return result
}

func v2TVTestMetainfo(t *testing.T, names ...string) []byte {
	t.Helper()
	files := make([]any, 0, len(names))
	for _, name := range names {
		files = append(files, map[string]any{
			"length": int64(1), "path": []any{name},
		})
	}
	info := map[string]any{
		"files": files, "name": "Show", "piece length": int64(16 << 10),
		"pieces": strings.Repeat("a", sha1.Size),
	}
	var buffer bytes.Buffer
	if err := bencode.NewEncoder(&buffer).Encode(map[string]any{"info": info}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

var _ indexer.VersionedDownloadResolver = (*v2TVTestAdapter)(nil)
