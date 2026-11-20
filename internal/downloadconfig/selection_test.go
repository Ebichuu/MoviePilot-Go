package downloadconfig

import "testing"

func TestSelectEnabledQBittorrent(t *testing.T) {
	t.Parallel()
	settings := []Downloader{
		{ID: "11111111-1111-4111-8111-111111111111", Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true},
		{ID: "22222222-2222-4222-8222-222222222222", Name: "Archive", Type: "qbittorrent", Enabled: true},
		{ID: "33333333-3333-4333-8333-333333333333", Name: "Disabled", Type: "qbittorrent", Enabled: false},
	}
	tests := []struct {
		name     string
		selector string
		wantID   string
		wantOK   bool
	}{
		{name: "默认", wantID: settings[0].ID, wantOK: true},
		{name: "按名称", selector: " archive ", wantID: settings[1].ID, wantOK: true},
		{name: "按稳定 ID", selector: settings[1].ID, wantID: settings[1].ID, wantOK: true},
		{name: "禁用实例", selector: "Disabled"},
		{name: "未知实例", selector: "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SelectEnabledQBittorrent(settings, tt.selector)
			if ok != tt.wantOK || got.ID != tt.wantID {
				t.Fatalf("SelectEnabledQBittorrent() = (%q, %v), want (%q, %v)", got.ID, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestAllowedLocalSavePath(t *testing.T) {
	t.Parallel()
	local, remote, path := "local", "rclone", "/downloads"
	directories := []Directory{
		{Storage: &local, DownloadPath: &path},
		{Storage: &remote, DownloadPath: &path},
	}
	tests := []struct {
		name      string
		requested string
		want      string
		wantOK    bool
	}{
		{name: "空路径使用下载器默认", wantOK: true},
		{name: "精确本地路径", requested: "/downloads", want: "/downloads", wantOK: true},
		{name: "拒绝子路径", requested: "/downloads/tv"},
		{name: "拒绝未知路径", requested: "/other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := AllowedLocalSavePath(directories, tt.requested)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("AllowedLocalSavePath() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
