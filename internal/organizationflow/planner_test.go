package organizationflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestBuildPlanMapsSingleFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadRoot := filepath.Join(root, "downloads")
	libraryRoot := filepath.Join(root, "library")
	source := filepath.Join(downloadRoot, "Movie.2026.mkv")
	writePlannerFile(t, source, "video")

	prepared, err := BuildPlan(t.Context(), downloader.TaskContent{
		Task: downloader.Task{
			State:       downloader.TaskStateCompleted,
			SavePath:    downloadRoot,
			ContentPath: source,
		},
		Files: []downloader.TaskContentFile{{
			RelativePath: filepath.Base(source),
			SizeBytes:    int64(len("video")),
		}},
	}, []downloadconfig.Directory{
		localDirectory(downloadRoot, libraryRoot, "copy", 0),
	}, plannerSuccessInput())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SourceRoot != source || prepared.Plan.TargetRoot != libraryRoot {
		t.Fatalf("prepared roots = %#v", prepared)
	}
	want := []organizer.Operation{{
		Source:         source,
		Target:         filepath.Join(libraryRoot, filepath.Base(source)),
		Mode:           organizer.Copy,
		ExpectedSize:   int64(len("video")),
		ExpectedSHA256: plannerTestSHA256("video"),
	}}
	if !reflect.DeepEqual(prepared.Plan.Operations, want) {
		t.Fatalf("operations = %#v, want %#v", prepared.Plan.Operations, want)
	}
	if len(prepared.Snapshots) != 1 || !prepared.Snapshots[0].NeedNotify ||
		len(prepared.Snapshots[0].Files) != 1 || prepared.Snapshots[0].Files[0].Role != organizationFileRoleVideo {
		t.Fatalf("success snapshots = %#v", prepared.Snapshots)
	}
}

func TestBuildPlanUsesOnlySnapshotAndPreservesContentTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadRoot := filepath.Join(root, "downloads")
	seriesRoot := filepath.Join(downloadRoot, "series")
	outerLibrary := filepath.Join(root, "outer-library")
	seriesLibrary := filepath.Join(root, "series-library")
	contentRoot := filepath.Join(seriesRoot, "Example.Show.S01")
	video := filepath.Join(contentRoot, "Example.Show.S01E01.mkv")
	subtitle := filepath.Join(contentRoot, "subs", "Example.Show.S01E01.srt")
	unselected := filepath.Join(contentRoot, "sample.mkv")
	writePlannerFile(t, video, "video")
	writePlannerFile(t, subtitle, "subtitle")
	writePlannerFile(t, unselected, "must-not-be-planned")

	prepared, err := BuildPlan(t.Context(), downloader.TaskContent{
		Task: downloader.Task{
			State:       downloader.TaskStateCompleted,
			SavePath:    downloadRoot,
			ContentPath: contentRoot,
		},
		Files: []downloader.TaskContentFile{
			{RelativePath: "series/Example.Show.S01/subs/Example.Show.S01E01.srt", SizeBytes: int64(len("subtitle"))},
			{RelativePath: "series/Example.Show.S01/Example.Show.S01E01.mkv", SizeBytes: int64(len("video"))},
		},
	}, []downloadconfig.Directory{
		localDirectory(downloadRoot, outerLibrary, "copy", 0),
		localDirectory(seriesRoot, seriesLibrary, "link", 10),
	}, plannerSuccessInput())
	if err != nil {
		t.Fatal(err)
	}
	want := []organizer.Operation{
		{
			Source:         video,
			Target:         filepath.Join(seriesLibrary, filepath.Base(contentRoot), filepath.Base(video)),
			Mode:           organizer.Hardlink,
			ExpectedSize:   int64(len("video")),
			ExpectedSHA256: plannerTestSHA256("video"),
		},
		{
			Source:         subtitle,
			Target:         filepath.Join(seriesLibrary, filepath.Base(contentRoot), "subs", filepath.Base(subtitle)),
			Mode:           organizer.Hardlink,
			ExpectedSize:   int64(len("subtitle")),
			ExpectedSHA256: plannerTestSHA256("subtitle"),
		},
	}
	if prepared.SourceRoot != contentRoot || prepared.Plan.TargetRoot != seriesLibrary ||
		!reflect.DeepEqual(prepared.Plan.Operations, want) {
		t.Fatalf("prepared plan = %#v, want operations %#v", prepared, want)
	}
	for _, operation := range prepared.Plan.Operations {
		if samePath(operation.Source, unselected) {
			t.Fatal("unselected file entered the organization plan")
		}
	}
}

func TestBuildPlanFreezesV2SuccessGroupsAcrossSeasons(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadRoot := filepath.Join(root, "downloads")
	libraryRoot := filepath.Join(root, "library")
	contentRoot := filepath.Join(downloadRoot, "Show")
	seasonOneVideo := filepath.Join(contentRoot, "Show.S01E01.mkv")
	seasonOneSubtitle := filepath.Join(contentRoot, "Subs", "Show.S01E01.zh.srt")
	seasonTwoVideo := filepath.Join(contentRoot, "Show.S02E03.mkv")
	writePlannerFile(t, seasonOneVideo, "season-one")
	writePlannerFile(t, seasonOneSubtitle, "subtitle")
	writePlannerFile(t, seasonTwoVideo, "season-two")

	notify := false
	directory := localDirectory(downloadRoot, libraryRoot, "copy", 0)
	directory.Notify = &notify
	input := organization.SuccessInput{
		IntentID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Version:  organization.CurrentSuccessVersion, Username: "alice",
		Manual: false, Background: true,
		Media: organization.MediaContext{
			Source: "themoviedb", MediaID: "42", TitleYear: "Fixture (2026)", MediaType: "电视剧",
		},
		ResourceTerm: "WEB-DL HDR 2160p",
		Episodes:     []organization.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 3}},
	}
	prepared, err := BuildPlan(t.Context(), downloader.TaskContent{
		Task: downloader.Task{
			State: downloader.TaskStateCompleted, SavePath: downloadRoot, ContentPath: contentRoot,
		},
		Files: []downloader.TaskContentFile{
			{RelativePath: "Show/Show.S02E03.mkv", SizeBytes: int64(len("season-two"))},
			{RelativePath: "Show/Subs/Show.S01E01.zh.srt", SizeBytes: int64(len("subtitle"))},
			{RelativePath: "Show/Show.S01E01.mkv", SizeBytes: int64(len("season-one"))},
		},
	}, []downloadconfig.Directory{directory}, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Snapshots) != 2 {
		t.Fatalf("snapshot count = %d, want 2: %#v", len(prepared.Snapshots), prepared.Snapshots)
	}
	wantBatchID := uuid.NewSHA1(input.IntentID, []byte(prepared.Plan.ID))
	bySeason := make(map[int]organization.SuccessSnapshot, len(prepared.Snapshots))
	for _, snapshot := range prepared.Snapshots {
		if snapshot.BatchID != wantBatchID || snapshot.IntentID != input.IntentID || snapshot.NeedNotify ||
			snapshot.Media != input.Media || snapshot.Username != input.Username || !snapshot.Background {
			t.Fatalf("snapshot identity = %#v", snapshot)
		}
		if len(snapshot.Files) == 0 {
			t.Fatal("empty success group")
		}
		season := snapshot.Files[0].Season
		wantKey, keyErr := organization.BuildGroupKey(input.Media, season)
		if keyErr != nil || snapshot.GroupKey != wantKey {
			t.Fatalf("group key = %q, want %q, error=%v", snapshot.GroupKey, wantKey, keyErr)
		}
		bySeason[season] = snapshot
	}
	seasonOne := bySeason[1]
	if len(seasonOne.Files) != 2 || seasonOne.Files[0].Role != organizationFileRoleVideo ||
		!reflect.DeepEqual(seasonOne.Files[0].Episodes, []int{1}) ||
		seasonOne.Files[1].Role != organizationFileRoleSubtitle ||
		!reflect.DeepEqual(seasonOne.Files[1].Episodes, []int{1}) {
		t.Fatalf("season one facts = %#v", seasonOne.Files)
	}
	seasonTwo := bySeason[2]
	if len(seasonTwo.Files) != 1 || seasonTwo.Files[0].Role != organizationFileRoleVideo ||
		!reflect.DeepEqual(seasonTwo.Files[0].Episodes, []int{3}) {
		t.Fatalf("season two facts = %#v", seasonTwo.Files)
	}
	for _, snapshot := range prepared.Snapshots {
		for _, file := range snapshot.Files {
			if file.ResourceTerm != input.ResourceTerm {
				t.Fatalf("resource term = %q", file.ResourceTerm)
			}
		}
	}
}

func TestBuildPlanRejectsInvalidSuccessInputBeforePersistence(t *testing.T) {
	t.Parallel()
	_, err := BuildPlan(t.Context(), downloader.TaskContent{
		Task:  downloader.Task{State: downloader.TaskStateCompleted},
		Files: []downloader.TaskContentFile{{RelativePath: "Fixture.mkv", SizeBytes: 1}},
	}, nil, organization.SuccessInput{})
	if !errors.Is(err, ErrInvalidOrganizationContent) {
		t.Fatalf("BuildPlan() error = %v, want %v", err, ErrInvalidOrganizationContent)
	}
}

func TestBuildPlanRejectsUnavailableOrUnsafeSnapshot(t *testing.T) {
	root := t.TempDir()
	downloadRoot := filepath.Join(root, "downloads")
	libraryRoot := filepath.Join(root, "library")
	contentRoot := filepath.Join(downloadRoot, "Show")
	source := filepath.Join(contentRoot, "Show.S01E01.mkv")
	writePlannerFile(t, source, "video")
	valid := downloader.TaskContent{
		Task: downloader.Task{
			State:       downloader.TaskStateCompleted,
			SavePath:    downloadRoot,
			ContentPath: contentRoot,
		},
		Files: []downloader.TaskContentFile{{
			RelativePath: "Show/Show.S01E01.mkv",
			SizeBytes:    int64(len("video")),
		}},
	}
	validDirectories := []downloadconfig.Directory{localDirectory(downloadRoot, libraryRoot, "copy", 0)}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	missingSource := valid
	missingSource.Files = []downloader.TaskContentFile{{RelativePath: "Show/missing.mkv", SizeBytes: 1}}
	wrongSize := valid
	wrongSize.Files = []downloader.TaskContentFile{{RelativePath: "Show/Show.S01E01.mkv", SizeBytes: 999}}
	outOfBoundary := valid
	outOfBoundary.Files = []downloader.TaskContentFile{{RelativePath: "Other/file.mkv", SizeBytes: 1}}
	writePlannerFile(t, filepath.Join(downloadRoot, "Other", "file.mkv"), "x")
	notCompleted := valid
	notCompleted.Task.State = downloader.TaskStateDownloading
	empty := valid
	empty.Files = nil
	tooMany := valid
	tooMany.Files = make([]downloader.TaskContentFile, maxPlannedFiles+1)
	missingContent := valid
	missingContent.Task.ContentPath = filepath.Join(downloadRoot, "Missing")

	tests := []struct {
		name        string
		ctx         context.Context
		content     downloader.TaskContent
		directories []downloadconfig.Directory
		wantErr     error
	}{
		{name: "nil context", content: valid, directories: validDirectories, wantErr: ErrInvalidOrganizationContent},
		{name: "canceled context", ctx: canceled, content: valid, directories: validDirectories, wantErr: context.Canceled},
		{name: "task not completed", ctx: t.Context(), content: notCompleted, directories: validDirectories, wantErr: ErrOrganizationContentUnavailable},
		{name: "empty selected files", ctx: t.Context(), content: empty, directories: validDirectories, wantErr: ErrOrganizationContentUnavailable},
		{name: "too many files", ctx: t.Context(), content: tooMany, directories: validDirectories, wantErr: ErrOrganizationPlanLimit},
		{name: "missing content root", ctx: t.Context(), content: missingContent, directories: validDirectories, wantErr: ErrOrganizationContentUnavailable},
		{name: "missing snapshot source", ctx: t.Context(), content: missingSource, directories: validDirectories, wantErr: ErrOrganizationContentUnavailable},
		{name: "snapshot size changed", ctx: t.Context(), content: wrongSize, directories: validDirectories, wantErr: ErrOrganizationContentUnavailable},
		{name: "parent traversal", ctx: t.Context(), content: plannerContentWithRelative(valid, "Show/../outside.mkv"), directories: validDirectories, wantErr: ErrInvalidOrganizationContent},
		{name: "absolute path", ctx: t.Context(), content: plannerContentWithRelative(valid, source), directories: validDirectories, wantErr: ErrInvalidOrganizationContent},
		{name: "source outside content root", ctx: t.Context(), content: outOfBoundary, directories: validDirectories, wantErr: ErrInvalidOrganizationContent},
		{name: "missing local rule", ctx: t.Context(), content: valid, wantErr: ErrOrganizationRuleUnavailable},
		{name: "overlapping target", ctx: t.Context(), content: valid, directories: []downloadconfig.Directory{localDirectory(downloadRoot, filepath.Join(contentRoot, "library"), "copy", 0)}, wantErr: ErrInvalidOrganizationContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildPlan(tt.ctx, tt.content, tt.directories, plannerSuccessInput())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("BuildPlan() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildPlanRejectsLinkedContentAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDownloadRoot := filepath.Join(root, "real-downloads")
	linkedDownloadRoot := filepath.Join(root, "linked-downloads")
	libraryRoot := filepath.Join(root, "library")
	source := filepath.Join(realDownloadRoot, "Movie.2026.mkv")
	writePlannerFile(t, source, "video")
	if err := os.Symlink(realDownloadRoot, linkedDownloadRoot); err != nil {
		t.Skipf("当前环境不能创建目录软链接: %v", err)
	}

	_, err := BuildPlan(t.Context(), downloader.TaskContent{
		Task: downloader.Task{
			State:       downloader.TaskStateCompleted,
			SavePath:    linkedDownloadRoot,
			ContentPath: filepath.Join(linkedDownloadRoot, filepath.Base(source)),
		},
		Files: []downloader.TaskContentFile{{
			RelativePath: filepath.Base(source),
			SizeBytes:    int64(len("video")),
		}},
	}, []downloadconfig.Directory{
		localDirectory(linkedDownloadRoot, libraryRoot, "copy", 0),
	}, plannerSuccessInput())
	if !errors.Is(err, ErrOrganizationContentUnavailable) {
		t.Fatalf("BuildPlan() error = %v, want %v", err, ErrOrganizationContentUnavailable)
	}
}

func FuzzNormalizedSnapshotRelativePath(f *testing.F) {
	for _, seed := range []string{
		"Fixture/Video.mkv",
		`Fixture\Subs\subtitle.srt`,
		"../outside.mkv",
		"/absolute/file.mkv",
		`C:\outside\file.mkv`,
		"0:/outside/file.mkv",
		"Fixture/./Video.mkv",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		relative, err := normalizedSnapshotRelativePath(input)
		if err != nil {
			return
		}
		if relative == "" || filepath.IsAbs(relative) || relativeEscapes(relative) ||
			strings.ContainsRune(relative, 0) || hasWindowsDrivePrefix(relative) {
			t.Fatalf("unsafe normalized path %q from %q", relative, input)
		}
		root := filepath.Join(string(filepath.Separator), "moviepilot-safe-root")
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		if !pathWithinRoot(root, candidate) {
			t.Fatalf("normalized path escaped root: %q", relative)
		}
	})
}

func plannerContentWithRelative(content downloader.TaskContent, relative string) downloader.TaskContent {
	content.Files = append([]downloader.TaskContentFile(nil), content.Files...)
	content.Files[0].RelativePath = relative
	return content
}

func writePlannerFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func plannerTestSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func plannerSuccessInput() organization.SuccessInput {
	return organization.SuccessInput{
		IntentID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		Version:  organization.CurrentSuccessVersion,
		Username: "alice",
	}
}

func localDirectory(downloadRoot, libraryRoot, transferType string, priority int) downloadconfig.Directory {
	local := "local"
	return downloadconfig.Directory{
		Storage:        &local,
		DownloadPath:   &downloadRoot,
		Priority:       &priority,
		TransferType:   &transferType,
		LibraryPath:    &libraryRoot,
		LibraryStorage: &local,
	}
}
