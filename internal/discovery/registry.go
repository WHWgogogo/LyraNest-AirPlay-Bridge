// Package discovery maintains the LAN AirPlay / HomePod device registry.
//
// Two mDNS service types are watched:
//
//	_airplay._tcp  — the AirPlay 2 control service (port 7000 on HomePod)
//	_raop._tcp     — the legacy RAOP audio service (instance name "MAC@Name")
//
// Both records for one physical device are merged into a single Device so the
// REST API exposes one entry per speaker.
package discovery

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source records how a device entered the registry.
const (
	SourceMDNS   = "mdns"
	SourceStatic = "static"
)

// Device is one AirPlay / HomePod speaker visible to the bridge.
type Device struct {
	// ID is the stable device identity, `<mac>@<name>` when a MAC is known.
	ID string `json:"id"`
	// Name is the human readable speaker name.
	Name string `json:"name"`
	// Model is the Apple model identifier from the `am` TXT record.
	Model string `json:"model"`
	// Address is the IPv4 address of the speaker.
	Address string `json:"address"`
	// Port is the AirPlay RTSP control port.
	Port int `json:"port"`
	// Features is the raw AirPlay feature bitmask, if advertised.
	Features string `json:"features"`
	// Protocol is "airplay2" for AirPlay 2 capable devices, else "raop".
	Protocol string `json:"protocol"`
	// RequiresPassword mirrors the RAOP `pw` / AirPlay `flags` password bit.
	RequiresPassword bool `json:"requires_password"`
	// IsBusy is true while the bridge holds a session on this device.
	IsBusy bool `json:"is_busy"`
	// LastSeen is the last time an announcement refreshed this entry.
	LastSeen time.Time `json:"last_seen"`
	// Source is "mdns" or "static".
	Source string `json:"source"`

	// RAOPPort is the port of the _raop._tcp service (0 when unknown).
	RAOPPort int `json:"raop_port,omitempty"`
	// AirPlayPort is the port of the _airplay._tcp service (0 when unknown).
	AirPlayPort int `json:"airplay_port,omitempty"`
	// MAC is the uppercase colon-separated hardware address, when known.
	MAC string `json:"mac,omitempty"`
	// TXT carries the merged mDNS TXT records used by the push kernel for
	// route auto-selection.
	TXT map[string]string `json:"txt,omitempty"`
}

// SupportsAirPlay2 reports whether the device advertises the AirPlay 2 feature
// set (bit 38 of the `ft`/`features` mask, or a deviceid TXT record).
func (d Device) SupportsAirPlay2() bool {
	if strings.TrimSpace(d.TXT["deviceid"]) != "" {
		return true
	}
	return strings.TrimSpace(d.Features) != ""
}

// Codecs returns the RAOP codec bitmask advertised in `cn`.
// 0 = L16 PCM, 1 = ALAC.
func (d Device) Codecs() []int {
	raw := strings.TrimSpace(d.TXT["cn"])
	if raw == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		if value, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out = append(out, value)
		}
	}
	return out
}

// SupportsPCM reports whether the receiver accepts uncompressed L16 audio.
func (d Device) SupportsPCM() bool {
	for _, codec := range d.Codecs() {
		if codec == 0 {
			return true
		}
	}
	return false
}

// Registry is a concurrency-safe, TTL-expiring device table.
type Registry struct {
	mu      sync.RWMutex
	devices map[string]*Device
	ttl     time.Duration
	now     func() time.Time
}

// NewRegistry creates a registry whose entries expire after ttl without a
// refresh. ttl <= 0 disables expiry.
func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{
		devices: make(map[string]*Device),
		ttl:     ttl,
		now:     time.Now,
	}
}

// TTL returns the configured entry lifetime.
func (r *Registry) TTL() time.Duration { return r.ttl }

// Upsert merges an observation into the registry. The merge key is the MAC
// address when known, otherwise the lower-cased device name.
func (r *Registry) Upsert(observation Device) Device {
	if observation.LastSeen.IsZero() {
		observation.LastSeen = r.now()
	}
	if observation.Source == "" {
		observation.Source = SourceMDNS
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey(observation)
	existing, ok := r.devices[key]
	if !ok {
		stored := observation
		stored.TXT = cloneTXT(observation.TXT)
		if stored.ID == "" {
			stored.ID = key
		}
		r.devices[key] = &stored
		return stored
	}

	mergeDevice(existing, observation)
	return *existing
}

// UpsertStatic registers an operator-supplied device that does not rely on
// mDNS. address may be `ip` or `ip:port`.
func (r *Registry) UpsertStatic(address string, defaultPort int) (Device, error) {
	host, port, err := splitHostPort(address, defaultPort)
	if err != nil {
		return Device{}, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, lookupErr := net.LookupIP(host)
		if lookupErr != nil || len(ips) == 0 {
			return Device{}, fmt.Errorf("cannot resolve %q", host)
		}
		ip = ips[0]
	}
	observation := Device{
		ID:          fmt.Sprintf("%s@%s", ip.String(), host),
		Name:        host,
		Address:     ip.String(),
		Port:        port,
		Protocol:    "airplay2",
		Source:      SourceStatic,
		LastSeen:    r.now(),
		TXT:         map[string]string{"cn": "0,1"},
		RAOPPort:    port,
		AirPlayPort: port,
	}
	observation.LastSeen = time.Time{} // static entries never expire
	return r.Upsert(observation), nil
}

// List returns a snapshot of the live devices sorted by name.
func (r *Registry) List() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Device, 0, len(r.devices))
	for _, device := range r.devices {
		copied := *device
		copied.TXT = cloneTXT(device.TXT)
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get resolves a device by exact ID, MAC address, name or address.
func (r *Registry) Get(id string) (Device, bool) {
	if id == "" {
		return Device{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	if device, ok := r.devices[id]; ok {
		copied := *device
		copied.TXT = cloneTXT(device.TXT)
		return copied, true
	}

	needle := strings.ToLower(strings.TrimSpace(id))
	for _, device := range r.devices {
		if strings.ToLower(device.ID) == needle ||
			strings.EqualFold(device.MAC, id) ||
			strings.EqualFold(device.Name, id) ||
			device.Address == id {
			copied := *device
			copied.TXT = cloneTXT(device.TXT)
			return copied, true
		}
	}
	return Device{}, false
}

// Count returns the number of live devices.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.devices)
}

// SetBusy marks or clears the streaming flag for a device.
func (r *Registry) SetBusy(id string, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	needle := strings.ToLower(strings.TrimSpace(id))
	for key, device := range r.devices {
		if key == id || strings.ToLower(device.ID) == needle || strings.EqualFold(device.MAC, id) {
			device.IsBusy = busy
			return
		}
	}
}

// Sweep removes entries whose LastSeen is older than the TTL. Static entries
// have a zero LastSeen and are never removed. It returns the expired devices.
func (r *Registry) Sweep() []Device {
	if r.ttl <= 0 {
		return nil
	}
	cutoff := r.now().Add(-r.ttl)

	r.mu.Lock()
	defer r.mu.Unlock()

	var expired []Device
	for key, device := range r.devices {
		if device.Source == SourceStatic || device.LastSeen.IsZero() {
			continue
		}
		if device.LastSeen.Before(cutoff) {
			expired = append(expired, *device)
			delete(r.devices, key)
		}
	}
	return expired
}

func registryKey(observation Device) string {
	if mac := normalizeMAC(observation.MAC); mac != "" {
		return mac
	}
	if observation.ID != "" {
		if mac := macFromInstanceName(observation.ID); mac != "" {
			return mac
		}
		return observation.ID
	}
	return strings.ToLower(observation.Name)
}

func mergeDevice(existing *Device, observation Device) {
	// Never let a static entry be downgraded by a weaker mDNS observation,
	// but always refresh the liveness timestamp.
	if observation.Source == SourceStatic && existing.Source != SourceStatic {
		existing.Source = SourceStatic
	}
	if observation.Source != SourceStatic {
		if !observation.LastSeen.IsZero() {
			existing.LastSeen = observation.LastSeen
		}
		existing.Source = observation.Source
	}
	if observation.Name != "" {
		existing.Name = observation.Name
	}
	if observation.Model != "" {
		existing.Model = observation.Model
	}
	if observation.Address != "" {
		existing.Address = observation.Address
	}
	if observation.MAC != "" {
		existing.MAC = normalizeMAC(observation.MAC)
	}
	if observation.Features != "" {
		existing.Features = observation.Features
	}
	if observation.Protocol != "" {
		if existing.Protocol != "airplay2" || observation.Protocol == "airplay2" {
			existing.Protocol = observation.Protocol
		}
	}
	if observation.RequiresPassword {
		existing.RequiresPassword = true
	}
	if observation.RAOPPort != 0 {
		existing.RAOPPort = observation.RAOPPort
	}
	if observation.AirPlayPort != 0 {
		existing.AirPlayPort = observation.AirPlayPort
	}
	if observation.Port != 0 {
		// Prefer the AirPlay control port (7000-class) over the RAOP port.
		if existing.Port == 0 || observation.AirPlayPort != 0 {
			existing.Port = observation.Port
		}
	}
	if existing.TXT == nil {
		existing.TXT = map[string]string{}
	}
	for key, value := range observation.TXT {
		if strings.TrimSpace(value) == "" {
			continue
		}
		existing.TXT[key] = value
	}
	if existing.ID == "" {
		existing.ID = registryKey(observation)
	}
}

func cloneTXT(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func splitHostPort(address string, defaultPort int) (string, int, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", 0, fmt.Errorf("empty device address")
	}
	if host, portText, err := net.SplitHostPort(address); err == nil {
		port, convErr := strconv.Atoi(portText)
		if convErr != nil || port <= 0 || port > 65535 {
			return "", 0, fmt.Errorf("invalid port in %q", address)
		}
		return host, port, nil
	}
	if defaultPort <= 0 {
		defaultPort = 7000
	}
	return address, defaultPort, nil
}

func normalizeMAC(value string) string {
	cleaned := strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.TrimSpace(value))
	if len(cleaned) != 12 {
		return ""
	}
	for _, r := range cleaned {
		if !isHexDigit(r) {
			return ""
		}
	}
	cleaned = strings.ToUpper(cleaned)
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, cleaned[i:i+2])
	}
	return strings.Join(parts, ":")
}

func macFromInstanceName(instance string) string {
	head, _, found := strings.Cut(instance, "@")
	if !found {
		return ""
	}
	return normalizeMAC(head)
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
