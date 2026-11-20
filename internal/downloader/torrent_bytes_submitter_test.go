package downloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const (
	torrentBytesTestHash = "0123456789ABCDEF0123456789ABCDEF01234567"
	torrentBytesSecret   = "TORRENT_BYTES_SECRET_MARKER"
)

func TestTorrentBytesSubmitterSubmit(t *testing.T) {
	metainfo := []byte("metainfo-" + torrentBytesSecret)
	baseCandidate := Candidate{
		Key:         "opaque-bytes-candidate",
		DownloadURL: "https://fixture.invalid/resource?passkey=" + torrentBytesSecret,
		SavePath:    "/library/fixture",
		Paused:      true,
	}

	tests := []struct {
		name            string
		state           IntentState
		intentHash      string
		candidate       Candidate
		metainfo        []byte
		labelResults    []bytesLookupResult
		hashResults     []bytesLookupResult
		addErrors       []error
		preCanceled     bool
		cancelDuringAdd bool
		wantState       IntentState
		wantHash        string
		wantErr         error
		wantErrText     string
		wantLabelCalls  int
		wantHashCalls   int
		wantAddCalls    int
	}{
		{
			name:           "提交前标签已存在直接复用",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			labelResults:   []bytesLookupResult{{hash: strings.ToLower(torrentBytesTestHash), found: true}},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 1,
		},
		{
			name:           "提交前 Hash 已存在直接复用",
			state:          IntentPlanned,
			intentHash:     strings.ToLower(torrentBytesTestHash),
			candidate:      baseCandidate,
			labelResults:   []bytesLookupResult{{}},
			hashResults:    []bytesLookupResult{{hash: torrentBytesTestHash, found: true}},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 1,
			wantHashCalls:  1,
		},
		{
			name:           "Add 成功但暂不可见保持未知",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{}, {}},
			addErrors:      []error{nil},
			wantState:      IntentSubmitting,
			wantErr:        ErrReconciliationPending,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:      "Add 成功且后对账可见",
			state:     IntentPlanned,
			candidate: baseCandidate,
			metainfo:  metainfo,
			labelResults: []bytesLookupResult{
				{},
				{hash: torrentBytesTestHash, found: true},
			},
			addErrors:      []error{nil},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:      "未知 Add 错误后对账成功",
			state:     IntentPlanned,
			candidate: baseCandidate,
			metainfo:  metainfo,
			labelResults: []bytesLookupResult{
				{},
				{hash: torrentBytesTestHash, found: true},
			},
			addErrors:      []error{errors.New("upstream-" + torrentBytesSecret)},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:       "未知 Add 错误后按 Hash 对账成功",
			state:      IntentPlanned,
			intentHash: strings.ToLower(torrentBytesTestHash),
			candidate:  baseCandidate,
			metainfo:   metainfo,
			labelResults: []bytesLookupResult{
				{},
				{},
			},
			hashResults: []bytesLookupResult{
				{},
				{hash: torrentBytesTestHash, found: true},
			},
			addErrors:      []error{errors.New("upstream-" + torrentBytesSecret)},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 2,
			wantHashCalls:  2,
			wantAddCalls:   1,
		},
		{
			name:           "未知 Add 错误且不可见保持未知",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{}, {}},
			addErrors:      []error{errors.New("upstream-" + torrentBytesSecret)},
			wantState:      IntentSubmitting,
			wantErr:        ErrReconciliationPending,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:      "未知 Add 错误且后对账不可用保持未知",
			state:     IntentPlanned,
			candidate: baseCandidate,
			metainfo:  metainfo,
			labelResults: []bytesLookupResult{
				{},
				{err: errors.New("lookup-" + torrentBytesSecret)},
			},
			addErrors:      []error{errors.New("upstream-" + torrentBytesSecret)},
			wantState:      IntentSubmitting,
			wantErr:        ErrReconciliationPending,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:           "明确失败才进入 Failed",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{}, {}},
			addErrors:      []error{ErrDefinitiveAddFailure},
			wantState:      IntentFailed,
			wantErr:        ErrDefinitiveAddFailure,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:      "明确失败但后对账存在时复用",
			state:     IntentPlanned,
			candidate: baseCandidate,
			metainfo:  metainfo,
			labelResults: []bytesLookupResult{
				{},
				{hash: torrentBytesTestHash, found: true},
			},
			addErrors:      []error{ErrDefinitiveAddFailure},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:      "Failed 状态允许受控重试",
			state:     IntentFailed,
			candidate: baseCandidate,
			metainfo:  metainfo,
			labelResults: []bytesLookupResult{
				{},
				{hash: torrentBytesTestHash, found: true},
			},
			addErrors:      []error{nil},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 2,
			wantAddCalls:   1,
		},
		{
			name:           "Submitting 重放不可见时不重复 Add",
			state:          IntentSubmitting,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{}},
			wantState:      IntentSubmitting,
			wantErr:        ErrReconciliationPending,
			wantLabelCalls: 1,
		},
		{
			name:           "Submitting 重放按标签完成对账",
			state:          IntentSubmitting,
			candidate:      baseCandidate,
			labelResults:   []bytesLookupResult{{hash: torrentBytesTestHash, found: true}},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 1,
		},
		{
			name:           "Submitting 重放按 Hash 完成对账",
			state:          IntentSubmitting,
			intentHash:     torrentBytesTestHash,
			candidate:      baseCandidate,
			labelResults:   []bytesLookupResult{{}},
			hashResults:    []bytesLookupResult{{hash: torrentBytesTestHash, found: true}},
			wantState:      IntentDownloading,
			wantHash:       torrentBytesTestHash,
			wantLabelCalls: 1,
			wantHashCalls:  1,
		},
		{
			name:           "Submitting 重放对账错误仍保持未知",
			state:          IntentSubmitting,
			candidate:      baseCandidate,
			labelResults:   []bytesLookupResult{{err: errors.New("lookup-" + torrentBytesSecret)}},
			wantState:      IntentSubmitting,
			wantErr:        ErrReconciliationPending,
			wantLabelCalls: 1,
		},
		{
			name:        "调用前取消不触发客户端",
			state:       IntentPlanned,
			candidate:   baseCandidate,
			metainfo:    metainfo,
			preCanceled: true,
			wantState:   IntentPlanned,
			wantErr:     context.Canceled,
		},
		{
			name:            "Add 调用中取消视为结果未知",
			state:           IntentPlanned,
			candidate:       baseCandidate,
			metainfo:        metainfo,
			labelResults:    []bytesLookupResult{{}},
			addErrors:       []error{context.Canceled},
			cancelDuringAdd: true,
			wantState:       IntentSubmitting,
			wantErr:         ErrReconciliationPending,
			wantLabelCalls:  1,
			wantAddCalls:    1,
		},
		{
			name:           "Add 超时视为结果未知",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{}},
			addErrors:      []error{context.DeadlineExceeded},
			wantState:      IntentSubmitting,
			wantErr:        ErrReconciliationPending,
			wantLabelCalls: 1,
			wantAddCalls:   1,
		},
		{
			name:            "Add 返回 nil 但调用中取消仍视为未知",
			state:           IntentPlanned,
			candidate:       baseCandidate,
			metainfo:        metainfo,
			labelResults:    []bytesLookupResult{{}},
			addErrors:       []error{nil},
			cancelDuringAdd: true,
			wantState:       IntentSubmitting,
			wantErr:         ErrReconciliationPending,
			wantLabelCalls:  1,
			wantAddCalls:    1,
		},
		{
			name:            "调用中取消优先于明确失败",
			state:           IntentPlanned,
			candidate:       baseCandidate,
			metainfo:        metainfo,
			labelResults:    []bytesLookupResult{{}},
			addErrors:       []error{ErrDefinitiveAddFailure},
			cancelDuringAdd: true,
			wantState:       IntentSubmitting,
			wantErr:         ErrReconciliationPending,
			wantLabelCalls:  1,
			wantAddCalls:    1,
		},
		{
			name:        "不同 Candidate 绑定在调用前拒绝",
			state:       IntentPlanned,
			candidate:   Candidate{Key: "opaque-other-candidate"},
			metainfo:    metainfo,
			wantState:   IntentPlanned,
			wantErrText: "下载候选与意图不匹配",
		},
		{
			name:           "计划状态前置对账失败不 Add",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{err: errors.New("lookup-" + torrentBytesSecret)}},
			wantState:      IntentPlanned,
			wantErr:        ErrReconciliationUnavailable,
			wantLabelCalls: 1,
		},
		{
			name:           "已知 Hash 前置对账失败不 Add",
			state:          IntentPlanned,
			intentHash:     torrentBytesTestHash,
			candidate:      baseCandidate,
			metainfo:       metainfo,
			labelResults:   []bytesLookupResult{{}},
			hashResults:    []bytesLookupResult{{err: errors.New("lookup-" + torrentBytesSecret)}},
			wantState:      IntentPlanned,
			wantHash:       torrentBytesTestHash,
			wantErr:        ErrReconciliationUnavailable,
			wantLabelCalls: 1,
			wantHashCalls:  1,
		},
		{
			name:           "真正需要 Add 时拒绝空 metainfo",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       nil,
			labelResults:   []bytesLookupResult{{}},
			wantState:      IntentPlanned,
			wantErrText:    "种子文件内容为空",
			wantLabelCalls: 1,
		},
		{
			name:           "真正需要 Add 时拒绝超限 metainfo",
			state:          IntentPlanned,
			candidate:      baseCandidate,
			metainfo:       make([]byte, maxTorrentMetainfoSize+1),
			labelResults:   []bytesLookupResult{{}},
			wantState:      IntentPlanned,
			wantErrText:    "种子文件超过大小上限",
			wantLabelCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := mustTorrentBytesIntent(t, "admin", baseCandidate)
			intent.State = tt.state
			intent.TorrentHash = tt.intentHash
			client := &scriptedTorrentBytesClient{
				labelResults: append([]bytesLookupResult(nil), tt.labelResults...),
				hashResults:  append([]bytesLookupResult(nil), tt.hashResults...),
				addErrors:    append([]error(nil), tt.addErrors...),
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.preCanceled {
				cancel()
			}
			if tt.cancelDuringAdd {
				client.addHook = cancel
			}

			got, err := NewTorrentBytesSubmitter(client).Submit(ctx, intent, tt.candidate, tt.metainfo)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Submit() error = %v, want %v", err, tt.wantErr)
				}
			} else if tt.wantErrText != "" {
				if err == nil || err.Error() != tt.wantErrText {
					t.Fatalf("Submit() error = %v, want %q", err, tt.wantErrText)
				}
			} else if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}

			want := intent
			want.State = tt.wantState
			want.TorrentHash = tt.wantHash
			if got != want {
				t.Fatalf("Submit() intent = %#v, want %#v", got, want)
			}
			if client.labelCalls != tt.wantLabelCalls || client.hashCalls != tt.wantHashCalls || client.addCalls != tt.wantAddCalls {
				t.Fatalf("client calls = label %d hash %d add %d, want %d/%d/%d",
					client.labelCalls, client.hashCalls, client.addCalls,
					tt.wantLabelCalls, tt.wantHashCalls, tt.wantAddCalls)
			}
			if tt.wantAddCalls > 0 {
				if !bytes.Equal(client.addMetainfo, tt.metainfo) {
					t.Fatal("AddTorrentBytes 未收到原始 metainfo")
				}
				if client.addSavePath != tt.candidate.SavePath || client.addLabel != intent.Label || client.addPaused != tt.candidate.Paused {
					t.Fatalf("AddTorrentBytes 参数 = path %q label %q paused %v",
						client.addSavePath, client.addLabel, client.addPaused)
				}
			}
			if err != nil {
				for _, secret := range []string{torrentBytesSecret, string(metainfo), baseCandidate.DownloadURL} {
					if secret != "" && strings.Contains(err.Error(), secret) {
						t.Fatalf("Submit() error 泄露输入 %q: %q", secret, err)
					}
				}
			}
		})
	}
}

func TestTorrentBytesSubmitterAddOncePausesTargetEpisodeTorrent(t *testing.T) {
	candidate := Candidate{
		Key:            "target-episode-candidate",
		SavePath:       "/downloads/tv",
		TargetEpisodes: []Episode{{Season: 1, Episode: 2}},
	}
	intent := mustTorrentBytesIntent(t, "alice", candidate)
	submitting, err := intent.Transition(IntentSubmitting)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedTorrentBytesClient{
		labelResults: []bytesLookupResult{{hash: torrentBytesTestHash, found: true}},
	}

	updated, err := NewTorrentBytesSubmitter(client).AddOnce(
		t.Context(),
		submitting,
		candidate,
		[]byte("bounded-metainfo"),
	)
	if err != nil {
		t.Fatalf("AddOnce() error = %v", err)
	}
	if updated.State != IntentDownloading || updated.TorrentHash != torrentBytesTestHash {
		t.Fatalf("AddOnce() = %#v", updated)
	}
	if !client.addPaused {
		t.Fatal("目标集任务 AddTorrentBytes paused = false")
	}
}

func TestTorrentBytesSubmitterSelectEpisodeFiles(t *testing.T) {
	tests := []struct {
		name     string
		client   TorrentBytesClient
		intent   Intent
		episodes []Episode
		wantErr  error
	}{
		{
			name:     "无目标集不要求下载器能力",
			client:   nil,
			intent:   Intent{},
			episodes: nil,
		},
		{
			name:     "下载器不支持逐集选择",
			client:   &scriptedTorrentBytesClient{},
			intent:   Intent{TorrentHash: torrentBytesTestHash},
			episodes: []Episode{{Season: 1, Episode: 1}},
			wantErr:  ErrEpisodeSelectionUnavailable,
		},
		{
			name: "委托并保留可重试结果",
			client: &episodeSelectingTorrentBytesClient{
				scriptedTorrentBytesClient: &scriptedTorrentBytesClient{},
				selectionErr:               ErrEpisodeSelectionPending,
			},
			intent:   Intent{TorrentHash: strings.ToLower(torrentBytesTestHash)},
			episodes: []Episode{{Season: 2, Episode: 3}, {Season: 2, Episode: 3}},
			wantErr:  ErrEpisodeSelectionPending,
		},
		{
			name: "脱敏未知依赖错误",
			client: &episodeSelectingTorrentBytesClient{
				scriptedTorrentBytesClient: &scriptedTorrentBytesClient{},
				selectionErr:               errors.New("upstream-" + torrentBytesSecret),
			},
			intent:   Intent{TorrentHash: torrentBytesTestHash},
			episodes: []Episode{{Season: 1, Episode: 1}},
			wantErr:  ErrEpisodeSelectionFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submitter := NewTorrentBytesSubmitter(tt.client)
			err := submitter.SelectEpisodeFiles(t.Context(), tt.intent, tt.episodes)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SelectEpisodeFiles() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), torrentBytesSecret) {
				t.Fatalf("SelectEpisodeFiles() 泄露依赖错误: %v", err)
			}
			selector, ok := tt.client.(*episodeSelectingTorrentBytesClient)
			if !ok {
				return
			}
			if selector.selectedHash != torrentBytesTestHash {
				t.Fatalf("selected hash = %q", selector.selectedHash)
			}
			if tt.wantErr == ErrEpisodeSelectionPending {
				want := Episode{Season: 2, Episode: 3}
				if len(selector.selectedEpisodes) != 1 || selector.selectedEpisodes[0] != want {
					t.Fatalf("selected episodes = %#v", selector.selectedEpisodes)
				}
			}
		})
	}
}

func TestTorrentBytesSubmitterReplayNeverRepeatsAdd(t *testing.T) {
	candidate := Candidate{Key: "opaque-replay", SavePath: "/library/replay"}
	intent := mustTorrentBytesIntent(t, "admin", candidate)
	client := &scriptedTorrentBytesClient{
		labelResults: []bytesLookupResult{
			{},
			{},
			{hash: torrentBytesTestHash, found: true},
		},
		addErrors: []error{errors.New("opaque-unknown-result")},
	}
	submitter := NewTorrentBytesSubmitter(client)
	metainfo := []byte("opaque-replay-metainfo")

	updated, err := submitter.Submit(t.Context(), intent, candidate, metainfo)
	if !errors.Is(err, ErrReconciliationPending) || updated.State != IntentSubmitting {
		t.Fatalf("首次 Submit() = %#v, %v", updated, err)
	}
	reconciled, err := submitter.Submit(t.Context(), updated, candidate, metainfo)
	if err != nil || reconciled.State != IntentDownloading || reconciled.TorrentHash != torrentBytesTestHash {
		t.Fatalf("重放 Submit() = %#v, %v", reconciled, err)
	}
	if client.addCalls != 1 || client.labelCalls != 3 {
		t.Fatalf("重放调用数 = label %d add %d, want 3/1", client.labelCalls, client.addCalls)
	}
}

func TestTorrentBytesSubmitterReconcileNeverAdds(t *testing.T) {
	t.Parallel()

	candidate := Candidate{Key: "opaque-reconcile", SavePath: "/library/reconcile"}
	tests := []struct {
		name        string
		state       IntentState
		labelResult bytesLookupResult
		wantFound   bool
		wantState   IntentState
		wantHash    string
		wantErr     error
	}{
		{
			name:      "计划任务不可见",
			state:     IntentPlanned,
			wantState: IntentPlanned,
		},
		{
			name:        "提交中任务按标签恢复",
			state:       IntentSubmitting,
			labelResult: bytesLookupResult{hash: torrentBytesTestHash, found: true},
			wantFound:   true,
			wantState:   IntentDownloading,
			wantHash:    torrentBytesTestHash,
		},
		{
			name:        "查询失败返回稳定错误",
			state:       IntentSubmitting,
			labelResult: bytesLookupResult{err: errors.New("cookie=" + torrentBytesSecret)},
			wantState:   IntentSubmitting,
			wantErr:     ErrReconciliationUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			intent := mustTorrentBytesIntent(t, "admin", candidate)
			intent.State = tt.state
			client := &scriptedTorrentBytesClient{labelResults: []bytesLookupResult{tt.labelResult}}
			got, found, err := NewTorrentBytesSubmitter(client).Reconcile(t.Context(), intent)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reconcile() error = %v, want %v", err, tt.wantErr)
			}
			if found != tt.wantFound || got.State != tt.wantState || got.TorrentHash != tt.wantHash {
				t.Fatalf("Reconcile() = %#v, found %v", got, found)
			}
			if client.addCalls != 0 {
				t.Fatal("Reconcile() 不得调用 AddTorrentBytes")
			}
			if err != nil && strings.Contains(err.Error(), torrentBytesSecret) {
				t.Fatal("Reconcile() 错误泄露上游输入")
			}
		})
	}
}

func TestTorrentBytesSubmitterAddOnceRequiresPersistedPermission(t *testing.T) {
	t.Parallel()

	candidate := Candidate{Key: "opaque-add-once", SavePath: "/library/add-once"}
	metainfo := []byte("opaque-metainfo")
	tests := []struct {
		name         string
		state        IntentState
		labelResults []bytesLookupResult
		addErr       error
		wantState    IntentState
		wantErr      error
		wantErrText  string
		wantAddCalls int
	}{
		{
			name:        "计划状态没有 CAS 许可",
			state:       IntentPlanned,
			wantState:   IntentPlanned,
			wantErrText: "下载意图尚未取得单次提交许可",
		},
		{
			name:         "提交许可只执行一次 Add",
			state:        IntentSubmitting,
			labelResults: []bytesLookupResult{{hash: torrentBytesTestHash, found: true}},
			wantState:    IntentDownloading,
			wantAddCalls: 1,
		},
		{
			name:         "明确拒绝但无法完成后对账保持未知",
			state:        IntentSubmitting,
			labelResults: []bytesLookupResult{{err: errors.New("lookup unavailable")}},
			addErr:       ErrDefinitiveAddFailure,
			wantState:    IntentSubmitting,
			wantErr:      ErrReconciliationPending,
			wantAddCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			intent := mustTorrentBytesIntent(t, "admin", candidate)
			intent.State = tt.state
			client := &scriptedTorrentBytesClient{
				labelResults: append([]bytesLookupResult(nil), tt.labelResults...),
				addErrors:    []error{tt.addErr},
			}
			got, err := NewTorrentBytesSubmitter(client).AddOnce(t.Context(), intent, candidate, metainfo)
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("AddOnce() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErrText != "" && (err == nil || err.Error() != tt.wantErrText) {
				t.Fatalf("AddOnce() error = %v, want %q", err, tt.wantErrText)
			}
			if tt.wantErr == nil && tt.wantErrText == "" && err != nil {
				t.Fatalf("AddOnce() error = %v", err)
			}
			if got.State != tt.wantState || client.addCalls != tt.wantAddCalls {
				t.Fatalf("AddOnce() state/add calls = %s/%d, want %s/%d", got.State, client.addCalls, tt.wantState, tt.wantAddCalls)
			}
		})
	}
}

func FuzzTorrentBytesSubmitterRedactsInputs(f *testing.F) {
	f.Add("seed")
	f.Add("another-seed")
	f.Fuzz(func(t *testing.T, seed string) {
		digest := sha256.Sum256([]byte(seed))
		marker := hex.EncodeToString(digest[:])
		candidate := Candidate{
			Key:         "opaque-fuzz-candidate",
			DownloadURL: "https://fixture.invalid/resource?passkey=" + marker,
		}
		intent := mustTorrentBytesIntent(t, "admin", candidate)
		metainfo := []byte("metainfo-secret-" + marker)
		client := &scriptedTorrentBytesClient{
			labelResults: []bytesLookupResult{{}, {}},
			addErrors:    []error{errors.New("upstream-secret-" + marker)},
		}

		updated, err := NewTorrentBytesSubmitter(client).Submit(t.Context(), intent, candidate, metainfo)
		if !errors.Is(err, ErrReconciliationPending) || updated.State != IntentSubmitting {
			t.Fatalf("Submit() = %#v, %v", updated, err)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("Submit() error 泄露输入标记 %q", marker)
		}
	})
}

type bytesLookupResult struct {
	hash  string
	found bool
	err   error
}

type scriptedTorrentBytesClient struct {
	observations map[string]TaskObservation
	observeErr   error
	labelResults []bytesLookupResult
	hashResults  []bytesLookupResult
	addErrors    []error
	addHook      func()
	labelCalls   int
	hashCalls    int
	addCalls     int
	addMetainfo  []byte
	addSavePath  string
	addLabel     string
	addPaused    bool
}

type episodeSelectingTorrentBytesClient struct {
	*scriptedTorrentBytesClient
	selectionErr     error
	selectedHash     string
	selectedEpisodes []Episode
}

func (c *episodeSelectingTorrentBytesClient) SelectEpisodeFiles(
	_ context.Context,
	torrentHash string,
	episodes []Episode,
) error {
	c.selectedHash = torrentHash
	c.selectedEpisodes = append([]Episode(nil), episodes...)
	return c.selectionErr
}

func (c *scriptedTorrentBytesClient) ObserveTasks(_ context.Context, hashes []string) (map[string]TaskObservation, error) {
	if c.observeErr != nil {
		return nil, c.observeErr
	}
	result := make(map[string]TaskObservation, len(hashes))
	for _, hash := range hashes {
		if observation, ok := c.observations[hash]; ok {
			result[hash] = observation
			continue
		}
		result[hash] = TaskObservation{Hash: hash, State: TaskObservationMissing}
	}
	return result, nil
}

func (c *scriptedTorrentBytesClient) FindByLabel(context.Context, string) (string, bool, error) {
	c.labelCalls++
	return popBytesLookup(&c.labelResults)
}

func (c *scriptedTorrentBytesClient) FindByHash(context.Context, string) (string, bool, error) {
	c.hashCalls++
	return popBytesLookup(&c.hashResults)
}

func (c *scriptedTorrentBytesClient) AddTorrentBytes(_ context.Context, metainfo []byte, savePath, label string, paused bool) error {
	c.addCalls++
	c.addMetainfo = metainfo
	c.addSavePath = savePath
	c.addLabel = label
	c.addPaused = paused
	if c.addHook != nil {
		c.addHook()
	}
	if len(c.addErrors) == 0 {
		return nil
	}
	err := c.addErrors[0]
	c.addErrors = c.addErrors[1:]
	return err
}

func popBytesLookup(results *[]bytesLookupResult) (string, bool, error) {
	if len(*results) == 0 {
		return "", false, nil
	}
	result := (*results)[0]
	*results = (*results)[1:]
	return result.hash, result.found, result.err
}

func mustTorrentBytesIntent(t *testing.T, username string, candidate Candidate) Intent {
	t.Helper()
	intent, err := NewIntent(username, candidate)
	if err != nil {
		t.Fatalf("创建种子字节意图 fixture: %v", err)
	}
	return intent
}
