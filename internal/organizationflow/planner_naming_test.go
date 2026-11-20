package organizationflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestBuildPlanWithTemplatesAppliesCustomNamingAndFolders(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	libraryDir := t.TempDir()
	sourceFile := filepath.Join(downloadDir, "Dune.Part.Two.2024.2160p.WEB-DL.mkv")
	payload := []byte("sample-video-payload")
	if err := os.WriteFile(sourceFile, payload, 0o600); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	transferType := "copy"
	renaming := true
	typeFolder := true
	catFolder := true
	directories := []downloadconfig.Directory{{
		DownloadPath:          &downloadDir,
		LibraryPath:           &libraryDir,
		TransferType:          &transferType,
		Renaming:              &renaming,
		LibraryTypeFolder:     &typeFolder,
		LibraryCategoryFolder: &catFolder,
	}}

	content := downloader.TaskContent{
		Task: downloader.Task{
			State:       downloader.TaskStateCompleted,
			SavePath:    downloadDir,
			ContentPath: sourceFile,
		},
		Files: []downloader.TaskContentFile{{
			RelativePath: filepath.Base(sourceFile),
			SizeBytes:    int64(len(payload)),
		}},
	}

	input := plannerSuccessInput()
	input.Media = organization.MediaContext{
		Source:    "themoviedb",
		MediaID:   "693134",
		TitleYear: "沙丘2 (2024)",
		MediaType: "电影",
		Category:  "科幻电影",
	}

	prepared, err := BuildPlanWithTemplates(
		context.Background(),
		content,
		directories,
		input,
		organization.NamingTemplates{
			Movie: "{{title}} ({{year}})/{{title}} - {{videoFormat}}{{fileExt}}",
		},
	)
	if err != nil {
		t.Fatalf("BuildPlanWithTemplates() error = %v", err)
	}
	if len(prepared.Plan.Operations) != 1 {
		t.Fatalf("operations = %d, want 1", len(prepared.Plan.Operations))
	}
	wantTarget := filepath.Join(libraryDir, "电影", "科幻电影", "沙丘2 (2024)", "沙丘2 - 2160p.mkv")
	if prepared.Plan.Operations[0].Target != wantTarget {
		t.Fatalf("target = %q, want %q", prepared.Plan.Operations[0].Target, wantTarget)
	}
}
