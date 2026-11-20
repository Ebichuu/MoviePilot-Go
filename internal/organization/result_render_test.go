package organization_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestRenderV2OrganizeResult(t *testing.T) {
	result := organization.ResultSnapshot{
		Snapshot: resultRenderSnapshot(t, []organization.FileFact{{
			Ordinal: 0, Role: "video", SizeBytes: 1 << 30, Season: 1,
			Episodes: []int{1}, ResourceTerm: "WEB-DL 1080p",
		}}),
		ErrMsg:          "磁盘空间不足",
		TerminalOrdinal: 1,
	}

	got, err := organization.RenderV2OrganizeResult(result, "https://mp.example")
	if err != nil {
		t.Fatalf("RenderV2OrganizeResult() error = %v", err)
	}
	if got.Title != "示例剧 (2026) S01 E01 已入库" {
		t.Fatalf("Title = %q", got.Title)
	}
	wantText := "评分：8.6，类型：电视剧，类别：国产剧，质量：WEB-DL 1080p，共1个文件，大小：1.0G，以下文件处理失败：磁盘空间不足"
	if got.Text != wantText || got.Context.ErrMsg != "磁盘空间不足" {
		t.Fatalf("rendered = %#v, want text %q", got, wantText)
	}
	if got.Link == nil || *got.Link != "https://mp.example/#/history" {
		t.Fatalf("Link = %#v", got.Link)
	}
}

func TestRenderV2ManualFailure(t *testing.T) {
	snapshot := resultRenderSnapshot(t, []organization.FileFact{{
		Ordinal: 4, Role: "video", SizeBytes: 1024, Season: 2, Episodes: []int{3, 4},
	}})
	failure := organization.FileFailure{
		Snapshot: organization.SnapshotContext{
			BatchID: snapshot.BatchID, IntentID: snapshot.IntentID,
			GroupKey: snapshot.GroupKey, Version: snapshot.Version,
			Username: snapshot.Username, NeedNotify: false, Manual: true,
			Media: snapshot.Media,
		},
		File: snapshot.Files[0], Error: "源文件不存在",
	}

	got, err := organization.RenderV2ManualFailure(failure, "mp.example/base")
	if err != nil {
		t.Fatalf("RenderV2ManualFailure() error = %v", err)
	}
	if got.Title != "示例剧 (2026) S02 E03-E04 入库失败！" ||
		got.Text != "原因：源文件不存在" || got.Image != "https://img.example/w500/backdrop.jpg" ||
		got.Username != "alice" || got.Link == nil || *got.Link != "http://mp.example/base/#/history" {
		t.Fatalf("RenderV2ManualFailure() = %#v", got)
	}
}

func TestRenderV2ManualFailureMoviePreservesV2TitleSpacing(t *testing.T) {
	snapshot := resultRenderSnapshot(t, []organization.FileFact{{
		Ordinal: 0, Role: "video", SizeBytes: 1024, Season: 0,
	}})
	snapshot.Media.MediaType = "电影"
	groupKey, err := organization.BuildGroupKey(snapshot.Media, 0)
	if err != nil {
		t.Fatal(err)
	}
	failure := organization.FileFailure{
		Snapshot: organization.SnapshotContext{
			BatchID: snapshot.BatchID, IntentID: snapshot.IntentID,
			GroupKey: groupKey, Version: snapshot.Version, Username: snapshot.Username,
			Media: snapshot.Media,
		},
		File: snapshot.Files[0], Error: "未知",
	}

	got, err := organization.RenderV2ManualFailure(failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "示例剧 (2026)  入库失败！" || got.Link != nil {
		t.Fatalf("movie rendering = %#v", got)
	}
}

func TestRenderV2TerminalResultsRejectInvalidInput(t *testing.T) {
	snapshot := resultRenderSnapshot(t, []organization.FileFact{{
		Ordinal: 2, Role: "video", SizeBytes: 1024, Season: 1, Episodes: []int{1},
	}})
	if _, err := organization.RenderV2OrganizeResult(organization.ResultSnapshot{
		Snapshot: snapshot, TerminalOrdinal: 1,
	}, ""); !errors.Is(err, organization.ErrInvalidTerminalResults) {
		t.Fatalf("invalid terminal ordinal error = %v", err)
	}
	if _, err := organization.RenderV2ManualFailure(organization.FileFailure{
		Snapshot: organization.SnapshotContext{
			BatchID: snapshot.BatchID, IntentID: snapshot.IntentID,
			GroupKey: snapshot.GroupKey, Version: snapshot.Version,
			Username: snapshot.Username, Media: snapshot.Media,
		},
		File: snapshot.Files[0], Error: "line\nbreak",
	}, ""); !errors.Is(err, organization.ErrInvalidTerminalResults) {
		t.Fatalf("invalid failure error = %v", err)
	}
}

func resultRenderSnapshot(t *testing.T, files []organization.FileFact) organization.SuccessSnapshot {
	t.Helper()
	media := organization.MediaContext{
		Source: "tmdb", MediaID: "123", TitleYear: "示例剧 (2026)",
		MediaType: "电视剧", Category: "国产剧", VoteAverage: 8.6,
		BackdropURL: "https://img.example/original/backdrop.jpg",
	}
	groupKey, err := organization.BuildGroupKey(media, files[0].Season)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := organization.NormalizeSuccessSnapshot(organization.SuccessSnapshot{
		BatchID:  uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		IntentID: uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		GroupKey: groupKey, Version: organization.CurrentSuccessVersion,
		Username: "alice", NeedNotify: true, Background: true,
		Media: media, Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
