package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"go2tv.app/go2tv/v2/devices"
)

type fakeAdapter struct {
	loadAllDevices func(delaySeconds int) ([]devices.Device, error)
	startLoopCalls int
}

func (f *fakeAdapter) StartChromecastDiscoveryLoop(ctx context.Context) {
	f.startLoopCalls++
}

func (f *fakeAdapter) LoadAllDevices(delaySeconds int) ([]devices.Device, error) {
	if f.loadAllDevices == nil {
		return nil, errors.New("not configured")
	}
	return f.loadAllDevices(delaySeconds)
}

func TestListLocalHardware_NormalizationSortingAndStableIDs(t *testing.T) {
	origReachable := isReachableAddress
	t.Cleanup(func() {
		isReachableAddress = origReachable
	})
	isReachableAddress = func(address string, timeout time.Duration) bool {
		return true
	}

	adapter := &fakeAdapter{
		loadAllDevices: func(delaySeconds int) ([]devices.Device, error) {
			return []devices.Device{
				{Name: "Kitchen Speaker (Chromecast Audio)", Addr: "http://192.168.1.30:8009", Type: "Chromecast", IsAudioOnly: true},
				{Name: "Bedroom TV", Addr: "http://192.168.1.10:1400/desc.xml", Type: "DLNA", IsAudioOnly: false},
				{Name: "Living Room TV", Addr: "http://192.168.1.20:8009", Type: "Chromecast", IsAudioOnly: false},
			}, nil
		},
	}

	svc := NewService(adapter, context.Background())

	first, err := svc.ListLocalHardware(context.Background(), 2500, true)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}
	second, err := svc.ListLocalHardware(context.Background(), 2500, true)
	if err != nil {
		t.Fatalf("list local hardware (second call): %v", err)
	}

	if len(first) != 3 {
		t.Fatalf("expected 3 devices, got %d", len(first))
	}
	if adapter.startLoopCalls != 1 {
		t.Fatalf("expected discovery loop to start once, got %d", adapter.startLoopCalls)
	}

	if first[0].Protocol != "dlna" {
		t.Fatalf("expected first protocol dlna, got %q", first[0].Protocol)
	}
	if first[1].Protocol != "chromecast" || first[2].Protocol != "chromecast" {
		t.Fatalf("expected chromecast devices after dlna, got %q and %q", first[1].Protocol, first[2].Protocol)
	}

	if first[0].Capabilities.SupportsHLSM3U8URL {
		t.Fatal("expected dlna hls support to be false")
	}
	if len(first[0].Capabilities.Limitations) == 0 || first[0].Capabilities.Limitations[0].Code != "HLS_M3U8_URL_UNSUPPORTED" {
		t.Fatalf("unexpected dlna limitations: %+v", first[0].Capabilities.Limitations)
	}

	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("expected stable IDs across calls at index %d", i)
		}
	}
}

func TestListLocalHardware_IncludeUnreachableFalseFiltersDevices(t *testing.T) {
	origReachable := isReachableAddress
	t.Cleanup(func() {
		isReachableAddress = origReachable
	})
	isReachableAddress = func(address string, timeout time.Duration) bool {
		return address == "http://192.168.1.10:1400/desc.xml"
	}

	adapter := &fakeAdapter{
		loadAllDevices: func(delaySeconds int) ([]devices.Device, error) {
			return []devices.Device{
				{Name: "Bedroom TV", Addr: "http://192.168.1.10:1400/desc.xml", Type: "DLNA"},
				{Name: "Living Room TV", Addr: "http://192.168.1.20:8009", Type: "Chromecast"},
			}, nil
		},
	}

	svc := NewService(adapter, context.Background())
	filtered, err := svc.ListLocalHardware(context.Background(), 2500, false)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}

	if len(filtered) != 1 {
		t.Fatalf("expected 1 reachable device, got %d", len(filtered))
	}
	if filtered[0].Address != "http://192.168.1.10:1400/desc.xml" {
		t.Fatalf("unexpected kept address: %s", filtered[0].Address)
	}
}

func TestListLocalHardware_TimeoutReturnsEmptyList(t *testing.T) {
	adapter := &fakeAdapter{
		loadAllDevices: func(delaySeconds int) ([]devices.Device, error) {
			time.Sleep(120 * time.Millisecond)
			return []devices.Device{{Name: "Late Device", Addr: "http://192.168.1.50:8009", Type: "Chromecast"}}, nil
		},
	}

	svc := NewService(adapter, context.Background())
	start := time.Now()
	items, err := svc.ListLocalHardware(context.Background(), 20, true)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}

	if len(items) != 0 {
		t.Fatalf("expected timeout to return empty list, got %d items", len(items))
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("expected timeout behavior, elapsed=%s", elapsed)
	}
}

func TestTimeoutToDelaySecondsUsesCeil(t *testing.T) {
	cases := []struct {
		timeoutMS int
		want      int
	}{
		{timeoutMS: 2500, want: 3},
		{timeoutMS: 2000, want: 2},
		{timeoutMS: 1, want: 1},
		{timeoutMS: 0, want: 1},
	}

	for _, tc := range cases {
		got := timeoutToDelaySeconds(tc.timeoutMS)
		if got != tc.want {
			t.Fatalf("timeoutToDelaySeconds(%d) = %d, want %d", tc.timeoutMS, got, tc.want)
		}
	}
}

func TestListLocalHardware_RetriesWithinTimeoutToCatchWarmupDevices(t *testing.T) {
	origReachable := isReachableAddress
	t.Cleanup(func() {
		isReachableAddress = origReachable
	})
	isReachableAddress = func(address string, timeout time.Duration) bool {
		return true
	}

	callCount := 0
	adapter := &fakeAdapter{
		loadAllDevices: func(delaySeconds int) ([]devices.Device, error) {
			callCount++
			if callCount == 1 {
				return nil, devices.ErrNoDeviceAvailable
			}
			return []devices.Device{
				{Name: "Living Room TV", Addr: "http://192.168.1.20:8009", Type: "Chromecast"},
			}, nil
		},
	}

	svc := NewService(adapter, context.Background())
	items, err := svc.ListLocalHardware(context.Background(), 4500, true)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 device, got %d", len(items))
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 discovery calls, got %d", callCount)
	}
}
