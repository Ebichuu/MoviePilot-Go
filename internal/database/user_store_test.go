package database

import (
	"errors"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

func TestScanUser(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("scan user")
	tests := []struct {
		name    string
		scanner rowScanner
		want    auth.User
		wantErr error
	}{
		{
			name: "完整用户",
			scanner: scannerFunc(func(dest ...any) error {
				avatar := "avatar.png"
				*dest[0].(*int64) = 42
				*dest[1].(*string) = "alice"
				*dest[2].(*string) = "password-hash"
				*dest[3].(*bool) = true
				*dest[4].(**string) = &avatar
				*dest[5].(*[]byte) = []byte(`{"library":{"read":true}}`)
				return nil
			}),
			want: auth.User{
				ID: 42, Username: "alice", Password: "password-hash", SuperUser: true,
				Avatar:      stringPointer("avatar.png"),
				Permissions: map[string]any{"library": map[string]any{"read": true}},
			},
		},
		{
			name:    "底层扫描错误",
			scanner: scannerFunc(func(...any) error { return wantErr }),
			wantErr: wantErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&Store{}).scanUser(tt.scanner)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("scanUser() error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("scanUser() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func stringPointer(value string) *string {
	return &value
}
