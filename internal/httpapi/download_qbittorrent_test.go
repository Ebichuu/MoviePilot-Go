package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
)

const usableDownloaderID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

func TestUsableQBittorrentRequiresEnabledSupportedInstance(t *testing.T) {
	tests := []struct {
		name       string
		configured downloadconfig.Downloader
		want       bool
	}{
		{
			name:       "启用且类型的 qBittorrent",
			configured: downloadconfig.Downloader{ID: usableDownloaderID, Enabled: true, Type: "qbittorrent"},
			want:       true,
		},
		{
			name:       "类型大小写与空白不敏感",
			configured: downloadconfig.Downloader{ID: usableDownloaderID, Enabled: true, Type: " QBitTorrent "},
			want:       true,
		},
		{
			name:       "Turn 与 rTorrent 同样可用",
			configured: downloadconfig.Downloader{ID: usableDownloaderID, Enabled: true, Type: "transmission"},
			want:       true,
		},
		{
			name:       "未启用",
			configured: downloadconfig.Downloader{ID: usableDownloaderID, Enabled: false, Type: "qbittorrent"},
			want:       false,
		},
		{
			name:       "缺少稳定 ID",
			configured: downloadconfig.Downloader{Enabled: true, Type: "qbittorrent"},
			want:       false,
		},
		{
			name:       "ID 不是 UUID",
			configured: downloadconfig.Downloader{ID: "not-a-uuid", Enabled: true, Type: "qbittorrent"},
			want:       false,
		},
		{
			name:       "ID 是空 UUID",
			configured: downloadconfig.Downloader{ID: "00000000-0000-0000-0000-000000000000", Enabled: true, Type: "qbittorrent"},
			want:       false,
		},
		{
			name:       "不支持的类型",
			configured: downloadconfig.Downloader{ID: usableDownloaderID, Enabled: true, Type: "deluge"},
			want:       false,
		},
		{
			name:       "缺少类型",
			configured: downloadconfig.Downloader{ID: usableDownloaderID, Enabled: true},
			want:       false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := usableQBittorrent(test.configured); got != test.want {
				t.Fatalf("usableQBittorrent(%+v) = %v, want %v", test.configured, got, test.want)
			}
		})
	}
}

func TestPluginRemotesReturnsEmptyList(t *testing.T) {
	recorder := httptest.NewRecorder()
	pluginRemotes(recorder, httptest.NewRequest(http.MethodGet, "/plugin/remotes?token=moviepilot", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("pluginRemotes = (%d, %s)", recorder.Code, recorder.Body.String())
	}
}
