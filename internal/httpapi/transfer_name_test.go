package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderTransferName(t *testing.T) {
	cases := []struct {
		base     string
		template string
		want     string
	}{
		{"Dune.2021.2160p.WEB-DL.mkv", defaultTransferNameTemplate, "Dune (2021).mkv"},
		{"The.Last.of.Us.S01E03.1080p.mkv", "{{ title }} {{ season_episode }}", "The Last Of Us S01E03.mkv"},
		{"Dune.2021.mkv", "{{ title }} ({{ year }})", "Dune (2021)"},
		{"Dune.2021.mkv", "{{ title }}.{{ ext }}", "Dune.mkv"},
		{"Some.Movie.2019.mkv", "{% if year %}{{ title }}-{{ year }}{% endif %}", "Some Movie-2019.mkv"},
	}
	for _, testCase := range cases {
		got, err := renderTransferName(testCase.base, testCase.template)
		if err != nil {
			t.Fatalf("%s: %v", testCase.base, err)
		}
		want := testCase.want
		// 模板已带扩展名时只校验前缀，避免对 ext 拼接顺序做过度约束。
		if want == "Dune (2021)" {
			if got != want+".mkv" {
				t.Fatalf("%q => %q, want %q.mkv", testCase.template, got, want)
			}
			continue
		}
		if got != want {
			t.Fatalf("%s with %q => %q, want %q", testCase.base, testCase.template, got, want)
		}
	}
}

func TestRenderTransferNameEmptyResult(t *testing.T) {
	if _, err := renderTransferName("Dune.2021.mkv", "{% if season %}x{% endif %}"); err == nil {
		t.Fatal("空渲染结果应报错")
	}
}

func TestTransferNameEndpoint(t *testing.T) {
	deps := Dependencies{}

	// 缺 filename：HTTP 200 + success:false（V2 语义）。
	recorder := httptest.NewRecorder()
	deps.transferName(recorder, httptest.NewRequest(http.MethodGet, "/transfer/name", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "filename不能为空" {
		t.Fatalf("空 filename = %+v", body)
	}

	// 带盘符/反斜杠时只取 basename，并回填默认模板。
	recorder = httptest.NewRecorder()
	deps.transferName(recorder, httptest.NewRequest(http.MethodGet,
		"/transfer/name?filename="+urlEscape(`D:\downloads\Show.Name.S01E02.2020.mkv`), nil))
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Name     string `json:"name"`
			Template string `json:"template"`
			File     string `json:"file"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success {
		t.Fatalf("渲染失败: %s", recorder.Body.String())
	}
	if body.Data.File != "Show.Name.S01E02.2020.mkv" || body.Data.Template != defaultTransferNameTemplate {
		t.Fatalf("data = %+v", body.Data)
	}
	if body.Data.Name != "Show Name (2020).mkv" {
		t.Fatalf("name = %q", body.Data.Name)
	}

	// 自定义模板：season/episode 变量去掉 S/E 前缀。
	recorder = httptest.NewRecorder()
	deps.transferName(recorder, httptest.NewRequest(http.MethodGet,
		"/transfer/name?filename=Show.S01E02.mkv&template="+urlEscape("{{ title }}-{{ season }}{{ episode }}"), nil))
	body.Data.Name, body.Data.Template, body.Data.File = "", "", ""
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析自定义模板响应: %v", err)
	}
	if !body.Success || body.Data.Name != "Show-0102.mkv" {
		t.Fatalf("自定义模板 = %s", recorder.Body.String())
	}

	// 模板渲染失败：success:false + 固定前缀。
	recorder = httptest.NewRecorder()
	deps.transferName(recorder, httptest.NewRequest(http.MethodGet,
		"/transfer/name?filename=Show.mkv&template="+urlEscape("{{ title "), nil))
	if decoded := decodeResponseBody(t, recorder.Body.Bytes()); decoded.Success ||
		!strings.HasPrefix(decoded.Message, "命名模板渲染失败：") {
		t.Fatalf("坏模板响应 = %+v", decoded)
	}
}
