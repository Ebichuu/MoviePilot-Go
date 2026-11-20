package app

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/siteflow"
)

type siteSigninListStoreFake struct {
	sites []database.Site
	err   error
}

func (s *siteSigninListStoreFake) ListSites(context.Context) ([]database.Site, error) {
	return s.sites, s.err
}

type siteSigninCookieStoreFake struct {
	sites      map[int]database.Site
	runtimeErr map[int]error
	updateErr  map[int]error
	updated    []database.Site
}

func (s *siteSigninCookieStoreFake) RuntimeSiteByID(_ context.Context, id int) (database.Site, error) {
	if err := s.runtimeErr[id]; err != nil {
		return database.Site{}, err
	}
	site, ok := s.sites[id]
	if !ok {
		return database.Site{}, errors.New("站点不存在")
	}
	return site, nil
}

func (s *siteSigninCookieStoreFake) UpdateSite(_ context.Context, site database.Site) error {
	if err := s.updateErr[site.ID]; err != nil {
		return err
	}
	s.updated = append(s.updated, site)
	return nil
}

type siteSigninAdapterFake struct {
	cookie string
	err    error
	user   string
	pass   string
}

func (siteSigninAdapterFake) Name() string { return "signin" }

func (siteSigninAdapterFake) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a *siteSigninAdapterFake) SignIn(_ context.Context, username, password string) (string, error) {
	a.user, a.pass = username, password
	return a.cookie, a.err
}

type siteSigninPlainAdapter struct{}

func (siteSigninPlainAdapter) Name() string { return "plain" }

func (siteSigninPlainAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func TestNewSiteSigninSweepRequiresDependencies(t *testing.T) {
	t.Parallel()

	store := &siteSigninListStoreFake{}
	updater := siteflow.NewCookieUpdater(&siteSigninCookieStoreFake{}, func(database.Site) (indexer.Adapter, error) {
		return siteSigninPlainAdapter{}, nil
	})
	if updater == nil {
		t.Fatal("测试前置：CookieUpdater 构造失败")
	}
	// 依赖缺失时返回 nil，jobs 层据此不注册周期任务，而不是每天空跑一轮。
	if got := newSiteSigninSweep(nil, updater, nil); got != nil {
		t.Fatalf("缺存储时 = %#v", got)
	}
	if got := newSiteSigninSweep(store, nil, nil); got != nil {
		t.Fatalf("缺更新器时 = %#v", got)
	}
	if got := newSiteSigninSweep(store, updater, nil); got == nil {
		t.Fatal("依赖齐全时应返回可用实例")
	}
}

func TestSiteSigninSweepClassifiesEverySite(t *testing.T) {
	t.Parallel()

	sites := []database.Site{
		{ID: 10, Name: "签名站", Enabled: true, Adapter: "signed", Config: map[string]any{
			"username": " alice ", "password": " pass ",
		}},
		{ID: 11, Name: "缺密码", Enabled: true, Adapter: "signed", Config: map[string]any{"username": "bob"}},
		{ID: 12, Name: "已禁用", Enabled: false, Adapter: "signed", Config: map[string]any{
			"username": "carol", "password": "pass",
		}},
		{ID: 13, Name: "不支持登录", Enabled: true, Adapter: "plain", Config: map[string]any{
			"username": "dave", "password": "pass",
		}},
		{ID: 14, Name: "密码错误", Enabled: true, Adapter: "denied", Config: map[string]any{
			"username": "erin", "password": "wrong",
		}},
		{ID: 15, Name: "读取失败", Enabled: true, Adapter: "signed", Config: map[string]any{
			"username": "frank", "password": "pass",
		}},
		{ID: 16, Name: "写回失败", Enabled: true, Adapter: "signed", Config: map[string]any{
			"username": "grace", "password": "pass",
		}},
	}
	built := make(map[int]int)
	adapters := make(map[int]*siteSigninAdapterFake)
	cookieStore := &siteSigninCookieStoreFake{
		sites: map[int]database.Site{
			10: {ID: 10, Name: "签名站", Adapter: "signed", Config: map[string]any{}},
			13: {ID: 13, Name: "不支持登录", Adapter: "plain"},
			14: {ID: 14, Name: "密码错误", Adapter: "denied"},
			15: {ID: 15, Name: "读取失败", Adapter: "signed"},
			16: {ID: 16, Name: "写回失败", Adapter: "signed", Config: map[string]any{}},
		},
		runtimeErr: map[int]error{15: errors.New("读取站点失败")},
		updateErr:  map[int]error{16: errors.New("更新站点失败")},
	}
	updater := siteflow.NewCookieUpdater(cookieStore, func(site database.Site) (indexer.Adapter, error) {
		built[site.ID]++
		switch site.Adapter {
		case "plain":
			return siteSigninPlainAdapter{}, nil
		case "denied":
			return &siteSigninAdapterFake{err: &indexer.HTTPStatusError{StatusCode: 401}}, nil
		default:
			adapter := &siteSigninAdapterFake{cookie: "uid=alice; pass=token"}
			adapters[site.ID] = adapter
			return adapter, nil
		}
	})
	sweep := newSiteSigninSweep(&siteSigninListStoreFake{sites: sites}, updater, nil)
	if sweep == nil {
		t.Fatal("测试前置：sweep 构造失败")
	}

	report, err := sweep.SigninEnabledSites(t.Context())
	if err != nil {
		t.Fatalf("SigninEnabledSites() error = %v", err)
	}

	want := []struct {
		id     int
		status string
		reason string
	}{
		{10, "signed", ""},
		{11, "skipped", "未配置登录凭据"},
		{12, "skipped", "站点未启用"},
		{13, "skipped", "该站点适配器不支持登录，请手动更新 Cookie"},
		{14, "skipped", "站点登录被拒绝：用户名或密码错误"},
		{15, "failed", "读取站点失败"},
		{16, "failed", "更新站点失败"},
	}
	if len(report.Items) != len(want) {
		t.Fatalf("结果条数 = %d, want %d", len(report.Items), len(want))
	}
	for index, expected := range want {
		item := report.Items[index]
		if item.SiteID != expected.id || item.Status != expected.status || item.Reason != expected.reason {
			t.Fatalf("结果[%d] = %+v, want %+v", index, item, expected)
		}
		if item.Site != sites[index].Name {
			t.Fatalf("结果[%d] 站点名 = %q", index, item.Site)
		}
	}
	if report.Signed != 1 || report.Skipped != 4 || report.Failed != 2 {
		t.Fatalf("汇总 = signed %d skipped %d failed %d", report.Signed, report.Skipped, report.Failed)
	}

	// 只用被启用且带凭据的站点去构建适配器：跳过项不该产生任何登录尝试，
	// 站点读取失败的也一样（还没走到构建就被拦下）。
	if len(built) != 4 || built[11] != 0 || built[12] != 0 || built[15] != 0 {
		t.Fatalf("适配器构建次数 = %#v", built)
	}
	// 用户名密码在登录前裁剪两端空白，Cookie 写回站点配置。
	if adapters[10] == nil || adapters[10].user != "alice" || adapters[10].pass != "pass" {
		t.Fatalf("登录凭据 = %#v", adapters[10])
	}
	if len(cookieStore.updated) != 1 || cookieStore.updated[0].Config["cookie"] != "uid=alice; pass=token" {
		t.Fatalf("写回结果 = %#v", cookieStore.updated)
	}
}

func TestSiteSigninSweepPropagatesListFailure(t *testing.T) {
	t.Parallel()

	updater := siteflow.NewCookieUpdater(&siteSigninCookieStoreFake{}, func(database.Site) (indexer.Adapter, error) {
		return siteSigninPlainAdapter{}, nil
	})
	sweep := newSiteSigninSweep(&siteSigninListStoreFake{err: context.Canceled}, updater, nil)
	report, err := sweep.SigninEnabledSites(t.Context())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SigninEnabledSites() error = %v", err)
	}
	// 站点清单都读不到时返回零报告，不能给出「0 个站点成功」这种误导性汇总。
	if report.Signed != 0 || report.Skipped != 0 || report.Failed != 0 || report.Items != nil {
		t.Fatalf("失败时报告 = %+v", report)
	}
}

func TestSiteCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		config       map[string]any
		wantUsername string
		wantPassword string
	}{
		{name: "nil 配置", config: nil},
		{name: "缺少键", config: map[string]any{"cookie": "x"}},
		{name: "非字符串值", config: map[string]any{"username": 123, "password": []string{"p"}}},
		{name: "两端空白被裁剪", config: map[string]any{"username": "  alice\t", "password": " pass\n"},
			wantUsername: "alice", wantPassword: "pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			username, password := siteCredentials(database.Site{Config: tt.config})
			if username != tt.wantUsername || password != tt.wantPassword {
				t.Fatalf("凭据 = (%q, %q)", username, password)
			}
		})
	}
}

var _ jobs.SiteSigninHandler = (*siteSigninSweep)(nil)
