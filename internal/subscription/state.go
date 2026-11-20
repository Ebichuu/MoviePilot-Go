package subscription

import (
	"errors"
	"fmt"
	"sort"
)

// State 是订阅内部状态，粒度高于 V2 的 N/R/P/S。
type State string

const (
	PendingIdentification State = "pending_identification"
	PendingSearch         State = "pending_search"
	Active                State = "active"
	Acquired              State = "acquired"
	Downloading           State = "downloading"
	PendingOrganization   State = "pending_organization"
	Organizing            State = "organizing"
	PendingIndex          State = "pending_index"
	Indexed               State = "indexed"
	Completed             State = "completed"
	Paused                State = "paused"
)

var allowedTransitions = map[State]map[State]struct{}{
	PendingIdentification: {PendingSearch: {}, Paused: {}},
	PendingSearch:         {Active: {}, Acquired: {}, Paused: {}},
	Active:                {Acquired: {}, Paused: {}},
	Acquired:              {Downloading: {}, PendingSearch: {}, Paused: {}},
	Downloading:           {PendingOrganization: {}, PendingSearch: {}, Paused: {}},
	PendingOrganization:   {Organizing: {}, Paused: {}},
	Organizing:            {PendingIndex: {}, Indexed: {}, PendingOrganization: {}, Paused: {}},
	PendingIndex:          {Indexed: {}, Paused: {}},
	Indexed:               {Completed: {}, PendingSearch: {}, Paused: {}},
	Paused:                {PendingIdentification: {}, PendingSearch: {}, Active: {}, Downloading: {}, PendingOrganization: {}},
	Completed:             {},
}

// Transition 校验并执行一次状态转换。
func Transition(current, next State) (State, error) {
	if current == next {
		return current, nil
	}
	if _, exists := allowedTransitions[current][next]; !exists {
		return current, fmt.Errorf("订阅状态不允许从 %s 转换到 %s", current, next)
	}
	return next, nil
}

// V2State 将内部状态投影为官方前端需要的状态码。
func V2State(state State) (string, error) {
	switch state {
	case PendingIdentification:
		return "P", nil
	case PendingSearch:
		return "N", nil
	case Active, Acquired, Downloading, PendingOrganization, Organizing, PendingIndex, Indexed:
		return "R", nil
	case Paused:
		return "S", nil
	case Completed:
		return "", nil
	default:
		return "", errors.New("未知订阅状态")
	}
}

// ParseV2ControlState 将 V2 用户控制命令解析为内部目标状态。
func ParseV2ControlState(state string) (State, error) {
	switch state {
	case "P":
		return PendingIdentification, nil
	case "R":
		return Active, nil
	case "S":
		return Paused, nil
	default:
		return "", fmt.Errorf("未知 V2 控制状态: %q", state)
	}
}

// Episode 唯一标识一季中的一集。
type Episode struct {
	Season  int `json:"season"`
	Episode int `json:"episode"`
}

// MissingEpisodes 根据目标总集数和已入库集数计算缺集，结果稳定排序且有硬上限。
func MissingEpisodes(totalBySeason map[int]int, indexed map[Episode]bool) ([]Episode, error) {
	missing := make([]Episode, 0)
	for season, total := range totalBySeason {
		if season < 0 || total < 0 || total > 5000 {
			return nil, errors.New("季号或总集数超出安全范围")
		}
		for episode := 1; episode <= total; episode++ {
			item := Episode{Season: season, Episode: episode}
			if !indexed[item] {
				missing = append(missing, item)
			}
		}
	}
	if len(missing) > 10000 {
		return nil, errors.New("单个订阅缺集超过 10000")
	}
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Season == missing[j].Season {
			return missing[i].Episode < missing[j].Episode
		}
		return missing[i].Season < missing[j].Season
	})
	return missing, nil
}
