package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

type fakeDirectoryMonitorStore struct {
	dirs      []downloadconfig.Directory
	succeeded []string
	failed    []string
}

func (f *fakeDirectoryMonitorStore) GetSetting(_ context.Context, key string, target any) (bool, error) {
	if key != directoryMonitorDirectoriesKey {
		return false, nil
	}
	ptr, ok := target.(*[]downloadconfig.Directory)
	if !ok {
		return false, nil
	}
	*ptr = append([]downloadconfig.Directory(nil), f.dirs...)
	return true, nil
}

func (f *fakeDirectoryMonitorStore) RecordManualOrganizationWithMode(_ context.Context, _, target, _ string) error {
	f.succeeded = append(f.succeeded, target)
	return nil
}

func (f *fakeDirectoryMonitorStore) RecordFailedOrganization(_ context.Context, source, _, _, errMsg string) error {
	f.failed = append(f.failed, source+":"+errMsg)
	return nil
}

func TestDirectoryMonitorOrganizesWithCustomTemplateAndRecordsFailure(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	libraryDir := t.TempDir()
	sourceFile := filepath.Join(downloadDir, "Dune.Part.Two.2024.2160p.WEB-DL.mkv")
	if err := os.WriteFile(sourceFile, []byte("video-bytes"), 0o600); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	// 非视频文件应被忽略。
	if err := os.WriteFile(filepath.Join(downloadDir, "readme.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatalf("write txt: %v", err)
	}

	monType := "monitor"
	transType := "copy"
	renaming := true
	store := &fakeDirectoryMonitorStore{
		dirs: []downloadconfig.Directory{{
			DownloadPath: &downloadDir,
			LibraryPath:  &libraryDir,
			MonitorType:  &monType,
			TransferType: &transType,
			Renaming:     &renaming,
		}},
	}

	monitor := newDirectoryMonitor(store, func() organization.NamingTemplates {
		return organization.NamingTemplates{
			Movie: "{{title}} ({{year}})/{{title}}{{fileExt}}",
		}
	}, nil)
	monitor.stability = 0

	if err := monitor.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce() error = %v", err)
	}
	if len(store.succeeded) != 1 {
		t.Fatalf("succeeded = %v, want 1", store.succeeded)
	}
	if _, err := os.Stat(store.succeeded[0]); err != nil {
		t.Fatalf("organized file missing at %q: %v", store.succeeded[0], err)
	}

	// 再次扫描同一个文件应幂等跳过，不会重复整理或报错。
	if err := monitor.ScanOnce(context.Background()); err != nil {
		t.Fatalf("second ScanOnce() error = %v", err)
	}
	if len(store.succeeded) != 1 || len(store.failed) != 0 {
		t.Fatalf("unexpected repeat run: succeeded=%v failed=%v", store.succeeded, store.failed)
	}

	// 验证 ScheduleInfo 投影正常。
	info, ok := monitor.ScheduleInfo(time.Now(), time.UTC)
	if !ok || info.ID != "directory_monitor" || info.NextRun == "" {
		t.Fatalf("ScheduleInfo = %+v ok=%v", info, ok)
	}
}
