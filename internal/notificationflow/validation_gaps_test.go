package notificationflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

// 本文件补齐纯校验函数里剩下的分支：空 JSON 边界、NUL 嵌套、事件键序数解析、
// 不支持的业务内容类型，以及业务计划里「幂等键/目标重复」这两种去重失败。

func TestJSONObjectIsEmptyBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		value json.RawMessage
		want  bool
	}{
		{name: "nil", value: nil, want: true},
		{name: "纯空白", value: json.RawMessage("  \n\t"), want: true},
		{name: "空对象", value: json.RawMessage(`{}`), want: true},
		{name: "带字段", value: json.RawMessage(`{"a":1}`), want: false},
	}
	for _, tc := range cases {
		if got := jsonObjectIsEmpty(tc.value); got != tc.want {
			t.Fatalf("%s: jsonObjectIsEmpty() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidBoundedNoteBoundaries(t *testing.T) {
	if !validBoundedNote(json.RawMessage("   ")) {
		t.Fatal("纯空白备注应判为合法")
	}
	if validBoundedNote(json.RawMessage(`{"broken"`)) {
		t.Fatal("非法 JSON 备注应判为不合法")
	}
	if validBoundedNote(json.RawMessage(`{"tags":["ok","bad\u0000tail"]}`)) {
		t.Fatal("数组元素含 NUL 应判为不合法")
	}
	if !validBoundedNote(json.RawMessage(`{"tags":["a","b"],"count":2}`)) {
		t.Fatal("正常备注应判为合法")
	}
	if validBoundedNote(json.RawMessage(`{"note":"` + strings.Repeat("a", MaxNoteBytes) + `"}`)) {
		t.Fatal("超长备注应判为不合法")
	}
}

func TestOrganizationEventKeyKindRejectsBadOrdinal(t *testing.T) {
	batchID := uuid.New()
	prefix := organizationFailureKeyPrefix + batchID.String() + ":"
	for _, ordinal := range []string{"abc", "-1", "01", "1.5", ""} {
		if kind := organizationEventKeyKind(prefix+ordinal, batchID); kind != organizationEventInvalid {
			t.Fatalf("序数 %q 应判为非法事件键，得到 %v", ordinal, kind)
		}
	}
	if kind := organizationEventKeyKind(prefix+"7", batchID); kind != organizationEventFileFailure {
		t.Fatalf("合法序数事件键 = %v", kind)
	}
	if kind := organizationEventKeyKind("随便什么键", uuid.Nil); kind != organizationEventInvalid {
		t.Fatalf("空批次的事件键 = %v", kind)
	}
}

func TestBusinessContentTypeRejectsUnsupported(t *testing.T) {
	for _, ctype := range []notification.ContentType{"", notification.ContentType("ContentUnknown")} {
		if mtype, resolved := businessContentType(ctype); mtype != "" || resolved != "" {
			t.Fatalf("不支持的内容类型 %q 应返回空值，得到 (%q, %q)", ctype, mtype, resolved)
		}
	}
}

func TestStableContextErrorPrefersContextState(t *testing.T) {
	if err := stableContextError(context.Background(), context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误应被识别，得到 %v", err)
	}
	if err := stableContextError(context.Background(), fmt.Errorf("包装: %w", context.DeadlineExceeded)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时错误应被识别，得到 %v", err)
	}
	if err := stableContextError(context.Background(), errors.New("boom")); err != nil {
		t.Fatalf("普通错误应返回 nil，得到 %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stableContextError(ctx, errors.New("boom")); !errors.Is(err, context.Canceled) {
		t.Fatalf("上下文已取消时优先返回取消错误，得到 %v", err)
	}
}

func TestBusinessPlanValidRejectsDuplicateIdentity(t *testing.T) {
	provider := &businessTargetProviderFake{targets: []notification.Target{{Channel: notification.ChannelTelegram, Source: "1"}}}
	planner := newBusinessPlanner(t, provider, nil, "")
	valid, err := planner.PlanBusiness(businessInput(notification.ContentDownloadAdded))
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if !valid.Valid() {
		t.Fatal("基准计划应为合法")
	}

	clone := func() BusinessPlan {
		return BusinessPlan{Namespace: valid.Namespace, Username: valid.Username, Events: []BusinessEvent{{
			History:  valid.Events[0].History,
			Requests: append([]BusinessRequest(nil), valid.Events[0].Requests...),
		}}}
	}

	duplicateKey := clone()
	conflicting := duplicateKey.Events[0].Requests[0]
	conflicting.ID = uuid.New()
	duplicateKey.Events[0].Requests = append(duplicateKey.Events[0].Requests, conflicting)
	if duplicateKey.Valid() {
		t.Fatal("幂等键重复应判为不合法")
	}

	duplicateTarget := clone()
	conflicting = duplicateTarget.Events[0].Requests[0]
	conflicting.ID = uuid.New()
	conflicting.IdempotencyKey = conflicting.IdempotencyKey + "-other"
	duplicateTarget.Events[0].Requests = append(duplicateTarget.Events[0].Requests, conflicting)
	if duplicateTarget.Valid() {
		t.Fatal("同事件内目标重复应判为不合法")
	}
}
