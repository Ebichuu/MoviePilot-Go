package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// 本文件覆盖业务通知的纯函数投影与发布闸门。

// businessPlannerStub 是发布闸门测试用的最小规划器，闸门分支不会真正调用它。
type businessPlannerStub struct{}

func (businessPlannerStub) PlanBusiness(notificationflow.BusinessInput) (notificationflow.BusinessPlan, error) {
	return notificationflow.BusinessPlan{}, notificationflow.ErrInvalidPlan
}

func TestDownloadAddedTemplateContextSeasons(t *testing.T) {
	base := organization.SuccessInput{
		Username: "alice", ResourceTerm: "WEB-DL",
		Episodes: []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
	}
	single := downloadAddedTemplateContext(base)
	if single["season_fmt"] != "S01" {
		t.Fatalf("季格式 = %v", single["season_fmt"])
	}
	if single["season_episode"] == "" {
		t.Fatal("单季应给出季集描述")
	}

	multi := base
	multi.Episodes = []organization.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 3}}
	contextValues := downloadAddedTemplateContext(multi)
	if contextValues["season_fmt"] != "" {
		t.Fatalf("多季不应给出单一季格式: %v", contextValues["season_fmt"])
	}
	if contextValues["season_episode"] == "" {
		t.Fatal("多季应拼接季集描述")
	}
}

func TestJoinNotificationContextEmpty(t *testing.T) {
	if got := joinNotificationContext(nil); got != "" {
		t.Fatalf("空切片 = %q", got)
	}
	if got := joinNotificationContext([]string{"A", "B", "C"}); got != "A, B, C" {
		t.Fatalf("拼接 = %q", got)
	}
}

func TestEnqueueSubscribeBusinessNotificationRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	if err := enqueueSubscribeBusinessNotification(ctx, nil, "unknown", "alice", 1, nil, ""); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("未知内容类型 error = %v", err)
	}
	if err := enqueueSubscribeBusinessNotification(ctx, nil, "subscribeAdded", "", 1, nil, ""); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("缺少用户 error = %v", err)
	}
	if err := enqueueSubscribeBusinessNotification(ctx, nil, "subscribeAdded", "alice", 0, nil, ""); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("非法订阅 ID error = %v", err)
	}
}

func TestPublishNextBusinessNotificationGuards(t *testing.T) {
	now := time.Now().UTC()
	planner := businessPlannerStub{}

	if _, err := (&Store{}).PublishNextBusinessNotification(context.Background(), nil, now); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("nil 规划器 error = %v", err)
	}
	if _, err := (&Store{}).PublishNextBusinessNotification(context.Background(), planner, time.Time{}); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("零时间 error = %v", err)
	}
	if _, err := (&Store{}).PublishNextBusinessNotification(nil, planner, now); !errors.Is(err, notificationflow.ErrInvalidPlan) {
		t.Fatalf("nil 上下文 error = %v", err)
	}

	store := mediaServerItemGuardStore(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.PublishNextBusinessNotification(canceled, planner, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}
	if _, err := store.PublishNextBusinessNotification(context.Background(), planner, now); err == nil {
		t.Fatal("连接不可用应报错")
	}
}
