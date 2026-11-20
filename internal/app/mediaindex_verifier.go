package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

var errLocalMediaFingerprintVerification = errors.New("local media fingerprint verification failed")

type localMediaFingerprintVerifier struct{}

func (localMediaFingerprintVerifier) Verify(
	ctx context.Context,
	path string,
	expectedSize int64,
	expectedSHA256 string,
) (bool, error) {
	if ctx == nil || strings.TrimSpace(path) == "" || expectedSize < 0 ||
		len(expectedSHA256) != 64 || expectedSHA256 != strings.TrimSpace(expectedSHA256) {
		return false, mediaindex.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	size, sha256Value, err := organizer.FingerprintFile(ctx, path)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		if errors.Is(err, context.Canceled) {
			return false, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return false, context.DeadlineExceeded
		}
		return false, errLocalMediaFingerprintVerification
	}
	if size != expectedSize {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(sha256Value), []byte(expectedSHA256)) == 1, nil
}

var _ mediaindex.LocalVerifier = localMediaFingerprintVerifier{}
