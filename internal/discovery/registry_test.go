package discovery

import (
	"testing"
	"time"
)

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"40:5b:d8:12:34:56": "40:5B:D8:12:34:56",
		"405BD8123456":      "40:5B:D8:12:34:56",
		"40-5b-d8-12-34-56": "40:5B:D8:12:34:56",
		"40.5b.d8.12.34.56": "40:5B:D8:12:34:56",
		"":                  "",
		"nope":              "",
		"40:5b:d8:12:34":    "",
	}
	for input, want := range cases {
		if got := normalizeMAC(input); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMacFromInstanceName(t *testing.T) {
	if got := macFromInstanceName("405BD8123456@Living Room"); got != "40:5B:D8:12:34:56" {
		t.Fatalf("unexpected mac %q", got)
	}
	if got := macFromInstanceName("Living Room"); got != "" {
		t.Fatalf("expected empty mac, got %q", got)
	}
}

// TestRegistryMergesAirPlayAndRAOPRecords is the behaviour the REST API relies
// on: one speaker, two mDNS services, one device entry.
func TestRegistryMergesAirPlayAndRAOPRecords(t *testing.T) {
	registry := NewRegistry(60 * time.Second)
	now := time.Now()

	raop := Device{
		Address:  "192.168.1.105",
		Port:     5000,
		RAOPPort: 5000,
		MAC:      "40:5b:d8:12:34:56",
		Name:     "客厅 HomePod mini",
		Protocol: "raop",
		Source:   SourceMDNS,
		LastSeen: now,
		TXT:      map[string]string{"cn": "0,1", "am": "AudioAccessory5,1", "sr": "44100"},
	}
	airplay := Device{
		Address:     "192.168.1.105",
		Port:        7000,
		AirPlayPort: 7000,
		MAC:         "40:5B:D8:12:34:56",
		Name:        "客厅 HomePod mini",
		Protocol:    "airplay2",
		Features:    "0x4A7FCA00,0x3C356BD0",
		Model:       "AudioAccessory5,1",
		Source:      SourceMDNS,
		LastSeen:    now,
		TXT:         map[string]string{"deviceid": "40:5B:D8:12:34:56", "features": "0x4A7FCA00,0x3C356BD0"},
	}

	registry.Upsert(raop)
	registry.Upsert(airplay)

	devices := registry.List()
	if len(devices) != 1 {
		t.Fatalf("expected the two records to merge into one device, got %d: %+v", len(devices), devices)
	}
	device := devices[0]
	if device.Protocol != "airplay2" {
		t.Errorf("protocol = %q, want airplay2", device.Protocol)
	}
	if device.Port != 7000 {
		t.Errorf("port = %d, want the AirPlay control port 7000", device.Port)
	}
	if device.MAC != "40:5B:D8:12:34:56" {
		t.Errorf("mac = %q", device.MAC)
	}
	if device.Model != "AudioAccessory5,1" {
		t.Errorf("model = %q", device.Model)
	}
	if !device.SupportsPCM() {
		t.Errorf("expected codec 0 (L16 PCM) to be advertised")
	}
	if len(device.Codecs()) != 2 {
		t.Errorf("codecs = %v, want [0 1]", device.Codecs())
	}
}

func TestRegistryExpiresAfterTTL(t *testing.T) {
	registry := NewRegistry(60 * time.Second)
	base := time.Now()
	registry.now = func() time.Time { return base }

	registry.Upsert(Device{
		ID:       "aa:bb:cc:dd:ee:ff@Kitchen",
		MAC:      "AA:BB:CC:DD:EE:FF",
		Name:     "Kitchen",
		Address:  "192.168.1.20",
		Port:     7000,
		Source:   SourceMDNS,
		LastSeen: base,
	})
	if registry.Count() != 1 {
		t.Fatalf("expected 1 device")
	}

	// Just inside the TTL the device must survive.
	registry.now = func() time.Time { return base.Add(59 * time.Second) }
	if expired := registry.Sweep(); len(expired) != 0 {
		t.Fatalf("device expired too early: %+v", expired)
	}
	if registry.Count() != 1 {
		t.Fatalf("device disappeared before the TTL")
	}

	// Past the TTL it must be reaped.
	registry.now = func() time.Time { return base.Add(61 * time.Second) }
	expired := registry.Sweep()
	if len(expired) != 1 {
		t.Fatalf("expected the device to expire, got %+v", expired)
	}
	if registry.Count() != 0 {
		t.Fatalf("expected an empty registry, got %d", registry.Count())
	}
}

func TestStaticDeviceNeverExpires(t *testing.T) {
	registry := NewRegistry(60 * time.Second)
	base := time.Now()
	registry.now = func() time.Time { return base }

	if _, err := registry.UpsertStatic("192.168.1.50", 7000); err != nil {
		t.Fatal(err)
	}
	registry.now = func() time.Time { return base.Add(10 * time.Hour) }
	if expired := registry.Sweep(); len(expired) != 0 {
		t.Fatalf("static device must never expire, got %+v", expired)
	}
	if registry.Count() != 1 {
		t.Fatalf("static device vanished")
	}
}

func TestRegistryGetResolvesAliases(t *testing.T) {
	registry := NewRegistry(60 * time.Second)
	registry.Upsert(Device{
		ID:       "40:5B:D8:12:34:56@LivingRoom",
		MAC:      "40:5B:D8:12:34:56",
		Name:     "LivingRoom",
		Address:  "192.168.1.105",
		Port:     7000,
		LastSeen: time.Now(),
	})

	for _, query := range []string{
		"40:5B:D8:12:34:56@LivingRoom",
		"40:5b:d8:12:34:56",
		"LivingRoom",
		"livingroom",
		"192.168.1.105",
	} {
		if _, ok := registry.Get(query); !ok {
			t.Errorf("Get(%q) did not resolve", query)
		}
	}
	if _, ok := registry.Get("nope"); ok {
		t.Errorf("Get(nope) unexpectedly resolved")
	}
}

func TestRegistrySetBusy(t *testing.T) {
	registry := NewRegistry(0)
	registry.Upsert(Device{
		ID:       "40:5B:D8:12:34:56@LivingRoom",
		MAC:      "40:5B:D8:12:34:56",
		Name:     "LivingRoom",
		LastSeen: time.Now(),
	})
	registry.SetBusy("40:5B:D8:12:34:56@LivingRoom", true)
	device, ok := registry.Get("40:5B:D8:12:34:56@LivingRoom")
	if !ok || !device.IsBusy {
		t.Fatalf("expected the device to be busy")
	}
	registry.SetBusy("40:5B:D8:12:34:56", false)
	device, _ = registry.Get("40:5B:D8:12:34:56@LivingRoom")
	if device.IsBusy {
		t.Fatalf("expected the device to be idle")
	}
}

func TestSplitHostPort(t *testing.T) {
	host, port, err := splitHostPort("192.168.1.10:7000", 7000)
	if err != nil || host != "192.168.1.10" || port != 7000 {
		t.Fatalf("got %q %d %v", host, port, err)
	}
	host, port, err = splitHostPort("192.168.1.10", 5000)
	if err != nil || host != "192.168.1.10" || port != 5000 {
		t.Fatalf("got %q %d %v", host, port, err)
	}
	if _, _, err := splitHostPort("", 7000); err == nil {
		t.Fatal("expected an error for an empty address")
	}
	if _, _, err := splitHostPort("host:99999", 7000); err == nil {
		t.Fatal("expected an error for an out-of-range port")
	}
}

func TestUnescapeDNSLabel(t *testing.T) {
	cases := map[string]string{
		`AABBCCDDEEFF\@Living Room`: "AABBCCDDEEFF@Living Room",
		`Kitchen\.Speaker`:          "Kitchen.Speaker",
		`Back\\slash`:               `Back\slash`,
		`Plain`:                     "Plain",
	}
	for input, want := range cases {
		if got := unescapeDNSLabel(input); got != want {
			t.Errorf("unescapeDNSLabel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPasswordRequired(t *testing.T) {
	cases := []struct {
		txt  map[string]string
		want bool
	}{
		{map[string]string{"pw": "false", "flags": "0x4"}, false},
		{map[string]string{"flags": "0x84"}, true},
		{map[string]string{"sf": "0x80"}, true},
		{map[string]string{"pw": "true"}, true},
		{map[string]string{}, false},
		// 0x200 is the legacy pairing bit, not the password bit.
		{map[string]string{"flags": "0x200"}, false},
	}
	for _, testCase := range cases {
		if got := passwordRequired(testCase.txt); got != testCase.want {
			t.Errorf("passwordRequired(%v) = %v, want %v", testCase.txt, got, testCase.want)
		}
	}
}
