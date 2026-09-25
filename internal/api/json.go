package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// flexibleInt64 decodes a JSON field that may arrive as a number or as a
// string.
//
// LyraNest's own clients send `track_id` as a string (library ids are opaque
// strings there), while the published REST example and external callers use a
// number. Rejecting either form would make the bridge unusable from one of
// them, so both are accepted.
type flexibleInt64 int64

// Int64 returns the decoded value.
func (v flexibleInt64) Int64() int64 { return int64(v) }

// UnmarshalJSON implements json.Unmarshaler.
func (v *flexibleInt64) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*v = 0
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			*v = 0
			return nil
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return fmt.Errorf("track_id %q is not an integer", text)
		}
		*v = flexibleInt64(parsed)
		return nil
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("track_id %s is not an integer", trimmed)
	}
	*v = flexibleInt64(parsed)
	return nil
}

// MarshalJSON always emits a number, which is what the architecture document
// specifies on the wire.
func (v flexibleInt64) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(v), 10)), nil
}
