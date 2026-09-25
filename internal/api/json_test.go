package api

import (
	"encoding/json"
	"testing"
)

// TestFlexibleInt64AcceptsBothForms is a regression guard.
//
// The LyraNest server forwards `track_id` as a JSON string (library ids are
// opaque strings there), while the published REST example and external callers
// use a number. An int64 field would reject one of them with a 400 at play
// time, which is exactly the kind of cross-service mismatch a mock-bridge test
// cannot see.
func TestFlexibleInt64AcceptsBothForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"number", `{"track_id":1024}`, 1024},
		{"string", `{"track_id":"1024"}`, 1024},
		{"string with spaces", `{"track_id":" 1024 "}`, 1024},
		{"empty string", `{"track_id":""}`, 0},
		{"null", `{"track_id":null}`, 0},
		{"absent", `{}`, 0},
		{"zero", `{"track_id":0}`, 0},
		{"large", `{"track_id":"9007199254740993"}`, 9007199254740993},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var payload struct {
				TrackID flexibleInt64 `json:"track_id"`
			}
			if err := json.Unmarshal([]byte(testCase.body), &payload); err != nil {
				t.Fatalf("Unmarshal(%s) failed: %v", testCase.body, err)
			}
			if payload.TrackID.Int64() != testCase.want {
				t.Fatalf("track_id = %d, want %d", payload.TrackID.Int64(), testCase.want)
			}
		})
	}
}

func TestFlexibleInt64RejectsGarbage(t *testing.T) {
	for _, body := range []string{
		`{"track_id":"abc"}`,
		`{"track_id":"12.5"}`,
		`{"track_id":true}`,
	} {
		var payload struct {
			TrackID flexibleInt64 `json:"track_id"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err == nil {
			t.Errorf("Unmarshal(%s) unexpectedly succeeded with %d", body, payload.TrackID.Int64())
		}
	}
}

// TestFlexibleInt64MarshalsAsNumber pins the wire form documented in the
// architecture specification.
func TestFlexibleInt64MarshalsAsNumber(t *testing.T) {
	payload, err := json.Marshal(struct {
		TrackID flexibleInt64 `json:"track_id"`
	}{TrackID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"track_id":1024}` {
		t.Fatalf("marshalled as %s, want a bare number", payload)
	}
}
