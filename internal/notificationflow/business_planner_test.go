package notificationflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
)

type businessTargetProviderFake struct {
	targets     []notification.Target
	calls       int
	lastMessage notification.Message
}

func (f *businessTargetProviderFake) Targets(message notification.Message) []notification.Target {
	f.calls++
	f.lastMessage = message
	return f.targets
}

type businessRendererFake struct {
	title      string
	text       string
	image      string
	link       string
	err        error
	calls      int
	gotMessage notification.Message
	gotContext map[string]any
}

func (f *businessRendererFake) Apply(message notification.Message, context map[string]any) (notification.Message, error) {
	f.calls++
	f.gotMessage = message
	f.gotContext = context
	if f.err != nil {
		return notification.Message{}, f.err
	}
	if f.title != "" {
		message.Title = f.title
	}
	if f.text != "" {
		message.Text = f.text
	}
	if f.image != "" {
		message.Image = f.image
	}
	if f.link != "" {
		message.Link = f.link
	}
	return message, nil
}

func newBusinessPlanner(t *testing.T, provider TargetProvider, renderer BusinessTemplateRenderer, domain string) *Planner {
	t.Helper()
	planner, err := NewBusinessPlanner(provider, renderer, domain)
	if err != nil {
		t.Fatalf("创建业务计划器失败: %v", err)
	}
	return planner
}

func businessInput(contentType notification.ContentType) BusinessInput {
	return BusinessInput{
		EventKey:    "event-1",
		Username:    "alice",
		ContentType: contentType,
		Title:       "沙丘",
		Text:        "下载完成",
	}
}

func TestNewBusinessPlannerRequiresProvider(t *testing.T) {
	if _, err := NewBusinessPlanner(nil, nil, "app.example"); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("缺少目标来源 = %v", err)
	}
	provider := &businessTargetProviderFake{}
	planner := newBusinessPlanner(t, provider, nil, "  https://app.example  ")
	if planner.businessRenderer != nil {
		t.Fatal("未提供渲染器时不应有渲染器")
	}
	if planner.businessAppDomain != "https://app.example" {
		t.Fatalf("应用域名 = %q", planner.businessAppDomain)
	}
}

func TestPlanBusinessDispatchesByContentType(t *testing.T) {
	provider := &businessTargetProviderFake{targets: []notification.Target{{Channel: notification.ChannelTelegram, Source: "1"}}}
	planner := newBusinessPlanner(t, provider, nil, "")

	cases := []struct {
		contentType notification.ContentType
		prefix      string
		mtype       notification.Type
	}{
		{notification.ContentDownloadAdded, downloadAddedEventKeyPrefix, notification.TypeDownload},
		{notification.ContentSubscribeAdded, subscribeAddedEventKeyPrefix, notification.TypeSubscribe},
		{notification.ContentSubscribeComplete, subscribeCompleteEventKeyPrefix, notification.TypeSubscribe},
	}
	for _, testCase := range cases {
		plan, err := planner.PlanBusiness(businessInput(testCase.contentType))
		if err != nil {
			t.Fatalf("%s 计划失败: %v", testCase.contentType, err)
		}
		if len(plan.Events) != 1 {
			t.Fatalf("%s 事件数 = %d", testCase.contentType, len(plan.Events))
		}
		history := plan.Events[0].History
		// 事件键必须带内容类型前缀，避免三类通知在历史表里互相覆盖。
		if history.EventKey != testCase.prefix+"event-1" {
			t.Fatalf("%s 事件键 = %q", testCase.contentType, history.EventKey)
		}
		if history.MType == nil || *history.MType != string(testCase.mtype) {
			t.Fatalf("%s 类型 = %v", testCase.contentType, history.MType)
		}
		if !plan.Valid() {
			t.Fatalf("%s 计划不合法", testCase.contentType)
		}
	}

	if _, err := planner.PlanBusiness(BusinessInput{EventKey: "e", Username: "alice", ContentType: notification.ContentType("unknown")}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("未知内容类型 = %v", err)
	}
}

func TestPlanBusinessValidatesIdentity(t *testing.T) {
	provider := &businessTargetProviderFake{}
	planner := newBusinessPlanner(t, provider, nil, "")

	missingUsername := businessInput(notification.ContentDownloadAdded)
	missingUsername.Username = "   "
	if _, err := planner.PlanBusiness(missingUsername); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("空用户名 = %v", err)
	}
	missingKey := businessInput(notification.ContentDownloadAdded)
	missingKey.EventKey = " "
	if _, err := planner.PlanBusiness(missingKey); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("空事件键 = %v", err)
	}
	longUsername := businessInput(notification.ContentDownloadAdded)
	longUsername.Username = strings.Repeat("名", MaxUsernameBytes)
	if _, err := planner.PlanBusiness(longUsername); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("超长用户名 = %v", err)
	}
	longKey := businessInput(notification.ContentDownloadAdded)
	longKey.EventKey = strings.Repeat("k", MaxSourceBytes+1)
	if _, err := planner.PlanBusiness(longKey); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("超长事件键 = %v", err)
	}
	// 没有渲染器时标题与正文都必须由业务方给出。
	emptyTitle := businessInput(notification.ContentDownloadAdded)
	emptyTitle.Title = "  "
	if _, err := planner.PlanBusiness(emptyTitle); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("缺标题 = %v", err)
	}
	emptyText := businessInput(notification.ContentDownloadAdded)
	emptyText.Text = ""
	if _, err := planner.PlanBusiness(emptyText); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("缺正文 = %v", err)
	}

	// 手工构造的 planner 没有 provider 时必须拒绝，而不是 panic。
	manual := &Planner{}
	if _, err := manual.PlanBusiness(businessInput(notification.ContentDownloadAdded)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("缺少目标来源 = %v", err)
	}
}

func TestPlanBusinessBuildsDeterministicRequests(t *testing.T) {
	provider := &businessTargetProviderFake{targets: []notification.Target{
		{Channel: notification.ChannelTelegram, Source: "1"},
		{Channel: notification.ChannelWebhook, Source: "2"},
	}}
	planner := newBusinessPlanner(t, provider, nil, "")
	input := businessInput(notification.ContentDownloadAdded)
	input.Image = " https://img.example/a.jpg "
	input.Link = "https://site.example/t/1"
	input.UserID = " 42 "

	first, err := planner.PlanBusiness(input)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	second, err := planner.PlanBusiness(input)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}

	if len(first.Events[0].Requests) != 2 {
		t.Fatalf("请求数 = %d", len(first.Events[0].Requests))
	}
	// 同一输入必须产生完全相同的 ID 与幂等键，否则重放会重复投递。
	for index, request := range first.Events[0].Requests {
		replay := second.Events[0].Requests[index]
		if request.ID != replay.ID {
			t.Fatalf("请求 ID 不稳定: %s != %s", request.ID, replay.ID)
		}
		if request.IdempotencyKey != replay.IdempotencyKey {
			t.Fatalf("幂等键不稳定: %s != %s", request.IdempotencyKey, replay.IdempotencyKey)
		}
		if !strings.HasPrefix(request.IdempotencyKey, businessRequestKeyPrefix) {
			t.Fatalf("幂等键前缀 = %q", request.IdempotencyKey)
		}
		// 请求上的渠道与来源必须与消息一致，Valid() 会检查这一致性。
		if request.Message.Channel != request.Channel || request.Message.Source != request.Source {
			t.Fatalf("请求与消息不一致: %+v", request)
		}
		if request.Message.Username != "alice" {
			t.Fatalf("用户名 = %q", request.Message.Username)
		}
		if request.Message.Image != "https://img.example/a.jpg" {
			t.Fatalf("图片未裁剪: %q", request.Message.Image)
		}
		if request.Message.UserID != "42" {
			t.Fatalf("用户 ID 未裁剪: %q", request.Message.UserID)
		}
		if request.Message.Type != notification.TypeDownload {
			t.Fatalf("消息类型 = %q", request.Message.Type)
		}
		if request.Message.ContentType != notification.ContentDownloadAdded {
			t.Fatalf("内容类型 = %q", request.Message.ContentType)
		}
	}
	// 命名空间由事件键派生，必须稳定且非零。
	expected := uuid.NewSHA1(uuid.NameSpaceOID, []byte(downloadAddedEventKeyPrefix+"event-1"))
	if first.Namespace != expected || second.Namespace != expected {
		t.Fatalf("命名空间 = %s", first.Namespace)
	}
	if first.Username != "alice" {
		t.Fatalf("计划用户名 = %q", first.Username)
	}
}

func TestPlanBusinessRejectsTooManyTargets(t *testing.T) {
	targets := make([]notification.Target, 0, MaxRequestsPerEvent+1)
	for index := 0; index <= MaxRequestsPerEvent; index++ {
		targets = append(targets, notification.Target{
			Channel: notification.ChannelTelegram, Source: fmt.Sprintf("src-%d", index),
		})
	}
	planner := newBusinessPlanner(t, &businessTargetProviderFake{targets: targets}, nil, "")
	if _, err := planner.PlanBusiness(businessInput(notification.ContentDownloadAdded)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("目标过多 = %v", err)
	}
}

func TestPlanBusinessWithNoTargetsIsValid(t *testing.T) {
	provider := &businessTargetProviderFake{}
	planner := newBusinessPlanner(t, provider, nil, "")
	plan, err := planner.PlanBusiness(businessInput(notification.ContentSubscribeAdded))
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	// 没有可用渠道时仍然生成历史记录（用户能看到「发生过」），只是没有投递请求。
	if len(plan.Events) != 1 || len(plan.Events[0].Requests) != 0 {
		t.Fatalf("计划 = %+v", plan)
	}
	if plan.Events[0].History.EventKey == "" {
		t.Fatal("历史事件键不应为空")
	}
}

func TestPlanBusinessUsesRenderer(t *testing.T) {
	renderer := &businessRendererFake{title: " 渲染标题 ", text: "渲染正文"}
	provider := &businessTargetProviderFake{targets: []notification.Target{{Channel: notification.ChannelWebPush, Source: "dev-1"}}}
	planner := newBusinessPlanner(t, provider, renderer, "")

	input := businessInput(notification.ContentSubscribeComplete)
	input.Context = map[string]any{"subscribe": map[string]any{"name": "沙丘"}}
	plan, err := planner.PlanBusiness(input)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if renderer.calls != 1 {
		t.Fatalf("渲染次数 = %d", renderer.calls)
	}
	// 渲染器拿到的是已填充类型/内容类型/用户名的消息与业务上下文。
	if renderer.gotMessage.ContentType != notification.ContentSubscribeComplete {
		t.Fatalf("渲染输入内容类型 = %q", renderer.gotMessage.ContentType)
	}
	if renderer.gotMessage.Username != "alice" {
		t.Fatalf("渲染输入用户名 = %q", renderer.gotMessage.Username)
	}
	if renderer.gotContext["subscribe"] == nil {
		t.Fatalf("渲染上下文 = %v", renderer.gotContext)
	}
	if *plan.Events[0].History.Title != "渲染标题" {
		t.Fatalf("历史标题 = %q", *plan.Events[0].History.Title)
	}
	// 有渲染器时业务方可以不提供标题/正文。
	withoutContent := businessInput(notification.ContentSubscribeComplete)
	withoutContent.Title = ""
	withoutContent.Text = ""
	if _, err := planner.PlanBusiness(withoutContent); err != nil {
		t.Fatalf("渲染器补齐内容时不应报错: %v", err)
	}

	// 渲染失败必须变成「计划不合法」，不能带着半成品继续投递。
	failing := &businessRendererFake{err: errors.New("模板缺变量")}
	failingPlanner := newBusinessPlanner(t, provider, failing, "")
	if _, err := failingPlanner.PlanBusiness(businessInput(notification.ContentDownloadAdded)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("渲染失败 = %v", err)
	}

	// 渲染结果超长同样拒绝。
	tooLong := &businessRendererFake{title: strings.Repeat("长", MaxTitleBytes+1)}
	longPlanner := newBusinessPlanner(t, provider, tooLong, "")
	if _, err := longPlanner.PlanBusiness(businessInput(notification.ContentDownloadAdded)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("超长渲染结果 = %v", err)
	}
}

func TestBusinessNotificationLink(t *testing.T) {
	cases := []struct {
		domain string
		route  string
		want   string
	}{
		{"https://app.example", "#/media/1", "https://app.example/#/media/1"},
		{"https://app.example/", "/media/1", "https://app.example/#/media/1"},
		{"app.example", "media/1", "http://app.example/#/media/1"},
		{"", "#/media/1", ""},
		{"https://app.example", "", ""},
		{"https://app.example", "#", ""},
		{"https://app.example", "   #/x", "https://app.example/#/x"},
		{"https://app.example\x00bad", "#/x", ""},
	}
	for _, testCase := range cases {
		if got := businessNotificationLink(testCase.domain, testCase.route); got != testCase.want {
			t.Fatalf("link(%q, %q) = %q，期望 %q", testCase.domain, testCase.route, got, testCase.want)
		}
	}
	// 超长域名或路由直接放弃链接，避免生成不可点击的地址。
	if got := businessNotificationLink(strings.Repeat("a", MaxLinkBytes+1), "#/x"); got != "" {
		t.Fatalf("超长域名 = %q", got)
	}
	if got := businessNotificationLink("https://app.example", "#/"+strings.Repeat("a", MaxLinkBytes)); got != "" {
		t.Fatalf("超长路由 = %q", got)
	}
}

func TestPlanBusinessDerivesLinkFromRoute(t *testing.T) {
	provider := &businessTargetProviderFake{targets: []notification.Target{{Channel: notification.ChannelTelegram, Source: "1"}}}
	planner := newBusinessPlanner(t, provider, nil, "https://app.example")
	input := businessInput(notification.ContentDownloadAdded)
	input.LinkRoute = "#/download/9"
	plan, err := planner.PlanBusiness(input)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	want := businessNotificationLink("https://app.example", "#/download/9")
	if plan.Events[0].Requests[0].Message.Link != want || want == "" {
		t.Fatalf("链接 = %q，期望 %q", plan.Events[0].Requests[0].Message.Link, want)
	}
	// 显式链接优先于路由。
	explicit := input
	explicit.Link = "https://site.example/t/1"
	plan, err = planner.PlanBusiness(explicit)
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if plan.Events[0].Requests[0].Message.Link != "https://site.example/t/1" {
		t.Fatalf("显式链接被覆盖: %q", plan.Events[0].Requests[0].Message.Link)
	}
}

func TestBusinessPlanValidRejectsInconsistentShapes(t *testing.T) {
	provider := &businessTargetProviderFake{targets: []notification.Target{{Channel: notification.ChannelTelegram, Source: "1"}}}
	planner := newBusinessPlanner(t, provider, nil, "")
	valid, err := planner.PlanBusiness(businessInput(notification.ContentDownloadAdded))
	if err != nil {
		t.Fatalf("计划失败: %v", err)
	}
	if !valid.Valid() {
		t.Fatal("基准计划应为合法")
	}

	mutations := map[string]func(*BusinessPlan){
		"空命名空间":    func(p *BusinessPlan) { p.Namespace = uuid.Nil },
		"空用户名":     func(p *BusinessPlan) { p.Username = " " },
		"事件过多":     func(p *BusinessPlan) { p.Events = append(p.Events, p.Events[0]) },
		"请求 ID 为空": func(p *BusinessPlan) { p.Events[0].Requests[0].ID = uuid.Nil },
		"幂等键为空":    func(p *BusinessPlan) { p.Events[0].Requests[0].IdempotencyKey = "" },
		"渠道不一致":    func(p *BusinessPlan) { p.Events[0].Requests[0].Message.Channel = notification.ChannelWebhook },
		"用户名不一致":   func(p *BusinessPlan) { p.Events[0].Requests[0].Message.Username = "bob" },
		"历史键为空":    func(p *BusinessPlan) { p.Events[0].History.EventKey = "" },
		"重复请求": func(p *BusinessPlan) {
			p.Events[0].Requests = append(p.Events[0].Requests, p.Events[0].Requests[0])
		},
	}
	for name, mutate := range mutations {
		plan := BusinessPlan{Namespace: valid.Namespace, Username: valid.Username, Events: []BusinessEvent{{
			History:  valid.Events[0].History,
			Requests: append([]BusinessRequest(nil), valid.Events[0].Requests...),
		}}}
		mutate(&plan)
		if plan.Valid() {
			t.Fatalf("%s 应判为不合法", name)
		}
	}
}

func TestBusinessRequestIdentityGuards(t *testing.T) {
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte("business-event:event-1"))
	historyKey := downloadAddedEventKeyPrefix + "event-1"

	if _, _, ok := businessRequestIdentity(namespace, historyKey, notification.Target{Channel: notification.Channel("nope")}); ok {
		t.Fatal("未知渠道应被拒绝")
	}
	if _, _, ok := businessRequestIdentity(namespace, historyKey, notification.Target{
		Channel: notification.ChannelTelegram, Source: strings.Repeat("s", MaxSourceBytes+1),
	}); ok {
		t.Fatal("超长来源应被拒绝")
	}
	id, key, ok := businessRequestIdentity(namespace, historyKey, notification.Target{
		Channel: notification.ChannelTelegram, Source: "1",
	})
	if !ok || id == uuid.Nil || !strings.HasPrefix(key, businessRequestKeyPrefix) {
		t.Fatalf("身份 = %s %q %v", id, key, ok)
	}
	// 相同输入必须得到相同身份；不同来源必须不同。
	repeatID, repeatKey, _ := businessRequestIdentity(namespace, historyKey, notification.Target{
		Channel: notification.ChannelTelegram, Source: "1",
	})
	if repeatID != id || repeatKey != key {
		t.Fatal("身份计算不稳定")
	}
	otherID, otherKey, _ := businessRequestIdentity(namespace, historyKey, notification.Target{
		Channel: notification.ChannelTelegram, Source: "2",
	})
	if otherID == id || otherKey == key {
		t.Fatal("不同来源应得到不同身份")
	}
}
