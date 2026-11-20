package organizationflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/events"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func newTestHeartbeat(ctx context.Context) *leaseHeartbeat {
	done := make(chan struct{})
	close(done)
	return &leaseHeartbeat{ctx: ctx, cancel: func() {}, done: done}
}

func TestSetEventBusAndPublishOrganizationCompleted(t *testing.T) {
	batch := database.OrganizationBatch{ID: uuid.New(), SourceRoot: "/library"}
	files := 3

	// nil 接收者与 nil 总线都必须是安全的空操作。
	var nilHandler *Handler
	nilHandler.SetEventBus(events.New(nil, 4))
	(&Handler{}).publishOrganizationCompleted(batch, files)

	bus := events.New(nil, 8)
	t.Cleanup(bus.Close)
	received := make(chan events.Event, 1)
	bus.Subscribe("test", func(_ context.Context, event events.Event) {
		select {
		case received <- event:
		default:
		}
	})

	handler := &Handler{}
	handler.SetEventBus(bus)
	handler.publishOrganizationCompleted(batch, files)

	select {
	case event := <-received:
		if event.Type != events.TypeOrganizationCompleted {
			t.Fatalf("事件类型 = %q", event.Type)
		}
		if event.Payload["batch_id"] != batch.ID.String() {
			t.Fatalf("batch_id = %v", event.Payload["batch_id"])
		}
		if event.Payload["files"] != files {
			t.Fatalf("files = %v", event.Payload["files"])
		}
		if event.Payload["source_root"] != "/library" {
			t.Fatalf("source_root = %v", event.Payload["source_root"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 organization.completed 事件")
	}

	// 没有来源根目录时不应塞入空字段。
	handler.publishOrganizationCompleted(database.OrganizationBatch{ID: uuid.New()}, 0)
	select {
	case event := <-received:
		if _, ok := event.Payload["source_root"]; ok {
			t.Fatalf("空来源根目录不应写入 payload: %#v", event.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到第二条事件")
	}
}

func TestNewHandlerValidation(t *testing.T) {
	store := &executionStoreFake{}
	enqueuer := &organizationEnqueuerFake{}
	planner := &mediaIndexCompletionPlannerFake{}
	mediaIndexEnqueuer := &mediaIndexEnqueuerFake{}

	cases := []struct {
		name string
		call func() (*Handler, error)
	}{
		{"缺 store", func() (*Handler, error) {
			return NewHandler(nil, enqueuer, planner, mediaIndexEnqueuer)
		}},
		{"缺 enqueuer", func() (*Handler, error) {
			return NewHandler(store, nil, planner, mediaIndexEnqueuer)
		}},
		{"缺 planner", func() (*Handler, error) {
			return NewHandler(store, enqueuer, nil, mediaIndexEnqueuer)
		}},
		{"缺 media index 入队", func() (*Handler, error) {
			return NewHandler(store, enqueuer, planner, nil)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.call(); !errors.Is(err, ErrInvalidOrganizationHandler) {
				t.Fatalf("%s 应返回 ErrInvalidOrganizationHandler，实际 %v", testCase.name, err)
			}
		})
	}

	handler, err := NewHandler(store, enqueuer, planner, mediaIndexEnqueuer)
	if err != nil {
		t.Fatalf("NewHandler() = %v", err)
	}
	if handler.now == nil || handler.now().IsZero() {
		t.Fatal("默认时钟未注入")
	}
	if handler.leaseDuration != organizationLeaseDuration ||
		handler.heartbeatInterval != organizationHeartbeat ||
		handler.fileClaimSize != organizationFileClaimSize {
		t.Fatalf("默认参数 = %+v", handler)
	}
	if handler.execute == nil {
		t.Fatal("默认执行器未注入")
	}
	if handler.notificationSender != nil || handler.nfoScraper != nil {
		t.Fatal("可选依赖默认应为 nil")
	}
}

func TestOrganizationModeMapping(t *testing.T) {
	value := func(input string) *string { return &input }
	cases := []struct {
		input string
		mode  organizer.Mode
		ok    bool
	}{
		{"copy", organizer.Copy, true},
		{" COPY ", organizer.Copy, true},
		{"move", organizer.Move, true},
		{"link", organizer.Hardlink, true},
		{"hardlink", organizer.Hardlink, true},
		{"softlink", organizer.Symlink, true},
		{"symlink", organizer.Symlink, true},
		{"unknown", "", false},
		{"", "", false},
	}
	for _, testCase := range cases {
		mode, ok := organizationMode(value(testCase.input))
		if mode != testCase.mode || ok != testCase.ok {
			t.Fatalf("organizationMode(%q) = (%q, %v)，期望 (%q, %v)",
				testCase.input, mode, ok, testCase.mode, testCase.ok)
		}
	}
	if _, ok := organizationMode(nil); ok {
		t.Fatal("nil 模式不应有效")
	}
}

func TestSuccessFileStemsAndRoles(t *testing.T) {
	related := []struct {
		video, extra string
		want         bool
	}{
		{"show.s01e01", "show.s01e01", true},
		{"show.s01e01", "show.s01e01.zh", true},
		{"show.s01e01", "show.s01e01-zh", true},
		{"show.s01e01", "show.s01e01_zh", true},
		{"show.s01e01", "show.s01e02", false},
		{"show", "showcase", false},
	}
	for _, testCase := range related {
		if got := successFileStemsRelated(testCase.video, testCase.extra); got != testCase.want {
			t.Fatalf("successFileStemsRelated(%q, %q) = %v，期望 %v",
				testCase.video, testCase.extra, got, testCase.want)
		}
	}

	roles := map[string]string{
		"/library/show.mkv":   organizationFileRoleVideo,
		"/library/show.MP4":   organizationFileRoleVideo,
		"/library/show.srt":   organizationFileRoleSubtitle,
		"/library/show.SSA":   organizationFileRoleSubtitle,
		"/library/show.flac":  organizationFileRoleAudio,
		"/library/poster.jpg": organizationFileRoleOther,
		"/library/noext":      organizationFileRoleOther,
	}
	for path, want := range roles {
		if got := organizationFileRole(path); got != want {
			t.Fatalf("organizationFileRole(%q) = %q，期望 %q", path, got, want)
		}
	}
	if got := successFileStem("/library/Show.S01E01.MKV"); got != "show.s01e01" {
		t.Fatalf("successFileStem = %q", got)
	}
}

func TestAssignRelatedFileEpisodes(t *testing.T) {
	files := []preparedSuccessFile{
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 1, Episodes: []int{4}}, stem: "show.s01e04"},
		{fact: organization.FileFact{Role: organizationFileRoleSubtitle, Season: 1}, stem: "show.s01e04"},
		{fact: organization.FileFact{Role: organizationFileRoleSubtitle, Season: 1}, stem: "show.s01e04.zh"},
		{fact: organization.FileFact{Role: organizationFileRoleAudio, Season: 1}, stem: "other"},
		{fact: organization.FileFact{Role: organizationFileRoleSubtitle, Season: 2}, stem: "show.s01e04"},
	}
	assignRelatedFileEpisodes(files)
	if len(files[1].fact.Episodes) != 1 || files[1].fact.Episodes[0] != 4 {
		t.Fatalf("同名字幕未继承集号: %#v", files[1].fact.Episodes)
	}
	if len(files[2].fact.Episodes) != 1 || files[2].fact.Episodes[0] != 4 {
		t.Fatalf("带语言后缀的字幕未继承集号: %#v", files[2].fact.Episodes)
	}
	if len(files[3].fact.Episodes) != 0 {
		t.Fatalf("无关文件不应继承集号: %#v", files[3].fact.Episodes)
	}
	if len(files[4].fact.Episodes) != 0 {
		t.Fatalf("跨季文件不应继承集号: %#v", files[4].fact.Episodes)
	}

	// 同前缀多个视频时选 stem 更长的那个。
	longest := []preparedSuccessFile{
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 1, Episodes: []int{1}}, stem: "show"},
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 1, Episodes: []int{2}}, stem: "show.e01"},
		{fact: organization.FileFact{Role: organizationFileRoleSubtitle, Season: 1}, stem: "show.e01.zh"},
	}
	assignRelatedFileEpisodes(longest)
	if len(longest[2].fact.Episodes) != 1 || longest[2].fact.Episodes[0] != 2 {
		t.Fatalf("未选择最长前缀: %#v", longest[2].fact.Episodes)
	}
}

type captureNotificationSender struct {
	messages []NotificationMessage
	err      error
}

func (s *captureNotificationSender) Send(_ context.Context, msg NotificationMessage) error {
	s.messages = append(s.messages, msg)
	return s.err
}

func TestSendOrganizationNotification(t *testing.T) {
	// 未注入发送器时是空操作。
	(&Handler{}).sendOrganizationNotification(context.Background(), OrganizationBatch{})

	sender := &captureNotificationSender{}
	handler := &Handler{notificationSender: sender}
	handler.sendOrganizationNotification(context.Background(), OrganizationBatch{
		Plan: organizer.Plan{Operations: make([]organizer.Operation, 2)},
	})
	if len(sender.messages) != 1 {
		t.Fatalf("发送次数 = %d", len(sender.messages))
	}
	message := sender.messages[0]
	if message.Type != "整理入库" || message.ContentType != "organizeSuccess" || message.Title != "整理完成" {
		t.Fatalf("通知固定字段 = %#v", message)
	}
	if !strings.Contains(message.Text, "2 个文件") {
		t.Fatalf("通知正文 = %q", message.Text)
	}

	handler.sendOrganizationNotification(context.Background(), OrganizationBatch{})
	if len(sender.messages) != 2 || sender.messages[1].Text != "文件已成功整理到媒体库" {
		t.Fatalf("空计划通知 = %#v", sender.messages)
	}

	// 发送失败只影响旁路，不 panic 也不返回错误。
	sender.err = errors.New("smtp down")
	handler.sendOrganizationNotification(context.Background(), OrganizationBatch{})
}

func TestHeartbeatOrStoreErrorBranches(t *testing.T) {
	handler := &Handler{}
	ctx := context.Background()

	if err := handler.heartbeatOrStoreError(ctx, nil, errors.New("boom")); !errors.Is(err, ErrOrganizationExecutionStorage) {
		t.Fatalf("普通存储错误 = %v", err)
	}
	if err := handler.heartbeatOrStoreError(ctx, nil, ErrOrganizationLeaseConflict); err != nil {
		t.Fatalf("租约冲突应静默 = %v", err)
	}
	if err := handler.heartbeatOrStoreError(ctx, nil, ErrOrganizationBatchNotFound); err != nil {
		t.Fatalf("批次不存在应静默 = %v", err)
	}

	conflict := newTestHeartbeat(ctx)
	conflict.err = ErrOrganizationLeaseConflict
	if err := handler.heartbeatOrStoreError(ctx, conflict, errors.New("boom")); err != nil {
		t.Fatalf("心跳租约冲突 = %v", err)
	}

	broken := newTestHeartbeat(ctx)
	broken.err = errors.New("renew failed")
	if err := handler.heartbeatOrStoreError(ctx, broken, errors.New("boom")); !errors.Is(err, ErrOrganizationExecutionStorage) {
		t.Fatalf("心跳失败 = %v", err)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	canceledHeartbeat := newTestHeartbeat(canceledCtx)
	if err := handler.heartbeatOrStoreError(ctx, canceledHeartbeat, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("心跳上下文取消 = %v", err)
	}

	brokenCtx := newTestHeartbeat(ctx)
	brokenCtx.err = errors.New("renew failed")
	if err := handler.heartbeatOrStoreError(canceledCtx, brokenCtx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方取消优先 = %v", err)
	}
}

// failingFailStore 让 Fail* 方法返回可配置错误。
type failingFailStore struct {
	*executionStoreFake
	failFileErr  error
	failBatchErr error
}

func (s failingFailStore) FailOrganizationFile(context.Context, uuid.UUID, uuid.UUID, int64, string) error {
	return s.failFileErr
}

func (s failingFailStore) FailOrganizationBatch(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error {
	return s.failBatchErr
}

func TestRecordOperationFailureBranches(t *testing.T) {
	ctx := context.Background()
	batch := OrganizationBatch{ID: uuid.New(), LeaseToken: uuid.New()}
	file := OrganizationFile{ID: 7}

	cases := []struct {
		name     string
		fileErr  error
		batchErr error
		wantDone bool
		wantErr  error
	}{
		{"单文件失败即致命化", nil, nil, true, ErrOrganizationFileOperation},
		{"单文件租约冲突", ErrOrganizationLeaseConflict, nil, true, nil},
		{"单文件批次不存在", ErrOrganizationBatchNotFound, nil, true, nil},
		{"单文件存储错误", errors.New("io"), nil, false, ErrOrganizationExecutionStorage},
		{"批次租约冲突", nil, ErrOrganizationLeaseConflict, true, nil},
		{"批次不存在", nil, ErrOrganizationBatchNotFound, true, nil},
		{"批次存储错误", nil, errors.New("io"), false, ErrOrganizationExecutionStorage},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := failingFailStore{
				executionStoreFake: &executionStoreFake{},
				failFileErr:        testCase.fileErr,
				failBatchErr:       testCase.batchErr,
			}
			handler := &Handler{store: store, now: time.Now}
			done, err := handler.recordOperationFailure(ctx, batch, file)
			if done != testCase.wantDone {
				t.Fatalf("done = %v，期望 %v", done, testCase.wantDone)
			}
			if testCase.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v，期望 nil", err)
				}
				return
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, testCase.wantErr)
			}
		})
	}
}

func TestReleaseOrganizationBatchIgnoresStoreErrors(t *testing.T) {
	store := failingFailStore{
		executionStoreFake: &executionStoreFake{},
		failBatchErr:       errors.New("io"),
	}
	handler := &Handler{store: store, now: time.Now}
	// 释放失败只影响旁路，必须静默返回。
	handler.releaseOrganizationBatch(context.Background(), OrganizationBatch{ID: uuid.New()}, "boom")

	conflictStore := failingFailStore{
		executionStoreFake: &executionStoreFake{},
		failBatchErr:       ErrOrganizationLeaseConflict,
	}
	(&Handler{store: conflictStore, now: time.Now}).
		releaseOrganizationBatch(context.Background(), OrganizationBatch{ID: uuid.New()}, "boom")

	successStore := &executionStoreFake{}
	handler = &Handler{store: successStore, now: time.Now}
	handler.releaseOrganizationBatch(context.Background(), OrganizationBatch{ID: uuid.New()}, "stable")
	if successStore.failBatchCalls != 1 || successStore.failedBatchError != "stable" {
		t.Fatalf("释放调用 = %d / %q", successStore.failBatchCalls, successStore.failedBatchError)
	}
}

func TestValidateHandlerDependencies(t *testing.T) {
	store := &executionStoreFake{}
	enqueuer := &organizationEnqueuerFake{}
	planner := &mediaIndexCompletionPlannerFake{}
	mediaIndexEnqueuer := &mediaIndexEnqueuerFake{}
	base := func() *Handler {
		return &Handler{
			store: store, enqueuer: enqueuer, completionPlanner: planner,
			mediaIndexEnqueuer: mediaIndexEnqueuer, now: time.Now,
			execute:       func(context.Context, string, organizer.Plan) error { return nil },
			leaseDuration: time.Minute, heartbeatInterval: 10 * time.Second, fileClaimSize: 10,
		}
	}
	validArgs := OrganizationArgs{BatchID: uuid.New()}
	ctx := context.Background()
	if err := base().validate(ctx, validArgs); err != nil {
		t.Fatalf("合法处理器 = %v", err)
	}

	broken := map[string]func(*Handler){
		"nil store":          func(h *Handler) { h.store = nil },
		"nil enqueuer":       func(h *Handler) { h.enqueuer = nil },
		"nil planner":        func(h *Handler) { h.completionPlanner = nil },
		"nil media index 入队": func(h *Handler) { h.mediaIndexEnqueuer = nil },
		"nil 时钟":             func(h *Handler) { h.now = nil },
		"nil 执行器":            func(h *Handler) { h.execute = nil },
		"租约为零":               func(h *Handler) { h.leaseDuration = 0 },
		"租约过长":               func(h *Handler) { h.leaseDuration = 2 * time.Hour },
		"心跳为零":               func(h *Handler) { h.heartbeatInterval = 0 },
		"心跳不小于租约":            func(h *Handler) { h.heartbeatInterval = h.leaseDuration },
		"文件批量过小":             func(h *Handler) { h.fileClaimSize = 0 },
		"文件批量过大":             func(h *Handler) { h.fileClaimSize = 501 },
	}
	for name, mutate := range broken {
		t.Run(name, func(t *testing.T) {
			handler := base()
			mutate(handler)
			if err := handler.validate(ctx, validArgs); !errors.Is(err, ErrInvalidOrganizationHandler) {
				t.Fatalf("%s validate() = %v", name, err)
			}
		})
	}

	if err := base().validate(nil, validArgs); !errors.Is(err, ErrInvalidOrganizationHandler) {
		t.Fatalf("nil ctx = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := base().validate(canceled, validArgs); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := base().validate(ctx, OrganizationArgs{}); !errors.Is(err, ErrInvalidOrganizationHandler) {
		t.Fatalf("零值参数 = %v", err)
	}
}

func TestAssignUnparsedVideoEpisodes(t *testing.T) {
	// 单个未解析视频独占剩余集号。
	single := []preparedSuccessFile{
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 1}, stem: "a"},
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 1, Episodes: []int{1}}, stem: "b"},
	}
	assignUnparsedVideoEpisodes(single, []organization.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
	})
	if len(single[0].fact.Episodes) != 1 || single[0].fact.Episodes[0] != 2 {
		t.Fatalf("单个未解析视频 = %#v", single[0].fact.Episodes)
	}

	// 数量相等时一一配对。
	paired := []preparedSuccessFile{
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 2}, stem: "c"},
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 2}, stem: "d"},
	}
	assignUnparsedVideoEpisodes(paired, []organization.Episode{
		{Season: 2, Episode: 5}, {Season: 2, Episode: 6},
	})
	if len(paired[0].fact.Episodes) != 1 || paired[0].fact.Episodes[0] != 5 ||
		len(paired[1].fact.Episodes) != 1 || paired[1].fact.Episodes[0] != 6 {
		t.Fatalf("一一配对 = %#v / %#v", paired[0].fact.Episodes, paired[1].fact.Episodes)
	}

	// 数量不匹配时保持原样。
	unmatched := []preparedSuccessFile{
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 3}, stem: "e"},
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 3}, stem: "f"},
	}
	assignUnparsedVideoEpisodes(unmatched, []organization.Episode{{Season: 3, Episode: 9}})
	if len(unmatched[0].fact.Episodes) != 0 || len(unmatched[1].fact.Episodes) != 0 {
		t.Fatalf("数量不匹配不应分配: %#v / %#v", unmatched[0].fact.Episodes, unmatched[1].fact.Episodes)
	}

	// 已经解析过的集号不会被重复分配。
	skipped := []preparedSuccessFile{
		{fact: organization.FileFact{Role: organizationFileRoleVideo, Season: 4, Episodes: []int{1, 2}}, stem: "g"},
	}
	assignUnparsedVideoEpisodes(skipped, []organization.Episode{
		{Season: 4, Episode: 1}, {Season: 4, Episode: 2},
	})
	if len(skipped[0].fact.Episodes) != 2 {
		t.Fatalf("已解析集号被改写: %#v", skipped[0].fact.Episodes)
	}
}

func TestHasOrganizationVideoOperation(t *testing.T) {
	withVideo := organizer.Plan{Operations: []organizer.Operation{
		{Target: "/library/show.mkv"},
		{Target: "/library/show.srt"},
	}}
	if !hasOrganizationVideoOperation(withVideo) {
		t.Fatal("包含视频文件时应为 true")
	}
	withoutVideo := organizer.Plan{Operations: []organizer.Operation{{Target: "/library/show.srt"}}}
	if hasOrganizationVideoOperation(withoutVideo) {
		t.Fatal("只有字幕时应为 false")
	}
	if hasOrganizationVideoOperation(organizer.Plan{}) {
		t.Fatal("空计划应为 false")
	}
}
