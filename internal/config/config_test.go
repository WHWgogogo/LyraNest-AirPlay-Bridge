package config

import (
	"testing"
	"time"
)

func TestDefaultsMatchTheArchitectureDocument(t *testing.T) {
	cfg := Default()
	if cfg.Port != 8092 {
		t.Errorf("default port = %d, want 8092", cfg.Port)
	}
	if cfg.DeviceTTL != 60*time.Second {
		t.Errorf("default device TTL = %s, want 60s", cfg.DeviceTTL)
	}
	if cfg.Engine != EngineAuto {
		t.Errorf("default engine = %q, want auto", cfg.Engine)
	}
	if cfg.SampleRate != 44100 || cfg.Channels != 2 || cfg.BitDepth != 16 {
		t.Errorf("default PCM format = %d/%d/%d", cfg.SampleRate, cfg.Channels, cfg.BitDepth)
	}
	if cfg.LyraNestServerURL != "http://127.0.0.1:8080" {
		t.Errorf("default server url = %q", cfg.LyraNestServerURL)
	}
	if cfg.Addr() != "0.0.0.0:8092" {
		t.Errorf("default addr = %q", cfg.Addr())
	}
	if cfg.FrameSize() != 4 {
		t.Errorf("frame size = %d", cfg.FrameSize())
	}
	if cfg.BytesPerSecond() != 176400 {
		t.Errorf("bytes per second = %d", cfg.BytesPerSecond())
	}
}

func TestLoadOverridesFromEnvironment(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "9092")
	t.Setenv("BRIDGE_BIND", "127.0.0.1")
	t.Setenv("BRIDGE_TOKEN", " tok ")
	t.Setenv("BRIDGE_ENGINE", "Native")
	t.Setenv("CLIAIRPLAY_PATH", "/usr/local/bin/cliairplay")
	t.Setenv("FFMPEG_PATH", "/usr/bin/ffmpeg")
	t.Setenv("BRIDGE_SAMPLE_RATE", "48000")
	t.Setenv("BRIDGE_CHANNELS", "1")
	t.Setenv("BRIDGE_LATENCY_MS", "900")
	t.Setenv("BRIDGE_DEFAULT_VOLUME", "42")
	t.Setenv("BRIDGE_DEVICE_TTL_SECONDS", "30")
	t.Setenv("BRIDGE_DISCOVERY", "false")
	t.Setenv("AIRPLAY_STATIC_DEVICES", "192.168.1.10:7000, 192.168.1.11 ,")
	t.Setenv("LYRANEST_SERVER_URL", "http://192.168.1.50:8080/")
	t.Setenv("BRIDGE_LOG_LEVEL", "DEBUG")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9092 || cfg.BindAddress != "127.0.0.1" {
		t.Errorf("addr = %s", cfg.Addr())
	}
	if cfg.BridgeToken != "tok" {
		t.Errorf("token = %q (must be trimmed)", cfg.BridgeToken)
	}
	if cfg.Engine != EngineNative {
		t.Errorf("engine = %q (must be case-insensitive)", cfg.Engine)
	}
	if cfg.CLIAirplayPath != "/usr/local/bin/cliairplay" || cfg.FFmpegPath != "/usr/bin/ffmpeg" {
		t.Errorf("binary paths not applied: %+v", cfg)
	}
	if cfg.SampleRate != 48000 || cfg.Channels != 1 || cfg.LatencyMS != 900 {
		t.Errorf("audio settings not applied: %+v", cfg)
	}
	if cfg.DefaultVolume != 42 {
		t.Errorf("volume = %d", cfg.DefaultVolume)
	}
	if cfg.DeviceTTL != 30*time.Second {
		t.Errorf("ttl = %s", cfg.DeviceTTL)
	}
	if cfg.DiscoveryEnabled {
		t.Errorf("discovery should be disabled")
	}
	if len(cfg.StaticDevices) != 2 {
		t.Errorf("static devices = %v", cfg.StaticDevices)
	}
	if cfg.LyraNestServerURL != "http://192.168.1.50:8080" {
		t.Errorf("server url = %q (trailing slash must be trimmed)", cfg.LyraNestServerURL)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level = %q", cfg.LogLevel)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"port not a number", "BRIDGE_PORT", "abc"},
		{"port out of range", "BRIDGE_PORT", "70000"},
		{"unknown engine", "BRIDGE_ENGINE", "magic"},
		{"sample rate too low", "BRIDGE_SAMPLE_RATE", "10"},
		{"channels too high", "BRIDGE_CHANNELS", "8"},
		{"bit depth invalid", "BRIDGE_BIT_DEPTH", "20"},
		{"latency too high", "BRIDGE_LATENCY_MS", "99999"},
		{"volume out of range", "BRIDGE_DEFAULT_VOLUME", "101"},
		{"ttl too small", "BRIDGE_DEVICE_TTL_SECONDS", "1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(testCase.key, testCase.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s=%s to be rejected", testCase.key, testCase.value)
			}
		})
	}
}

func TestLoadIgnoresBlankValues(t *testing.T) {
	t.Setenv("BRIDGE_PORT", "   ")
	t.Setenv("BRIDGE_ENGINE", "")
	t.Setenv("AIRPLAY_STATIC_DEVICES", " , , ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8092 || cfg.Engine != EngineAuto || len(cfg.StaticDevices) != 0 {
		t.Fatalf("blank environment values must keep the defaults: %+v", cfg)
	}
}

func TestEngineKindValues(t *testing.T) {
	if EngineAuto != "auto" || EngineCLIAirplay != "cliairplay" || EngineNative != "native" {
		t.Fatalf("engine identifiers are part of the documented contract")
	}
}
