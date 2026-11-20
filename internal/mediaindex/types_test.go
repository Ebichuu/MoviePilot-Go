package mediaindex

import (
	"errors"
	"strings"
	"testing"
)

func TestServerRefValidRequiresStableFingerprint(t *testing.T) {
	t.Parallel()
	valid := ServerRef{
		ID: "server-id", Name: "Primary Emby", Type: "emby", Fingerprint: strings.Repeat("a", 64),
	}
	tests := []struct {
		name   string
		mutate func(*ServerRef)
		want   bool
	}{
		{name: "valid", want: true},
		{name: "missing fingerprint", mutate: func(ref *ServerRef) { ref.Fingerprint = "" }},
		{name: "short fingerprint", mutate: func(ref *ServerRef) { ref.Fingerprint = strings.Repeat("a", 63) }},
		{name: "uppercase fingerprint", mutate: func(ref *ServerRef) { ref.Fingerprint = strings.Repeat("A", 64) }},
		{name: "non hex fingerprint", mutate: func(ref *ServerRef) { ref.Fingerprint = strings.Repeat("g", 64) }},
		{name: "surrounding whitespace", mutate: func(ref *ServerRef) { ref.Fingerprint = " " + strings.Repeat("a", 64) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := valid
			if tt.mutate != nil {
				tt.mutate(&ref)
			}
			if got := ref.Valid(); got != tt.want {
				t.Fatalf("ServerRef.Valid() = %v, want %v for %#v", got, tt.want, ref)
			}
		})
	}
}

func TestServerErrorExposesStableClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		want      error
		notWanted error
	}{
		{
			name: "unavailable",
			err:  &ServerError{Cause: errors.New("sensitive transport detail"), Retryable: true},
			want: ErrServerUnavailable, notWanted: ErrOutcomeUnknown,
		},
		{
			name: "outcome unknown",
			err:  &ServerError{Cause: errors.New("sensitive response detail"), Unknown: true, Retryable: true},
			want: ErrOutcomeUnknown, notWanted: ErrServerUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.want) {
				t.Fatalf("errors.Is(%v, %v) = false", tt.err, tt.want)
			}
			if errors.Is(tt.err, tt.notWanted) {
				t.Fatalf("errors.Is(%v, %v) = true", tt.err, tt.notWanted)
			}
			if strings.Contains(tt.err.Error(), "sensitive") {
				t.Fatalf("error leaked adapter cause: %q", tt.err)
			}
		})
	}
}
