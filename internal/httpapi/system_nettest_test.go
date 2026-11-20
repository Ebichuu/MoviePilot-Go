package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNettestErrorMessageClassification(t *testing.T) {
	cases := map[string]string{
		"Get \"https://api.themoviedb.org\": context deadline exceeded":     "连接超时",
		"Get \"https://x\": Client.Timeout exceeded while awaiting headers": "连接超时",
		"dial tcp: lookup api.bgm.tv: no such host":                         "域名解析失败",
		"dial tcp 127.0.0.1:443: connect: connection refused":               "连接被拒绝",
		"x509: certificate signed by unknown authority":                     "证书校验失败",
		"some other failure": "网络不可达",
	}
	for message, want := range cases {
		if got := nettestErrorMessage(errors.New(message)); got != want {
			t.Fatalf("nettestErrorMessage(%q) = %q，期望 %q", message, got, want)
		}
	}
}

func TestSystemNettestTargetsHidesURLs(t *testing.T) {
	recorder := httptest.NewRecorder()
	systemNettestTargets(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/system/nettest/targets", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	body := recorder.Body.String()
	// 前端只需要 id/name/icon：URL 与代理策略留在服务端，避免被改造成 SSRF 探测。
	if strings.Contains(body, "https://") || strings.Contains(body, `"url"`) {
		t.Fatalf("响应不应暴露目标地址: %s", body)
	}
	for _, id := range []string{"tmdb_api", "tmdb_api_alt", "tmdb_web", "douban", "bangumi", "github", "github_raw", "pypi"} {
		if !strings.Contains(body, `"`+id+`"`) {
			t.Fatalf("缺少内置目标 %q: %s", id, body)
		}
	}
	envelope := decodeBody[Response](t, recorder)
	if !envelope.Success {
		t.Fatalf("应返回成功信封: %+v", envelope)
	}
	data, ok := envelope.Data.([]any)
	if !ok {
		t.Fatalf("data 类型异常: %#v", envelope.Data)
	}
	if len(data) != 8 {
		t.Fatalf("内置目标数量 = %d，期望 8", len(data))
	}
	first, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("目标条目类型异常: %#v", data[0])
	}
	if first["id"] != "tmdb_api" || first["name"] != "api.themoviedb.org" || first["icon"] != "tmdb" {
		t.Fatalf("目标条目字段异常: %#v", first)
	}
}

func TestSystemNettestRejectsUnknownTarget(t *testing.T) {
	// 目标是白名单精确匹配（入参会 trim）：空值、纯空白、未知 id 都只报「不存在」，
	// 绝不能退化成「拿用户输入去发请求」。
	for _, targetID := range []string{"", "%20%20", "unknown", "TMDB_API"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/system/nettest?target_id="+targetID, nil)
		systemNettest(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("target_id=%q 状态码 = %d", targetID, recorder.Code)
		}
		envelope := decodeBody[Response](t, recorder)
		if envelope.Success {
			t.Fatalf("target_id=%q 不应返回成功", targetID)
		}
		// 大小写不同也算不存在。
		if !strings.Contains(envelope.Message, "不存在") {
			t.Fatalf("target_id=%q 提示 = %q", targetID, envelope.Message)
		}
	}
}

func TestSystemUsageStatisticReturnsEmptyObject(t *testing.T) {
	recorder := httptest.NewRecorder()
	systemUsageStatistic(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/system/usage/statistic", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	// 无外网时不编造统计数据：返回空对象而不是 null。
	if body := strings.TrimSpace(recorder.Body.String()); !strings.Contains(body, `"data":{}`) {
		t.Fatalf("响应 = %s", body)
	}
}

type nettestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f nettestRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// TestSystemNettestProbeBranches 用替换 http.DefaultClient 的假 RoundTripper 覆盖
// 真实探测分支：传输错误分档、HTTP 错误码与成功文案，全程不发起外网请求。
func TestSystemNettestProbeBranches(t *testing.T) {
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })

	targetID := nettestTargets()[0].ID
	probe := func() Response {
		t.Helper()
		recorder := httptest.NewRecorder()
		systemNettest(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/system/nettest?target_id="+targetID, nil))
		return decodeBody[Response](t, recorder)
	}

	failures := []struct {
		name    string
		message string
		client  *http.Client
	}{
		{"超时", "连接失败：连接超时", &http.Client{Transport: nettestRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("context deadline exceeded")
		})}},
		{"解析失败", "连接失败：域名解析失败", &http.Client{Transport: nettestRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: lookup api.themoviedb.org: no such host")
		})}},
		{"其它错误", "连接失败：网络不可达", &http.Client{Transport: nettestRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("boom")
		})}},
		{"HTTP 错误码", "连接失败：HTTP 502", &http.Client{Transport: nettestRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			http.DefaultClient = failure.client
			envelope := probe()
			if envelope.Success || envelope.Message != failure.message {
				t.Fatalf("响应 = %+v，期望 %q", envelope, failure.message)
			}
		})
	}

	var userAgent string
	http.DefaultClient = &http.Client{Transport: nettestRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		userAgent = request.Header.Get("User-Agent")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	envelope := probe()
	if !envelope.Success {
		t.Fatalf("成功响应 = %+v", envelope)
	}
	if !strings.HasPrefix(envelope.Message, "连接成功，耗时 ") || !strings.HasSuffix(envelope.Message, " ms") {
		t.Fatalf("成功文案 = %q", envelope.Message)
	}
	if userAgent != "MoviePilot-Go-NetTest/1.0" {
		t.Fatalf("User-Agent = %q", userAgent)
	}
}
