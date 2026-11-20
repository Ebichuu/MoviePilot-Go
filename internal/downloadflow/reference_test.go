package downloadflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const referenceTestDownloaderID = "a17c89af-63fa-42b0-89a0-44d7f3456f2d"

func TestProtectAndRestoreResourceLocator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		locator    string
		wantSecret bool
		wantErr    error
	}{
		{name: "纯数字 ID 可明文", locator: "42"},
		{name: "零值视为不透明定位", locator: "0", wantSecret: true},
		{name: "首零数字视为不透明定位", locator: "0042", wantSecret: true},
		{name: "Base64 定位信息必须加密", locator: "ZXhhbXBsZS1kb3duaGFzaA", wantSecret: true},
		{name: "拒绝换行", locator: "42\nsecret", wantErr: ErrInvalidSourceReference},
		{name: "拒绝空值", locator: " ", wantErr: ErrInvalidSourceReference},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resourceID, secret, err := ProtectResourceLocator(tt.locator)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ProtectResourceLocator() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if (secret != "") != tt.wantSecret {
				t.Fatalf("secret present = %v, want %v", secret != "", tt.wantSecret)
			}
			if tt.wantSecret && len(strings.TrimSpace(tt.locator)) >= 8 && strings.Contains(resourceID, strings.TrimSpace(tt.locator)) {
				t.Fatal("稳定资源 ID 泄露原始定位信息")
			}
			restored, err := RestoreResourceLocator(resourceID, secret)
			if err != nil || restored != strings.TrimSpace(tt.locator) {
				t.Fatalf("RestoreResourceLocator() = %q, %v", restored, err)
			}
		})
	}
}

func TestRestoreResourceLocatorRejectsTampering(t *testing.T) {
	t.Parallel()

	resourceID, secret, err := ProtectResourceLocator("opaque-downhash")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		resourceID string
		secret     string
		wantErr    error
	}{
		{name: "摘要被修改", resourceID: strings.Repeat("0", len(resourceID)), secret: secret, wantErr: ErrSourceReferenceIntegrity},
		{name: "秘密被修改", resourceID: resourceID, secret: secret + "x", wantErr: ErrSourceReferenceIntegrity},
		{name: "摘要缺少秘密", resourceID: resourceID, wantErr: ErrInvalidSourceReference},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := RestoreResourceLocator(tt.resourceID, tt.secret); !errors.Is(err, tt.wantErr) {
				t.Fatalf("RestoreResourceLocator() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSourceReferenceCandidateIsDeterministicAndSensitiveFree(t *testing.T) {
	t.Parallel()

	reference := SourceReference{
		SiteID: 7, ResolverKind: " Haidan ", ResolverVersion: 1,
		ResourceID: "sha256:" + strings.Repeat("a", 64), DownloaderID: referenceTestDownloaderID,
	}
	first, err := reference.Candidate("/downloads/tv", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reference.Candidate("/downloads/movies", false)
	if err != nil {
		t.Fatal(err)
	}
	firstKey, err := downloader.CandidateAssociationKey(first)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := downloader.CandidateAssociationKey(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstKey != secondKey {
		t.Fatal("保存路径或暂停标记不应改变来源关联身份")
	}
	if strings.Contains(firstKey, reference.ResourceID) || strings.Contains(firstKey, referenceTestDownloaderID) {
		t.Fatal("候选关联键泄露来源或下载器身份")
	}
}

func FuzzResourceLocatorIntegrity(f *testing.F) {
	f.Add("opaque-locator")
	f.Add("42")
	f.Fuzz(func(t *testing.T, locator string) {
		resourceID, secret, err := ProtectResourceLocator(locator)
		if err != nil {
			return
		}
		restored, err := RestoreResourceLocator(resourceID, secret)
		if err != nil || restored != strings.TrimSpace(locator) {
			t.Fatalf("round trip = %q, %v", restored, err)
		}
	})
}
