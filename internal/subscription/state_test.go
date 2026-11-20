package subscription

import "testing"

func TestParseV2ControlState(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    State
		wantErr bool
	}{
		{name: "P maps to pending identification", input: "P", want: PendingIdentification},
		{name: "R maps to active", input: "R", want: Active},
		{name: "S maps to paused", input: "S", want: Paused},
		{name: "N is not a control command", input: "N", wantErr: true},
		{name: "empty state is rejected", input: "", wantErr: true},
		{name: "unknown state is rejected", input: "X", wantErr: true},
		{name: "lowercase state is rejected", input: "r", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseV2ControlState(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseV2ControlState(%q) 未返回错误，得到 %q", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseV2ControlState(%q) 返回错误: %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("ParseV2ControlState(%q) = %q，期望 %q", test.input, got, test.want)
			}
		})
	}
}

func TestV2State(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		want    string
		wantErr bool
	}{
		{name: "pending identification", state: PendingIdentification, want: "P"},
		{name: "pending search", state: PendingSearch, want: "N"},
		{name: "active", state: Active, want: "R"},
		{name: "acquired", state: Acquired, want: "R"},
		{name: "downloading", state: Downloading, want: "R"},
		{name: "pending organization", state: PendingOrganization, want: "R"},
		{name: "organizing", state: Organizing, want: "R"},
		{name: "pending index", state: PendingIndex, want: "R"},
		{name: "indexed", state: Indexed, want: "R"},
		{name: "paused", state: Paused, want: "S"},
		{name: "completed", state: Completed, want: ""},
		{name: "unknown", state: State("unknown"), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := V2State(test.state)
			if test.wantErr {
				if err == nil {
					t.Fatalf("V2State(%q) 未返回错误，得到 %q", test.state, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("V2State(%q) 返回错误: %v", test.state, err)
			}
			if got != test.want {
				t.Fatalf("V2State(%q) = %q，期望 %q", test.state, got, test.want)
			}
		})
	}
}

func TestActiveStateTransitions(t *testing.T) {
	tests := []struct {
		name    string
		current State
		next    State
	}{
		{name: "pending search becomes active", current: PendingSearch, next: Active},
		{name: "active becomes acquired", current: Active, next: Acquired},
		{name: "active can pause", current: Active, next: Paused},
		{name: "paused can resume as active", current: Paused, next: Active},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Transition(test.current, test.next)
			if err != nil {
				t.Fatalf("Transition(%q, %q) 返回错误: %v", test.current, test.next, err)
			}
			if got != test.next {
				t.Fatalf("Transition(%q, %q) = %q，期望 %q", test.current, test.next, got, test.next)
			}
		})
	}
}

func TestTransitionRejectsIllegalJumps(t *testing.T) {
	tests := []struct {
		name    string
		current State
		next    State
	}{
		{name: "identification cannot skip search", current: PendingIdentification, next: Active},
		{name: "active cannot skip acquisition", current: Active, next: Downloading},
		{name: "active cannot return to pending search", current: Active, next: PendingSearch},
		{name: "pending search cannot skip to downloading", current: PendingSearch, next: Downloading},
		{name: "completed is immutable", current: Completed, next: PendingSearch},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Transition(test.current, test.next)
			if err == nil {
				t.Fatalf("Transition(%q, %q) 未返回错误，得到 %q", test.current, test.next, got)
			}
			if got != test.current {
				t.Fatalf("非法转换改变了状态: 得到 %q，期望保持 %q", got, test.current)
			}
		})
	}
}

func TestSubscriptionCompletesOnlyAfterIndexed(t *testing.T) {
	state := PendingIdentification
	path := []State{PendingSearch, Active, Acquired, Downloading, PendingOrganization, Organizing, Indexed, Completed}
	for _, next := range path {
		var err error
		state, err = Transition(state, next)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state != Completed {
		t.Fatalf("最终状态错误: %s", state)
	}
	if _, err := Transition(Downloading, Completed); err == nil {
		t.Fatal("下载完成不得直接完成订阅")
	}
}

func TestRequiredMediaIndexStateTransitions(t *testing.T) {
	state := Organizing
	for _, next := range []State{PendingIndex, Indexed, Completed} {
		var err error
		state, err = Transition(state, next)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state != Completed {
		t.Fatalf("最终状态 = %q，期望 %q", state, Completed)
	}
	if _, err := Transition(PendingIndex, Completed); err == nil {
		t.Fatal("待媒体服务器确认状态不能跳过 indexed")
	}
}

func TestMissingEpisodes(t *testing.T) {
	indexed := map[Episode]bool{{Season: 1, Episode: 1}: true, {Season: 1, Episode: 3}: true}
	missing, err := MissingEpisodes(map[int]int{1: 4}, indexed)
	if err != nil || len(missing) != 2 || missing[0].Episode != 2 || missing[1].Episode != 4 {
		t.Fatalf("缺集结果错误: %#v err=%v", missing, err)
	}
}
