package downloader

import (
	"context"
	"time"
)

// CompletedTaskLister is the optional capability used by the download history view.
// Implementations must only report finished tasks and must not expose tracker or
// authentication data.
type CompletedTaskLister interface {
	ListCompleted(context.Context) ([]Task, error)
}

// DownloadingTaskLister is the common contract used to aggregate downloader tasks.
type DownloadingTaskLister interface {
	ListDownloading(context.Context) ([]Task, error)
}

// TaskInspector resolves one MoviePilot-owned task by its deterministic label
// and optional torrent hash. Implementations must not expose tracker or
// authentication data through the returned task.
type TaskInspector interface {
	InspectTask(ctx context.Context, label, torrentHash string) (Task, bool, error)
}

// TaskContentInspector resolves one safe task projection together with the
// selected, completed files stored relative to the task's save path.
type TaskContentInspector interface {
	InspectTaskContent(ctx context.Context, label, torrentHash string) (TaskContent, bool, error)
}

// TaskContent is a bounded, secret-free snapshot of one downloader task and
// its selected, completed files.
type TaskContent struct {
	Task  Task
	Files []TaskContentFile
}

// TaskContentFile identifies one selected, completed file relative to the
// downloader task's save path.
type TaskContentFile struct {
	RelativePath string
	SizeBytes    int64
}

// Task is the safe, numeric representation of a downloader task.
// It deliberately excludes tracker, magnet, comment, and authentication data.
type Task struct {
	Hash               string
	Title              string
	SizeBytes          int64
	ProgressPercent    float64
	State              string
	DownSpeedBytes     int64
	UpSpeedBytes       int64
	ETASeconds         int64
	Label              string
	Tags               string
	SavePath           string
	ContentPath        string
	Category           string
	DownloadLimitBytes int64
	UploadLimitBytes   int64
	RatioLimit         float64
	SeedingTimeLimit   int64
	// CompletedAt is the downloader-reported completion time; zero when unknown.
	CompletedAt time.Time
}

// Normalized task states exposed by downloader adapters.
const (
	TaskStateDownloading = "downloading"
	TaskStatePaused      = "paused"
	TaskStateCompleted   = "completed"
)
