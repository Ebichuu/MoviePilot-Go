//go:build integration

package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	qbFaultTestEnabledEnv     = "MOVIEPILOT_FAULT_TEST"
	qbFaultProxyURLEnv        = "MOVIEPILOT_TEST_QB_FAULT_PROXY_URL"
	qbFixtureAdminURLEnv      = "MOVIEPILOT_TEST_QB_FIXTURE_ADMIN_URL"
	toxiproxyAPIURLEnv        = "MOVIEPILOT_TEST_TOXIPROXY_API_URL"
	toxiproxyProxyNameEnv     = "MOVIEPILOT_TEST_QB_TOXIPROXY_PROXY"
	qbFixtureUpstreamEnv      = "MOVIEPILOT_TEST_QB_FIXTURE_UPSTREAM"
	qbFaultFixtureName        = "moviepilot-qb-fault-fixture-v1"
	qbFaultFixtureHash        = "0123456789ABCDEF0123456789ABCDEF01234567"
	qbFaultRequiredProxyName  = "qb-response-loss"
	qbFaultToxicName          = "drop-add-response"
	qbFaultProxyListen        = "[::]:58080"
	qbFaultRequestTimeout     = 1500 * time.Millisecond
	qbFaultManagementTimeout  = 3 * time.Second
	qbFaultReceiptTimeout     = 3 * time.Second
	qbFaultAddCompletionLimit = 8 * time.Second
)

type qbFaultEnvironment struct {
	proxyURL        string
	adminURL        string
	toxiproxyURL    string
	proxyName       string
	fixtureUpstream string
	httpClient      *http.Client
}

type qbFixtureIdentity struct {
	Fixture    string `json:"fixture"`
	InstanceID string `json:"instance_id"`
}

type qbFixtureStats struct {
	Fixture        string `json:"fixture"`
	InstanceID     string `json:"instance_id"`
	AddCalls       int    `json:"add_calls"`
	InfoCalls      int    `json:"info_calls"`
	LabelInfoCalls int    `json:"label_info_calls"`
	HashInfoCalls  int    `json:"hash_info_calls"`
	Torrents       []struct {
		Hash string `json:"hash"`
		Tags string `json:"tags"`
	} `json:"torrents"`
}

type toxiproxyProxy struct {
	Name     string `json:"name"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	Enabled  bool   `json:"enabled"`
}

type qbFaultAddResult struct {
	intent Intent
	err    error
}

func TestQBittorrentResponseLossReconcilesByLabel(t *testing.T) {
	env := loadQBFaultEnvironment(t)

	directIdentity := getQBFixtureIdentity(t, env.httpClient, env.adminURL)
	if directIdentity.Fixture != qbFaultFixtureName || directIdentity.InstanceID == "" {
		t.Fatalf("管理端不是专用 qB fixture: %#v", directIdentity)
	}
	resetQBFixture(t, env)
	ownedProxy := createQBFaultProxy(t, env)
	t.Cleanup(func() { deleteOwnedQBFaultProxy(t, env, ownedProxy) })

	proxiedIdentity := getQBFixtureIdentity(t, env.httpClient, env.proxyURL)
	if proxiedIdentity != directIdentity {
		t.Fatalf("代理上游不是已验证的专用 fixture: direct=%#v proxied=%#v", directIdentity, proxiedIdentity)
	}
	assertQBWebAPIVersion(t, env)
	addQBFaultToxic(t, env)

	client, err := NewQBittorrent(QBittorrentConfig{
		URL:             env.proxyURL,
		APIKey:          "fault-fixture-only",
		Timeout:         2 * time.Second,
		MaxConnsPerHost: 1,
	})
	if err != nil {
		t.Fatalf("NewQBittorrent() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	submitter := NewTorrentBytesSubmitter(client)
	candidate := Candidate{Key: "qb-response-loss-fixture", SavePath: "/fixture/downloads"}
	intent, err := NewIntent("qb-fault-integration", candidate)
	if err != nil {
		t.Fatalf("NewIntent() error = %v", err)
	}
	intent, err = intent.Transition(IntentSubmitting)
	if err != nil {
		t.Fatalf("persist submitting permission: %v", err)
	}

	addCtx, cancelAdd := context.WithTimeout(t.Context(), qbFaultRequestTimeout)
	defer cancelAdd()
	started := time.Now()
	addResultCh := make(chan qbFaultAddResult, 1)
	go func() {
		updated, addErr := submitter.AddOnce(addCtx, intent, candidate, []byte("fixture-metainfo"))
		addResultCh <- qbFaultAddResult{intent: updated, err: addErr}
	}()
	waitForQBFixtureAdd(t, env, addResultCh)

	var addResult qbFaultAddResult
	select {
	case addResult = <-addResultCh:
	case <-time.After(qbFaultAddCompletionLimit):
		t.Fatal("AddOnce() 未在显式完成上限内结束")
	}
	unknown, err := addResult.intent, addResult.err
	if !errors.Is(err, ErrReconciliationPending) {
		t.Fatalf("响应丢失后的 AddOnce() error = %v, want ErrReconciliationPending", err)
	}
	if !errors.Is(addCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("AddOnce() 未因显式上下文超时结束: %v", addCtx.Err())
	}
	if elapsed := time.Since(started); elapsed > qbFaultAddCompletionLimit {
		t.Fatalf("AddOnce() 超时未受控: %v", elapsed)
	}
	if unknown.State != IntentSubmitting || unknown.TorrentHash != "" {
		t.Fatalf("未知 Add 结果的意图 = %#v, want submitting 且无 hash", unknown)
	}
	assertQBFixtureAfterUnknownAdd(t, getQBFixtureStats(t, env), unknown.Label)

	deleteQBFaultToxic(t, env, true)
	client.CloseIdleConnections()
	replayCtx, cancelReplay := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelReplay()
	reconciled, err := submitter.Submit(replayCtx, unknown, candidate, []byte("must-not-be-added-again"))
	if err != nil {
		t.Fatalf("同一 submitting 意图重放 error = %v", err)
	}
	if reconciled.State != IntentDownloading || reconciled.TorrentHash != qbFaultFixtureHash {
		t.Fatalf("标签对账后的意图 = %#v, want downloading/%s", reconciled, qbFaultFixtureHash)
	}

	stats := getQBFixtureStats(t, env)
	if stats.AddCalls != 1 {
		t.Fatalf("重放后的 fixture add 次数 = %d, want 1", stats.AddCalls)
	}
	if stats.LabelInfoCalls != 1 || stats.HashInfoCalls != 0 || stats.InfoCalls != 1 {
		t.Fatalf("重放没有只通过标签对账: info=%d label=%d hash=%d", stats.InfoCalls, stats.LabelInfoCalls, stats.HashInfoCalls)
	}
}

func loadQBFaultEnvironment(t *testing.T) qbFaultEnvironment {
	t.Helper()
	if strings.TrimSpace(os.Getenv(qbFaultTestEnabledEnv)) != "1" {
		t.Skipf("%s != 1，跳过专用 qB 故障集成测试", qbFaultTestEnabledEnv)
	}
	values := map[string]string{
		qbFaultProxyURLEnv:    strings.TrimSpace(os.Getenv(qbFaultProxyURLEnv)),
		qbFixtureAdminURLEnv:  strings.TrimSpace(os.Getenv(qbFixtureAdminURLEnv)),
		toxiproxyAPIURLEnv:    strings.TrimSpace(os.Getenv(toxiproxyAPIURLEnv)),
		toxiproxyProxyNameEnv: strings.TrimSpace(os.Getenv(toxiproxyProxyNameEnv)),
		qbFixtureUpstreamEnv:  strings.TrimSpace(os.Getenv(qbFixtureUpstreamEnv)),
	}
	for key, value := range values {
		if value == "" {
			t.Fatalf("%s=1 时必须设置 %s", qbFaultTestEnabledEnv, key)
		}
	}
	ports := make(map[string]string, 3)
	for _, entry := range []struct {
		key            string
		forbiddenPorts []string
	}{
		{key: qbFaultProxyURLEnv, forbiddenPorts: []string{"8080"}},
		{key: qbFixtureAdminURLEnv, forbiddenPorts: []string{"8080", "8081"}},
		{key: toxiproxyAPIURLEnv, forbiddenPorts: []string{"8474"}},
	} {
		ports[entry.key] = validateLoopbackHTTPURL(t, entry.key, values[entry.key], entry.forbiddenPorts...)
	}
	if ports[qbFaultProxyURLEnv] == ports[qbFixtureAdminURLEnv] ||
		ports[qbFaultProxyURLEnv] == ports[toxiproxyAPIURLEnv] ||
		ports[qbFixtureAdminURLEnv] == ports[toxiproxyAPIURLEnv] {
		t.Fatal("qB proxy、fixture admin 与 Toxiproxy API 必须使用不同的专用端口")
	}
	if values[toxiproxyProxyNameEnv] != qbFaultRequiredProxyName {
		t.Fatalf("%s 必须精确为 %q", toxiproxyProxyNameEnv, qbFaultRequiredProxyName)
	}
	if values[qbFixtureUpstreamEnv] == "" || strings.ContainsAny(values[qbFixtureUpstreamEnv], "/?#") {
		t.Fatalf("%s 不是 host:port", qbFixtureUpstreamEnv)
	}

	return qbFaultEnvironment{
		proxyURL:        strings.TrimRight(values[qbFaultProxyURLEnv], "/"),
		adminURL:        strings.TrimRight(values[qbFixtureAdminURLEnv], "/"),
		toxiproxyURL:    strings.TrimRight(values[toxiproxyAPIURLEnv], "/"),
		proxyName:       values[toxiproxyProxyNameEnv],
		fixtureUpstream: values[qbFixtureUpstreamEnv],
		httpClient: &http.Client{
			Transport: &http.Transport{Proxy: nil},
			Timeout:   qbFaultManagementTimeout,
		},
	}
}

func validateLoopbackHTTPURL(t *testing.T, key, value string, forbiddenPorts ...string) string {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Host == "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatalf("%s 必须是无路径的本机 HTTP URL", key)
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("%s 必须使用回环 IP，禁止命中远程或生产服务", key)
	}
	port := parsed.Port()
	if port == "" {
		t.Fatalf("%s 必须显式指定专用端口", key)
	}
	for _, forbidden := range forbiddenPorts {
		if port == forbidden {
			t.Fatalf("%s 禁止使用默认服务端口 %s", key, port)
		}
	}
	return port
}

func getQBFixtureIdentity(t *testing.T, client *http.Client, baseURL string) qbFixtureIdentity {
	t.Helper()
	var identity qbFixtureIdentity
	doJSON(t, client, http.MethodGet, baseURL+"/__fixture/identity", nil, http.StatusOK, &identity)
	return identity
}

func getQBFixtureStats(t *testing.T, env qbFaultEnvironment) qbFixtureStats {
	t.Helper()
	var stats qbFixtureStats
	doJSON(t, env.httpClient, http.MethodGet, env.adminURL+"/__fixture/stats", nil, http.StatusOK, &stats)
	if stats.Fixture != qbFaultFixtureName || stats.InstanceID == "" {
		t.Fatalf("fixture stats 身份无效: %#v", stats)
	}
	return stats
}

func resetQBFixture(t *testing.T, env qbFaultEnvironment) {
	t.Helper()
	doJSON(t, env.httpClient, http.MethodPost, env.adminURL+"/__fixture/reset", nil, http.StatusNoContent, nil)
	stats := getQBFixtureStats(t, env)
	if stats.AddCalls != 0 || stats.InfoCalls != 0 || len(stats.Torrents) != 0 {
		t.Fatalf("fixture reset 后状态非空: %#v", stats)
	}
}

func assertQBWebAPIVersion(t *testing.T, env qbFaultEnvironment) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.proxyURL+"/api/v2/app/webapiVersion", nil)
	if err != nil {
		t.Fatalf("build webapiVersion request: %v", err)
	}
	resp, err := env.httpClient.Do(req)
	if err != nil {
		t.Fatalf("fixture webapiVersion request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		t.Fatalf("read fixture webapiVersion: %v", err)
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) == "" {
		t.Fatalf("fixture webapiVersion = status %d body %q", resp.StatusCode, body)
	}
}

func createQBFaultProxy(t *testing.T, env qbFaultEnvironment) toxiproxyProxy {
	t.Helper()
	var existing map[string]toxiproxyProxy
	doJSON(t, env.httpClient, http.MethodGet, env.toxiproxyURL+"/proxies", nil, http.StatusOK, &existing)
	if len(existing) != 0 {
		t.Fatalf("专用 Toxiproxy 必须以空 proxy 集合启动，当前存在: %#v", existing)
	}
	want := toxiproxyProxy{
		Name: env.proxyName, Listen: qbFaultProxyListen, Upstream: env.fixtureUpstream, Enabled: true,
	}
	var got toxiproxyProxy
	doJSON(t, env.httpClient, http.MethodPost, env.toxiproxyURL+"/proxies", want, http.StatusCreated, &got)
	if got != want {
		t.Fatalf("Toxiproxy proxy 配置不匹配: got=%#v want=%#v", got, want)
	}
	return want
}

func deleteOwnedQBFaultProxy(t *testing.T, env qbFaultEnvironment, owned toxiproxyProxy) {
	t.Helper()
	var current toxiproxyProxy
	doJSON(t, env.httpClient, http.MethodGet,
		env.toxiproxyURL+"/proxies/"+url.PathEscape(owned.Name), nil,
		http.StatusOK, &current)
	if current != owned {
		t.Errorf("拒绝删除配置已漂移的 Toxiproxy proxy: current=%#v owned=%#v", current, owned)
		return
	}
	doRequest(t, env.httpClient, http.MethodDelete,
		env.toxiproxyURL+"/proxies/"+url.PathEscape(owned.Name), nil,
		http.StatusNoContent)
}

func addQBFaultToxic(t *testing.T, env qbFaultEnvironment) {
	t.Helper()
	payload := map[string]any{
		"name": qbFaultToxicName, "type": "timeout", "stream": "downstream", "toxicity": 1,
		"attributes": map[string]int{"timeout": 10_000},
	}
	doJSON(t, env.httpClient, http.MethodPost,
		env.toxiproxyURL+"/proxies/"+url.PathEscape(env.proxyName)+"/toxics",
		payload, http.StatusOK, nil)
}

func deleteQBFaultToxic(t *testing.T, env qbFaultEnvironment, required bool) {
	t.Helper()
	status := doRequest(t, env.httpClient, http.MethodDelete,
		env.toxiproxyURL+"/proxies/"+url.PathEscape(env.proxyName)+"/toxics/"+url.PathEscape(qbFaultToxicName),
		nil, http.StatusNoContent, http.StatusNotFound)
	if required && status == http.StatusNotFound {
		t.Fatalf("待移除的 Toxiproxy toxic %q 不存在", qbFaultToxicName)
	}
}

func assertQBFixtureAfterUnknownAdd(t *testing.T, stats qbFixtureStats, label string) {
	t.Helper()
	if stats.AddCalls != 1 || stats.InfoCalls != 0 || len(stats.Torrents) != 1 {
		t.Fatalf("未知 Add 后 fixture 状态 = %#v, want add=1 info=0 torrents=1", stats)
	}
	if stats.Torrents[0].Tags != label || strings.ToUpper(stats.Torrents[0].Hash) != qbFaultFixtureHash {
		t.Fatalf("fixture torrent = %#v, want label=%q hash=%s", stats.Torrents[0], label, qbFaultFixtureHash)
	}
}

func waitForQBFixtureAdd(t *testing.T, env qbFaultEnvironment, addResultCh <-chan qbFaultAddResult) {
	t.Helper()
	deadline := time.Now().Add(qbFaultReceiptTimeout)
	for {
		stats := getQBFixtureStats(t, env)
		if stats.AddCalls == 1 {
			select {
			case result := <-addResultCh:
				t.Fatalf("fixture 已处理 Add 时调用方不应已收到结果: %v", result.err)
			default:
				return
			}
		}
		if stats.AddCalls > 1 {
			t.Fatalf("首次 Add 在 fixture 被调用 %d 次", stats.AddCalls)
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture 未在 %v 内收到 Add 请求", qbFaultReceiptTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func doJSON(t *testing.T, client *http.Client, method, endpoint string, payload any, wantStatus int, output any) {
	t.Helper()
	body := io.Reader(nil)
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode %s payload: %v", endpoint, err)
		}
		body = bytes.NewReader(encoded)
	}
	status, responseBody := doRequestBody(t, client, method, endpoint, body, wantStatus)
	_ = status
	if output != nil {
		if err := json.Unmarshal(responseBody, output); err != nil {
			t.Fatalf("decode %s response: %v; body=%q", endpoint, err, responseBody)
		}
	}
}

func doRequest(t *testing.T, client *http.Client, method, endpoint string, payload any, wantStatuses ...int) int {
	t.Helper()
	body := io.Reader(nil)
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode %s payload: %v", endpoint, err)
		}
		body = bytes.NewReader(encoded)
	}
	status, _ := doRequestBody(t, client, method, endpoint, body, wantStatuses...)
	return status
}

func doRequestBody(t *testing.T, client *http.Client, method, endpoint string, body io.Reader, wantStatuses ...int) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), qbFaultManagementTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, endpoint, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, endpoint, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, endpoint, err)
	}
	for _, status := range wantStatuses {
		if resp.StatusCode == status {
			return resp.StatusCode, responseBody
		}
	}
	t.Fatalf("%s %s status = %d, want %v; body=%q", method, endpoint, resp.StatusCode, wantStatuses, responseBody)
	return 0, nil
}
