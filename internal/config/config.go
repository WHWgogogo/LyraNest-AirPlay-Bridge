// Package config loads the bridge runtime configuration from the environment.
//
// The bridge is intentionally configuration-light: it is a sidecar that talks
// to exactly one LyraNest server and one LAN. Every knob has a working default
// so `airplay-bridge` can be started with no arguments at all.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// EngineKind selects the audio push kernel used for streaming.
type EngineKind string

const (
	// EngineAuto prefers the mature external cliairplay kernel and silently
	// falls back to the built-in native RAOP sender when it is not installed.
	EngineAuto EngineKind = "auto"
	// EngineCLIAirplay forces the external Music Assistant cliairplay kernel.
	EngineCLIAirplay EngineKind = "cliairplay"
	// EngineNative forces the built-in pure-Go RAOP sender.
	EngineNative EngineKind = "native"
)

// Config is the fully resolved bridge configuration.
type Config struct {
	// Port is the HTTP control port. Default 8092.
	Port int
	// BindAddress is the HTTP listen address. Default 0.0.0.0.
	BindAddress string
	// BridgeToken, when set, is required in x-bridge-token / Authorization on
	// every /api/* request. Empty means the bridge trusts its network.
	BridgeToken string

	// Engine selects the push kernel.
	Engine EngineKind
	// CLIAirplayPath is the path (or bare name) of the cliairplay binary.
	CLIAirplayPath string
	// FFmpegPath is the path (or bare name) of ffmpeg used to decode
	// non-PCM stream URLs into raw s16le PCM.
	FFmpegPath string
	// NativeProtocol forces the native engine onto a specific RAOP route.
	// One of "auto", "raop". Reserved for future AirPlay 2 native support.
	NativeProtocol string

	// SampleRate / Channels / BitDepth describe the PCM fed to the kernel.
	SampleRate int
	Channels   int
	BitDepth   int

	// LatencyMS is the receiver queue depth hint (native engine).
	LatencyMS int
	// DeviceTTL is how long a discovered device stays in the registry without
	// a fresh mDNS announcement. Default 60s.
	DeviceTTL time.Duration
	// DiscoveryEnabled turns the mDNS browser on or off.
	DiscoveryEnabled bool
	// StaticDevices are `ip:port` (or bare `ip`) entries that are always
	// registered even when mDNS is unavailable. Useful on hosts where
	// multicast is filtered.
	StaticDevices []string
	// IfaceIP optionally pins all sockets (and mDNS) to one local interface.
	IfaceIP string

	// LyraNestServerURL is the default base URL used to rewrite relative
	// stream_url values coming from the LyraNest server.
	LyraNestServerURL string

	// DefaultVolume is applied to a device before the first audio packet.
	DefaultVolume int

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Default returns the built-in defaults, which match the published
// architecture document (port 8092, ffmpeg decode, TTL 60s).
func Default() Config {
	return Config{
		Port:              8092,
		BindAddress:       "0.0.0.0",
		Engine:            EngineAuto,
		CLIAirplayPath:    "cliairplay",
		FFmpegPath:        "ffmpeg",
		NativeProtocol:    "auto",
		SampleRate:        44100,
		Channels:          2,
		BitDepth:          16,
		LatencyMS:         600,
		DeviceTTL:         60 * time.Second,
		DiscoveryEnabled:  true,
		DefaultVolume:     60,
		LyraNestServerURL: "http://127.0.0.1:8080",
		LogLevel:          "info",
	}
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	cfg := Default()

	if v := strings.TrimSpace(os.Getenv("BRIDGE_PORT")); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return cfg, fmt.Errorf("invalid BRIDGE_PORT %q", v)
		}
		cfg.Port = port
	}
	if v := strings.TrimSpace(os.Getenv("BRIDGE_BIND")); v != "" {
		cfg.BindAddress = v
	}
	cfg.BridgeToken = strings.TrimSpace(os.Getenv("BRIDGE_TOKEN"))

	if v := strings.TrimSpace(os.Getenv("BRIDGE_ENGINE")); v != "" {
		switch EngineKind(strings.ToLower(v)) {
		case EngineAuto:
			cfg.Engine = EngineAuto
		case EngineCLIAirplay:
			cfg.Engine = EngineCLIAirplay
		case EngineNative:
			cfg.Engine = EngineNative
		default:
			return cfg, fmt.Errorf("invalid BRIDGE_ENGINE %q (want auto|cliairplay|native)", v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("CLIAIRPLAY_PATH")); v != "" {
		cfg.CLIAirplayPath = v
	}
	if v := strings.TrimSpace(os.Getenv("FFMPEG_PATH")); v != "" {
		cfg.FFmpegPath = v
	}
	if v := strings.TrimSpace(os.Getenv("AIRPLAY_PROTOCOL")); v != "" {
		cfg.NativeProtocol = strings.ToLower(v)
	}

	if err := parseIntEnv("BRIDGE_SAMPLE_RATE", &cfg.SampleRate, 8000, 192000); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("BRIDGE_CHANNELS", &cfg.Channels, 1, 2); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("BRIDGE_BIT_DEPTH", &cfg.BitDepth, 16, 24); err != nil {
		return cfg, err
	}
	// RAOP L16 is 16-bit and cliairplay's high-resolution path is 24-bit, so
	// the range check above is not enough: 17..23 are not usable formats.
	if cfg.BitDepth != 16 && cfg.BitDepth != 24 {
		return cfg, fmt.Errorf("invalid BRIDGE_BIT_DEPTH %d (want 16 or 24)", cfg.BitDepth)
	}
	if err := parseIntEnv("BRIDGE_LATENCY_MS", &cfg.LatencyMS, 100, 3000); err != nil {
		return cfg, err
	}
	if err := parseIntEnv("BRIDGE_DEFAULT_VOLUME", &cfg.DefaultVolume, 0, 100); err != nil {
		return cfg, err
	}

	if v := strings.TrimSpace(os.Getenv("BRIDGE_DEVICE_TTL_SECONDS")); v != "" {
		seconds, err := strconv.Atoi(v)
		if err != nil || seconds < 5 || seconds > 3600 {
			return cfg, fmt.Errorf("invalid BRIDGE_DEVICE_TTL_SECONDS %q", v)
		}
		cfg.DeviceTTL = time.Duration(seconds) * time.Second
	}
	if v := strings.TrimSpace(os.Getenv("BRIDGE_DISCOVERY")); v != "" {
		cfg.DiscoveryEnabled = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := strings.TrimSpace(os.Getenv("AIRPLAY_STATIC_DEVICES")); v != "" {
		for _, entry := range strings.Split(v, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				cfg.StaticDevices = append(cfg.StaticDevices, entry)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("BRIDGE_IFACE")); v != "" {
		cfg.IfaceIP = v
	}
	if v := strings.TrimSpace(os.Getenv("LYRANEST_SERVER_URL")); v != "" {
		cfg.LyraNestServerURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("BRIDGE_LOG_LEVEL")); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}

	return cfg, nil
}

func parseIntEnv(name string, target *int, min, max int) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return fmt.Errorf("invalid %s %q (want %d..%d)", name, raw, min, max)
	}
	*target = value
	return nil
}

// Addr is the HTTP listen address.
func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.BindAddress, c.Port)
}

// DefaultCLIAirplayPath resolves the bundled kernel location used by the
// container image and the local development tree.
func DefaultCLIAirplayPath() string {
	if runtime.GOOS == "windows" {
		return "cliairplay.exe"
	}
	return filepath.Join("bin", "cliairplay")
}

// FrameSize is the byte size of one PCM frame (all channels).
func (c Config) FrameSize() int {
	return c.Channels * (c.BitDepth / 8)
}

// BytesPerSecond is the raw PCM throughput of the configured format.
func (c Config) BytesPerSecond() int {
	return c.SampleRate * c.FrameSize()
}
