package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestGetDownloadIntentFileSelectionValidation(t *testing.T) {
	t.Parallel()

	intentID := uuid.New()
	tests := []struct {
		name     string
		store    *Store
		ctx      context.Context
		username string
		intentID uuid.UUID
		wantErr  error
	}{
		{name: "空用户名", username: " \t", ctx: t.Context(), intentID: intentID, wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "空意图", username: "alice", ctx: t.Context(), wantErr: ErrInvalidDownloadIntentSuccessInput},
		{name: "空 store", username: "alice", ctx: t.Context(), intentID: intentID, wantErr: ErrDownloadIntentSuccessInputPersistence},
		{name: "空连接", store: &Store{}, username: "alice", ctx: t.Context(), intentID: intentID, wantErr: ErrDownloadIntentSuccessInputPersistence},
		{name: "空连接池", store: &Store{db: &DB{}}, username: "alice", ctx: t.Context(), intentID: intentID, wantErr: ErrDownloadIntentSuccessInputPersistence},
		{name: "空 context", store: &Store{}, username: "alice", intentID: intentID, wantErr: ErrDownloadIntentSuccessInputPersistence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.store.GetDownloadIntentFileSelection(tt.ctx, tt.username, tt.intentID)
			if got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("GetDownloadIntentFileSelection() = %v, %v; want nil, %v", got, err, tt.wantErr)
			}
		})
	}
}

func TestGetDownloadIntentFileSelectionPlanSemantics(t *testing.T) {
	t.Parallel()

	selected := []downloader.Episode{{Season: 0, Episode: 1}, {Season: 1, Episode: 2}}
	tests := []struct {
		name    string
		knownTV bool
		paused  bool
		empty   bool
		want    []downloader.Episode
	}{
		{name: "已知 TV 整季直接下载", knownTV: true},
		{name: "已知 TV 拆包选集", knownTV: true, paused: true, want: selected},
		{name: "未知历史计划未暂停", want: selected},
		{name: "未知历史计划已暂停", paused: true, want: selected},
		{name: "已知 TV 空集直接下载", knownTV: true, empty: true},
		{name: "已知 TV 空集暂停", knownTV: true, paused: true, empty: true},
		{name: "未知历史空集未暂停", empty: true},
		{name: "未知历史空集已暂停", paused: true, empty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := fileSelectionSuccessInputFixture()
			if tt.empty {
				input.Episodes = []organization.Episode{}
			}
			before := input.Clone()
			query := &fileSelectionTestQuery{input: input, paused: tt.paused, knownTV: tt.knownTV}
			got, err := getDownloadIntentFileSelection(t.Context(), query, "alice", input.IntentID)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("getDownloadIntentFileSelection() = %v, %v; want %v", got, err, tt.want)
			}
			if len(query.queries) != 2 {
				t.Fatalf("读取快照和计划次数 = %d, want 2", len(query.queries))
			}
			for _, args := range query.args {
				if !reflect.DeepEqual(args, []any{input.IntentID, "alice"}) {
					t.Fatalf("owner 查询参数 = %#v", args)
				}
			}
			if len(got) > 0 {
				got[0].Episode = 99
			}
			if !reflect.DeepEqual(query.input, before) {
				t.Fatalf("文件选择改写了质量或通知快照: got %#v, want %#v", query.input, before)
			}
		})
	}
}

func TestGetDownloadIntentFileSelectionErrors(t *testing.T) {
	t.Parallel()

	rawError := errors.New("SELECT private_table resource_secret=passkey-forbidden")
	tests := []struct {
		name      string
		configure func(*fileSelectionTestQuery)
		wantErr   error
		wantCalls int
	}{
		{name: "缺成功输入或其他 owner", configure: func(q *fileSelectionTestQuery) { q.inputErr = pgx.ErrNoRows }, wantErr: ErrDownloadIntentSuccessInputNotFound, wantCalls: 1},
		{name: "成功输入数据库失败", configure: func(q *fileSelectionTestQuery) { q.inputErr = rawError }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照意图不一致", configure: func(q *fileSelectionTestQuery) { q.input.IntentID = uuid.New() }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照 owner 不一致", configure: func(q *fileSelectionTestQuery) { q.input.Username = "mallory" }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照集数损坏", configure: func(q *fileSelectionTestQuery) { q.episodesJSON = `[{"Season":1,"Episode":0}]` }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照 JSON 损坏", configure: func(q *fileSelectionTestQuery) { q.episodesJSON = `[{` }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照重复集数未规范化", configure: func(q *fileSelectionTestQuery) { q.input.Episodes = append(q.input.Episodes, q.input.Episodes[0]) }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照媒体字段未规范化", configure: func(q *fileSelectionTestQuery) { q.input.Media.TitleYear = " 未规范化 " }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "快照版本损坏", configure: func(q *fileSelectionTestQuery) { q.input.Version = 99 }, wantErr: ErrDownloadIntentSuccessInputPersistence, wantCalls: 1},
		{name: "缺计划不得当未知", configure: func(q *fileSelectionTestQuery) { q.planErr = pgx.ErrNoRows }, wantErr: ErrDownloadIntentPlanNotFound, wantCalls: 2},
		{name: "计划数据库失败不得当未知", configure: func(q *fileSelectionTestQuery) { q.planErr = rawError }, wantErr: ErrDownloadIntentPlanPersistence, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := fileSelectionSuccessInputFixture()
			query := &fileSelectionTestQuery{input: input, knownTV: true}
			tt.configure(query)
			got, err := getDownloadIntentFileSelection(t.Context(), query, "alice", input.IntentID)
			if got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("getDownloadIntentFileSelection() = %v, %v; want nil, %v", got, err, tt.wantErr)
			}
			if len(query.queries) != tt.wantCalls {
				t.Fatalf("读取次数 = %d, want %d", len(query.queries), tt.wantCalls)
			}
			for _, forbidden := range []string{"SELECT", "private_table", "resource_secret", "passkey-forbidden", "mallory", input.IntentID.String()} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("错误泄露数据库或快照字段 %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestGetDownloadIntentFileSelectionCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cancelAt  int
		wantCalls int
	}{
		{name: "读取前取消", wantCalls: 0},
		{name: "读取成功输入时取消", cancelAt: 1, wantCalls: 1},
		{name: "读取计划时取消", cancelAt: 2, wantCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			query := &fileSelectionTestQuery{input: fileSelectionSuccessInputFixture()}
			if tt.cancelAt == 0 {
				cancel()
			} else {
				query.beforeQuery = func(call int) {
					if call == tt.cancelAt {
						cancel()
					}
				}
			}
			got, err := getDownloadIntentFileSelection(ctx, query, "alice", query.input.IntentID)
			if got != nil || !errors.Is(err, context.Canceled) || len(query.queries) != tt.wantCalls {
				t.Fatalf("取消读取 = %v, %v, calls=%d; want nil, Canceled, %d", got, err, len(query.queries), tt.wantCalls)
			}
		})
	}
}

func TestDownloadIntentFileSelectionPlanSQLIsOwnerScopedAndPublic(t *testing.T) {
	t.Parallel()

	query := compactDatabaseIntentSQL(getDownloadIntentFileSelectionPlanSQL)
	for _, required := range []string{
		"SELECT p.paused, p.tv_coverage IS NOT NULL",
		"JOIN download_intents AS di ON di.id=p.intent_id",
		"WHERE p.intent_id=$1 AND di.username=$2",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("文件选择计划查询缺少 %q: %s", required, query)
		}
	}
	for _, forbidden := range []string{"resource_secret", "resource_id", "downloader_id", "save_path", "settings", "UPDATE", "INSERT", "DELETE"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("文件选择计划查询读取私密字段或改写计划 %q: %s", forbidden, query)
		}
	}
}

func fileSelectionSuccessInputFixture() organization.SuccessInput {
	return organization.SuccessInput{
		IntentID: uuid.New(), Version: organization.CurrentSuccessVersion, Username: "alice",
		Media: organization.MediaContext{MediaType: "电视剧"}, ResourceTerm: "2160p WEB-DL",
		Episodes: []organization.Episode{{Season: 0, Episode: 1}, {Season: 1, Episode: 2}},
	}
}

type fileSelectionTestQuery struct {
	input        organization.SuccessInput
	episodesJSON string
	inputErr     error
	planErr      error
	paused       bool
	knownTV      bool
	queries      []string
	args         [][]any
	beforeQuery  func(int)
}

func (q *fileSelectionTestQuery) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.queries = append(q.queries, sql)
	q.args = append(q.args, args)
	if q.beforeQuery != nil {
		q.beforeQuery(len(q.queries))
	}
	return downloadIntentPlanRowFunc(func(dest ...any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch sql {
		case getDownloadIntentSuccessInputSQL:
			if q.inputErr != nil {
				return q.inputErr
			}
			input := q.input
			*dest[0].(*uuid.UUID), *dest[1].(*int), *dest[2].(*bool) = input.IntentID, input.Version, input.Legacy
			*dest[3].(*string), *dest[4].(*bool), *dest[5].(*bool) = input.Username, input.Manual, input.Background
			*dest[6].(*string), *dest[7].(*string), *dest[8].(*string) = input.Media.Source, input.Media.MediaID, input.Media.TitleYear
			*dest[9].(*string), *dest[10].(*string), *dest[11].(*float64) = input.Media.MediaType, input.Media.Category, input.Media.VoteAverage
			*dest[12].(*string), *dest[13].(*string), *dest[14].(*string) = input.ResourceTerm, input.Media.PosterURL, input.Media.BackdropURL
			encoded, err := json.Marshal(input.Episodes)
			if err != nil {
				return err
			}
			if q.episodesJSON != "" {
				encoded = []byte(q.episodesJSON)
			}
			*dest[15].(*[]byte) = encoded
			return nil
		case getDownloadIntentFileSelectionPlanSQL:
			if q.planErr != nil {
				return q.planErr
			}
			*dest[0].(*bool), *dest[1].(*bool) = q.paused, q.knownTV
			return nil
		default:
			return errors.New("unexpected file selection query")
		}
	})
}
