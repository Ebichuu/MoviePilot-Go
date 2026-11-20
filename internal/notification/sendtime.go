package notification

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// SendTimePeriod 是 V2 `NotificationSendTime` 的一个允许发送区间，用自当日
// 00:00 起的分钟数表示。
type SendTimePeriod struct {
	StartMinutes int
	EndMinutes   int
}

// ParseSendTimeSchedule 复刻 V2 `MessageQueueManager._parse_schedule`
// （app/helper/message.py:640–680）：
//   - 空值返回空切片；非列表包装成单元素列表；
//   - 逐项跳过空项、非对象项、缺失/空 start 或 end 的项；
//   - 时间只取 `HH:MM` 或 `HH:MM:SS` 的前两段，解析失败即跳过该项。
//
// 为了兼容 JSON 配置来源，字符串/RawMessage/[]byte 会先按 JSON 解码；解码失败
// 视为空配置。
func ParseSendTimeSchedule(value any) []SendTimePeriod {
	items := normalizeSendTimeItems(value)
	if len(items) == 0 {
		return nil
	}
	periods := make([]SendTimePeriod, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		startValue, hasStart := object["start"]
		endValue, hasEnd := object["end"]
		if !hasStart || !hasEnd {
			continue
		}
		startMinutes, startOK := parseClockMinutes(startValue)
		if !startOK {
			continue
		}
		endMinutes, endOK := parseClockMinutes(endValue)
		if !endOK {
			continue
		}
		periods = append(periods, SendTimePeriod{StartMinutes: startMinutes, EndMinutes: endMinutes})
	}
	return periods
}

// WithinSendTime 复刻 V2 `_is_in_scheduled_time`（app/helper/message.py:690–708）：
// 无配置时段时始终允许；否则当前分钟落在任一时段内即允许，跨午夜时段按
// `>= start 或 <= end` 判定。
func WithinSendTime(periods []SendTimePeriod, at time.Time) bool {
	if len(periods) == 0 {
		return true
	}
	current := at.Hour()*60 + at.Minute()
	for _, period := range periods {
		start := period.StartMinutes
		end := period.EndMinutes
		if start <= end {
			if current >= start && current <= end {
				return true
			}
			continue
		}
		if current >= start || current <= end {
			return true
		}
	}
	return false
}

func normalizeSendTimeItems(value any) []any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		return typed
	case json.RawMessage:
		return decodeSendTimeJSON(typed)
	case []byte:
		return decodeSendTimeJSON(typed)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		return decodeSendTimeJSON([]byte(trimmed))
	default:
		return []any{value}
	}
}

func decodeSendTimeJSON(raw []byte) []any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return normalizeSendTimeItems(decoded)
}

// parseClockMinutes 接受 `HH:MM` 或 `HH:MM:SS`（只取前两段），等价 V2 的
// `map(int, parts)` + 分钟换算。
func parseClockMinutes(value any) (int, bool) {
	text, ok := value.(string)
	if !ok {
		return 0, false
	}
	parts := strings.Split(text, ":")
	if len(parts) < 2 {
		return 0, false
	}
	hours, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, false
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, false
	}
	return hours*60 + minutes, true
}
