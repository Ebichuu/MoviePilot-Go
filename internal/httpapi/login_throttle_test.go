package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// failingUserStore 让 Authenticate 始终失败，用来模拟密码错误。
type failingUserStore struct{}

func (failingUserStore) UserByName(context.Context, string) (auth.User, error) {
	return auth.User{}, errors.New("user not found")
}

func (failingUserStore) UserByID(context.Context, int64) (auth.User, error) {
	return auth.User{}, errors.New("user not found")
}

func testThrottle(now *time.Time, cfg LoginThrottleConfig) *loginThrottle {
	return NewLoginThrottle(cfg, func() time.Time { return *now })
}

// TestLoginThrottleLocksAndEscalates 验证账号维度：达到阈值即锁定，锁定时长按次数翻倍。
func TestLoginThrottleLocksAndEscalates(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	throttle := testThrottle(&now, LoginThrottleConfig{
		MaxUserFailures: 3, MaxIPFailures: 100, IPWindow: time.Hour,
		LockBase: time.Minute, LockMax: 10 * time.Minute,
	})

	// 前两次失败不锁定。
	for i := 0; i < 2; i++ {
		if wait, locked := throttle.fail("Admin", "1.2.3.4"); locked {
			t.Fatalf("第 %d 次失败不应锁定（wait=%v）", i+1, wait)
		}
	}
	// 第三次达到阈值：锁 1 分钟。
	wait, locked := throttle.fail("admin", "1.2.3.4")
	if !locked || wait != time.Minute {
		t.Fatalf("第三次失败应锁 1 分钟，实际 (%v, %v)", wait, locked)
	}
	// 大小写与空白变体必须命中同一账号。
	if blockedFor, blocked := throttle.blocked("  ADMIN ", "9.9.9.9"); !blocked || blockedFor != time.Minute {
		t.Fatalf("账号锁定应对大小写不敏感，实际 (%v, %v)", blockedFor, blocked)
	}

	now = now.Add(time.Minute + time.Second)
	if _, blocked := throttle.blocked("admin", "1.2.3.4"); blocked {
		t.Fatal("锁定到期后应放行")
	}
	// 解锁后再失败一次：次数已累计到 4，锁定翻倍到 2 分钟。
	if wait, locked := throttle.fail("admin", "1.2.3.4"); !locked || wait != 2*time.Minute {
		t.Fatalf("再次失败应锁 2 分钟，实际 (%v, %v)", wait, locked)
	}
}

// TestLoginThrottleResetsOnSuccess 验证登录成功清零计数。
func TestLoginThrottleResetsOnSuccess(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	throttle := testThrottle(&now, LoginThrottleConfig{
		MaxUserFailures: 2, MaxIPFailures: 100, IPWindow: time.Hour,
		LockBase: time.Minute, LockMax: time.Minute,
	})
	if _, locked := throttle.fail("admin", "1.2.3.4"); locked {
		t.Fatal("第一次失败不应锁定")
	}
	throttle.reset("admin", "1.2.3.4")
	if _, locked := throttle.fail("admin", "1.2.3.4"); locked {
		t.Fatal("成功登录后计数应清零，第二次失败不应锁定")
	}
}

// TestLoginThrottleIPWindow 验证 IP 维度：同一 IP 换用户名撞库也会被锁。
func TestLoginThrottleIPWindow(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	throttle := testThrottle(&now, LoginThrottleConfig{
		MaxUserFailures: 10, MaxIPFailures: 3, IPWindow: 5 * time.Minute,
		LockBase: time.Minute, LockMax: time.Minute,
	})
	for i := 0; i < 3; i++ {
		if _, locked := throttle.fail("user"+string(rune('a'+i)), "1.2.3.4"); locked && i < 2 {
			t.Fatalf("第 %d 次不应锁定", i+1)
		}
	}
	wait, blocked := throttle.blocked("another-user", "1.2.3.4")
	if !blocked || wait != 5*time.Minute {
		t.Fatalf("IP 应被锁一个窗口，实际 (%v, %v)", wait, blocked)
	}
	// 别的 IP 不受影响。
	if _, blocked := throttle.blocked("another-user", "5.6.7.8"); blocked {
		t.Fatal("其他 IP 不应被牵连")
	}
}

// TestClientIPFromRequest 验证代理头的采信规则：只有直连来源是本机/内网才信。
func TestClientIPFromRequest(t *testing.T) {
	public := httptest.NewRequest(http.MethodPost, "/login/access-token", nil)
	public.RemoteAddr = "203.0.113.9:51234"
	public.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIPFromRequest(public); got != "203.0.113.9" {
		t.Fatalf("公网直连时不应采信 X-Forwarded-For，实际 %q", got)
	}

	proxied := httptest.NewRequest(http.MethodPost, "/login/access-token", nil)
	proxied.RemoteAddr = "127.0.0.1:51234"
	proxied.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	if got := clientIPFromRequest(proxied); got != "1.2.3.4" {
		t.Fatalf("反代场景应取最左侧 IP，实际 %q", got)
	}

	realIP := httptest.NewRequest(http.MethodPost, "/login/access-token", nil)
	realIP.RemoteAddr = "192.168.1.5:51234"
	realIP.Header.Set("X-Real-IP", "5.6.7.8")
	if got := clientIPFromRequest(realIP); got != "5.6.7.8" {
		t.Fatalf("内网反代应采信 X-Real-IP，实际 %q", got)
	}

	bare := httptest.NewRequest(http.MethodPost, "/login/access-token", nil)
	bare.RemoteAddr = "198.51.100.7"
	if got := clientIPFromRequest(bare); got != "198.51.100.7" {
		t.Fatalf("无端口地址应可用，实际 %q", got)
	}
}

// TestLoginHandlerReturns429WhenLocked 走真实 handler：第 N+1 次尝试返回 429 + Retry-After。
func TestLoginHandlerReturns429WhenLocked(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	deps := Dependencies{
		Auth: auth.NewService(failingUserStore{}, "jwt-secret", "resource-secret", time.Hour),
		LoginThrottle: NewLoginThrottle(LoginThrottleConfig{
			MaxUserFailures: 3, MaxIPFailures: 100, IPWindow: time.Hour,
			LockBase: time.Minute, LockMax: 10 * time.Minute,
		}, func() time.Time { return now }),
	}
	attempt := func() *httptest.ResponseRecorder {
		form := url.Values{"username": {"admin"}, "password": {"wrong-password"}}
		request := httptest.NewRequest(http.MethodPost, "/login/access-token", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		deps.login(recorder, request)
		return recorder
	}

	for i := 0; i < 3; i++ {
		if got := attempt(); got.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次尝试 = %d（%s）", i+1, got.Code, got.Body.String())
		}
	}
	locked := attempt()
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定后应返回 429，实际 %d（%s）", locked.Code, locked.Body.String())
	}
	if retry := locked.Header().Get("Retry-After"); retry != "60" {
		t.Fatalf("Retry-After = %q", retry)
	}
	if !strings.Contains(locked.Body.String(), "过于频繁") {
		t.Fatalf("锁定提示 = %s", locked.Body.String())
	}

	// 锁定到期后恢复正常校验流程（仍然是 401，而不是 429）。
	now = now.Add(time.Minute + time.Second)
	if got := attempt(); got.Code != http.StatusUnauthorized {
		t.Fatalf("解锁后第一次 = %d", got.Code)
	}
}

// TestHumanShortDuration 覆盖提示文案。
func TestHumanShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:        "45 秒",
		90 * time.Second:        "1 分 30 秒",
		2 * time.Minute:         "2 分钟",
		1500 * time.Millisecond: "2 秒",
	}
	for input, want := range cases {
		if got := humanShortDuration(input); got != want {
			t.Fatalf("humanShortDuration(%v) = %q, want %q", input, got, want)
		}
	}
}
