package downloader

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTorrentBytesSubmitterInspectTask(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 40)
	sensitive := "upstream-sensitive-marker"

	tests := []struct {
		name      string
		submitter *TorrentBytesSubmitter
		wantFound bool
		wantPath  string
		wantErr   error
	}{
		{
			name: "completed task",
			submitter: NewTorrentBytesSubmitter(&taskInspectingTorrentBytesClient{
				task:  Task{Hash: hash, State: TaskStateCompleted, ContentPath: "/downloads/fixture.mkv"},
				found: true,
			}),
			wantFound: true,
			wantPath:  "/downloads/fixture.mkv",
		},
		{
			name:      "client without inspector",
			submitter: NewTorrentBytesSubmitter(&scriptedTorrentBytesClient{}),
			wantErr:   ErrReconciliationUnavailable,
		},
		{
			name: "dependency error is sanitized",
			submitter: NewTorrentBytesSubmitter(&taskInspectingTorrentBytesClient{
				err: errors.New(sensitive),
			}),
			wantErr: ErrReconciliationUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task, found, err := tt.submitter.InspectTask(t.Context(), "", hash)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("InspectTask() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatal("InspectTask() error leaked dependency detail")
			}
			if found != tt.wantFound || task.ContentPath != tt.wantPath {
				t.Fatalf("InspectTask() = (%#v, %v), want path %q found %v", task, found, tt.wantPath, tt.wantFound)
			}
		})
	}
}

func TestTorrentBytesSubmitterInspectTaskContent(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("b", 40)
	sensitive := "upstream-sensitive-marker"
	want := TaskContent{
		Task: Task{Hash: hash, State: TaskStateCompleted, SavePath: "/downloads"},
		Files: []TaskContentFile{
			{RelativePath: "Fixture.mkv", SizeBytes: 1024},
		},
	}

	tests := []struct {
		name      string
		submitter *TorrentBytesSubmitter
		want      TaskContent
		wantFound bool
		wantErr   error
	}{
		{
			name: "委托内容快照",
			submitter: NewTorrentBytesSubmitter(&taskInspectingTorrentBytesClient{
				content: want, contentFound: true,
			}),
			want:      want,
			wantFound: true,
		},
		{
			name:      "客户端不支持内容快照",
			submitter: NewTorrentBytesSubmitter(&scriptedTorrentBytesClient{}),
			wantErr:   ErrReconciliationUnavailable,
		},
		{
			name: "依赖错误脱敏",
			submitter: NewTorrentBytesSubmitter(&taskInspectingTorrentBytesClient{
				contentErr: errors.New(sensitive),
			}),
			wantErr: ErrReconciliationUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := tt.submitter.InspectTaskContent(t.Context(), "", hash)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("InspectTaskContent() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatal("InspectTaskContent() error 泄露依赖细节")
			}
			if found != tt.wantFound || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("InspectTaskContent() = (%#v, %v), want (%#v, %v)", got, found, tt.want, tt.wantFound)
			}
		})
	}
}

func TestTorrentBytesSubmitterObserveTasksSanitizesDependencyErrors(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("A", 40)
	tests := []struct {
		name      string
		client    *scriptedTorrentBytesClient
		wantState TaskObservationState
		wantErr   error
		forbidden string
	}{
		{
			name: "成功",
			client: &scriptedTorrentBytesClient{observations: map[string]TaskObservation{
				hash: {Hash: hash, State: TaskObservationCompleted},
			}},
			wantState: TaskObservationCompleted,
		},
		{
			name:      "错误脱敏",
			client:    &scriptedTorrentBytesClient{observeErr: errors.New("upstream-sensitive-marker")},
			wantErr:   ErrReconciliationUnavailable,
			forbidden: "upstream-sensitive-marker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewTorrentBytesSubmitter(tt.client).ObserveTasks(t.Context(), []string{hash})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ObserveTasks() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got[hash].State != tt.wantState {
				t.Fatalf("ObserveTasks() = %#v", got)
			}
			if err != nil && tt.forbidden != "" && strings.Contains(err.Error(), tt.forbidden) {
				t.Fatal("ObserveTasks() error 泄露下载器输入")
			}
		})
	}
}

type taskInspectingTorrentBytesClient struct {
	task         Task
	found        bool
	err          error
	content      TaskContent
	contentFound bool
	contentErr   error
}

func (c *taskInspectingTorrentBytesClient) InspectTask(context.Context, string, string) (Task, bool, error) {
	return c.task, c.found, c.err
}

func (c *taskInspectingTorrentBytesClient) InspectTaskContent(context.Context, string, string) (TaskContent, bool, error) {
	return c.content, c.contentFound, c.contentErr
}

func (*taskInspectingTorrentBytesClient) ObserveTasks(_ context.Context, hashes []string) (map[string]TaskObservation, error) {
	result := make(map[string]TaskObservation, len(hashes))
	for _, hash := range hashes {
		result[hash] = TaskObservation{Hash: hash, State: TaskObservationMissing}
	}
	return result, nil
}

func (*taskInspectingTorrentBytesClient) FindByLabel(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (*taskInspectingTorrentBytesClient) FindByHash(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (*taskInspectingTorrentBytesClient) AddTorrentBytes(context.Context, []byte, string, string, bool) error {
	return nil
}
