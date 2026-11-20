package storage

import "testing"

func TestMatchCharacterClass(t *testing.T) {
	cases := []struct {
		name      string
		pattern   string
		character byte
		matched   bool
		remaining string
		ok        bool
	}{
		{"普通字符集命中", "[abc]rest", 'b', true, "rest", true},
		{"普通字符集未命中", "[abc]rest", 'z', false, "rest", true},
		{"区间命中下边界", "[a-z]x", 'a', true, "x", true},
		{"区间命中中值", "[a-z]x", 'm', true, "x", true},
		{"区间命中上边界", "[a-z]x", 'z', true, "x", true},
		// glob 是大小写敏感的。
		{"区间不匹配大写", "[a-z]x", 'M', false, "x", true},
		{"取反命中", "[!abc]x", 'z', true, "x", true},
		{"取反未命中", "[!abc]x", 'a', false, "x", true},
		{"空字符集", "[]x", 'a', false, "x", true},
		// 没有闭合方括号时 pattern 原样返回，调用方按「不是字符集」处理。
		{"未闭合", "[abc", 'a', false, "[abc", false},
		{"只有开括号", "[", 'a', false, "[", false},
		{"区间后还有字符", "[a-c0-2]tail", '1', true, "tail", true},
	}
	for _, testCase := range cases {
		matched, remaining, ok := matchCharacterClass(testCase.pattern, testCase.character)
		if matched != testCase.matched || remaining != testCase.remaining || ok != testCase.ok {
			t.Fatalf("%s: (%q, %c) → (%v, %q, %v)，期望 (%v, %q, %v)",
				testCase.name, testCase.pattern, testCase.character,
				matched, remaining, ok, testCase.matched, testCase.remaining, testCase.ok)
		}
	}
}
