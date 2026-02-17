package discovery

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"go2tv.app/go2tv/v2/devices"
	"go2tv.app/mcp-beam/internal/adapters"
	"go2tv.app/mcp-beam/internal/domain"
)

const (
	defaultTimeoutMS             = 2500
	reachabilityWait             = 400 * time.Millisecond
	defaultDiscoveryDelaySeconds = 1
	maxPerAttemptTimeoutMS       = 3000
)

var isReachableAddress = defaultReachableAddress

type Service struct {
	adapter adapters.Discovery
	loopCtx context.Context
	once    sync.Once
}

func NewService(adapter adapters.Discovery, loopCtx context.Context) *Service {
	if loopCtx == nil {
		loopCtx = context.Background()
	}

	return &Service{
		adapter: adapter,
		loopCtx: loopCtx,
	}
}

func (s *Service) ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error) {
	if s.adapter == nil {
		return nil, errors.New("discovery adapter is not configured")
	}
	if timeoutMS <= 0 {
		timeoutMS = defaultTimeoutMS
	}

	s.once.Do(func() {
		s.adapter.StartChromecastDiscoveryLoop(s.loopCtx)
	})

	resultCh := make(chan struct {
		devices []devices.Device
		err     error
	}, 1)

	go func() {
		loaded, err := s.loadAllDevicesUntilTimeout(ctx, timeoutMS)
		resultCh <- struct {
			devices []devices.Device
			err     error
		}{devices: loaded, err: err}
	}()

	timeout := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timeout.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		return []domain.Device{}, nil
	case result := <-resultCh:
		if result.err != nil {
			if errors.Is(result.err, devices.ErrNoDeviceAvailable) {
				return []domain.Device{}, nil
			}
			return nil, result.err
		}

		normalized := normalizeDevices(result.devices)
		if !includeUnreachable {
			normalized = filterReachable(normalized)
		}
		sortDevices(normalized)
		return normalized, nil
	}
}

func (s *Service) loadAllDevicesUntilTimeout(ctx context.Context, timeoutMS int) ([]devices.Device, error) {
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		remainingMS := int(time.Until(deadline).Milliseconds())
		if remainingMS <= 0 {
			if errors.Is(lastErr, devices.ErrNoDeviceAvailable) || lastErr == nil {
				return []devices.Device{}, nil
			}
			return nil, lastErr
		}

		attemptTimeoutMS := remainingMS
		if attemptTimeoutMS > maxPerAttemptTimeoutMS {
			attemptTimeoutMS = maxPerAttemptTimeoutMS
		}
		attemptDelaySeconds := timeoutToDelaySeconds(attemptTimeoutMS)

		loaded, err := s.adapter.LoadAllDevices(attemptDelaySeconds)
		if err == nil {
			if len(loaded) > 0 {
				return loaded, nil
			}
			return []devices.Device{}, nil
		}
		if !errors.Is(err, devices.ErrNoDeviceAvailable) {
			return nil, err
		}

		lastErr = err
	}
}

func timeoutToDelaySeconds(timeoutMS int) int {
	seconds := int(math.Ceil(float64(timeoutMS) / 1000.0))
	if seconds <= 0 {
		return defaultDiscoveryDelaySeconds
	}
	return seconds
}

func normalizeDevices(discovered []devices.Device) []domain.Device {
	result := make([]domain.Device, 0, len(discovered))
	for _, raw := range discovered {
		protocol := normalizeProtocol(raw.Type)
		address := strings.TrimSpace(raw.Addr)

		result = append(result, domain.Device{
			ID:           stableID(protocol, address),
			Name:         strings.TrimSpace(raw.Name),
			Type:         strings.TrimSpace(raw.Type),
			Address:      address,
			IsAudioOnly:  raw.IsAudioOnly,
			Protocol:     protocol,
			Capabilities: capabilitiesForProtocol(protocol),
		})
	}

	return result
}

func filterReachable(all []domain.Device) []domain.Device {
	filtered := make([]domain.Device, 0, len(all))
	for _, dev := range all {
		if isReachableAddress(dev.Address, reachabilityWait) {
			filtered = append(filtered, dev)
		}
	}
	return filtered
}

func sortDevices(all []domain.Device) {
	sort.Slice(all, func(i, j int) bool {
		if protocolRank(all[i].Protocol) != protocolRank(all[j].Protocol) {
			return protocolRank(all[i].Protocol) < protocolRank(all[j].Protocol)
		}
		if strings.ToLower(all[i].Name) != strings.ToLower(all[j].Name) {
			return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
		}
		if strings.ToLower(all[i].Address) != strings.ToLower(all[j].Address) {
			return strings.ToLower(all[i].Address) < strings.ToLower(all[j].Address)
		}
		return all[i].ID < all[j].ID
	})
}

func protocolRank(protocol string) int {
	switch protocol {
	case "dlna":
		return 0
	case "chromecast":
		return 1
	default:
		return 2
	}
}

func stableID(protocol, address string) string {
	canonical := fmt.Sprintf("%s|%s", protocol, canonicalAddress(address))
	sum := sha1.Sum([]byte(canonical))
	return "dev_" + hex.EncodeToString(sum[:8])
}

func canonicalAddress(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(address))
	}

	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}

	path := strings.TrimSpace(strings.ToLower(parsed.EscapedPath()))
	if path == "" {
		path = "/"
	}

	return fmt.Sprintf("%s://%s:%s%s", strings.ToLower(parsed.Scheme), host, port, path)
}

func normalizeProtocol(kind string) string {
	lower := strings.ToLower(strings.TrimSpace(kind))
	if strings.Contains(lower, "chrome") {
		return "chromecast"
	}
	if strings.Contains(lower, "dlna") {
		return "dlna"
	}
	return lower
}

func capabilitiesForProtocol(protocol string) domain.Capabilities {
	caps := domain.Capabilities{
		SupportsFileSource: true,
		SupportsURLSource:  true,
		Limitations:        []domain.Limitation{},
	}

	switch protocol {
	case "chromecast":
		caps.SupportsHLSM3U8URL = true
	case "dlna":
		caps.SupportsHLSM3U8URL = false
		caps.Limitations = append(caps.Limitations, domain.Limitation{
			Code:    "HLS_M3U8_URL_UNSUPPORTED",
			Message: "HLS .m3u8 URLs are Chromecast-only by default in v1.",
		})
	default:
		caps.SupportsHLSM3U8URL = false
	}

	return caps
}

func defaultReachableAddress(address string, timeout time.Duration) bool {
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}

	hostPort := parsed.Host
	if hostPort == "" {
		return false
	}
	if parsed.Port() == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			hostPort = net.JoinHostPort(parsed.Hostname(), "443")
		} else {
			hostPort = net.JoinHostPort(parsed.Hostname(), "80")
		}
	}

	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
