package downloader

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeXMLRPCResponseScalars(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		check   func(t *testing.T, value xmlrpcValue)
	}{
		{
			name:    "string",
			payload: `<methodResponse><params><param><value><string>ok</string></value></param></params></methodResponse>`,
			check: func(t *testing.T, value xmlrpcValue) {
				if value.String == nil || *value.String != "ok" {
					t.Fatalf("string 解析错误: %+v", value)
				}
			},
		},
		{
			name:    "int",
			payload: `<methodResponse><params><param><value><i4>42</i4></value></param></params></methodResponse>`,
			check: func(t *testing.T, value xmlrpcValue) {
				if value.Int == nil || *value.Int != 42 {
					t.Fatalf("i4 解析错误: %+v", value)
				}
			},
		},
		{
			name:    "long int",
			payload: `<methodResponse><params><param><value><int>9001</int></value></param></params></methodResponse>`,
			check: func(t *testing.T, value xmlrpcValue) {
				if value.Int == nil || *value.Int != 9001 {
					t.Fatalf("int 解析错误: %+v", value)
				}
			},
		},
		{
			name:    "double",
			payload: `<methodResponse><params><param><value><double>1.5</double></value></param></params></methodResponse>`,
			check: func(t *testing.T, value xmlrpcValue) {
				if value.Double == nil || *value.Double != 1.5 {
					t.Fatalf("double 解析错误: %+v", value)
				}
			},
		},
		{
			name:    "boolean true",
			payload: `<methodResponse><params><param><value><boolean>1</boolean></value></param></params></methodResponse>`,
			check: func(t *testing.T, value xmlrpcValue) {
				if value.Bool == nil || !*value.Bool {
					t.Fatalf("boolean 解析错误: %+v", value)
				}
			},
		},
		{
			name:    "base64",
			payload: `<methodResponse><params><param><value><base64>aGVsbG8=</base64></value></param></params></methodResponse>`,
			check: func(t *testing.T, value xmlrpcValue) {
				if value.String == nil || *value.String != "aGVsbG8=" {
					t.Fatalf("base64 应保留原文由调用方解码: %+v", value)
				}
			},
		},
	}
	for _, testCase := range cases {
		value, err := decodeXMLRPCResponse([]byte(testCase.payload))
		if err != nil {
			t.Fatalf("%s 解析失败: %v", testCase.name, err)
		}
		testCase.check(t, value)
	}
}

func TestDecodeXMLRPCResponseContainers(t *testing.T) {
	arrayPayload := `<methodResponse><params><param><value><array><data>` +
		`<value><string>a</string></value><value><string>b</string></value>` +
		`</data></array></value></param></params></methodResponse>`
	value, err := decodeXMLRPCResponse([]byte(arrayPayload))
	if err != nil {
		t.Fatalf("array 解析失败: %v", err)
	}
	if len(value.Array) != 2 {
		t.Fatalf("array 长度 = %d", len(value.Array))
	}
	if value.Array[1].String == nil || *value.Array[1].String != "b" {
		t.Fatalf("array 元素解析错误: %+v", value.Array)
	}

	structPayload := `<methodResponse><params><param><value><struct>` +
		`<member><name>hash</name><value><string>abc</string></value></member>` +
		`<member><name>done</name><value><boolean>0</boolean></value></member>` +
		`<member><name>size</name><value><i4>7</i4></value></member>` +
		`</struct></value></param></params></methodResponse>`
	value, err = decodeXMLRPCResponse([]byte(structPayload))
	if err != nil {
		t.Fatalf("struct 解析失败: %v", err)
	}
	if len(value.Struct) != 3 {
		t.Fatalf("struct 成员数 = %d: %+v", len(value.Struct), value.Struct)
	}
	if member := value.Struct["hash"]; member.String == nil || *member.String != "abc" {
		t.Fatalf("struct 字符串成员错误: %+v", member)
	}
	if member := value.Struct["done"]; member.Bool == nil || *member.Bool {
		t.Fatalf("struct 布尔成员错误: %+v", member)
	}
	if member := value.Struct["size"]; member.Int == nil || *member.Int != 7 {
		t.Fatalf("struct 整数成员错误: %+v", member)
	}
}

func TestDecodeXMLRPCResponseErrors(t *testing.T) {
	if _, err := decodeXMLRPCResponse([]byte(`<methodResponse><fault><value><string>boom</string></value></fault></methodResponse>`)); err == nil {
		t.Fatal("fault 必须转成错误，否则会把失败当成功")
	}
	if _, err := decodeXMLRPCResponse([]byte(`not-xml`)); err == nil {
		t.Fatal("非法 XML 必须报错")
	}
	// 没有参数的响应是合法的（rTorrent 的部分方法返回空）。
	value, err := decodeXMLRPCResponse([]byte(`<methodResponse><params></params></methodResponse>`))
	if err != nil {
		t.Fatalf("空参数不应报错: %v", err)
	}
	if value.String != nil || value.Int != nil || len(value.Array) != 0 || len(value.Struct) != 0 {
		t.Fatalf("空参数应返回零值: %+v", value)
	}
}

func TestXMLRPCCallSendsEscapedRequest(t *testing.T) {
	var (
		gotMethod string
		gotParams []string
		gotAuth   string
		gotType   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		text := string(body)
		start := strings.Index(text, "<methodName>") + len("<methodName>")
		end := strings.Index(text, "</methodName>")
		gotMethod = text[start:end]
		for _, chunk := range strings.Split(text, "<param>") {
			if index := strings.Index(chunk, "<value><string>"); index >= 0 {
				rest := chunk[index+len("<value><string>"):]
				if stop := strings.Index(rest, "</string>"); stop >= 0 {
					gotParams = append(gotParams, rest[:stop])
				}
			}
		}
		_, _ = w.Write([]byte(`<methodResponse><params><param><value><string>done</string></value></param></params></methodResponse>`))
	}))
	defer server.Close()

	value, err := xmlrpcCall(context.Background(), server.Client(), server.URL, "d.load.raw",
		[]string{`a<b&c>"d'`, "second"}, "user", "pass")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if value.String == nil || *value.String != "done" {
		t.Fatalf("返回值错误: %+v", value)
	}
	if gotMethod != "d.load.raw" {
		t.Fatalf("methodName = %q", gotMethod)
	}
	if gotType != "text/xml" {
		t.Fatalf("Content-Type = %q", gotType)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("应带 Basic 认证，实际 %q", gotAuth)
	}
	// 请求体里必须是转义后的文本：特殊字符不能破坏 XML 结构。
	if len(gotParams) != 2 {
		t.Fatalf("参数个数 = %d: %v", len(gotParams), gotParams)
	}
	if gotParams[0] != "a&lt;b&amp;c&gt;&quot;d&apos;" {
		t.Fatalf("参数未正确转义: %q", gotParams[0])
	}
	if gotParams[1] != "second" {
		t.Fatalf("第二个参数 = %q", gotParams[1])
	}
}

func TestXMLRPCCallFailurePaths(t *testing.T) {
	client := &http.Client{}
	cases := map[string]struct {
		ctx      context.Context
		client   *http.Client
		endpoint string
		want     string
	}{
		"nil 上下文": {nil, client, "http://example.com", "上下文无效"},
		"nil 客户端": {context.Background(), nil, "http://example.com", "未初始化"},
		"空端点":     {context.Background(), client, "   ", "未初始化"},
	}
	for name, testCase := range cases {
		if _, err := xmlrpcCall(testCase.ctx, testCase.client, testCase.endpoint, "m", nil, "", ""); err == nil ||
			!strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s 错误 = %v，期望包含 %q", name, err, testCase.want)
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := xmlrpcCall(cancelled, client, "http://example.com", "m", nil, "", ""); err == nil {
		t.Fatal("已取消的上下文应直接返回错误")
	}

	// 非 2xx 与 fault 响应都必须变成错误，而不是空值。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	if _, err := xmlrpcCall(context.Background(), bad.Client(), bad.URL, "m", nil, "", ""); err == nil {
		t.Fatal("非 2xx 应报错")
	}

	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<methodResponse><fault><value><string>err</string></value></fault></methodResponse>`))
	}))
	defer fault.Close()
	if _, err := xmlrpcCall(context.Background(), fault.Client(), fault.URL, "m", nil, "", ""); err == nil {
		t.Fatal("fault 响应应报错")
	}

	// 没有凭据时不应发送 Authorization 头。
	var authHeader string
	noAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`<methodResponse><params></params></methodResponse>`))
	}))
	defer noAuth.Close()
	if _, err := xmlrpcCall(context.Background(), noAuth.Client(), noAuth.URL, "m", nil, "", ""); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if authHeader != "" {
		t.Fatalf("无凭据时不应带 Authorization，实际 %q", authHeader)
	}
}

func TestXMLRPCEncodingHelpers(t *testing.T) {
	if got := xmlrpcString("x&y"); got != "<value><string>x&amp;y</string></value>" {
		t.Fatalf("xmlrpcString = %q", got)
	}
	escaped := escapeXML(`<>&"'`)
	if escaped != "&lt;&gt;&amp;&quot;&apos;" {
		t.Fatalf("escapeXML = %q", escaped)
	}
	if escapeXML("") != "" {
		t.Fatal("空串应保持空")
	}

	payload := []byte{0x00, 0x01, 0xFF}
	encoded := xmlrpcEncodeBase64(payload)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	if string(decoded) != string(payload) {
		t.Fatalf("base64 往返不一致: %x", decoded)
	}
}
