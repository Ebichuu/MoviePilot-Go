package downloadconfig

import "strings"

// Directory is the stable server-side projection of one V2 Directories item.
type Directory struct {
	Name                   *string `json:"name"`
	Storage                *string `json:"storage"`
	DownloadPath           *string `json:"download_path"`
	Priority               *int    `json:"priority"`
	MediaType              *string `json:"media_type"`
	MediaCategory          *string `json:"media_category"`
	DownloadTypeFolder     *bool   `json:"download_type_folder"`
	DownloadCategoryFolder *bool   `json:"download_category_folder"`
	MonitorType            *string `json:"monitor_type"`
	MonitorMode            *string `json:"monitor_mode"`
	TransferType           *string `json:"transfer_type"`
	OverwriteMode          *string `json:"overwrite_mode"`
	LibraryPath            *string `json:"library_path"`
	LibraryStorage         *string `json:"library_storage"`
	Renaming               *bool   `json:"renaming"`
	Scraping               *bool   `json:"scraping"`
	Notify                 *bool   `json:"notify"`
	LibraryTypeFolder      *bool   `json:"library_type_folder"`
	LibraryCategoryFolder  *bool   `json:"library_category_folder"`
}

// LocalDownloadPath returns an exact local download root, if configured.
func (d Directory) LocalDownloadPath() (string, bool) {
	return localPath(d.Storage, d.DownloadPath)
}

// LocalLibraryPath returns the configured local media-library root. An empty
// storage type retains V2's local-storage default.
func (d Directory) LocalLibraryPath() (string, bool) {
	return localPath(d.LibraryStorage, d.LibraryPath)
}

// AllowedLocalSavePath accepts only an exact configured root. An empty path
// keeps the downloader's own default location.
func AllowedLocalSavePath(directories []Directory, requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", true
	}
	for _, directory := range directories {
		path, ok := directory.LocalDownloadPath()
		if ok && requested == path {
			return path, true
		}
	}
	return "", false
}

func localPath(storageValue, pathValue *string) (string, bool) {
	storage := ""
	if storageValue != nil {
		storage = strings.TrimSpace(*storageValue)
	}
	if storage != "" && !strings.EqualFold(storage, "local") {
		return "", false
	}
	if pathValue == nil {
		return "", false
	}
	path := strings.TrimSpace(*pathValue)
	return path, path != ""
}
