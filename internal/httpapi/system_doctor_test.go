package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/doctor"
)

// 体检端点在没有任何探针时也必须返回结构化报告，而不是空体或 500。
func TestSystemDoctorReturnsStructuredReport(t *testing.T) {
	deps := Dependencies{
		Config: config.Config{Environment: "test"},
		Doctor: &doctor.Runner{},
	}
	recorder := httptest.NewRecorder()
	deps.systemDoctor(recorder, httptest.NewRequest("GET", "/api/v1/system/doctor", nil))

	if recorder.Code != 200 {
		t.Fatalf("状态码 = %d，期望 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"checks"`, `"generated_at"`, `"counts"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("体检响应缺少 %s: %s", want, body)
		}
	}
}

// 没有装配 Runner 时应如实报错，而不是假装体检通过。
func TestSystemDoctorWithoutRunnerFails(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test"}}
	recorder := httptest.NewRecorder()
	deps.systemDoctor(recorder, httptest.NewRequest("GET", "/api/v1/system/doctor", nil))
	if recorder.Code == 200 {
		t.Fatalf("未装配体检能力时不应返回 200: %s", recorder.Body.String())
	}
}
