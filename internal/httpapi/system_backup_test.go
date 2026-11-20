package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// TestSystemBackupRequiresDatabase 验证没有数据库时导出给出明确错误而不是 panic。
func TestSystemBackupRequiresDatabase(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.systemBackup(recorder, httptest.NewRequest(http.MethodGet, "/system/backup", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无数据库导出 = %d（%s）", recorder.Code, recorder.Body.String())
	}
}

// TestSystemRestoreRequiresConfirm 验证恢复必须显式确认，防止误触覆盖数据库。
func TestSystemRestoreRequiresConfirm(t *testing.T) {
	deps := Dependencies{DB: &database.DB{}}
	recorder := httptest.NewRecorder()
	deps.systemRestore(recorder, httptest.NewRequest(http.MethodPost, "/system/restore", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("缺少确认时恢复 = %d（%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "confirm=true") {
		t.Fatalf("提示文案 = %s", recorder.Body.String())
	}
}

// TestSystemRestoreRejectsOversizeUpload 验证超大包被拒。
func TestSystemRestoreRejectsOversizeUpload(t *testing.T) {
	deps := Dependencies{DB: &database.DB{}}
	request := httptest.NewRequest(http.MethodPost, "/system/restore?confirm=true", nil)
	request.ContentLength = maxRestoreUpload + 1
	recorder := httptest.NewRecorder()
	deps.systemRestore(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大包恢复 = %d（%s）", recorder.Code, recorder.Body.String())
	}
}

// TestIsTruthyQuery 覆盖确认参数的写法。
func TestIsTruthyQuery(t *testing.T) {
	cases := map[string]bool{
		"?confirm=true": true, "?confirm=1": true, "?confirm=yes": true, "?confirm=on": true,
		"?confirm=false": false, "?confirm=": false, "": false,
	}
	for rawQuery, want := range cases {
		request := httptest.NewRequest(http.MethodPost, "/system/restore"+rawQuery, nil)
		if got := isTruthyQuery(request, "confirm"); got != want {
			t.Fatalf("isTruthyQuery(%q) = %v, want %v", rawQuery, got, want)
		}
	}
}
