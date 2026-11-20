package downloadflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

func TestSubmitterRegistryReplaceAndResolve(t *testing.T) {
	t.Parallel()

	first := noopTorrentSubmitter{id: "first"}
	second := noopTorrentSubmitter{id: "second"}
	registry, err := NewSubmitterRegistry(map[string]TorrentSubmitter{
		referenceTestDownloaderID: first,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := registry.ResolveSubmitter(" A17C89AF-63FA-42B0-89A0-44D7F3456F2D ")
	if err != nil || got != first {
		t.Fatalf("ResolveSubmitter() = %#v, %v", got, err)
	}
	if err := registry.Replace(map[string]TorrentSubmitter{
		"58b08ab7-c5c1-43da-82af-16d3a3f4912e": second,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveSubmitter(referenceTestDownloaderID); !errors.Is(err, ErrDownloaderUnavailable) {
		t.Fatalf("旧下载器仍可解析: %v", err)
	}
	got, err = registry.ResolveSubmitter("58b08ab7-c5c1-43da-82af-16d3a3f4912e")
	if err != nil || got != second {
		t.Fatalf("新下载器解析失败: %#v, %v", got, err)
	}
}

func TestSubmitterRegistryRejectsInvalidReplacementWithoutMutation(t *testing.T) {
	t.Parallel()

	stable := noopTorrentSubmitter{id: "stable"}
	registry, err := NewSubmitterRegistry(map[string]TorrentSubmitter{referenceTestDownloaderID: stable})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		submitters map[string]TorrentSubmitter
	}{
		{name: "无效 UUID", submitters: map[string]TorrentSubmitter{"not-a-uuid": stable}},
		{name: "空提交器", submitters: map[string]TorrentSubmitter{referenceTestDownloaderID: nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := registry.Replace(tt.submitters); !errors.Is(err, ErrInvalidDownloaderRegistry) {
				t.Fatalf("Replace() error = %v", err)
			}
			got, err := registry.ResolveSubmitter(referenceTestDownloaderID)
			if err != nil || got != stable {
				t.Fatal("失败替换破坏了旧注册表快照")
			}
		})
	}
}

func TestSubmitterRegistryConcurrentResolveAndReplace(t *testing.T) {
	registry, err := NewSubmitterRegistry(map[string]TorrentSubmitter{
		referenceTestDownloaderID: noopTorrentSubmitter{id: "initial"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				_, _ = registry.ResolveSubmitter(referenceTestDownloaderID)
			}
		}()
	}
	for range 100 {
		if err := registry.Replace(map[string]TorrentSubmitter{
			referenceTestDownloaderID: noopTorrentSubmitter{id: "replacement"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
}

type noopTorrentSubmitter struct{ id string }

func (noopTorrentSubmitter) ObserveTasks(context.Context, []string) (map[string]downloader.TaskObservation, error) {
	return map[string]downloader.TaskObservation{}, nil
}

func (noopTorrentSubmitter) Reconcile(context.Context, downloader.Intent) (downloader.Intent, bool, error) {
	return downloader.Intent{}, false, nil
}

func (noopTorrentSubmitter) AddOnce(context.Context, downloader.Intent, downloader.Candidate, []byte) (downloader.Intent, error) {
	return downloader.Intent{}, nil
}
