package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSubscriptionRSSSitesSQLShape(t *testing.T) {
	t.Parallel()

	query := compactSubscriptionPreciseSearchSQL(getSubscriptionRSSSitesSQL)
	for _, fragment := range []string{
		"COUNT(*) > 0",
		"completed_at IS NULL",
		"v2_state IN ('R', 'P')",
		"config->'sites'",
		"jsonb_agg",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("RSS 订阅站点快照查询缺少 %q: %s", fragment, query)
		}
	}
	for _, fragment := range []string{"username", "title", "media_type", "internal_state"} {
		if strings.Contains(query, fragment) {
			t.Fatalf("RSS 订阅站点快照查询不应读取 %q: %s", fragment, query)
		}
	}
}

func TestScanSubscriptionRSSSites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		hasSubscriptions bool
		raw              string
		want             SubscriptionRSSSitesSnapshot
	}{
		{
			name: "没有订阅",
			raw:  `[]`,
			want: SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}},
		},
		{
			name:             "只有非 R P 订阅",
			hasSubscriptions: true,
			raw:              `[]`,
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{},
			},
		},
		{
			name:             "保留独立选择并稳定去重",
			hasSubscriptions: true,
			raw:              `[[3,1,3],[2],[3,1],[9,7]]`,
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{1, 3}, {2}, {7, 9}},
			},
		},
		{
			name:             "空选择保留为独立组",
			hasSubscriptions: true,
			raw:              `[[],null,{},"bad"]`,
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{}},
			},
		},
		{
			name:             "非法元素按 V2 语义忽略",
			hasSubscriptions: true,
			raw:              `[[4.0,"5",2.5,null,1,true,4]]`,
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{1, 4}},
			},
		},
		{
			name:             "损坏聚合值按空选择处理",
			hasSubscriptions: true,
			raw:              `{`,
			want: SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			row := subscriptionRSSSitesRowFunc(func(dest ...any) error {
				*dest[0].(*bool) = tt.hasSubscriptions
				*dest[1].(*[]byte) = []byte(tt.raw)
				return nil
			})
			got, err := scanSubscriptionRSSSites(row)
			if err != nil {
				t.Fatalf("scanSubscriptionRSSSites() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("scanSubscriptionRSSSites() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestScanSubscriptionRSSSitesReturnsScanError(t *testing.T) {
	t.Parallel()

	want := errors.New("sensitive SELECT failure")
	_, err := scanSubscriptionRSSSites(subscriptionRSSSitesRowFunc(func(...any) error { return want }))
	if !errors.Is(err, want) {
		t.Fatalf("scanSubscriptionRSSSites() error = %v, want %v", err, want)
	}
}

func TestGetSubscriptionRSSSitesBoundaries(t *testing.T) {
	t.Parallel()

	var store *Store
	if _, err := store.GetSubscriptionRSSSites(nil); !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) {
		t.Fatalf("nil context error = %v, want persistence error", err)
	}
	if _, err := store.GetSubscriptionRSSSites(context.Background()); !errors.Is(err, ErrSubscriptionPreciseSearchPersistence) {
		t.Fatalf("nil store error = %v, want persistence error", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.GetSubscriptionRSSSites(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v, want context.Canceled", err)
	}
}

type subscriptionRSSSitesRowFunc func(...any) error

func (scan subscriptionRSSSitesRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}
