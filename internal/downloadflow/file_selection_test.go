package downloadflow

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestLoadDownloadFileSelectionValidatesLegacySuccessInput(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("fbf5b60c-c46f-4c59-948a-0460d7f6b0d4")
	input := organization.SuccessInput{
		IntentID: intentID,
		Username: " alice ",
		Episodes: []organization.Episode{{Season: 2, Episode: 3}, {Season: 1, Episode: 7}, {Season: 2, Episode: 3}},
	}
	tests := []struct {
		name      string
		configure func(*organization.SuccessInput)
		wantErr   bool
	}{
		{name: "旧输入继续规范化选集"},
		{name: "owner不一致", configure: func(input *organization.SuccessInput) { input.Username = "bob" }, wantErr: true},
		{name: "intentID不一致", configure: func(input *organization.SuccessInput) { input.IntentID = uuid.Nil }, wantErr: true},
		{name: "坐标非法", configure: func(input *organization.SuccessInput) { input.Episodes[0].Episode = 0 }, wantErr: true},
	}
	for _, tt := range tests {
		for _, boundary := range []struct {
			name string
			err  error
		}{
			{name: "下载处理器", err: ErrDownloadStateUnavailable},
			{name: "周期对账", err: ErrDownloadReconciliationUnavailable},
		} {
			t.Run(tt.name+"/"+boundary.name, func(t *testing.T) {
				t.Parallel()

				store := &reconciliationSuccessInputStore{input: input.Clone()}
				if tt.configure != nil {
					tt.configure(&store.input)
				}
				if _, supported := any(store).(DownloadFileSelectionStore); supported {
					t.Fatal("legacy fallback 测试不能提供新选集能力")
				}
				episodes, err := loadDownloadFileSelection(t.Context(), store, "alice", intentID, boundary.err)
				if tt.wantErr {
					if !errors.Is(err, boundary.err) || episodes != nil {
						t.Fatalf("loadDownloadFileSelection() = %#v, %v; want nil/%v", episodes, err, boundary.err)
					}
					return
				}
				want := []downloader.Episode{{Season: 1, Episode: 7}, {Season: 2, Episode: 3}}
				if err != nil || !slices.Equal(episodes, want) {
					t.Fatalf("loadDownloadFileSelection() = %#v, %v; want %#v/nil", episodes, err, want)
				}
			})
		}
	}
}
