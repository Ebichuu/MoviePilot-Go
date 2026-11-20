package downloadconfig

import "testing"

func TestDirectoryLocalPaths(t *testing.T) {
	t.Parallel()
	local := "local"
	remote := "alist"
	download := " /downloads "
	library := " /library "

	tests := []struct {
		name           string
		directory      Directory
		wantDownload   string
		wantDownloadOK bool
		wantLibrary    string
		wantLibraryOK  bool
	}{
		{
			name:         "empty storage defaults to local",
			directory:    Directory{DownloadPath: &download, LibraryPath: &library},
			wantDownload: "/downloads", wantDownloadOK: true,
			wantLibrary: "/library", wantLibraryOK: true,
		},
		{
			name:         "explicit local storage",
			directory:    Directory{Storage: &local, DownloadPath: &download, LibraryStorage: &local, LibraryPath: &library},
			wantDownload: "/downloads", wantDownloadOK: true,
			wantLibrary: "/library", wantLibraryOK: true,
		},
		{
			name:      "remote roots are excluded",
			directory: Directory{Storage: &remote, DownloadPath: &download, LibraryStorage: &remote, LibraryPath: &library},
		},
		{
			name:      "missing roots are excluded",
			directory: Directory{Storage: &local, LibraryStorage: &local},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			downloadPath, downloadOK := tt.directory.LocalDownloadPath()
			if downloadPath != tt.wantDownload || downloadOK != tt.wantDownloadOK {
				t.Fatalf("LocalDownloadPath() = (%q, %v), want (%q, %v)", downloadPath, downloadOK, tt.wantDownload, tt.wantDownloadOK)
			}
			libraryPath, libraryOK := tt.directory.LocalLibraryPath()
			if libraryPath != tt.wantLibrary || libraryOK != tt.wantLibraryOK {
				t.Fatalf("LocalLibraryPath() = (%q, %v), want (%q, %v)", libraryPath, libraryOK, tt.wantLibrary, tt.wantLibraryOK)
			}
		})
	}
}
