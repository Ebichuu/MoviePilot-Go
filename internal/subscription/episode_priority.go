package subscription

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// NormalizeEpisodePriorities reads V2 episode-quality facts. Invalid entries
// are discarded individually; a non-object becomes an empty map. Oversized
// maps and conflicting aliases fail rather than depending on map iteration.
func NormalizeEpisodePriorities(value any) (map[int]int, error) {
	result := make(map[int]int)
	switch values := value.(type) {
	case map[int]int:
		if len(values) > maxTVTargetFacts {
			return nil, ErrInvalidTVTarget
		}
		for key, priority := range values {
			result[key] = priority
		}
	case map[string]int:
		if len(values) > maxTVTargetFacts {
			return nil, ErrInvalidTVTarget
		}
		for key, priority := range values {
			if err := addEpisodePriority(result, key, priority); err != nil {
				return nil, err
			}
		}
	case map[string]any:
		if len(values) > maxTVTargetFacts {
			return nil, ErrInvalidTVTarget
		}
		for key, priority := range values {
			if err := addEpisodePriority(result, key, priority); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func addEpisodePriority(result map[int]int, key, value any) error {
	episode, keyOK := priorityInteger(key)
	priority, valueOK := priorityInteger(value)
	if !keyOK || !valueOK {
		return nil
	}
	if previous, exists := result[episode]; exists && previous != priority {
		return ErrInvalidTVTarget
	}
	result[episode] = priority
	return nil
}

func priorityInteger(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int32:
		return int(number), true
	case int64:
		converted := int(number)
		return converted, int64(converted) == number
	case bool:
		if number {
			return 1, true
		}
		return 0, true
	case float32:
		return priorityFloatInteger(float64(number))
	case float64:
		return priorityFloatInteger(number)
	case json.Number:
		if integer, err := strconv.ParseInt(string(number), 10, strconv.IntSize); err == nil {
			return int(integer), true
		}
		decimal, err := number.Float64()
		if err != nil {
			return 0, false
		}
		return priorityFloatInteger(decimal)
	case string:
		integer, err := strconv.Atoi(priorityIntegerText(number))
		return integer, err == nil
	default:
		return 0, false
	}
}

func priorityFloatInteger(number float64) (int, bool) {
	limit := math.Ldexp(1, strconv.IntSize-1)
	if math.IsNaN(number) || math.IsInf(number, 0) || number < -limit || number >= limit {
		return 0, false
	}
	// Python int() truncates numeric floats; it rejects fractional strings.
	return int(number), true
}

func priorityIntegerText(value string) string {
	text := strings.Map(func(character rune) rune {
		if character >= '0' && character <= '9' {
			return character
		}
		for _, item := range unicode.Digit.R16 {
			if character >= rune(item.Lo) && character <= rune(item.Hi) && (character-rune(item.Lo))%rune(item.Stride) == 0 {
				return '0' + (character-rune(item.Lo))/rune(item.Stride)%10
			}
		}
		for _, item := range unicode.Digit.R32 {
			if character >= rune(item.Lo) && character <= rune(item.Hi) && (character-rune(item.Lo))%rune(item.Stride) == 0 {
				return '0' + (character-rune(item.Lo))/rune(item.Stride)%10
			}
		}
		return character
	}, strings.TrimSpace(value))
	for index := range len(text) {
		if text[index] != '_' {
			continue
		}
		if index == 0 || index == len(text)-1 || text[index-1] < '0' || text[index-1] > '9' || text[index+1] < '0' || text[index+1] > '9' {
			return ""
		}
	}
	return strings.ReplaceAll(text, "_", "")
}
