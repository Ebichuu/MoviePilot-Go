package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// 未接数据库/缓存时，监控端点仍应给出进程级观测值，而不是 500 或空白。
func TestMonitorRuntimeDegradesWithoutDependencies(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test"}}
	recorder := httptest.NewRecorder()
	deps.monitorRuntime(recorder, httptest.NewRequest("GET", "/api/v1/monitor/runtime", nil))

	if recorder.Code != 200 {
		t.Fatalf("状态码 = %d，期望 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"goroutines"`, `"heap_alloc"`, `"environment":"test"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应缺少 %s: %s", want, body)
		}
	}
	// 依赖缺失时不应伪造数据库/缓存健康状况。
	if strings.Contains(body, `"database"`) || strings.Contains(body, `"redis"`) {
		t.Fatalf("缺少依赖时不应报告数据库/缓存状态: %s", body)
	}
}
