package mediaconfig

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	mediaConfigPrimaryID   = "11111111-1111-4111-8111-111111111111"
	mediaConfigSecondaryID = "22222222-2222-4222-8222-222222222222"
	mediaConfigAPIKey      = "apikey-secret-value"
	mediaConfigControlText = "value\r\ncontinuation"
)

func TestDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     json.RawMessage
		wantCount int
		wantHadID bool
		wantErr   bool
	}{
		{name: "空数组", value: json.RawMessage(`[]`)},
		{
			name:      "保留是否提供 ID",
			value:     json.RawMessage(`[{"id":"` + mediaConfigPrimaryID + `","name":"Emby","type":"emby","enabled":false,"sync_interval":0,"sync_libraries":[],"path_mapping":[],"config":{"host":"","play_host":"","username":"","apikey":""}}]`),
			wantCount: 1, wantHadID: true,
		},
		{name: "拒绝对象", value: json.RawMessage(`{}`), wantErr: true},
		{name: "拒绝 null", value: json.RawMessage(`null`), wantErr: true},
		{name: "拒绝尾随值", value: json.RawMessage(`[] []`), wantErr: true},
		{name: "拒绝未知字段", value: json.RawMessage(`[{"name":"Emby","type":"emby","unexpected":true}]`), wantErr: true},
		{name: "拒绝错误字段类型", value: json.RawMessage(`[{"name":"Emby","type":"emby","config":{"apikey":123}}]`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			servers, err := Decode(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidMediaServers) {
					t.Fatalf("Decode() error = %v, want ErrInvalidMediaServers", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if len(servers) != tt.wantCount {
				t.Fatalf("Decode() len = %d, want %d", len(servers), tt.wantCount)
			}
			if len(servers) > 0 && servers[0].hadID != tt.wantHadID {
				t.Fatalf("Decode() hadID = %v, want %v", servers[0].hadID, tt.wantHadID)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	legacy := []Server{
		{
			Name: " Disabled ", Type: " EMBY ",
			SyncLibraries: nil, PathMapping: nil,
			Config: Config{Host: " HTTPS://Media.Example/emby/ ", PlayHost: " https://Play.Example/ ", Username: " admin "},
		},
		{
			ID: strings.ToUpper(mediaConfigSecondaryID), Name: " Primary ", Type: "EmBy", Enabled: true,
			SyncInterval: 15, SyncLibraries: []string{" movies ", "", "movies", "series"},
			PathMapping: []PathMapping{{LocalRoot: " /mnt/media/ ", EmbyRoot: " /library/ "}},
			Config:      Config{Host: " https://Emby.Example/emby/ ", APIKey: " " + mediaConfigAPIKey + " "},
		},
	}

	snapshot, changed, err := Normalize(legacy)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if !changed {
		t.Fatal("Normalize() changed = false")
	}
	if len(snapshot.Servers) != 2 || snapshot.RequiredID != mediaConfigSecondaryID {
		t.Fatalf("Normalize() snapshot = %#v", snapshot)
	}
	if _, err := uuid.Parse(snapshot.Servers[0].ID); err != nil {
		t.Fatalf("legacy ID = %q: %v", snapshot.Servers[0].ID, err)
	}
	first := snapshot.Servers[0]
	if first.Name != "Disabled" || first.Type != "emby" || first.Config.Host != "https://media.example/emby" ||
		first.Config.PlayHost != "https://play.example" || first.Config.Username != "admin" ||
		first.SyncLibraries == nil || first.PathMapping == nil {
		t.Fatalf("首个服务器规范化错误: %#v", first)
	}
	second := snapshot.Servers[1]
	if second.ID != mediaConfigSecondaryID || second.Name != "Primary" || second.Config.APIKey != mediaConfigAPIKey ||
		!reflect.DeepEqual(second.SyncLibraries, []string{"movies", "series"}) {
		t.Fatalf("第二个服务器规范化错误: %#v", second)
	}
	if second.PathMapping[0] != (PathMapping{
		LocalRoot: "/mnt/media", EmbyRoot: "/library", RemoteStyle: PathStylePOSIX, CasePolicy: CasePolicySensitive,
	}) {
		t.Fatalf("路径映射规范化错误: %#v", second.PathMapping[0])
	}

	if legacy[0].ID != "" || legacy[0].Name != " Disabled " || legacy[0].SyncLibraries != nil || legacy[0].PathMapping != nil {
		t.Fatalf("Normalize() 修改了调用方输入: %#v", legacy[0])
	}

	stable, stableChanged, err := Normalize(snapshot.Servers)
	if err != nil {
		t.Fatalf("Normalize(normalized) error = %v", err)
	}
	if stableChanged || !reflect.DeepEqual(stable, snapshot) {
		t.Fatalf("二次规范化不稳定: changed=%v got=%#v want=%#v", stableChanged, stable, snapshot)
	}
}

func TestNormalizeNameLengthBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "ASCII 255 字节", value: strings.Repeat("a", 255)},
		{name: "ASCII 256 字节", value: strings.Repeat("a", 256), wantErr: true},
		{name: "中文多字节组合 255 字节", value: strings.Repeat("名", 84) + "abc"},
		{name: "中文多字节组合 256 字节", value: strings.Repeat("名", 84) + "abcd", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := Server{ID: mediaConfigPrimaryID, Name: tt.value, Type: "emby"}
			snapshot, _, err := Normalize([]Server{server})
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidMediaServers) {
					t.Fatalf("Normalize() error = %v, want ErrInvalidMediaServers", err)
				}
				if validateErr := Validate([]Server{server}); !errors.Is(validateErr, ErrInvalidMediaServers) {
					t.Fatalf("Validate() error = %v, want ErrInvalidMediaServers", validateErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if got := snapshot.Servers[0].Name; got != tt.value {
				t.Fatalf("Normalize() name = %q, want %q", got, tt.value)
			}
			if err := Validate([]Server{server}); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestNormalizeAPIKeyLengthBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "ASCII 4096 字节", value: strings.Repeat("a", 4096)},
		{name: "ASCII 4097 字节", value: strings.Repeat("a", 4097), wantErr: true},
		{name: "中文多字节组合 4096 字节", value: strings.Repeat("密", 1365) + "a"},
		{name: "中文多字节组合 4097 字节", value: strings.Repeat("密", 1365) + "ab", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := Server{
				ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby",
				Config: Config{APIKey: tt.value},
			}
			snapshot, _, err := Normalize([]Server{server})
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidMediaServers) {
					t.Fatalf("Normalize() error = %v, want ErrInvalidMediaServers", err)
				}
				if strings.Contains(err.Error(), tt.value) {
					t.Fatal("Normalize() 错误信息泄漏 API Key")
				}
				validateErr := Validate([]Server{server})
				if !errors.Is(validateErr, ErrInvalidMediaServers) {
					t.Fatalf("Validate() error = %v, want ErrInvalidMediaServers", validateErr)
				}
				if strings.Contains(validateErr.Error(), tt.value) {
					t.Fatal("Validate() 错误信息泄漏 API Key")
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if got := snapshot.Servers[0].Config.APIKey; got != tt.value {
				t.Fatalf("Normalize() API Key 长度 = %d, want %d", len(got), len(tt.value))
			}
			if err := Validate([]Server{server}); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestNormalizeRequiredID(t *testing.T) {
	t.Parallel()

	server := func(id, name string, enabled bool) Server {
		return Server{ID: id, Name: name, Type: "emby", Enabled: enabled}
	}
	tests := []struct {
		name string
		in   []Server
		want string
	}{
		{
			name: "配置顺序中首个已启用 Emby",
			in: []Server{
				server(mediaConfigPrimaryID, "Disabled", false),
				server(mediaConfigSecondaryID, "First", true),
				server("33333333-3333-4333-8333-333333333333", "Second", true),
			},
			want: mediaConfigSecondaryID,
		},
		{
			name: "没有已启用服务器",
			in:   []Server{server(mediaConfigPrimaryID, "Disabled", false)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot, _, err := Normalize(tt.in)
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if snapshot.RequiredID != tt.want {
				t.Fatalf("RequiredID = %q, want %q", snapshot.RequiredID, tt.want)
			}
		})
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "保留 Emby 子路径", value: " HTTPS://Emby.Example/emby/ ", want: "https://emby.example/emby"},
		{name: "保留百分号编码子路径", value: "https://Emby.Example/emby%20server/", want: "https://emby.example/emby%20server"},
		{name: "规范化根地址", value: "https://Emby.Example/", want: "https://emby.example"},
		{name: "拒绝 userinfo", value: "https://user:password@emby.example/emby", wantErr: true},
		{name: "拒绝查询参数", value: "https://emby.example/emby?api_key=secret", wantErr: true},
		{name: "拒绝片段", value: "https://emby.example/emby#secret", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := normalizeEndpoint(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidMediaServers) {
					t.Fatalf("normalizeEndpoint() error = %v, want ErrInvalidMediaServers", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeEndpoint() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeRejectsInvalidSettings(t *testing.T) {
	t.Parallel()

	valid := func() Server {
		return Server{
			ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby",
			SyncLibraries: []string{}, PathMapping: []PathMapping{}, Config: Config{},
		}
	}
	tests := []struct {
		name   string
		mutate func(*[]Server)
	}{
		{name: "非法 ID", mutate: func(servers *[]Server) { (*servers)[0].ID = "not-a-uuid" }},
		{name: "重复 ID", mutate: func(servers *[]Server) { *servers = append(*servers, valid()); (*servers)[1].Name = "Other" }},
		{name: "空名称", mutate: func(servers *[]Server) { (*servers)[0].Name = " " }},
		{name: "名称忽略大小写重复", mutate: func(servers *[]Server) {
			other := valid()
			other.ID = mediaConfigSecondaryID
			other.Name = " emBY "
			*servers = append(*servers, other)
		}},
		{name: "不支持的类型", mutate: func(servers *[]Server) { (*servers)[0].Type = "plex" }},
		{name: "负同步间隔", mutate: func(servers *[]Server) { (*servers)[0].SyncInterval = -1 }},
		{name: "host 含 userinfo", mutate: func(servers *[]Server) { (*servers)[0].Config.Host = "https://user:pass@emby.example/emby" }},
		{name: "host 含查询参数", mutate: func(servers *[]Server) { (*servers)[0].Config.Host = "https://emby.example/emby?api_key=hidden" }},
		{name: "play_host 含片段", mutate: func(servers *[]Server) { (*servers)[0].Config.PlayHost = "https://play.example/#hidden" }},
		{name: "API Key 含控制字符", mutate: func(servers *[]Server) { (*servers)[0].Config.APIKey = mediaConfigControlText }},
		{name: "相对路径映射", mutate: func(servers *[]Server) {
			(*servers)[0].PathMapping = []PathMapping{{LocalRoot: "media", EmbyRoot: "/library"}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			servers := []Server{valid()}
			tt.mutate(&servers)
			if _, _, err := Normalize(servers); !errors.Is(err, ErrInvalidMediaServers) {
				t.Fatalf("Normalize() error = %v, want ErrInvalidMediaServers", err)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  Server
		wantErr bool
	}{
		{
			name: "已启用且完整",
			server: Server{ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby", Enabled: true,
				Config: Config{Host: "https://emby.example/emby", APIKey: mediaConfigAPIKey}},
		},
		{
			name:   "禁用时允许连接信息为空",
			server: Server{ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby"},
		},
		{
			name: "启用时缺少 host",
			server: Server{ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby", Enabled: true,
				Config: Config{APIKey: mediaConfigAPIKey}},
			wantErr: true,
		},
		{
			name: "启用时缺少 apikey",
			server: Server{ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby", Enabled: true,
				Config: Config{Host: "https://emby.example/emby"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate([]Server{tt.server})
			if tt.wantErr && !errors.Is(err, ErrInvalidMediaServers) {
				t.Fatalf("Validate() error = %v, want ErrInvalidMediaServers", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestMergeSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		existingJSON string
		incomingJSON string
		wantID       string
		wantAPIKey   string
	}{
		{
			name:         "按 ID 保留空 API Key",
			existingJSON: mediaServerJSON(mediaConfigPrimaryID, "Emby", mediaConfigAPIKey),
			incomingJSON: mediaServerJSON(mediaConfigPrimaryID, "Emby", ""),
			wantID:       mediaConfigPrimaryID, wantAPIKey: mediaConfigAPIKey,
		},
		{
			name:         "旧请求无 ID 时按名称保留稳定 ID",
			existingJSON: mediaServerJSON(mediaConfigPrimaryID, "Emby", mediaConfigAPIKey),
			incomingJSON: mediaServerJSON("", " emby ", ""),
			wantID:       mediaConfigPrimaryID, wantAPIKey: mediaConfigAPIKey,
		},
		{
			name:         "非空 API Key 替换旧值",
			existingJSON: mediaServerJSON(mediaConfigPrimaryID, "Emby", mediaConfigAPIKey),
			incomingJSON: mediaServerJSON(mediaConfigPrimaryID, "Emby", "new-key"),
			wantID:       mediaConfigPrimaryID, wantAPIKey: "new-key",
		},
		{
			name:         "新服务器空 API Key 保持为空",
			existingJSON: `[]`,
			incomingJSON: mediaServerJSON("", "New", ""),
			wantAPIKey:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			existing := mustDecode(t, tt.existingJSON)
			incoming := mustDecode(t, tt.incomingJSON)
			merged, err := MergeSecrets(incoming, existing)
			if err != nil {
				t.Fatalf("MergeSecrets() error = %v", err)
			}
			if len(merged) != 1 {
				t.Fatalf("MergeSecrets() len = %d, want 1", len(merged))
			}
			if tt.wantID != "" && merged[0].ID != tt.wantID {
				t.Fatalf("MergeSecrets() ID = %q, want %q", merged[0].ID, tt.wantID)
			}
			if merged[0].Config.APIKey != tt.wantAPIKey {
				t.Fatalf("MergeSecrets() APIKey = %q, want %q", merged[0].Config.APIKey, tt.wantAPIKey)
			}
			merged[0].SyncLibraries = append(merged[0].SyncLibraries, "mutated")
			if len(incoming[0].SyncLibraries) != 0 {
				t.Fatal("MergeSecrets() 与 incoming 共享切片")
			}
		})
	}
}

func TestRedactSecretsReturnsIndependentSnapshot(t *testing.T) {
	t.Parallel()

	original := Snapshot{
		RequiredID: mediaConfigPrimaryID,
		Servers: []Server{{
			ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby",
			SyncLibraries: []string{"movies"},
			PathMapping:   []PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: "/library"}},
			Config:        Config{Host: "https://emby.example/emby", APIKey: mediaConfigAPIKey},
		}},
	}
	redacted := RedactSecrets(original)
	if redacted.RequiredID != original.RequiredID || redacted.Servers[0].Config.APIKey != "" {
		t.Fatalf("RedactSecrets() = %#v", redacted)
	}
	if original.Servers[0].Config.APIKey != mediaConfigAPIKey {
		t.Fatal("RedactSecrets() 修改了原始 API Key")
	}
	redacted.Servers[0].SyncLibraries[0] = "changed"
	redacted.Servers[0].PathMapping[0].LocalRoot = "/changed"
	if original.Servers[0].SyncLibraries[0] != "movies" || original.Servers[0].PathMapping[0].LocalRoot != "/mnt/media" {
		t.Fatal("RedactSecrets() 返回值与原快照共享切片")
	}
}

func TestErrorsDoNotLeakAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "地址查询参数",
			run: func() error {
				_, _, err := Normalize([]Server{{
					ID: mediaConfigPrimaryID, Name: "Emby", Type: "emby",
					Config: Config{Host: "https://emby.example/emby?api_key=" + mediaConfigAPIKey},
				}})
				return err
			},
		},
		{
			name: "错误 JSON 字段类型",
			run: func() error {
				_, err := Decode(json.RawMessage(`[{"name":"Emby","type":"emby","config":{"apikey":{"value":"` + mediaConfigAPIKey + `"}}}]`))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.run()
			if err == nil {
				t.Fatal("error = nil")
			}
			if strings.Contains(err.Error(), mediaConfigAPIKey) {
				t.Fatalf("错误信息泄漏 API Key: %v", err)
			}
		})
	}
}

func mustDecode(t *testing.T, value string) []Server {
	t.Helper()
	servers, err := Decode(json.RawMessage(value))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	return servers
}

func mediaServerJSON(id, name, apiKey string) string {
	idField := ""
	if id != "" {
		idField = `"id":"` + id + `",`
	}
	return `[{` + idField + `"name":"` + name + `","type":"emby","enabled":false,"sync_interval":0,"sync_libraries":[],"path_mapping":[],"config":{"host":"","play_host":"","username":"","apikey":"` + apiKey + `"}}]`
}
