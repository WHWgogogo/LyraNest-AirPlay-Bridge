package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// mDNS service types browsed by the bridge.
const (
	ServiceAirPlay = "_airplay._tcp"
	ServiceRAOP    = "_raop._tcp"
	// domain is the conventional mDNS domain for the local link.
	domain = "local."
)

// Browser continuously browses the LAN mDNS services and feeds a Registry.
type Browser struct {
	registry *Registry
	logger   *slog.Logger
	ifaceIP  string
	// rescan is how often the browse query is re-issued. mDNS responders also
	// announce on their own schedule, but re-querying bounds the worst-case
	// detection latency for devices that only answer direct queries.
	rescan time.Duration
}

// NewBrowser creates a browser writing into registry. ifaceIP optionally pins
// the browse to a single local interface address.
func NewBrowser(registry *Registry, logger *slog.Logger, ifaceIP string) *Browser {
	return &Browser{
		registry: registry,
		logger:   logger,
		ifaceIP:  strings.TrimSpace(ifaceIP),
		rescan:   20 * time.Second,
	}
}

// Run browses until ctx is cancelled. It never returns an error: transient
// network failures are logged and retried, because a bridge with a broken
// mDNS socket must still serve its REST API.
func (b *Browser) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, service := range []string{ServiceAirPlay, ServiceRAOP} {
		wg.Add(1)
		go func(service string) {
			defer wg.Done()
			b.browseLoop(ctx, service)
		}(service)
	}
	wg.Wait()
}

func (b *Browser) browseLoop(ctx context.Context, service string) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := b.browseOnce(ctx, service); err != nil && ctx.Err() == nil {
			b.logger.Warn("mDNS browse failed", "service", service, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.rescan):
		}
	}
}

func (b *Browser) browseOnce(ctx context.Context, service string) error {
	resolver, err := zeroconf.NewResolver(b.resolverOptions()...)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry, 32)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for entry := range entries {
			b.ingest(service, entry)
		}
	}()

	browseCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := resolver.Browse(browseCtx, service, domain, entries); err != nil {
		cancel()
		wg.Wait()
		return fmt.Errorf("browse %s: %w", service, err)
	}
	<-browseCtx.Done()
	cancel()
	wg.Wait()
	return nil
}

func (b *Browser) resolverOptions() []zeroconf.ClientOption {
	if b.ifaceIP == "" {
		return nil
	}
	target := net.ParseIP(b.ifaceIP)
	if target == nil {
		return nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var selected []net.Interface
	for _, iface := range ifaces {
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.Equal(target) {
				selected = append(selected, iface)
				break
			}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return []zeroconf.ClientOption{zeroconf.SelectIfaces(selected)}
}

// Refresh performs one synchronous browse round for both service types and
// waits up to timeout for responses. It backs the REST `refresh=1` option.
func (b *Browser) Refresh(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, service := range []string{ServiceAirPlay, ServiceRAOP} {
		wg.Add(1)
		go func(service string) {
			defer wg.Done()
			if err := b.browseOnce(refreshCtx, service); err != nil && refreshCtx.Err() == nil {
				b.logger.Debug("forced mDNS refresh failed", "service", service, "error", err)
			}
		}(service)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout + 500*time.Millisecond):
	}
	return nil
}

func (b *Browser) ingest(service string, entry *zeroconf.ServiceEntry) {
	if entry == nil {
		return
	}
	address := firstIPv4(entry)
	if address == "" {
		return
	}

	txt := map[string]string{}
	for _, field := range entry.Text {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		txt[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	// zeroconf hands back the DNS-SD instance label with its escape sequences
	// intact, so `AABBCCDDEEFF@Living Room` arrives as `AABBCCDDEEFF\@Living\
	// Room`. Undo that before parsing the MAC prefix out of it.
	instance := unescapeDNSLabel(strings.TrimSuffix(entry.Instance, "."))

	observation := Device{
		Address:  address,
		Port:     entry.Port,
		TXT:      txt,
		LastSeen: time.Now(),
		Source:   SourceMDNS,
	}

	switch service {
	case ServiceAirPlay:
		observation.AirPlayPort = entry.Port
		observation.Name = strings.TrimSpace(instance)
		observation.MAC = normalizeMAC(txt["deviceid"])
		observation.Model = txt["model"]
		observation.Features = txt["features"]
		observation.Protocol = "airplay2"
		observation.RequiresPassword = passwordRequired(txt)
		if observation.Name == "" {
			observation.Name = entry.HostName
		}
	case ServiceRAOP:
		observation.RAOPPort = entry.Port
		observation.MAC = macFromInstanceName(instance)
		if _, name, found := strings.Cut(instance, "@"); found {
			observation.Name = strings.TrimSpace(name)
		} else {
			observation.Name = strings.TrimSpace(instance)
		}
		observation.Model = txt["am"]
		observation.RequiresPassword = strings.EqualFold(txt["pw"], "true") || txt["pw"] == "1" || passwordRequired(txt)
		observation.Protocol = "raop"
	default:
		return
	}

	if observation.Name == "" {
		observation.Name = observation.Address
	}
	if observation.ID == "" {
		if observation.MAC != "" {
			observation.ID = fmt.Sprintf("%s@%s", observation.MAC, observation.Name)
		} else {
			observation.ID = fmt.Sprintf("%s@%s", observation.Address, observation.Name)
		}
	}

	stored := b.registry.Upsert(observation)
	b.logger.Debug("mDNS device observed",
		"service", service,
		"instance", entry.Instance,
		"host", entry.HostName,
		"id", stored.ID,
		"mac", stored.MAC,
		"name", stored.Name,
		"address", stored.Address,
		"port", stored.Port,
		"protocol", stored.Protocol,
	)
}

func firstIPv4(entry *zeroconf.ServiceEntry) string {
	if len(entry.AddrIPv4) > 0 && entry.AddrIPv4[0] != nil {
		return entry.AddrIPv4[0].String()
	}
	if len(entry.AddrIPv6) > 0 && entry.AddrIPv6[0] != nil {
		return entry.AddrIPv6[0].String()
	}
	return ""
}

// passwordRequired mirrors pyatv's is_password_required: the AirPlay "pw" TXT
// record, or bit 0x80 of the status flags published as "sf" (AirPlay 1) or
// "flags" (AirPlay 2).
func passwordRequired(txt map[string]string) bool {
	if strings.EqualFold(txt["pw"], "true") || txt["pw"] == "1" {
		return true
	}
	flags := txt["sf"]
	if strings.TrimSpace(flags) == "" {
		flags = txt["flags"]
	}
	return parseHexMask(flags)&0x80 != 0
}

// parseHexMask decodes a `0x...` or bare hexadecimal bitmask. It returns 0 for
// anything it cannot parse, which is the safe default: a device is only treated
// as password protected when it explicitly says so.
func parseHexMask(flags string) int {
	flags = strings.TrimSpace(flags)
	if flags == "" {
		return 0
	}
	flags = strings.TrimPrefix(strings.ToLower(flags), "0x")
	value := 0
	for _, r := range flags {
		digit := 0
		switch {
		case r >= '0' && r <= '9':
			digit = int(r - '0')
		case r >= 'a' && r <= 'f':
			digit = int(r-'a') + 10
		default:
			return 0
		}
		value = value*16 + digit
	}
	return value
}

// unescapeDNSLabel reverses the DNS-SD label escaping (`\.`, `\\`, `\@` and the
// `\DDD` decimal form) that mDNS responders put on the wire.
func unescapeDNSLabel(value string) string {
	if !strings.Contains(value, "\\") {
		return value
	}
	var builder strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			builder.WriteByte(value[i])
			i++
			continue
		}
		i++
		if i >= len(value) {
			break
		}
		if i+2 < len(value) && isDigit(value[i]) && isDigit(value[i+1]) && isDigit(value[i+2]) {
			code, err := strconv.Atoi(value[i : i+3])
			if err == nil {
				builder.WriteByte(byte(code))
				i += 3
				continue
			}
		}
		builder.WriteByte(value[i])
		i++
	}
	return builder.String()
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
