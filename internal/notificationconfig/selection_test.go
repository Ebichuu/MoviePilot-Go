package notificationconfig

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSnapshotSelect(t *testing.T) {
	t.Parallel()

	snapshot, err := Decode(json.RawMessage(`[
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"primary-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库","订阅"],"enabled":true},
		{"name":"Secondary","type":"telegram","config":{"TELEGRAM_TOKEN":"secondary-token","TELEGRAM_CHAT_ID":"2"},"switchs":["资源下载"],"enabled":true},
		{"name":"Disabled","type":"telegram","config":{"TELEGRAM_TOKEN":"disabled-token","TELEGRAM_CHAT_ID":"3"},"switchs":["整理入库"],"enabled":false}
	]`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	tests := []struct {
		name    string
		channel string
		source  string
		mtype   string
		userid  string
		want    []string
	}{
		{name: "无条件匹配所有有效配置", want: []string{"Primary", "Secondary"}},
		{name: "Telegram 渠道", channel: TelegramType, want: []string{"Primary", "Secondary"}},
		{name: "其它渠道不匹配", channel: "webpush", want: []string{}},
		{name: "按来源", source: "Secondary", want: []string{"Secondary"}},
		{name: "来源严格匹配", source: "secondary", want: []string{}},
		{name: "按场景开关", mtype: "整理入库", want: []string{"Primary"}},
		{name: "其它场景开关", mtype: "资源下载", want: []string{"Secondary"}},
		{name: "未知场景", mtype: "媒体服务器", want: []string{}},
		{name: "定向用户绕过场景开关", mtype: "媒体服务器", userid: "42", want: []string{"Primary", "Secondary"}},
		{name: "定向用户仍遵守来源", source: "Secondary", mtype: "整理入库", userid: "42", want: []string{"Secondary"}},
		{name: "空来源时广播", source: "", mtype: "订阅", want: []string{"Primary"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			selected := snapshot.Select(tt.channel, tt.source, tt.mtype, tt.userid)
			got := make([]string, len(selected))
			for index := range selected {
				got[index] = selected[index].Name
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Select() names = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSnapshotSelectReturnsDetachedTargets(t *testing.T) {
	t.Parallel()

	snapshot, err := Decode(json.RawMessage(`[{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true}]`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	selected := snapshot.Select(TelegramType, "", "整理入库", "")
	if len(selected) != 1 {
		t.Fatalf("Select() len = %d, want 1", len(selected))
	}
	selected[0].Switchs[0] = "changed"
	selected[0].Name = "changed"

	again := snapshot.Select(TelegramType, "", "整理入库", "")
	if len(again) != 1 || again[0].Name != "Primary" || !reflect.DeepEqual(again[0].Switchs, []string{"整理入库"}) {
		t.Fatalf("Select() 返回值与快照共享数据: %#v", again)
	}
}
