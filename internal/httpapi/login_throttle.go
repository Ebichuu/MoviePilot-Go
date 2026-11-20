package httpapi

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// humanShortDuration 把等待时长写成人话，用于登录锁定的提示。
func humanShortDuration(d time.Duration) string {
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 60 {
		return strconv.Itoa(seconds) + " 秒"
	}
	minutes := seconds / 60
	if remaining := seconds % 60; remaining > 0 {
		return strconv.Itoa(minutes) + " 分 " + strconv.Itoa(remaining) + " 秒"
	}
	return strconv.Itoa(minutes) + " 分钟"
}

// loginThrottleConfig 是登录防爆破的阈值，由 config 注入（可用环境变量调整）。
type loginThrottleConfig struct {
	maxUserFailures int
	maxIPFailures   int
	ipWindow        time.Duration
	lockBase        time.Duration
	lockMax         time.Duration
}

// defaultLoginThrottleConfig 给未配置场景与单元测试兜底：
// 5 次失败锁 1 分钟，之后每次多锁一倍，最长 15 分钟。
func defaultLoginThrottleConfig() loginThrottleConfig {
	return loginThrottleConfig{
		maxUserFailures: 5,
		maxIPFailures:   20,
		ipWindow:        10 * time.Minute,
		lockBase:        time.Minute,
		lockMax:         15 * time.Minute,
	}
}

// loginFailureState 是单个账号或 IP 的失败计数与锁定状态。
type loginFailureState struct {
	failures    int
	windowStart time.Time
	lastFailure time.Time
	lockedUntil time.Time
}

// loginThrottle 对连续登录失败做临时锁定，防暴力破解。
//
// 为什么需要：V2 是自用定位，登录接口没有失败限制；一旦对外提供（哪怕是挂在
// 公网的小站），攻击者可以无限次尝试密码。bcrypt cost=12 只能让每次尝试变慢，
// 挡不住持续爆破。
//
// 两道防线：
//   - 账号维度（主）：同一用户名累计失败达到阈值后锁定，锁定时长按超出次数指数
//     翻倍并封顶。成功登录清零。
//   - IP 维度（辅）：窗口内失败过多整体锁一个窗口，防止换着用户名撞库。
//     只有直连来源是本机/内网时才采信 X-Forwarded-For，否则伪造该头就能绕过。
//
// 状态存在进程内存里：单实例部署足够；多副本部署必须换成 Redis 实现。
// 这里如实说明，不假装它是分布式限流。
type loginThrottle struct {
	cfg   loginThrottleConfig
	now   func() time.Time
	mu    sync.Mutex
	users map[string]*loginFailureState
	ips   map[string]*loginFailureState
}

func newLoginThrottle(cfg loginThrottleConfig, now func() time.Time) *loginThrottle {
	if cfg.maxUserFailures <= 0 || cfg.lockBase <= 0 || cfg.lockMax < cfg.lockBase {
		cfg = defaultLoginThrottleConfig()
	}
	if now == nil {
		now = time.Now
	}
	return &loginThrottle{
		cfg: cfg, now: now,
		users: make(map[string]*loginFailureState), ips: make(map[string]*loginFailureState),
	}
}

// blocked 报告账号或 IP 是否处于锁定中，并给出建议的重试等待时长。
func (t *loginThrottle) blocked(username, ip string) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var wait time.Duration
	if state, ok := t.users[normalizeLoginUser(username)]; ok && now.Before(state.lockedUntil) {
		wait = state.lockedUntil.Sub(now)
	}
	if state, ok := t.ips[strings.TrimSpace(ip)]; ok && now.Before(state.lockedUntil) {
		if remaining := state.lockedUntil.Sub(now); remaining > wait {
			wait = remaining
		}
	}
	return wait, wait > 0
}

// fail 记录一次登录失败，返回是否因此锁定以及锁定时长。
func (t *loginThrottle) fail(username, ip string) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var lockFor time.Duration

	if userKey := normalizeLoginUser(username); userKey != "" {
		state := t.users[userKey]
		if state == nil {
			state = &loginFailureState{}
			t.users[userKey] = state
		}
		// 太久没有失败过的账号重新计数，避免历史失败永久叠高锁定时长。
		if !state.lastFailure.IsZero() && now.Sub(state.lastFailure) > t.staleAfter() {
			*state = loginFailureState{}
		}
		state.failures++
		state.lastFailure = now
		if state.failures >= t.cfg.maxUserFailures {
			// 超出的次数翻倍，最多翻 8 次（1m → 256m 但被 lockMax 封顶）。
			steps := state.failures - t.cfg.maxUserFailures
			if steps > 8 {
				steps = 8
			}
			lock := t.cfg.lockBase << uint(steps)
			if lock > t.cfg.lockMax {
				lock = t.cfg.lockMax
			}
			state.lockedUntil = now.Add(lock)
			if lock > lockFor {
				lockFor = lock
			}
		}
	}

	if ipKey := strings.TrimSpace(ip); ipKey != "" {
		state := t.ips[ipKey]
		if state == nil {
			state = &loginFailureState{windowStart: now}
			t.ips[ipKey] = state
		}
		if now.Sub(state.windowStart) > t.cfg.ipWindow {
			state.failures = 0
			state.windowStart = now
			state.lockedUntil = time.Time{}
		}
		state.failures++
		state.lastFailure = now
		if state.failures >= t.cfg.maxIPFailures {
			state.lockedUntil = now.Add(t.cfg.ipWindow)
			if t.cfg.ipWindow > lockFor {
				lockFor = t.cfg.ipWindow
			}
		}
	}

	t.sweepLocked(now)
	return lockFor, lockFor > 0
}

// reset 在登录成功后清零计数。
func (t *loginThrottle) reset(username, ip string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.users, normalizeLoginUser(username))
	delete(t.ips, strings.TrimSpace(ip))
}

// staleAfter 是失败计数的自然过期时间。
func (t *loginThrottle) staleAfter() time.Duration {
	if t.cfg.ipWindow > time.Hour {
		return t.cfg.ipWindow * 6
	}
	return time.Hour
}

// sweepLocked 清理长期空闲的条目，防止被大量随机用户名把内存撑爆。
// 调用方必须已持有 t.mu。
func (t *loginThrottle) sweepLocked(now time.Time) {
	if len(t.users) < 4096 && len(t.ips) < 4096 {
		return
	}
	stale := t.staleAfter()
	for name, state := range t.users {
		if now.Before(state.lockedUntil) {
			continue
		}
		if !state.lastFailure.IsZero() && now.Sub(state.lastFailure) > stale {
			delete(t.users, name)
		}
	}
	for address, state := range t.ips {
		if now.Before(state.lockedUntil) {
			continue
		}
		if !state.lastFailure.IsZero() && now.Sub(state.lastFailure) > stale {
			delete(t.ips, address)
		}
	}
}

// normalizeLoginUser 统一账号写法，避免用大小写或空白变体绕过计数。
func normalizeLoginUser(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// clientIPFromRequest 取来源 IP。
//
// 只有直连对端是本机/内网时才采信 X-Forwarded-For / X-Real-IP：反代部署下这是
// 唯一能拿到真实客户端 IP 的方式；而直接暴露公网时，这两个头任何人都能伪造，
// 采信它等于放弃 IP 维度这道防线。账号维度的锁定不依赖这个值。
func clientIPFromRequest(r *http.Request) string {
	remote := remoteIP(r.RemoteAddr)
	if isLocalOrPrivate(remote) {
		if forwarded := firstForwardedIP(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			return forwarded
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			if net.ParseIP(real) != nil {
				return real
			}
		}
	}
	return remote
}

// remoteIP 从 host:port 里取出 IP，去掉端口与 IPv6 方括号。
func remoteIP(remoteAddr string) string {
	address := strings.TrimSpace(remoteAddr)
	if address == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(address, "[]")
}

// firstForwardedIP 取 X-Forwarded-For 里最左侧的合法 IP。
func firstForwardedIP(header string) string {
	for _, part := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(part)
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// isLocalOrPrivate 判断地址是否回环或私有网段。
func isLocalOrPrivate(address string) bool {
	ip := net.ParseIP(strings.Trim(address, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// LoginThrottleConfig 是对外暴露的阈值配置，字段与 config.Config 一一对应。
type LoginThrottleConfig struct {
	MaxUserFailures int
	MaxIPFailures   int
	IPWindow        time.Duration
	LockBase        time.Duration
	LockMax         time.Duration
}

// NewLoginThrottle 按配置创建登录限流器；now 为 nil 时使用 time.Now。
// 阈值非法时回落到默认值（5 次失败、1 分钟起锁、15 分钟封顶）。
func NewLoginThrottle(cfg LoginThrottleConfig, now func() time.Time) *loginThrottle {
	return newLoginThrottle(loginThrottleConfig{
		maxUserFailures: cfg.MaxUserFailures,
		maxIPFailures:   cfg.MaxIPFailures,
		ipWindow:        cfg.IPWindow,
		lockBase:        cfg.LockBase,
		lockMax:         cfg.LockMax,
	}, now)
}
