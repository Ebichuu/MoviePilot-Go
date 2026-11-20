package httpapi

import (
	"bytes"
	"errors"
)

type siteFlag struct {
	Set   bool
	Value int
}

func (flag *siteFlag) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "true", "1":
		*flag = siteFlag{Set: true, Value: 1}
	case "false", "0":
		*flag = siteFlag{Set: true, Value: 0}
	case "null":
		*flag = siteFlag{}
	default:
		return errors.New("site flag must be true, false, 0, 1, or null")
	}
	return nil
}
