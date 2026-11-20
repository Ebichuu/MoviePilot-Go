package mediaconfig

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizePathMappings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mappings   []PathMapping
		want       []PathMapping
		wantErr    bool
		wantChange bool
	}{
		{
			name:       "POSIX 默认风格和大小写",
			mappings:   []PathMapping{{LocalRoot: " /mnt/media/ ", EmbyRoot: " /library/ "}},
			want:       []PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: "/library", RemoteStyle: PathStylePOSIX, CasePolicy: CasePolicySensitive}},
			wantChange: true,
		},
		{
			name:       "Windows 分隔符和大小写",
			mappings:   []PathMapping{{LocalRoot: `c:/Media/`, EmbyRoot: `d:/Library/`}},
			want:       []PathMapping{{LocalRoot: `C:\Media`, EmbyRoot: `D:\Library`, RemoteStyle: PathStyleWindows, CasePolicy: CasePolicyInsensitive}},
			wantChange: true,
		},
		{
			name:       "显式策略规范化",
			mappings:   []PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: `D:\Library`, RemoteStyle: " WINDOWS ", CasePolicy: " INSENSITIVE "}},
			want:       []PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: `D:\Library`, RemoteStyle: PathStyleWindows, CasePolicy: CasePolicyInsensitive}},
			wantChange: true,
		},
		{
			name:     "空映射归一为空数组",
			mappings: nil, want: []PathMapping{}, wantChange: true,
		},
		{
			name:     "拒绝相对本地根",
			mappings: []PathMapping{{LocalRoot: "media", EmbyRoot: "/library"}}, wantErr: true,
		},
		{
			name:     "拒绝相对 Emby 根",
			mappings: []PathMapping{{LocalRoot: "/media", EmbyRoot: "library"}}, wantErr: true,
		},
		{
			name:     "拒绝重复根",
			mappings: []PathMapping{{LocalRoot: "/media", EmbyRoot: "/a"}, {LocalRoot: "/media/", EmbyRoot: "/b"}}, wantErr: true,
		},
		{
			name:     "拒绝不支持风格",
			mappings: []PathMapping{{LocalRoot: "/media", EmbyRoot: "/library", RemoteStyle: "url"}}, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, changed, err := normalizePathMappings(tt.mappings)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidMediaServers) {
					t.Fatalf("normalizePathMappings() error = %v, want ErrInvalidMediaServers", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePathMappings() error = %v", err)
			}
			if changed != tt.wantChange {
				t.Fatalf("normalizePathMappings() changed = %v, want %v", changed, tt.wantChange)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizePathMappings() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMapPathUsesLongestBoundarySafePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mappings   []PathMapping
		localPath  string
		wantRemote string
		wantOK     bool
		wantErr    bool
	}{
		{
			name: "最长前缀",
			mappings: []PathMapping{
				{LocalRoot: "/mnt/media", EmbyRoot: "/library"},
				{LocalRoot: "/mnt/media/movies", EmbyRoot: `Z:\Movies`, RemoteStyle: PathStyleWindows, CasePolicy: CasePolicyInsensitive},
			},
			localPath: "/mnt/media/movies/Film.mkv", wantRemote: `Z:\Movies\Film.mkv`, wantOK: true,
		},
		{
			name:       "边界不误匹配",
			mappings:   []PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: "/library"}},
			localPath:  "/mnt/media2/Film.mkv",
			wantRemote: "", wantOK: false,
		},
		{
			name:       "根目录精确匹配",
			mappings:   []PathMapping{{LocalRoot: "/", EmbyRoot: "/remote"}},
			localPath:  "/Film.mkv",
			wantRemote: "/remote/Film.mkv", wantOK: true,
		},
		{
			name:      "显式大小写不敏感",
			mappings:  []PathMapping{{LocalRoot: `C:\Media`, EmbyRoot: "/library", CasePolicy: CasePolicyInsensitive}},
			localPath: `c:\media\Film.mkv`, wantRemote: "/library/Film.mkv", wantOK: true,
		},
		{
			name:      "大小写敏感不匹配",
			mappings:  []PathMapping{{LocalRoot: "/Media", EmbyRoot: "/library", CasePolicy: CasePolicySensitive}},
			localPath: "/media/Film.mkv", wantRemote: "", wantOK: false,
		},
		{
			name:      "输入点段先规范化",
			mappings:  []PathMapping{{LocalRoot: "/mnt/media/movies", EmbyRoot: "/library"}},
			localPath: "/mnt/media/series/../movies/Film.mkv", wantRemote: "/library/Film.mkv", wantOK: true,
		},
		{
			name:      "UNC 根",
			mappings:  []PathMapping{{LocalRoot: `\\server\media`, EmbyRoot: `\\emby\library`, RemoteStyle: PathStyleWindows}},
			localPath: `\\SERVER\MEDIA\Film.mkv`, wantRemote: `\\emby\library\Film.mkv`, wantOK: true,
		},
		{
			name:      "拒绝相对输入",
			mappings:  []PathMapping{{LocalRoot: "/mnt/media", EmbyRoot: "/library"}},
			localPath: "media/Film.mkv", wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := MapPath(tt.mappings, tt.localPath)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidMediaPath) {
					t.Fatalf("MapPath() error = %v, want ErrInvalidMediaPath", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MapPath() error = %v", err)
			}
			if ok != tt.wantOK || got != tt.wantRemote {
				t.Fatalf("MapPath() = (%q, %v), want (%q, %v)", got, ok, tt.wantRemote, tt.wantOK)
			}
		})
	}
}

func TestResolvePathReturnsMatchedPolicy(t *testing.T) {
	t.Parallel()

	mappings := []PathMapping{
		{LocalRoot: "/mnt/media", EmbyRoot: "/library"},
		{
			LocalRoot: "/mnt/media/movies", EmbyRoot: `Z:\Movies`,
			RemoteStyle: PathStyleWindows, CasePolicy: CasePolicyInsensitive,
		},
	}
	result, ok, err := ResolvePath(mappings, "/mnt/media/movies/Film.mkv")
	if err != nil {
		t.Fatalf("ResolvePath() error = %v", err)
	}
	want := MapPathResult{
		Path: `Z:\Movies\Film.mkv`, Style: PathStyleWindows, CasePolicy: CasePolicyInsensitive,
	}
	if !ok || result != want {
		t.Fatalf("ResolvePath() = (%#v, %v), want (%#v, true)", result, ok, want)
	}

	path, pathOK, err := MapPath(mappings, "/mnt/media/movies/Film.mkv")
	if err != nil || pathOK != ok || path != result.Path {
		t.Fatalf("MapPath() 与 ResolvePath() 不一致: path=%q ok=%v err=%v result=%#v", path, pathOK, err, result)
	}
}
