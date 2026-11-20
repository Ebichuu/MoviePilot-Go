package downloader

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// identityMetainfo is a minimal valid v1 torrent whose projection is
// name="fixture" and total_size=7; it matches the frozen V2
// `torrent_from_file.name` / `.total_size` inputs used by the duplicate lookup.
const identityMetainfo = "d4:infod6:lengthi7e4:name7:fixture12:piece lengthi16384e6:pieces20:01234567890123456789ee"

const (
	identityMetaName = "fixture"
	identityMetaSize = int64(7)
)

type identityResult struct {
	hash  string
	found bool
	err   error
}

type identityAndResumeTorrentBytesClient struct {
	*scriptedTorrentBytesClient
	identityResults []identityResult
	identityCalls   int
	identityName    string
	identitySize    int64
	resumeCalls     int
	resumeErr       error
}

func (c *identityAndResumeTorrentBytesClient) FindByNameAndSize(
	_ context.Context,
	name string,
	totalSize int64,
) (string, bool, error) {
	c.identityCalls++
	c.identityName = name
	c.identitySize = totalSize
	if len(c.identityResults) == 0 {
		return "", false, nil
	}
	result := c.identityResults[0]
	c.identityResults = c.identityResults[1:]
	return result.hash, result.found, result.err
}

func (c *identityAndResumeTorrentBytesClient) ResumeAddedTorrent(_ context.Context, _ string) error {
	c.resumeCalls++
	return c.resumeErr
}

// TestTorrentBytesSubmitterAdoptsExistingTorrentByIdentity covers the frozen V2
// `app/modules/qbittorrent/__init__.py:198–225` duplicate branch: when the add
// produced no new task, a torrent with the same name and total_size is adopted
// without resuming it again.
func TestTorrentBytesSubmitterAdoptsExistingTorrentByIdentity(t *testing.T) {
	candidate := Candidate{Key: "opaque-identity-candidate", SavePath: "/library/identity"}
	metainfo := []byte(identityMetainfo)

	tests := []struct {
		name         string
		labelResults []bytesLookupResult
		addErr       error
		identity     []identityResult
		wantState    IntentState
		wantHash     string
		wantErr      error
		wantIdentity int
		wantAdopted  bool
	}{
		{
			name:         "明确拒绝后按名称与大小接管已存在任务",
			labelResults: []bytesLookupResult{{}},
			addErr:       ErrDefinitiveAddFailure,
			identity:     []identityResult{{hash: torrentBytesTestHash, found: true}},
			wantState:    IntentDownloading,
			wantHash:     torrentBytesTestHash,
			wantIdentity: 1,
			wantAdopted:  true,
		},
		{
			name:         "新增成功但标签暂不可见时不查名称，保持未知",
			labelResults: []bytesLookupResult{{}},
			identity:     []identityResult{{hash: torrentBytesTestHash, found: true}},
			wantState:    IntentSubmitting,
			wantErr:      ErrReconciliationPending,
			wantIdentity: 0,
		},
		{
			name:         "明确拒绝且无匹配时保持失败",
			labelResults: []bytesLookupResult{{}},
			addErr:       ErrDefinitiveAddFailure,
			identity:     []identityResult{{}},
			wantState:    IntentFailed,
			wantErr:      ErrDefinitiveAddFailure,
			wantIdentity: 1,
		},
		{
			name:         "新增成功且无匹配时保持未知",
			labelResults: []bytesLookupResult{{}},
			identity:     []identityResult{{}},
			wantState:    IntentSubmitting,
			wantErr:      ErrReconciliationPending,
			wantIdentity: 0,
		},
		{
			name:         "名称与大小查询失败时保持未知",
			labelResults: []bytesLookupResult{{}},
			addErr:       ErrDefinitiveAddFailure,
			identity:     []identityResult{{err: errors.New("lookup-" + torrentBytesSecret)}},
			wantState:    IntentSubmitting,
			wantErr:      ErrReconciliationPending,
			wantIdentity: 1,
		},
		{
			name:         "传输错误不按名称与大小接管，保持未知",
			labelResults: []bytesLookupResult{{}},
			addErr:       errors.New("transport-" + torrentBytesSecret),
			identity:     []identityResult{{hash: torrentBytesTestHash, found: true}},
			wantState:    IntentSubmitting,
			wantErr:      ErrReconciliationPending,
			wantIdentity: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := mustTorrentBytesIntent(t, "admin", candidate)
			intent, err := intent.Transition(IntentSubmitting)
			if err != nil {
				t.Fatalf("Transition() error = %v", err)
			}
			client := &identityAndResumeTorrentBytesClient{
				scriptedTorrentBytesClient: &scriptedTorrentBytesClient{
					labelResults: append([]bytesLookupResult(nil), tt.labelResults...),
					addErrors:    []error{tt.addErr},
				},
				identityResults: append([]identityResult(nil), tt.identity...),
			}

			got, err := NewTorrentBytesSubmitter(client).AddOnce(t.Context(), intent, candidate, metainfo)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("AddOnce() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), torrentBytesSecret) {
				t.Fatalf("AddOnce() 泄露依赖错误: %v", err)
			}
			if got.State != tt.wantState || got.TorrentHash != tt.wantHash {
				t.Fatalf("AddOnce() = %#v, want state=%s hash=%q", got, tt.wantState, tt.wantHash)
			}
			if got.AdoptedExisting != tt.wantAdopted {
				t.Fatalf("AdoptedExisting = %v, want %v", got.AdoptedExisting, tt.wantAdopted)
			}
			if client.identityCalls != tt.wantIdentity {
				t.Fatalf("identity calls = %d, want %d", client.identityCalls, tt.wantIdentity)
			}
			if tt.wantIdentity == 1 && (client.identityName != identityMetaName || client.identitySize != identityMetaSize) {
				t.Fatalf("identity lookup = %q/%d, want %q/%d",
					client.identityName, client.identitySize, identityMetaName, identityMetaSize)
			}
			// V2 returns "下载任务已存在" before any resume step.
			if client.resumeCalls != 0 {
				t.Fatalf("identity adoption must not resume, got %d calls", client.resumeCalls)
			}
		})
	}
}
