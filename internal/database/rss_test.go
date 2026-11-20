package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

func TestRSSMediaRecognitionMigrationDefinesDurableBoundedFields(t *testing.T) {
	content, err := migrationFS.ReadFile("migrations/035_rss_media_recognition.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS recognized_media_title TEXT NOT NULL DEFAULT ''",
		"ADD COLUMN IF NOT EXISTS media_recognize_fail_count INTEGER NOT NULL DEFAULT 1",
		"CHECK (media_recognize_fail_count BETWEEN 0 AND 3)",
		"DROP COLUMN IF EXISTS media_recognize_fail_count",
		"DROP COLUMN IF EXISTS recognized_media_title",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
}

func TestNormalizeRSSItems(t *testing.T) {
	published := time.Date(2026, time.August, 12, 12, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	tests := []struct {
		name    string
		items   []RSSItemInput
		wantErr error
	}{
		{
			name: "normalizes bounded item",
			items: []RSSItemInput{{
				ItemKey:     " sha256:0123456789abcdef ",
				GUID:        " item-1 ",
				DownloadID:  "download-1",
				DetailURL:   "https://tracker.example/details/1",
				DownloadURL: "https://tracker.example/download/1",
				Title:       " Fixture Release ",
				PublishedAt: &published,
				Resource:    json.RawMessage(`{"title":"Fixture Release"}`),
			}},
		},
		{
			name: "rejects sensitive locator",
			items: []RSSItemInput{{
				ItemKey: "sha256:0123456789abcdef", Title: "Fixture",
				DownloadURL: "https://tracker.example/download?passkey=secret",
			}},
			wantErr: ErrInvalidRSSInput,
		},
		{
			name: "rejects sensitive resource",
			items: []RSSItemInput{{
				ItemKey: "sha256:0123456789abcdef", Title: "Fixture",
				Resource: json.RawMessage(`{"nested":{"authorization":"secret"}}`),
			}},
			wantErr: ErrInvalidRSSInput,
		},
		{
			name:    "rejects unbounded page",
			items:   make([]RSSItemInput, maxRSSItemsLimit+1),
			wantErr: ErrInvalidRSSInput,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRSSItems(tt.items)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("normalizeRSSItems() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if len(got) != 1 || got[0].Title != "Fixture Release" || got[0].PublishedAt == nil || got[0].PublishedAt.Location() != time.UTC {
				t.Fatalf("normalizeRSSItems() = %#v", got)
			}
		})
	}
}

func TestNormalizeRSSResource(t *testing.T) {
	tests := []struct {
		name    string
		input   json.RawMessage
		want    string
		wantErr error
	}{
		{name: "empty becomes object", want: `{}`},
		{name: "object is accepted", input: json.RawMessage(`{"seeders":2}`), want: `{"seeders":2}`},
		{name: "V2 private media title is preserved", input: json.RawMessage(`{"_v2_media_title":"Canonical Show","title":"Release"}`), want: `{"_v2_media_title":"Canonical Show","title":"Release"}`},
		{name: "array is rejected", input: json.RawMessage(`[1]`), wantErr: ErrInvalidRSSInput},
		{name: "invalid JSON is rejected", input: json.RawMessage(`{`), wantErr: ErrInvalidRSSInput},
		{name: "secret URL value is rejected", input: json.RawMessage(`{"download_url":"https://tracker.example/download?token=secret"}`), wantErr: ErrInvalidRSSInput},
		{name: "oversized value is rejected", input: json.RawMessage(`{"value":"` + strings.Repeat("x", maxRSSResourceBytes) + `"}`), wantErr: ErrInvalidRSSInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRSSResource(tt.input)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("normalizeRSSResource() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && string(got) != tt.want {
				t.Fatalf("normalizeRSSResource() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFilterRSSItemsByCacheSignaturesUsesRawConcatenation(t *testing.T) {
	t.Parallel()
	cached := map[string]struct{}{"abc": {}}
	items := []RSSItemInput{
		{ItemKey: "sha256:drop", Title: "ab", Resource: json.RawMessage(`{"description":"c"}`)},
		{ItemKey: "sha256:keep", Title: "a", Resource: json.RawMessage(`{"description":"bc"}`)},
	}
	got := filterRSSItemsByCacheSignatures(cached, items)
	if len(got) != 0 {
		t.Fatalf("filtered items = %#v, want both concatenation collisions filtered", got)
	}
}

func TestSanitizeRSSError(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "trims controls", input: "  timeout\r\nfailed\x00  ", want: "timeoutfailed"},
		{name: "empty", input: "  \r\n", want: ""},
		{name: "redacts secret marker", input: "request failed: https://tracker.example/rss?passkey=secret", want: "rss operation failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeRSSError(tt.input); got != tt.want {
				t.Fatalf("sanitizeRSSError(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
	if got := sanitizeRSSError(strings.Repeat("x", maxRSSLastError+100)); len(got) != maxRSSLastError {
		t.Fatalf("sanitizeRSSError() length = %d, want %d", len(got), maxRSSLastError)
	}
}

func TestValidateRSSStore(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		store   *Store
		ctx     context.Context
		wantErr error
	}{
		{name: "nil context", store: &Store{}, wantErr: ErrRSSPersistence},
		{name: "nil store", ctx: context.Background(), wantErr: ErrRSSPersistence},
		{name: "cancelled context", store: &Store{}, ctx: cancelled, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateRSSStore(tt.store, tt.ctx); !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateRSSStore() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if err := rssPersistenceError(nil); !errors.Is(err, ErrRSSPersistence) {
		t.Fatalf("rssPersistenceError(nil) = %v, want ErrRSSPersistence", err)
	}
	if err := rssPersistenceError(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("rssPersistenceError(cancelled) = %v, want context.Canceled", err)
	}
}

func TestCleanRSSHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "trim surrounding whitespace", value: "  fixture-etag\t", want: "fixture-etag"},
		{name: "preserve bounded header", value: strings.Repeat("x", 512), want: strings.Repeat("x", 512)},
		{name: "truncate oversized header", value: strings.Repeat("y", 513), want: strings.Repeat("y", 512)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cleanRSSHeader(tt.value); got != tt.want {
				t.Fatalf("cleanRSSHeader() length/value = %d/%q, want %d/%q", len(got), got, len(tt.want), tt.want)
			}
		})
	}
}

func TestEnsureRSSCursorSQLResetsEndpointScopedState(t *testing.T) {
	t.Parallel()

	query := strings.Join(strings.Fields(ensureRSSCursorSQL), " ")
	for _, fragment := range []string{
		"last_attempt_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.last_attempt_at ELSE NULL END",
		"last_success_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.last_success_at ELSE NULL END",
		"next_poll_at=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.next_poll_at ELSE now() END",
		"lease_token=CASE WHEN site_rss_cursors.endpoint=EXCLUDED.endpoint THEN site_rss_cursors.lease_token ELSE NULL END",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("ensureRSSCursorSQL missing endpoint reset contract %q", fragment)
		}
	}
}

func TestPrepareRSSLocatorSecrets(t *testing.T) {
	box, err := auth.NewSecretBox("rss-locator-secret-master-key")
	if err != nil {
		t.Fatalf("创建 RSS SecretBox: %v", err)
	}
	plain := "https://tracker.example/download?id=42&passkey=fixture"
	input := []RSSItemInput{
		{ItemKey: "sha256:fixture", ResourceSecret: plain},
		{ItemKey: "sha256:empty"},
	}
	prepared, err := (&Store{secretBox: box}).prepareRSSLocatorSecrets(input)
	if err != nil {
		t.Fatalf("prepareRSSLocatorSecrets() error = %v", err)
	}
	if input[0].ResourceSecret != plain {
		t.Fatalf("prepareRSSLocatorSecrets() modified input secret = %q", input[0].ResourceSecret)
	}
	if prepared[0].ResourceSecret == plain || !strings.HasPrefix(prepared[0].ResourceSecret, "v1:") {
		t.Fatalf("prepared secret = %q, want encrypted v1 value", prepared[0].ResourceSecret)
	}
	decrypted, err := box.Decrypt(prepared[0].ResourceSecret)
	if err != nil || decrypted != plain {
		t.Fatalf("prepared secret decrypt = %q, %v; want %q", decrypted, err, plain)
	}
	if prepared[1].ResourceSecret != "" {
		t.Fatalf("empty secret = %q, want empty", prepared[1].ResourceSecret)
	}

	tests := []struct {
		name   string
		store  *Store
		secret string
		want   error
	}{
		{name: "缺少密钥", store: &Store{}, secret: plain, want: ErrRSSPersistence},
		{name: "超长明文", store: &Store{secretBox: box}, secret: strings.Repeat("x", maxRSSLocator+1), want: ErrInvalidRSSInput},
		{name: "换行明文", store: &Store{secretBox: box}, secret: "locator\nsecret", want: ErrInvalidRSSInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, prepareErr := tt.store.prepareRSSLocatorSecrets([]RSSItemInput{{ItemKey: "sha256:fixture", ResourceSecret: tt.secret}})
			if !errors.Is(prepareErr, tt.want) {
				t.Fatalf("prepareRSSLocatorSecrets() error = %v, want %v", prepareErr, tt.want)
			}
		})
	}
}

func TestScanRSSItemDecryptsLocatorSecret(t *testing.T) {
	box, err := auth.NewSecretBox("rss-scan-secret-master-key")
	if err != nil {
		t.Fatalf("创建 RSS SecretBox: %v", err)
	}
	plain := "https://tracker.example/download?id=42&passkey=fixture"
	ciphertext, err := box.Encrypt(plain)
	if err != nil {
		t.Fatalf("加密 RSS locator: %v", err)
	}
	published := time.Date(2026, time.August, 12, 12, 30, 0, 0, time.UTC)
	firstSeen := published.Add(-time.Hour)
	lastSeen := published.Add(time.Minute)
	leaseExpires := published.Add(time.Hour)
	leaseToken := uuidFromRSSFixture(t)
	row := rssItemRowFunc(func(dest ...any) error {
		if len(dest) != 22 {
			return errors.New("unexpected RSS item column count")
		}
		*dest[0].(*int64) = 7
		*dest[1].(*int64) = 3
		*dest[2].(*string) = "sha256:item"
		*dest[3].(*string) = "fixture-guid"
		*dest[4].(*string) = "sha256:resource"
		*dest[5].(*string) = ciphertext
		*dest[6].(*string) = "https://tracker.example/details/42"
		*dest[7].(*string) = "https://tracker.example/download/42"
		*dest[8].(*string) = "Fixture Release"
		*dest[9].(*string) = ""
		*dest[10].(*int) = 1
		*dest[11].(**time.Time) = &published
		*dest[12].(*json.RawMessage) = json.RawMessage(`{"title":"Fixture Release"}`)
		*dest[13].(*time.Time) = firstSeen
		*dest[14].(*time.Time) = lastSeen
		*dest[15].(*string) = "pending"
		*dest[16].(*int) = 1
		*dest[17].(*string) = ""
		*dest[18].(**time.Time) = nil
		*dest[19].(**uuid.UUID) = &leaseToken
		*dest[20].(**time.Time) = &leaseExpires
		*dest[21].(*time.Time) = published
		return nil
	})
	item, err := (&Store{secretBox: box}).scanRSSItem(row)
	if err != nil {
		t.Fatalf("scanRSSItem() error = %v", err)
	}
	if item.ResourceSecret != plain || item.DetailURL != "https://tracker.example/details/42" || item.DownloadURL != "https://tracker.example/download/42" {
		t.Fatalf("scanRSSItem() locator fields = (%q, %q, %q)", item.ResourceSecret, item.DetailURL, item.DownloadURL)
	}
	if item.LeaseToken != leaseToken || item.PublishedAt == nil || !item.PublishedAt.Equal(published) {
		t.Fatalf("scanRSSItem() metadata = %#v", item)
	}

	for _, tt := range []struct {
		name  string
		store *Store
		row   rowScanner
	}{
		{name: "缺少密钥", store: &Store{}, row: row},
		{name: "损坏密文", store: &Store{secretBox: box}, row: rssItemRowFunc(func(dest ...any) error {
			if err := row.Scan(dest...); err != nil {
				return err
			}
			*dest[5].(*string) = "v1:invalid"
			return nil
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, scanErr := tt.store.scanRSSItem(tt.row); !errors.Is(scanErr, ErrRSSPersistence) {
				t.Fatalf("scanRSSItem() error = %v, want %v", scanErr, ErrRSSPersistence)
			}
		})
	}
}

type rssItemRowFunc func(...any) error

func (scan rssItemRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}

func uuidFromRSSFixture(t *testing.T) uuid.UUID {
	t.Helper()
	value, err := uuid.Parse("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatalf("解析 RSS fixture UUID: %v", err)
	}
	return value
}
