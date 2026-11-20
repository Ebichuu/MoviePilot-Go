package notificationflow

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// 本文件补齐计划器里「目标来源异常」与「链接拼装」两类分支：
// 目标数量超限、目标身份非法、同事件目标重复，以及域名解析失败与链接超长。

func manyTargets(count int) staticTargetProvider {
	targets := make([]notification.Target, 0, count)
	for index := 0; index < count; index++ {
		targets = append(targets, notification.Target{
			Channel: notification.ChannelTelegram, Source: strconv.Itoa(index),
		})
	}
	return staticTargetProvider(targets)
}

func TestPlannerPlanRejectsTooManyTargets(t *testing.T) {
	planner, err := NewPlanner(manyTargets(MaxRequestsPerEvent + 1))
	if err != nil {
		t.Fatalf("创建计划器失败: %v", err)
	}
	if _, err := planner.Plan(uuid.New(), "alice", organizer.Plan{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("目标超限 Plan() error = %v", err)
	}
}

func TestPlannerPlanSuccessRejectsInvalidTargetIdentity(t *testing.T) {
	batchID := uuid.New()
	intentID := uuid.New()
	media := organization.MediaContext{
		Source: "themoviedb", MediaID: "9", TitleYear: "示例电影 (2026)", MediaType: "电影",
	}
	snapshot := plannerSuccessSnapshot(t, batchID, intentID, "alice", 0, media)

	planner, err := NewPlanner(staticTargetProvider([]notification.Target{{
		Channel: notification.Channel("not-a-channel"), Source: "1",
	}}))
	if err != nil {
		t.Fatalf("创建计划器失败: %v", err)
	}
	if _, err := planner.PlanSuccess(batchID, "alice", []organization.SuccessSnapshot{snapshot}, ""); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("非法目标 PlanSuccess() error = %v", err)
	}
}

func TestBusinessPlanRejectsOversizedInputAndBadTargets(t *testing.T) {
	t.Run("渲染器存在时仍拒绝超长标题", func(t *testing.T) {
		planner := newBusinessPlanner(t, staticTargetProvider([]notification.Target{
			{Channel: notification.ChannelTelegram, Source: "1"},
		}), &businessRendererFake{}, "")
		input := businessInput(notification.ContentDownloadAdded)
		input.Title = strings.Repeat("长", MaxTitleBytes)
		if _, err := planner.PlanBusiness(input); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("超长标题 PlanBusiness() error = %v", err)
		}
	})

	t.Run("非法目标身份", func(t *testing.T) {
		planner := newBusinessPlanner(t, staticTargetProvider([]notification.Target{{
			Channel: notification.ChannelTelegram, Source: strings.Repeat("s", MaxSourceBytes+1),
		}}), nil, "")
		if _, err := planner.PlanBusiness(businessInput(notification.ContentDownloadAdded)); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("非法目标 PlanBusiness() error = %v", err)
		}
	})

	t.Run("同事件内目标重复", func(t *testing.T) {
		planner := newBusinessPlanner(t, staticTargetProvider([]notification.Target{
			{Channel: notification.ChannelTelegram, Source: "1"},
			{Channel: notification.ChannelTelegram, Source: "1"},
		}), nil, "")
		if _, err := planner.PlanBusiness(businessInput(notification.ContentDownloadAdded)); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("重复目标 PlanBusiness() error = %v", err)
		}
	})
}

func TestBusinessNotificationLinkRejectsUnparsableAndOversized(t *testing.T) {
	if got := businessNotificationLink("http://[::1", "#/history"); got != "" {
		t.Fatalf("不可解析域名应返回空链接，得到 %q", got)
	}
	if got := businessNotificationLink("https://app.example", "#/"+strings.Repeat("a", MaxLinkBytes-16)); got != "" {
		t.Fatalf("超长链接应返回空值，得到 %d 字节", len(got))
	}
	if got := businessNotificationLink("app.example", ""); got != "" {
		t.Fatalf("空路由应返回空链接，得到 %q", got)
	}
}
