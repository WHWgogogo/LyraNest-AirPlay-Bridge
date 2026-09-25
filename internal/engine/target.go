package engine

import (
	"strings"

	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
)

// Target is the resolved receiver endpoint handed to a kernel.
type Target struct {
	// ID is the stable device identity exposed by the REST API.
	ID string
	// Name is the human readable speaker name.
	Name string
	// Address is the receiver IP literal.
	Address string
	// Port is the RTSP control port.
	Port int
	// Protocol is "airplay2" or "raop".
	Protocol string
	// RequiresPassword mirrors the mDNS password bit.
	RequiresPassword bool
	// TXT carries the merged mDNS TXT records, used for kernel route
	// auto-selection and for the RAOP codec negotiation.
	TXT map[string]string
}

// TargetFromDevice converts a discovered device into a kernel target.
func TargetFromDevice(device discovery.Device) Target {
	port := device.AirPlayPort
	if port == 0 {
		port = device.Port
	}
	if port == 0 {
		port = device.RAOPPort
	}
	if port == 0 {
		port = 7000
	}
	return Target{
		ID:               device.ID,
		Name:             device.Name,
		Address:          device.Address,
		Port:             port,
		Protocol:         device.Protocol,
		RequiresPassword: device.RequiresPassword,
		TXT:              device.TXT,
	}
}

// Codecs returns the RAOP codec bitmask the receiver advertised, if any.
// 0 means uncompressed L16 PCM, 1 means ALAC.
func (t Target) Codecs() []int {
	raw := strings.TrimSpace(t.TXT["cn"])
	if raw == "" {
		return nil
	}
	var codecs []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				value = -1
				break
			}
			value = value*10 + int(r-'0')
		}
		if value >= 0 {
			codecs = append(codecs, value)
		}
	}
	return codecs
}

// AcceptsPCM reports whether the receiver accepts uncompressed L16 audio.
// A receiver that advertises no codec list at all is assumed to accept it,
// because every RAOP implementation in the wild does.
func (t Target) AcceptsPCM() bool {
	codecs := t.Codecs()
	if len(codecs) == 0 {
		return true
	}
	for _, codec := range codecs {
		if codec == 0 {
			return true
		}
	}
	return false
}
