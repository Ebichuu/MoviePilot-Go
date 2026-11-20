package torrentmeta

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zeebo/bencode"
)

func TestInspectHashesOriginalInfoDictionary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		info       map[string]any
		wantV1     bool
		wantV2     bool
		wantHybrid bool
	}{
		{
			name: "v1 单文件",
			info: map[string]any{
				"length": int64(5), "name": "movie.mkv", "piece length": int64(4),
				"pieces": strings.Repeat("a", 2*sha1.Size),
			},
			wantV1: true,
		},
		{
			name: "v2 单文件",
			info: map[string]any{
				"file tree": map[string]any{
					"movie.mkv": map[string]any{"": map[string]any{
						"length": int64(1), "pieces root": strings.Repeat("b", sha256.Size),
					}},
				},
				"meta version": int64(2), "name": "movie", "piece length": int64(16 << 10),
			},
			wantV2: true,
		},
		{
			name: "hybrid 单文件",
			info: map[string]any{
				"file tree": map[string]any{
					"movie.mkv": map[string]any{"": map[string]any{
						"length": int64(1), "pieces root": strings.Repeat("c", sha256.Size),
					}},
				},
				"length": int64(1), "meta version": int64(2), "name": "movie.mkv",
				"piece length": int64(16 << 10), "pieces": strings.Repeat("d", sha1.Size),
			},
			wantV1: true, wantV2: true, wantHybrid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := encodeMetainfo(t, tt.info)
			infoBytes := encodeBencode(t, tt.info)
			got, err := Inspect(body)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if got.Hybrid != tt.wantHybrid {
				t.Fatalf("Hybrid = %v, want %v", got.Hybrid, tt.wantHybrid)
			}
			if tt.wantV1 {
				digest := sha1.Sum(infoBytes)
				want := strings.ToUpper(hex.EncodeToString(digest[:]))
				if got.InfoHashV1 != want {
					t.Fatalf("InfoHashV1 = %q, want %q", got.InfoHashV1, want)
				}
			} else if got.InfoHashV1 != "" {
				t.Fatalf("unexpected v1 hash %q", got.InfoHashV1)
			}
			if tt.wantV2 {
				digest := sha256.Sum256(infoBytes)
				want := strings.ToUpper(hex.EncodeToString(digest[:]))
				if got.InfoHashV2 != want {
					t.Fatalf("InfoHashV2 = %q, want %q", got.InfoHashV2, want)
				}
			} else if got.InfoHashV2 != "" {
				t.Fatalf("unexpected v2 hash %q", got.InfoHashV2)
			}
			wantPrimary := got.InfoHashV2
			if tt.wantV1 {
				wantPrimary = got.InfoHashV1
			}
			if got.PrimaryHash != wantPrimary {
				t.Fatalf("PrimaryHash = %q, want %q", got.PrimaryHash, wantPrimary)
			}
		})
	}
}

func TestInspectProjectsCanonicalFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info map[string]any
		want []File
	}{
		{
			name: "v1 single file",
			info: map[string]any{
				"length": int64(5), "name": "Example.S01E01.mkv", "piece length": int64(16 << 10),
				"pieces": strings.Repeat("a", sha1.Size),
			},
			want: []File{{Path: "Example.S01E01.mkv", Length: 5}},
		},
		{
			name: "v1 multi file",
			info: map[string]any{
				"files": []any{
					map[string]any{"length": int64(7), "path": []any{"Season 01", "Example.S01E02.mkv"}},
					map[string]any{"length": int64(3), "path": []any{"Season 01", "Example.S01E01.srt"}},
				},
				"name": "Example", "piece length": int64(16 << 10),
				"pieces": strings.Repeat("b", sha1.Size),
			},
			want: []File{
				{Path: "Season 01/Example.S01E01.srt", Length: 3},
				{Path: "Season 01/Example.S01E02.mkv", Length: 7},
			},
		},
		{
			name: "v2 file tree",
			info: map[string]any{
				"file tree": map[string]any{
					"Season 01": map[string]any{
						"Example.S01E02.mkv": map[string]any{"": map[string]any{"length": int64(2)}},
						"Example.S01E01.mkv": map[string]any{"": map[string]any{"length": int64(1)}},
					},
				},
				"meta version": int64(2), "name": "Example", "piece length": int64(16 << 10),
			},
			want: []File{
				{Path: "Season 01/Example.S01E01.mkv", Length: 1},
				{Path: "Season 01/Example.S01E02.mkv", Length: 2},
			},
		},
		{
			name: "hybrid uses v1 layout once",
			info: map[string]any{
				"file tree": map[string]any{
					"Example.S01E01.mkv": map[string]any{"": map[string]any{
						"length": int64(1), "pieces root": strings.Repeat("c", sha256.Size),
					}},
				},
				"length": int64(1), "meta version": int64(2), "name": "Example.S01E01.mkv",
				"piece length": int64(16 << 10), "pieces": strings.Repeat("d", sha1.Size),
			},
			want: []File{{Path: "Example.S01E01.mkv", Length: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Inspect(encodeMetainfo(t, tt.info))
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if !reflect.DeepEqual(got.Files, tt.want) {
				t.Fatalf("Files = %#v, want %#v", got.Files, tt.want)
			}
		})
	}
}

// TestInspectProjectsTorrentIdentity pins the V2 `torrent_from_file.name` and
// `.total_size` projection used by the qBittorrent duplicate-torrent lookup.
func TestInspectProjectsTorrentIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		info          map[string]any
		wantName      string
		wantTotalSize int64
	}{
		{
			name: "v1 单文件",
			info: map[string]any{
				"length": int64(5), "name": "movie.mkv", "piece length": int64(16 << 10),
				"pieces": strings.Repeat("a", sha1.Size),
			},
			wantName: "movie.mkv", wantTotalSize: 5,
		},
		{
			name: "v1 多文件求和",
			info: map[string]any{
				"files": []any{
					map[string]any{"length": int64(7), "path": []any{"Season 01", "a.mkv"}},
					map[string]any{"length": int64(3), "path": []any{"Season 01", "b.srt"}},
				},
				"name": "Example", "piece length": int64(16 << 10),
				"pieces": strings.Repeat("b", sha1.Size),
			},
			wantName: "Example", wantTotalSize: 10,
		},
		{
			name: "v2 文件树求和",
			info: map[string]any{
				"file tree": map[string]any{
					"Season 01": map[string]any{
						"a.mkv": map[string]any{"": map[string]any{"length": int64(2)}},
						"b.mkv": map[string]any{"": map[string]any{"length": int64(1)}},
					},
				},
				"meta version": int64(2), "name": "Example", "piece length": int64(16 << 10),
			},
			wantName: "Example", wantTotalSize: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Inspect(encodeMetainfo(t, tt.info))
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if got.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.TotalSize != tt.wantTotalSize {
				t.Fatalf("TotalSize = %d, want %d", got.TotalSize, tt.wantTotalSize)
			}
		})
	}
}

func TestInspectRejectsInvalidMetainfo(t *testing.T) {
	t.Parallel()

	validV1 := map[string]any{
		"length": int64(1), "name": "movie.mkv", "piece length": int64(16 << 10),
		"pieces": strings.Repeat("a", sha1.Size),
	}
	tests := []struct {
		name    string
		body    func(*testing.T) []byte
		wantErr error
	}{
		{name: "空内容", body: func(*testing.T) []byte { return nil }, wantErr: ErrInvalid},
		{name: "超过上限", body: func(*testing.T) []byte { return make([]byte, MaxSize+1) }, wantErr: ErrTooLarge},
		{name: "缺少 info", body: func(t *testing.T) []byte {
			return encodeBencode(t, map[string]any{"announce": "https://tracker.invalid"})
		}, wantErr: ErrInvalid},
		{name: "尾随内容", body: func(t *testing.T) []byte { return append(encodeMetainfo(t, validV1), 'x') }, wantErr: ErrInvalid},
		{name: "v1 分片数不匹配", body: func(t *testing.T) []byte {
			return encodeMetainfo(t, map[string]any{
				"length": int64(20_000), "name": "movie.mkv", "piece length": int64(16 << 10),
				"pieces": strings.Repeat("a", sha1.Size),
			})
		}, wantErr: ErrInvalid},
		{name: "v2 piece length 不是规范幂", body: func(t *testing.T) []byte {
			return encodeMetainfo(t, map[string]any{
				"file tree":    map[string]any{"movie.mkv": map[string]any{"": map[string]any{"length": int64(1)}}},
				"meta version": int64(2), "name": "movie", "piece length": int64(20_000),
			})
		}, wantErr: ErrInvalid},
		{name: "v2 路径穿越", body: func(t *testing.T) []byte {
			return encodeMetainfo(t, map[string]any{
				"file tree":    map[string]any{"..": map[string]any{"": map[string]any{"length": int64(1)}}},
				"meta version": int64(2), "name": "movie", "piece length": int64(16 << 10),
			})
		}, wantErr: ErrInvalid},
		{name: "不支持的 meta version", body: func(t *testing.T) []byte {
			return encodeMetainfo(t, map[string]any{
				"file tree":    map[string]any{"movie.mkv": map[string]any{"": map[string]any{"length": int64(1)}}},
				"meta version": int64(3), "name": "movie", "piece length": int64(16 << 10),
			})
		}, wantErr: ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Inspect(tt.body(t))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Inspect() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func FuzzInspectNeverLeaksInput(f *testing.F) {
	f.Add(encodeMetainfoForFuzz(map[string]any{
		"length": int64(1), "name": "movie.mkv", "piece length": int64(16 << 10),
		"pieces": strings.Repeat("a", sha1.Size),
	}))
	f.Add([]byte("secret-passkey-marker"))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, err := Inspect(body)
		if err != nil && len(body) > 0 && strings.Contains(err.Error(), string(body)) {
			t.Fatal("Inspect() 错误泄露原始 metainfo")
		}
	})
}

func encodeMetainfo(t *testing.T, info map[string]any) []byte {
	t.Helper()
	return encodeBencode(t, map[string]any{
		"announce": "https://tracker.invalid/announce",
		"info":     info,
	})
}

func encodeBencode(t *testing.T, value any) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := bencode.NewEncoder(&buffer).Encode(value); err != nil {
		t.Fatalf("编码 metainfo fixture: %v", err)
	}
	return buffer.Bytes()
}

func encodeMetainfoForFuzz(info map[string]any) []byte {
	var buffer bytes.Buffer
	_ = bencode.NewEncoder(&buffer).Encode(map[string]any{"info": info})
	return buffer.Bytes()
}
