package notificationconfig

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const notificationConfigToken = "123456:token-secret"

func TestDecodePreservesV2ShapeAndUnknownKeys(t *testing.T) {
	t.Parallel()

	value := json.RawMessage(`[
		{
			"name":"Primary",
			"type":"telegram",
			"config":{
				"TELEGRAM_TOKEN":"` + notificationConfigToken + `",
				"TELEGRAM_CHAT_ID":"123456789",
				"TELEGRAM_USERS":"11,22",
				"TELEGRAM_ADMINS":"11",
				"API_URL":"https://telegram.example/base",
				"future":{"nested":["kept"]}
			},
			"switchs":["资源下载","整理入库"],
			"enabled":true,
			"future_field":{"enabled":true}
		}
	]`)

	snapshot, err := Decode(value)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	assertJSONEqual(t, snapshot.Raw, value)
	if len(snapshot.Configs) != 1 {
		t.Fatalf("Decode() Configs len = %d, want 1", len(snapshot.Configs))
	}
	configured := snapshot.Configs[0]
	if configured.Name != "Primary" || configured.Type != TelegramType || !configured.Enabled {
		t.Fatalf("Decode() Config = %#v", configured)
	}
	if got := configured.Config["TELEGRAM_USERS"]; got != "11,22" {
		t.Fatalf("TELEGRAM_USERS = %#v, want %q", got, "11,22")
	}
	future, ok := configured.Config["future"].(map[string]any)
	if !ok || !reflect.DeepEqual(future["nested"], []any{"kept"}) {
		t.Fatalf("future config = %#v", configured.Config["future"])
	}
	if len(snapshot.Telegrams) != 1 {
		t.Fatalf("Decode() Telegrams len = %d, want 1", len(snapshot.Telegrams))
	}
	telegram := snapshot.Telegrams[0]
	if telegram.Name != "Primary" || telegram.BotToken != notificationConfigToken ||
		telegram.ChatID != "123456789" || telegram.APIURL != "https://telegram.example/base" ||
		!reflect.DeepEqual(telegram.Switchs, []string{"资源下载", "整理入库"}) {
		t.Fatalf("Decode() Telegram = %#v", telegram)
	}
}

func TestDecodeAcceptsAnyTopLevelJSONValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value json.RawMessage
	}{
		{name: "对象", value: json.RawMessage(`{"future":true}`)},
		{name: "布尔值", value: json.RawMessage(`false`)},
		{name: "整数", value: json.RawMessage(`42`)},
		{name: "字符串", value: json.RawMessage(`"telegram"`)},
		{name: "null", value: json.RawMessage(`null`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshot, err := Decode(tt.value)
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			assertJSONEqual(t, snapshot.Raw, tt.value)
			if len(snapshot.Configs) != 0 || len(snapshot.Telegrams) != 0 {
				t.Fatalf("非列表生成运行态配置: %#v", snapshot)
			}
		})
	}
}

func TestDecodeFiltersPythonFalsyListItems(t *testing.T) {
	t.Parallel()

	valid := `{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"` + notificationConfigToken + `","TELEGRAM_CHAT_ID":"123"},"switchs":[],"enabled":true}`
	value := json.RawMessage(`[null,false,0,-0,0.0,0e999,"",[],{},` + valid + `,"keep",1,true,[0],{"other":1}]`)
	want := json.RawMessage(`[` + valid + `,"keep",1,true,[0],{"other":1}]`)

	snapshot, err := Decode(value)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	assertJSONEqual(t, snapshot.Raw, want)
	if len(snapshot.Configs) != 2 {
		t.Fatalf("Decode() Configs len = %d, want 2", len(snapshot.Configs))
	}
	if len(snapshot.Telegrams) != 1 || snapshot.Telegrams[0].Name != "Primary" {
		t.Fatalf("Decode() Telegrams = %#v", snapshot.Telegrams)
	}

	empty, err := Decode(json.RawMessage(`[null,false,0,"",[],{}]`))
	if err != nil {
		t.Fatalf("Decode(empty) error = %v", err)
	}
	if string(empty.Raw) != "null" || len(empty.Configs) != 0 || len(empty.Telegrams) != 0 {
		t.Fatalf("Decode(empty) = %#v, want Raw null", empty)
	}
}

func TestDecodeSkipsInvalidItemsIndependently(t *testing.T) {
	t.Parallel()

	value := json.RawMessage(`[
		"not-an-object",
		{"name":1,"type":"telegram","config":{},"switchs":[],"enabled":true},
		{"name":"bad-config","type":"telegram","config":[],"switchs":[],"enabled":true},
		{"name":"bad-switchs","type":"telegram","config":{},"switchs":"整理入库","enabled":true},
		{"name":"bad-enabled","type":"telegram","config":{},"switchs":[],"enabled":"true"},
		{"name":"bad-token","type":"telegram","config":{"TELEGRAM_TOKEN":123,"TELEGRAM_CHAT_ID":"123"},"switchs":[],"enabled":true},
		{"name":"disabled","type":"telegram","config":{"TELEGRAM_TOKEN":"disabled","TELEGRAM_CHAT_ID":"123"},"switchs":[],"enabled":false},
		{"name":"missing-chat","type":"telegram","config":{"TELEGRAM_TOKEN":"missing"},"switchs":[],"enabled":true},
		{"name":"other","type":"webpush","config":{},"switchs":[],"enabled":true},
		{"name":"valid","type":"telegram","config":{"TELEGRAM_TOKEN":"` + notificationConfigToken + `","TELEGRAM_CHAT_ID":"123"},"switchs":["整理入库"],"enabled":true}
	]`)

	snapshot, err := Decode(value)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(snapshot.Configs) != 5 {
		t.Fatalf("Decode() Configs len = %d, want 5: %#v", len(snapshot.Configs), snapshot.Configs)
	}
	if len(snapshot.Telegrams) != 1 || snapshot.Telegrams[0].Name != "valid" {
		t.Fatalf("Decode() Telegrams = %#v", snapshot.Telegrams)
	}
}

func TestDecodeDuplicateNamesUseLastValueAndFirstPosition(t *testing.T) {
	t.Parallel()

	value := json.RawMessage(`[
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"old-token","TELEGRAM_CHAT_ID":"1"},"switchs":["整理入库"],"enabled":true},
		{"name":"Secondary","type":"telegram","config":{"TELEGRAM_TOKEN":"secondary-token","TELEGRAM_CHAT_ID":"2"},"switchs":["整理入库"],"enabled":true},
		{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"new-token","TELEGRAM_CHAT_ID":"3"},"switchs":["资源下载"],"enabled":true}
	]`)

	snapshot, err := Decode(value)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	assertJSONEqual(t, snapshot.Raw, value)
	if len(snapshot.Configs) != 2 || snapshot.Configs[0].Name != "Primary" || snapshot.Configs[1].Name != "Secondary" {
		t.Fatalf("Decode() Configs order = %#v, want Primary then Secondary", snapshot.Configs)
	}
	if got := snapshot.Configs[0].Config["TELEGRAM_TOKEN"]; got != "new-token" {
		t.Fatalf("Primary config token = %#v, want last value", got)
	}
	if len(snapshot.Telegrams) != 2 || snapshot.Telegrams[0].Name != "Primary" || snapshot.Telegrams[1].Name != "Secondary" {
		t.Fatalf("Decode() Telegrams order = %#v, want Primary then Secondary", snapshot.Telegrams)
	}
	if snapshot.Telegrams[0].BotToken != "new-token" || snapshot.Telegrams[0].ChatID != "3" {
		t.Fatalf("Primary Telegram = %#v, want last value", snapshot.Telegrams[0])
	}
	selected := snapshot.Select(TelegramType, "", "资源下载", "")
	if len(selected) != 1 || selected[0].Name != "Primary" || selected[0].BotToken != "new-token" {
		t.Fatalf("Select() = %#v, want updated Primary", selected)
	}
}

func TestDecodeRejectsInvalidJSONWithoutLeakingInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value json.RawMessage
	}{
		{name: "空输入", value: nil},
		{name: "不完整对象", value: json.RawMessage(`{"token":"` + notificationConfigToken + `"`)},
		{name: "尾随值", value: json.RawMessage(`[] "` + notificationConfigToken + `"`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode(tt.value)
			if !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("Decode() error = %v, want ErrInvalidSettings", err)
			}
			if strings.Contains(err.Error(), notificationConfigToken) {
				t.Fatalf("Decode() error 泄漏 Token: %v", err)
			}
		})
	}
}

func TestSnapshotCloneIsDeep(t *testing.T) {
	t.Parallel()

	value := json.RawMessage(`[{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"` + notificationConfigToken + `","TELEGRAM_CHAT_ID":"123","nested":{"items":[{"value":"kept"}]}},"switchs":["整理入库"],"enabled":true}]`)
	snapshot, err := Decode(value)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	cloned := snapshot.Clone()

	cloned.Raw[0] = '{'
	cloned.Configs[0].Name = "Changed"
	cloned.Configs[0].Switchs[0] = "资源下载"
	nested := cloned.Configs[0].Config["nested"].(map[string]any)
	items := nested["items"].([]any)
	items[0].(map[string]any)["value"] = "changed"
	cloned.Telegrams[0].Switchs[0] = "资源下载"

	if snapshot.Raw[0] != '[' || snapshot.Configs[0].Name != "Primary" ||
		snapshot.Configs[0].Switchs[0] != "整理入库" || snapshot.Telegrams[0].Switchs[0] != "整理入库" {
		t.Fatalf("Clone() 与原快照共享顶层可变数据: %#v", snapshot)
	}
	originalNested := snapshot.Configs[0].Config["nested"].(map[string]any)
	originalItems := originalNested["items"].([]any)
	if got := originalItems[0].(map[string]any)["value"]; got != "kept" {
		t.Fatalf("Clone() 与原快照共享嵌套值: %#v", got)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`null`))
	f.Add([]byte(`[null,false,0,"",[],{}]`))
	f.Add([]byte(`[{"name":"Primary","type":"telegram","config":{"TELEGRAM_TOKEN":"token","TELEGRAM_CHAT_ID":"123"},"switchs":["整理入库"],"enabled":true}]`))
	f.Add([]byte(`{"future":{"nested":[1,true,null]}}`))

	f.Fuzz(func(t *testing.T, input []byte) {
		snapshot, err := Decode(json.RawMessage(input))
		if err != nil {
			if !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("Decode() error = %v, want ErrInvalidSettings", err)
			}
			return
		}
		if !json.Valid(snapshot.Raw) {
			t.Fatalf("Decode() Raw is invalid JSON: %q", snapshot.Raw)
		}
		cloned := snapshot.Clone()
		if len(cloned.Raw) > 0 {
			cloned.Raw[0] ^= 0xff
		}
		_ = snapshot.Select(TelegramType, "", "整理入库", "")
	})
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("got JSON invalid: %v", err)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("want JSON invalid: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}
