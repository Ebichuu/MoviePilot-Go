package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalMediaFingerprintVerifier(t *testing.T) {
	t.Parallel()
	content := []byte("moviepilot-media-index-fixture")
	path := filepath.Join(t.TempDir(), "video.mkv")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	wantSHA256 := hex.EncodeToString(sum[:])
	verifier := localMediaFingerprintVerifier{}

	tests := []struct {
		name         string
		path         string
		expectedSize int64
		expectedSHA  string
		want         bool
		wantErr      error
	}{
		{name: "match", path: path, expectedSize: int64(len(content)), expectedSHA: wantSHA256, want: true},
		{name: "size changed", path: path, expectedSize: int64(len(content) + 1), expectedSHA: wantSHA256},
		{name: "content changed", path: path, expectedSize: int64(len(content)), expectedSHA: strings.Repeat("0", 64)},
		{name: "missing file", path: path + ".missing", expectedSize: int64(len(content)), expectedSHA: wantSHA256, wantErr: errLocalMediaFingerprintVerification},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := verifier.Verify(t.Context(), tt.path, tt.expectedSize, tt.expectedSHA)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Verify() = %v, want %v", got, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), tt.path) {
				t.Fatal("Verify() leaked local path")
			}
		})
	}
}

func TestLocalMediaFingerprintVerifierPropagatesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (localMediaFingerprintVerifier{}).Verify(ctx, "video.mkv", 1, strings.Repeat("0", 64))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify() error = %v, want context.Canceled", err)
	}
}
