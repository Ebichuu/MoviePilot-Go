package httpapi

import (
	"encoding/json"
	"testing"
)

func TestSiteFlagUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  siteFlag
	}{
		{name: "true", input: "true", want: siteFlag{Set: true, Value: 1}},
		{name: "false", input: "false", want: siteFlag{Set: true, Value: 0}},
		{name: "one", input: "1", want: siteFlag{Set: true, Value: 1}},
		{name: "zero", input: "0", want: siteFlag{Set: true, Value: 0}},
		{name: "null", input: "null", want: siteFlag{}},
		{name: "whitespace", input: " \n\t1 \r", want: siteFlag{Set: true, Value: 1}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := siteFlag{Set: true, Value: 99}
			if err := json.Unmarshal([]byte(test.input), &got); err != nil {
				t.Fatalf("UnmarshalJSON(%s) returned error: %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("UnmarshalJSON(%s) = %#v, want %#v", test.input, got, test.want)
			}
		})
	}
}

func TestSiteFlagUnmarshalJSONRejectsInvalidValues(t *testing.T) {
	inputs := []string{
		"2",
		"-1",
		"1.0",
		`"1"`,
		`{}`,
		`[]`,
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			var flag siteFlag
			if err := json.Unmarshal([]byte(input), &flag); err == nil {
				t.Fatalf("UnmarshalJSON(%s) returned nil error", input)
			}
		})
	}
}

func TestSiteFlagImplementsJSONUnmarshaler(t *testing.T) {
	var _ json.Unmarshaler = (*siteFlag)(nil)
}
