// Command bridge is the LyraNest AirPlay / HomePod streaming bridge.
//
// It is a sidecar: LyraNest keeps the device registry, the media stream and the
// client API, while this process owns mDNS discovery, audio decoding, RTP
// framing, the UDP timing responder and the high-frequency feedback heartbeat.
// That split is what keeps the main server inside its 192MiB memory budget and
// keeps its container image unchanged.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/api"
	"github.com/lyranest/lyranest-airplay-bridge/internal/config"
	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
	"github.com/lyranest/lyranest-airplay-bridge/internal/engine"
	"github.com/lyranest/lyranest-airplay-bridge/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "airplay-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("starting lyranest-airplay-bridge",
		"version", api.Version,
		"port", cfg.Port,
		"engine", cfg.Engine,
		"device_ttl", cfg.DeviceTTL.String(),
		"discovery", cfg.DiscoveryEnabled,
	)

	registry := discovery.NewRegistry(cfg.DeviceTTL)
	browser := discovery.NewBrowser(registry, logger, cfg.IfaceIP)

	manager := engine.NewManager(registry, engine.ManagerConfig{
		Engine:         string(cfg.Engine),
		CLIAirplayPath: cfg.CLIAirplayPath,
		FFmpegPath:     cfg.FFmpegPath,
		SampleRate:     cfg.SampleRate,
		Channels:       cfg.Channels,
		BitDepth:       cfg.BitDepth,
		LatencyMS:      cfg.LatencyMS,
		IfaceIP:        cfg.IfaceIP,
		PublishIP:      os.Getenv("BRIDGE_PUBLISH_IP"),
		Protocol:       cfg.NativeProtocol,
		DefaultVolume:  cfg.DefaultVolume,
	}, logger)

	for _, entry := range cfg.StaticDevices {
		device, err := registry.UpsertStatic(entry, 7000)
		if err != nil {
			logger.Warn("static device rejected", "address", entry, "error", err)
			continue
		}
		logger.Info("static device registered", "id", device.ID, "address", device.Address, "port", device.Port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.DiscoveryEnabled {
		go browser.Run(ctx)
		go sweepLoop(ctx, registry, logger)
	} else {
		logger.Warn("mDNS discovery disabled; only static devices will be available")
	}
	go manager.Reap(ctx, 10*time.Second)

	server := api.New(api.Options{
		Registry:         registry,
		Browser:          browser,
		Manager:          manager,
		Logger:           logger,
		Token:            cfg.BridgeToken,
		ServerURL:        cfg.LyraNestServerURL,
		DiscoveryEnabled: cfg.DiscoveryEnabled,
	})

	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           server.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http control plane listening", "addr", cfg.Addr(), "kernel", manager.EngineName())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager.Shutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", "error", err)
	}
	logger.Info("airplay-bridge stopped")
	return nil
}

// sweepLoop expires devices that stopped announcing, which is how the 60s TTL
// in the architecture document is enforced.
func sweepLoop(ctx context.Context, registry *discovery.Registry, logger *slog.Logger) {
	interval := registry.TTL() / 4
	if interval < time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, device := range registry.Sweep() {
				logger.Info("device expired", "id", device.ID, "name", device.Name, "address", device.Address)
			}
		}
	}
}
