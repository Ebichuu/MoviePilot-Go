package database

import (
	"strings"
	"testing"
)

func TestSetSettingRejectsUnencodableValueBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()

	err := (&Store{}).SetSetting(t.Context(), "unencodable-setting", make(chan int), false)
	if err == nil || !strings.Contains(err.Error(), "编码设置") {
		t.Fatalf("SetSetting(unencodable) error = %v", err)
	}
}
