package mediaindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func branchServerRef() ServerRef {
	return ServerRef{ID: "s1", Name: "Emby", Type: "emby", Fingerprint: strings.Repeat("b", 64)}
}

// branchStore 是可配置返回值的 Store 假实现，用来驱动 Service 的失败分支。
type branchStore struct {
	claimErr       error
	claimSkipped   bool
	claimLease     *Lease
	renewErr       error
	rescheduleErr  error
	recordErr      error
	files          []File
	listErr        error
	confirmFileErr error
	confirmErr     error
	confirmed      bool
	recoverErr     error
	listDueErr     error
	dueIDs         []uuid.UUID
	request        Request
	leaseToken     uuid.UUID
	lastNext       time.Time
	lastStable     string
	fileConfirmed  bool
}

func newBranchStore(requestID uuid.UUID, now time.Time) *branchStore {
	token := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	return &branchStore{
		request: Request{
			ID: requestID, Server: branchServerRef(), State: RequestPending,
			RefreshRequestedAt: &now, NextAttemptAt: now,
		},
		leaseToken: token,
	}
}

func (s *branchStore) Claim(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error) {
	if s.claimErr != nil {
		return Lease{}, false, s.claimErr
	}
	if s.claimSkipped {
		return Lease{}, false, nil
	}
	if s.claimLease != nil {
		return *s.claimLease, true, nil
	}
	return Lease{Request: s.request, LeaseToken: s.leaseToken}, true, nil
}

func (s *branchStore) Renew(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) error {
	return s.renewErr
}

func (s *branchStore) Reschedule(_ context.Context, _ uuid.UUID, _ uuid.UUID, next time.Time, stableErr string) error {
	if s.rescheduleErr != nil {
		return s.rescheduleErr
	}
	s.lastNext = next
	s.lastStable = stableErr
	return nil
}

func (s *branchStore) RecordRefresh(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) error {
	return s.recordErr
}

func (s *branchStore) ListPendingFiles(context.Context, uuid.UUID, uuid.UUID, int) ([]File, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]File(nil), s.files...), nil
}

func (s *branchStore) ConfirmFile(context.Context, uuid.UUID, uuid.UUID, int64, string) error {
	if s.confirmFileErr != nil {
		return s.confirmFileErr
	}
	s.fileConfirmed = true
	return nil
}

func (s *branchStore) ConfirmRequest(context.Context, uuid.UUID, uuid.UUID, time.Time) (bool, error) {
	if s.confirmErr != nil {
		return false, s.confirmErr
	}
	return s.confirmed, nil
}

func (s *branchStore) RecoverExpired(context.Context, time.Time, int) (int64, error) {
	if s.recoverErr != nil {
		return 0, s.recoverErr
	}
	return 1, nil
}

func (s *branchStore) ListDue(context.Context, time.Time, int) ([]uuid.UUID, error) {
	if s.listDueErr != nil {
		return nil, s.listDueErr
	}
	return s.dueIDs, nil
}

func branchFile(requestID uuid.UUID) File {
	return File{
		ID: 1, RequestID: requestID, LocalPath: "/library/episode.mkv",
		RemotePath: "/emby/show/episode.mkv", PathStyle: "posix", CaseSensitive: true,
		ExpectedSize: 10, ExpectedSHA256: strings.Repeat("a", 64),
	}
}

func newBranchService(t *testing.T, store *branchStore, server Server, verifier LocalVerifier) *Service {
	t.Helper()
	ref := branchServerRef()
	service, err := NewService(store, store, store, &registryFake{ref: ref, server: server}, verifier)
	if err != nil {
		t.Fatalf("NewService() = %v", err)
	}
	service.renewEvery = time.Hour
	return service
}

func TestNewServiceRejectsNilDependencies(t *testing.T) {
	store := &branchStore{}
	server := &registryFake{}
	verifier := &localVerifierFake{valid: true}
	cases := []struct {
		name  string
		build func() (*Service, error)
	}{
		{"leases", func() (*Service, error) { return NewService(nil, store, store, server, verifier) }},
		{"files", func() (*Service, error) { return NewService(store, nil, store, server, verifier) }},
		{"completion", func() (*Service, error) { return NewService(store, store, nil, server, verifier) }},
		{"registry", func() (*Service, error) { return NewService(store, store, store, nil, verifier) }},
		{"verifier", func() (*Service, error) { return NewService(store, store, store, server, nil) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.build(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("缺 %s 时应报 ErrInvalidRequest，实际 %v", testCase.name, err)
			}
		})
	}
}

func TestServiceHandleGuards(t *testing.T) {
	ctx := context.Background()
	requestID := uuid.New()
	store := newBranchStore(requestID, time.Now())
	service := newBranchService(t, store, &mediaIndexServerFake{}, &localVerifierFake{valid: true})

	if err := (*Service)(nil).Handle(ctx, requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil service = %v", err)
	}
	if err := service.Handle(nil, requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil ctx = %v", err)
	}
	if err := service.Handle(ctx, uuid.Nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil request = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := service.Handle(canceled, requestID); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}

	brokenNow := newBranchService(t, store, &mediaIndexServerFake{}, &localVerifierFake{valid: true})
	brokenNow.now = nil
	if err := brokenNow.Handle(ctx, requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil clock = %v", err)
	}
	brokenRenew := newBranchService(t, store, &mediaIndexServerFake{}, &localVerifierFake{valid: true})
	brokenRenew.renewEvery = 0
	if err := brokenRenew.Handle(ctx, requestID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("零续租间隔 = %v", err)
	}

	claimFails := newBranchStore(requestID, time.Now())
	claimFails.claimErr = errors.New("boom")
	if err := newBranchService(t, claimFails, &mediaIndexServerFake{}, &localVerifierFake{valid: true}).
		Handle(ctx, requestID); !errors.Is(err, ErrPersistence) {
		t.Fatalf("Claim 失败 = %v", err)
	}

	notClaimed := newBranchStore(requestID, time.Now())
	notClaimed.claimSkipped = true
	if err := newBranchService(t, notClaimed, &mediaIndexServerFake{}, &localVerifierFake{valid: true}).
		Handle(ctx, requestID); err != nil {
		t.Fatalf("未抢到租约应静默返回，实际 %v", err)
	}

	badLease := newBranchStore(requestID, time.Now())
	invalid := Lease{Request: badLease.request}
	badLease.claimLease = &invalid
	if err := newBranchService(t, badLease, &mediaIndexServerFake{}, &localVerifierFake{valid: true}).
		Handle(ctx, requestID); !errors.Is(err, ErrPersistence) {
		t.Fatalf("非法租约 = %v", err)
	}
}

func TestServiceRefreshBranches(t *testing.T) {
	ctx := context.Background()
	requestID := uuid.New()
	now := time.Now()

	// 注册表解析不到服务器：推迟 15 分钟并记录 server_unavailable。
	store := newBranchStore(requestID, now)
	store.request.RefreshRequestedAt = nil
	service, err := NewService(store, store, store, &registryFake{ref: branchServerRef()}, &localVerifierFake{valid: true})
	if err != nil {
		t.Fatalf("NewService() = %v", err)
	}
	service.renewEvery = time.Hour
	if err := service.Handle(ctx, requestID); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if store.lastStable != "server_unavailable" {
		t.Fatalf("缺少服务器时 stable = %q", store.lastStable)
	}

	// 刷新失败：推迟 serverBackoff 并记录 server_unavailable。
	store = newBranchStore(requestID, now)
	store.request.RefreshRequestedAt = nil
	failing := &mediaIndexServerFake{refreshErr: errors.New("boom")}
	service = newBranchService(t, store, failing, &localVerifierFake{valid: true})
	if err := service.Handle(ctx, requestID); err != nil {
		t.Fatalf("刷新失败 Handle() = %v", err)
	}
	if store.lastStable != "server_unavailable" {
		t.Fatalf("刷新失败 stable = %q", store.lastStable)
	}

	// 刷新结果未知：视为已发起，继续记录刷新时间。
	store = newBranchStore(requestID, now)
	store.request.RefreshRequestedAt = nil
	unknown := &mediaIndexServerFake{refreshErr: &ServerError{Cause: errors.New("x"), Unknown: true}}
	service = newBranchService(t, store, unknown, &localVerifierFake{valid: true})
	if err := service.Handle(ctx, requestID); err != nil {
		t.Fatalf("结果未知 Handle() = %v", err)
	}
	if store.lastStable != "" {
		t.Fatalf("结果未知不应推迟，stable = %q", store.lastStable)
	}

	// 刷新时的上下文错误优先返回。
	store = newBranchStore(requestID, now)
	store.request.RefreshRequestedAt = nil
	deadline := &mediaIndexServerFake{refreshErr: context.DeadlineExceeded}
	service = newBranchService(t, store, deadline, &localVerifierFake{valid: true})
	if err := service.Handle(ctx, requestID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("刷新超时 = %v", err)
	}

	// RecordRefresh 失败 → ErrPersistence。
	store = newBranchStore(requestID, now)
	store.request.RefreshRequestedAt = nil
	store.recordErr = errors.New("boom")
	service = newBranchService(t, store, &mediaIndexServerFake{}, &localVerifierFake{valid: true})
	if err := service.Handle(ctx, requestID); !errors.Is(err, ErrPersistence) {
		t.Fatalf("记录刷新失败 = %v", err)
	}
}

func TestServiceConfirmPageBranches(t *testing.T) {
	ctx := context.Background()
	requestID := uuid.New()
	now := time.Now()

	cases := []struct {
		name       string
		configure  func(*branchStore, *mediaIndexServerFake, *localVerifierFake)
		wantErr    error
		wantStable string
	}{
		{
			name:      "列文件失败",
			configure: func(s *branchStore, _ *mediaIndexServerFake, _ *localVerifierFake) { s.listErr = errors.New("boom") },
			wantErr:   ErrPersistence,
		},
		{
			name: "超过单页上限",
			configure: func(s *branchStore, _ *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = make([]File, pageSize+1)
			},
			wantErr: ErrPersistence,
		},
		{
			name: "文件快照非法",
			configure: func(s *branchStore, _ *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{{ID: 1, RequestID: requestID}}
			},
			wantErr: ErrPersistence,
		},
		{
			name: "本地校验出错",
			configure: func(s *branchStore, _ *mediaIndexServerFake, v *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				v.err = errors.New("io")
			},
			wantStable: "local_file_unavailable",
		},
		{
			name: "本地校验上下文错误",
			configure: func(s *branchStore, _ *mediaIndexServerFake, v *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				v.err = context.Canceled
			},
			wantErr: context.Canceled,
		},
		{
			name: "本地指纹不一致",
			configure: func(s *branchStore, _ *mediaIndexServerFake, v *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				v.valid = false
			},
			wantStable: "local_fingerprint_mismatch",
		},
		{
			name: "远端查询失败",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.findErr = errors.New("boom")
			},
			wantStable: "server_unavailable",
		},
		{
			name: "远端匹配不唯一",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.findErr = ErrAmbiguousMatch
			},
			wantStable: "ambiguous_match",
		},
		{
			name: "远端路径不匹配",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.findErr = ErrPathMismatch
			},
			wantStable: "path_mismatch",
		},
		{
			name: "远端 Provider 不匹配",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.findErr = ErrProviderMismatch
			},
			wantStable: "provider_mismatch",
		},
		{
			name: "确认单文件失败",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.items = []Item{{ID: "item-1", Path: "/emby/show/episode.mkv"}}
				s.confirmFileErr = errors.New("boom")
			},
			wantErr: ErrPersistence,
		},
		{
			name: "完成请求失败",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.items = []Item{{ID: "item-1", Path: "/emby/show/episode.mkv"}}
				s.confirmErr = errors.New("boom")
			},
			wantErr: ErrPersistence,
		},
		{
			name: "全部确认完成",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.items = []Item{{ID: "item-1", Path: "/emby/show/episode.mkv"}}
				s.confirmed = true
			},
		},
		{
			name: "还有文件未就绪",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.items = []Item{{ID: "item-1", Path: "/emby/show/episode.mkv"}}
				s.confirmed = false
			},
			wantStable: "awaiting_remaining_files",
		},
		{
			name: "远端尚未可见",
			configure: func(s *branchStore, _ *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
			},
			wantStable: "not_visible_yet",
		},
		{
			name: "调整失败",
			configure: func(s *branchStore, srv *mediaIndexServerFake, _ *localVerifierFake) {
				s.files = []File{branchFile(requestID)}
				srv.items = []Item{{ID: "item-1", Path: "/emby/show/episode.mkv"}}
				s.confirmed = false
				s.rescheduleErr = errors.New("boom")
			},
			wantErr: ErrPersistence,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newBranchStore(requestID, now)
			server := &mediaIndexServerFake{}
			verifier := &localVerifierFake{valid: true}
			testCase.configure(store, server, verifier)
			service := newBranchService(t, store, server, verifier)
			err := service.Handle(ctx, requestID)
			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("error = %v，期望 %v", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Handle() = %v", err)
			}
			if store.lastStable != testCase.wantStable {
				t.Fatalf("stable = %q，期望 %q", store.lastStable, testCase.wantStable)
			}
		})
	}
}

func TestStableServerErrorClasses(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "server_unavailable"},
		{ErrOutcomeUnknown, "outcome_unknown"},
		{ErrAmbiguousMatch, "ambiguous_match"},
		{ErrPathMismatch, "path_mismatch"},
		{ErrProviderMismatch, "provider_mismatch"},
		{errors.New("boom"), "server_unavailable"},
	}
	for _, testCase := range cases {
		if got := stableServerError(testCase.err); got != testCase.want {
			t.Fatalf("stableServerError(%v) = %q, want %q", testCase.err, got, testCase.want)
		}
	}
}

func TestPreferMatchMismatchKeepsHigherPriority(t *testing.T) {
	if got := preferMatchMismatch(stableAmbiguousMatch, stablePathMismatch); got != stableAmbiguousMatch {
		t.Fatalf("高优先级不应被覆盖: %q", got)
	}
	if got := preferMatchMismatch(stableNotVisible, stablePathMismatch); got != stablePathMismatch {
		t.Fatalf("低优先级应被覆盖: %q", got)
	}
	if got := preferMatchMismatch("", "unknown"); got != "" {
		t.Fatalf("未知分类不应覆盖空值: %q", got)
	}
	if got := preferMatchMismatch("unknown", ""); got != "unknown" {
		t.Fatalf("空候选不应覆盖: %q", got)
	}
}

func TestTrimStableErrorAndContextError(t *testing.T) {
	if got := trimStableError("  spaced  "); got != "spaced" {
		t.Fatalf("trimStableError = %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := trimStableError(long); len(got) != 128 {
		t.Fatalf("超长错误应截断到 128，实际 %d", len(got))
	}

	ctx := context.Background()
	if got := mediaIndexContextError(ctx, errors.New("boom")); got != nil {
		t.Fatalf("普通错误不应算上下文错误: %v", got)
	}
	if got := mediaIndexContextError(ctx, context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("取消错误 = %v", got)
	}
	if got := mediaIndexContextError(ctx, context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("超时错误 = %v", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := mediaIndexContextError(canceled, errors.New("boom")); !errors.Is(got, context.Canceled) {
		t.Fatalf("上下文已取消时应优先返回: %v", got)
	}
	if got := mediaIndexContextError(nil, context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("nil 上下文兜底 = %v", got)
	}
}

func TestSweepHandlerBranches(t *testing.T) {
	ctx := context.Background()
	if _, err := NewSweepHandler(nil, &enqueuerFake{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil store = %v", err)
	}
	if _, err := NewSweepHandler(&sweepStoreFake{}, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil enqueuer = %v", err)
	}
	if err := (*SweepHandler)(nil).Handle(ctx); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil handler = %v", err)
	}

	handler, err := NewSweepHandler(&sweepStoreFake{}, &enqueuerFake{})
	if err != nil {
		t.Fatalf("NewSweepHandler() = %v", err)
	}
	if err := handler.Handle(nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil ctx = %v", err)
	}
	brokenClock := &SweepHandler{store: &sweepStoreFake{}, enqueuer: &enqueuerFake{}}
	if err := brokenClock.Handle(ctx); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil clock = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := handler.Handle(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}

	recoverErr := &branchStore{recoverErr: errors.New("boom")}
	if err := (&SweepHandler{store: recoverErr, enqueuer: &enqueuerFake{}, now: time.Now}).Handle(ctx); !errors.Is(err, ErrPersistence) {
		t.Fatalf("恢复过期租约失败 = %v", err)
	}
	listErr := &branchStore{listDueErr: errors.New("boom")}
	if err := (&SweepHandler{store: listErr, enqueuer: &enqueuerFake{}, now: time.Now}).Handle(ctx); !errors.Is(err, ErrPersistence) {
		t.Fatalf("列出到期请求失败 = %v", err)
	}
	tooMany := &branchStore{dueIDs: make([]uuid.UUID, maxSweepPage+1)}
	if err := (&SweepHandler{store: tooMany, enqueuer: &enqueuerFake{}, now: time.Now}).Handle(ctx); !errors.Is(err, ErrPersistence) {
		t.Fatalf("超过单页上限 = %v", err)
	}
	nilID := &branchStore{dueIDs: []uuid.UUID{uuid.Nil}}
	if err := (&SweepHandler{store: nilID, enqueuer: &enqueuerFake{}, now: time.Now}).Handle(ctx); !errors.Is(err, ErrPersistence) {
		t.Fatalf("零值请求 ID = %v", err)
	}
}
