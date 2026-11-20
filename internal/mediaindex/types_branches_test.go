package mediaindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCompletionPlanHasRequiredServer(t *testing.T) {
	if (CompletionPlan{}).HasRequiredServer() {
		t.Fatal("空计划不应有必需服务器")
	}
	invalid := branchServerRef()
	invalid.Fingerprint = "short"
	if (CompletionPlan{RequiredServer: &invalid}).HasRequiredServer() {
		t.Fatal("指纹非法时不应视为有效服务器")
	}
	valid := branchServerRef()
	if !(CompletionPlan{RequiredServer: &valid}).HasRequiredServer() {
		t.Fatal("合法服务器应被识别")
	}
}

func TestCompletionPlanCloneDetaches(t *testing.T) {
	server := branchServerRef()
	plan := CompletionPlan{
		RequiredServer: &server,
		Files:          []TargetFile{{LocalPath: "/a.mkv", RemotePath: "/b.mkv"}},
	}
	cloned := plan.Clone()
	if cloned.RequiredServer == plan.RequiredServer {
		t.Fatal("服务器指针不应共享")
	}
	plan.RequiredServer.Name = "changed"
	if cloned.RequiredServer.Name != "Emby" {
		t.Fatalf("克隆的服务器被改写: %q", cloned.RequiredServer.Name)
	}
	plan.Files[0].LocalPath = "changed"
	if cloned.Files[0].LocalPath != "/a.mkv" {
		t.Fatalf("克隆的文件被改写: %q", cloned.Files[0].LocalPath)
	}
	empty := CompletionPlan{}.Clone()
	if empty.RequiredServer != nil || empty.Files != nil {
		t.Fatalf("空计划克隆 = %#v", empty)
	}
}

func TestServerErrorNilReceiver(t *testing.T) {
	var serverErr *ServerError
	if got := serverErr.Error(); got != "媒体服务器错误" {
		t.Fatalf("Error() = %q", got)
	}
	if serverErr.Unwrap() != nil {
		t.Fatal("nil 接收者 Unwrap 应为 nil")
	}
	if serverErr.Is(ErrServerUnavailable) {
		t.Fatal("nil 接收者不应匹配任何分类")
	}
}

func TestServerErrorClassificationAndUnwrap(t *testing.T) {
	cause := errors.New("upstream")
	unavailable := &ServerError{Cause: cause}
	if !errors.Is(unavailable, ErrServerUnavailable) || errors.Is(unavailable, ErrOutcomeUnknown) {
		t.Fatalf("不可用分类 = %v", unavailable)
	}
	if !errors.Is(unavailable, cause) {
		t.Fatal("Unwrap 应保留原始原因")
	}
	if unavailable.Error() != ErrServerUnavailable.Error() {
		t.Fatalf("Error() = %q", unavailable.Error())
	}

	unknown := &ServerError{Cause: cause, Unknown: true}
	if !errors.Is(unknown, ErrOutcomeUnknown) || errors.Is(unknown, ErrServerUnavailable) {
		t.Fatalf("未知结果分类 = %v", unknown)
	}
	if unknown.Error() != ErrOutcomeUnknown.Error() {
		t.Fatalf("Error() = %q", unknown.Error())
	}
}

func TestServerRefAndLeaseValidityEdges(t *testing.T) {
	ref := branchServerRef()
	if !ref.Valid() {
		t.Fatal("合法引用应通过")
	}
	// 名称只要求去除空白后非空，首尾空白本身不算非法。
	padded := ref
	padded.Name = " Emby "
	if !padded.Valid() {
		t.Fatal("首尾带空白的名称仍应通过")
	}
	emptyName := ref
	emptyName.Name = "   "
	if emptyName.Valid() {
		t.Fatal("空白名称不应通过")
	}
	upper := ref
	upper.Fingerprint = strings.ToUpper(ref.Fingerprint)
	if upper.Valid() {
		t.Fatal("大写十六进制指纹不应通过")
	}
	short := ref
	short.Fingerprint = ref.Fingerprint[:63]
	if short.Valid() {
		t.Fatal("短指纹不应通过")
	}

	requestID := uuid.New()
	lease := Lease{Request: Request{ID: requestID, Server: ref}, LeaseToken: uuid.New()}
	if !lease.Valid(requestID) {
		t.Fatal("合法租约应通过")
	}
	if lease.Valid(uuid.New()) {
		t.Fatal("请求不匹配时租约应无效")
	}
	noToken := lease
	noToken.LeaseToken = uuid.Nil
	if noToken.Valid(requestID) {
		t.Fatal("缺少租约令牌时应无效")
	}
	badServer := lease
	badServer.Request.Server = ServerRef{}
	if badServer.Valid(requestID) {
		t.Fatal("服务器引用无效时租约应无效")
	}
}

// enqueueErrorFake 让 SweepHandler 的入队失败分支可达。
type enqueueErrorFake struct{}

func (enqueueErrorFake) Enqueue(context.Context, uuid.UUID) error { return errors.New("boom") }

func TestSweepHandlerEnqueueFailure(t *testing.T) {
	store := &branchStore{dueIDs: []uuid.UUID{uuid.New()}}
	handler := &SweepHandler{store: store, enqueuer: enqueueErrorFake{}, now: time.Now}
	if err := handler.Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("入队失败 = %v", err)
	}
}
