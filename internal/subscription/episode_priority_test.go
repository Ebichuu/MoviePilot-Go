package subscription

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestNormalizeEpisodePrioritiesMatchesFrozenV2(t *testing.T) {
	t.Parallel()
	fixture := readR08FactsFixture(t)
	for _, test := range fixture.Normalization {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeEpisodePriorities(test.Input)
			if err != nil || !reflect.DeepEqual(got, test.Expected) {
				t.Fatalf("priorities=%v want=%v err=%v", got, test.Expected, err)
			}
		})
	}
}

func TestNormalizeEpisodePrioritiesTypedAndMalformedValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input any
		want  map[int]int
	}{
		{"typed integer map", map[int]int{1: 100, 2: -1}, map[int]int{1: 100, 2: -1}},
		{"typed string map", map[string]int{"01": 100, "bad": 90}, map[int]int{1: 100}},
		{"json numbers", map[string]any{"1": json.Number("100"), "2": json.Number("90.9")}, map[int]int{1: 100, 2: 90}},
		{"non finite values", map[string]any{"1": math.Inf(1), "2": math.NaN(), "3": 100}, map[int]int{3: 100}},
		{"oversized numbers", map[string]any{"1": math.Ldexp(1, 100), "2": "999999999999999999999999999999999999"}, map[int]int{}},
		{"equal aliases", map[string]any{"01": 100, "1": 100}, map[int]int{1: 100}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeEpisodePriorities(test.input)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("priorities=%v want=%v err=%v", got, test.want, err)
			}
			got[1] = 0
			if original, ok := test.input.(map[int]int); ok && original[1] != 100 {
				t.Fatal("normalization mutated input")
			}
		})
	}
}

func TestNormalizeEpisodePrioritiesRejectsConflictingAliasesAndCapacity(t *testing.T) {
	t.Parallel()
	tooMany := make(map[int]int, maxTVTargetFacts+1)
	for index := range maxTVTargetFacts + 1 {
		tooMany[index] = 100
	}
	tests := []struct {
		name  string
		input any
	}{
		{"conflicting aliases", map[string]any{"1": 90, "01": 100}},
		{"typed conflicting aliases", map[string]int{"1": 90, "+1": 100}},
		{"capacity", tooMany},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for range 20 {
				if _, err := NormalizeEpisodePriorities(test.input); !errors.Is(err, ErrInvalidTVTarget) {
					t.Fatalf("expected stable error, got %v", err)
				}
			}
		})
	}
}
