package app

import (
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/subscriptionflow"
)

func TestRSSHandlerBinderBindsOnce(t *testing.T) {
	t.Parallel()

	var nilBinder *rssHandlerBinder
	if err := nilBinder.Bind(&subscriptionflow.RSSHandler{}); !errors.Is(err, jobs.ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("nil binder Bind error = %v", err)
	}
	binder := &rssHandlerBinder{}
	if err := binder.Bind(nil); !errors.Is(err, jobs.ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("nil handler Bind error = %v", err)
	}

	handler := &subscriptionflow.RSSHandler{}
	if err := binder.Bind(handler); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	// 构造环只允许绑定一次：第二次必须拒绝，避免两个处理器争同一个 River 周期任务。
	if err := binder.Bind(&subscriptionflow.RSSHandler{}); !errors.Is(err, errRSSHandlerAlreadyBound) {
		t.Fatalf("重复 Bind error = %v", err)
	}
}

func TestRSSHandlerBinderUnbound(t *testing.T) {
	t.Parallel()

	var nilBinder *rssHandlerBinder
	state := jobs.RSSSweepState{Initialized: true, SiteIDs: []int{3}}
	got, err := nilBinder.HandleRSSSweep(t.Context(), time.Now(), state)
	if !errors.Is(err, jobs.ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("nil binder HandleRSSSweep error = %v", err)
	}
	// 未绑定时把传入状态原样退回，不能让调用方误以为进度推进了。
	if got.Initialized != state.Initialized || len(got.SiteIDs) != 1 || got.SiteIDs[0] != 3 {
		t.Fatalf("状态被改写 = %+v", got)
	}
	if err := nilBinder.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{}); !errors.Is(err, jobs.ErrRSSPollHandlerUnavailable) {
		t.Fatalf("nil binder HandleRSSPoll error = %v", err)
	}
	if err := nilBinder.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{}); !errors.Is(err, jobs.ErrRSSMatchHandlerUnavailable) {
		t.Fatalf("nil binder HandleRSSMatch error = %v", err)
	}

	unbound := &rssHandlerBinder{}
	if _, err := unbound.HandleRSSSweep(t.Context(), time.Now(), state); !errors.Is(err, jobs.ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("未绑定 HandleRSSSweep error = %v", err)
	}
	if err := unbound.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{}); !errors.Is(err, jobs.ErrRSSPollHandlerUnavailable) {
		t.Fatalf("未绑定 HandleRSSPoll error = %v", err)
	}
	if err := unbound.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{}); !errors.Is(err, jobs.ErrRSSMatchHandlerUnavailable) {
		t.Fatalf("未绑定 HandleRSSMatch error = %v", err)
	}
}

func TestRSSHandlerBinderDelegatesToBoundHandler(t *testing.T) {
	t.Parallel()

	binder := &rssHandlerBinder{}
	// 零值 RSSHandler 的依赖不完整：三个入口都应返回它自己的错误，
	// 证明调用确实穿透到了绑定的处理器，而不是被绑定层拦下。
	if err := binder.Bind(&subscriptionflow.RSSHandler{}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	state := jobs.RSSSweepState{}
	if _, err := binder.HandleRSSSweep(t.Context(), time.Now(), state); !errors.Is(err, subscriptionflow.ErrInvalidRSSHandler) {
		t.Fatalf("HandleRSSSweep error = %v", err)
	}
	if err := binder.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{}); !errors.Is(err, subscriptionflow.ErrInvalidRSSHandler) {
		t.Fatalf("HandleRSSPoll error = %v", err)
	}
	if err := binder.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{}); !errors.Is(err, subscriptionflow.ErrInvalidRSSHandler) {
		t.Fatalf("HandleRSSMatch error = %v", err)
	}
}
