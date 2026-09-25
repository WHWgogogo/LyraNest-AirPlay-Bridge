package engine

import (
	"testing"

	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
)

func TestTargetFromDevicePrefersTheAirPlayPort(t *testing.T) {
	target := TargetFromDevice(discovery.Device{
		ID:          "40:5B:D8:12:34:56@LivingRoom",
		Name:        "LivingRoom",
		Address:     "192.168.1.105",
		Port:        7000,
		AirPlayPort: 7000,
		RAOPPort:    5000,
		Protocol:    "airplay2",
	})
	if target.Port != 7000 {
		t.Fatalf("port = %d, want the AirPlay control port", target.Port)
	}
}

func TestTargetFromDeviceFallsBackToRAOPPort(t *testing.T) {
	target := TargetFromDevice(discovery.Device{
		ID:       "40:5B:D8:12:34:56@Old",
		Address:  "192.168.1.9",
		RAOPPort: 5000,
		Protocol: "raop",
	})
	if target.Port != 5000 {
		t.Fatalf("port = %d, want 5000", target.Port)
	}
}

func TestTargetFromDeviceDefaultsToAirPlayPort(t *testing.T) {
	target := TargetFromDevice(discovery.Device{ID: "x", Address: "10.0.0.1"})
	if target.Port != 7000 {
		t.Fatalf("port = %d, want the 7000 default", target.Port)
	}
}

func TestTargetCodecs(t *testing.T) {
	cases := []struct {
		cn   string
		want []int
	}{
		{"0,1", []int{0, 1}},
		{"1", []int{1}},
		{"", nil},
		{"0,1,2,3", []int{0, 1, 2, 3}},
		{" 0 , 1 ", []int{0, 1}},
		{"bogus", nil},
		{"0,x", []int{0}},
	}
	for _, testCase := range cases {
		target := Target{TXT: map[string]string{"cn": testCase.cn}}
		got := target.Codecs()
		if len(got) != len(testCase.want) {
			t.Errorf("Codecs(%q) = %v, want %v", testCase.cn, got, testCase.want)
			continue
		}
		for i := range got {
			if got[i] != testCase.want[i] {
				t.Errorf("Codecs(%q) = %v, want %v", testCase.cn, got, testCase.want)
				break
			}
		}
	}
}

// TestAcceptsPCM is the gate that decides whether the built-in kernel can serve
// a device at all: RAOP codec 0 is uncompressed L16, which is what it sends.
func TestAcceptsPCM(t *testing.T) {
	cases := []struct {
		name string
		txt  map[string]string
		want bool
	}{
		{"pcm and alac", map[string]string{"cn": "0,1"}, true},
		{"alac only", map[string]string{"cn": "1"}, false},
		{"pcm only", map[string]string{"cn": "0"}, true},
		{"aac and alac only", map[string]string{"cn": "2,1"}, false},
		{"no codec advertised", map[string]string{}, true},
		{"nil txt", nil, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			target := Target{TXT: testCase.txt}
			if got := target.AcceptsPCM(); got != testCase.want {
				t.Fatalf("AcceptsPCM() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestSessionBasePositionClock(t *testing.T) {
	base := newSessionBase("native", "dev", "LivingRoom")
	base.request = PlayRequest{StartPositionMS: 5000, DurationMS: 60000}
	base.setState(StateBuffering)
	base.beginPlaybackClock()
	base.setState(StatePlaying)

	status := base.snapshot()
	if !status.Active {
		t.Errorf("a playing session must report active")
	}
	if status.State != StatePlaying {
		t.Errorf("state = %q", status.State)
	}
	if status.PositionMS < 5000 {
		t.Errorf("position = %d, want at least the start offset", status.PositionMS)
	}
	if status.DeviceID != "dev" || status.DeviceName != "LivingRoom" || status.Engine != "native" {
		t.Errorf("identity fields wrong: %+v", status)
	}
	if status.Error != nil {
		t.Errorf("error = %v, want nil", status.Error)
	}
}

func TestSessionBasePositionIsClampedToDuration(t *testing.T) {
	base := newSessionBase("native", "dev", "LivingRoom")
	base.request = PlayRequest{DurationMS: 1000}
	base.setState(StatePlaying)
	base.resetPosition(1000)
	status := base.snapshot()
	if status.PositionMS != 1000 {
		t.Errorf("position = %d, want it clamped to the duration", status.PositionMS)
	}
}

func TestSessionBaseErrorState(t *testing.T) {
	base := newSessionBase("native", "dev", "LivingRoom")
	base.setError(errTest)
	status := base.snapshot()
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	if status.Error == nil || *status.Error == "" {
		t.Errorf("error message must be surfaced")
	}
	if status.Active {
		t.Errorf("a failed session must not report active")
	}
}

func TestSessionBaseFinishedIsInactive(t *testing.T) {
	base := newSessionBase("native", "dev", "LivingRoom")
	base.setState(StatePlaying)
	base.markFinished()
	status := base.snapshot()
	if status.State != StateStopped {
		t.Errorf("state = %q, want stopped", status.State)
	}
	if status.Active {
		t.Errorf("a finished session must not report active")
	}
}

func TestNormalizeID(t *testing.T) {
	if got := normalizeID("40:5B:D8:12:34:56@Living Room"); got != "405bd8123456@livingroom" {
		t.Errorf("normalizeID = %q", got)
	}
}

var errTest = testError("decoder exploded")

type testError string

func (e testError) Error() string { return string(e) }
