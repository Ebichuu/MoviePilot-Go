package database

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeDownloadIntentOwnerLabels(t *testing.T) {
	t.Parallel()
	first := "mp-" + strings.Repeat("a", 32)
	second := "mp-" + strings.Repeat("b", 32)
	tests := []struct {
		name    string
		labels  []string
		want    []string
		wantErr error
	}{
		{name: "空输入", labels: nil, want: []string{}},
		{name: "去重并保留顺序", labels: []string{" " + first + " ", second, first}, want: []string{first, second}},
		{name: "拒绝非标签", labels: []string{"password-secret"}, wantErr: ErrInvalidDownloadIntentOwnerLookup},
		{name: "拒绝大写摘要", labels: []string{"mp-" + strings.Repeat("A", 32)}, wantErr: ErrInvalidDownloadIntentOwnerLookup},
		{name: "拒绝超量", labels: make([]string, maxDownloadIntentOwnerLabels+1), wantErr: ErrInvalidDownloadIntentOwnerLookup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeDownloadIntentOwnerLabels(tt.labels)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("normalizeDownloadIntentOwnerLabels() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDownloadIntentOwnerLabels() error = %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("normalizeDownloadIntentOwnerLabels() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestListDownloadIntentOwnersDependencyBoundary(t *testing.T) {
	t.Parallel()
	var store *Store
	owners, err := store.ListDownloadIntentOwners(t.Context(), nil)
	if err != nil || len(owners) != 0 {
		t.Fatalf("空查询 = %#v, %v", owners, err)
	}
	_, err = store.ListDownloadIntentOwners(t.Context(), []string{"mp-" + strings.Repeat("a", 32)})
	if !errors.Is(err, ErrDownloadIntentOwnerLookup) {
		t.Fatalf("未初始化 Store error = %v, want ErrDownloadIntentOwnerLookup", err)
	}
}

func TestListDownloadIntentOwnersSQLUsesSingleBoundedQuery(t *testing.T) {
	t.Parallel()
	query := strings.Join(strings.Fields(listDownloadIntentOwnersSQL), " ")
	for _, required := range []string{
		"JOIN users AS u ON u.username=di.username",
		"WHERE di.label=ANY($1::text[])",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("owner query 缺少 %q: %s", required, query)
		}
	}
}
