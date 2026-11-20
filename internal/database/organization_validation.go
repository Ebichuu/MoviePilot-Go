package database

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func sanitizeOrganizationError(value string) string {
	var builder strings.Builder
	builder.Grow(min(len(value), maxOrganizationErrorLength))
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			continue
		}
		width := utf8.RuneLen(character)
		if width < 0 || builder.Len()+width > maxOrganizationErrorLength {
			break
		}
		builder.WriteRune(character)
	}
	value = strings.TrimSpace(builder.String())
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization", "cookie", "passkey", "token=", "secret="} {
		if strings.Contains(lower, marker) {
			return "organization operation failed"
		}
	}
	if value == "" {
		value = "organization operation failed"
	}
	return value
}

func validOrganizationBatchState(state OrganizationBatchState) bool {
	switch state {
	case OrganizationBatchPlanned, OrganizationBatchOrganizing, OrganizationBatchFailed, OrganizationBatchCompleted:
		return true
	default:
		return false
	}
}

func validOrganizationFileState(state OrganizationFileState) bool {
	switch state {
	case OrganizationFilePlanned, OrganizationFileOrganizing, OrganizationFileFailed,
		OrganizationFileTerminalFailed, OrganizationFileCompleted:
		return true
	default:
		return false
	}
}

func validOrganizationMode(mode organizer.Mode) bool {
	switch mode {
	case organizer.Copy, organizer.Move, organizer.Hardlink, organizer.Symlink:
		return true
	default:
		return false
	}
}

func validOrganizationSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validateOrganizationStore(s *Store, ctx context.Context) error {
	if ctx == nil {
		return ErrOrganizationPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil || s.db.Pool == nil {
		return ErrOrganizationPersistence
	}
	return nil
}

func organizationPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrOrganizationPersistence
}
