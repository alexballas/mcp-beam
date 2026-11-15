package discovery

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go2tv.app/go2tv/v2/devices"
)

type fakeAdapter struct {
	loadAllDevices func() ([]devices.Device, error)
	// refresh defaults to returning at once, as if the first scans were done.
	refresh        func(ctx context.Context) error
	startLoopCalls atomic.Int32
	refreshCalls   atomic.Int32
}

func (f *fakeAdapter) StartDiscovery(ctx context.Context) {
	f.startLoopCalls.Add(1)
}

func (f *fakeAdapter) RefreshDiscovery(ctx context.Context) error {
	f.refreshCalls.Add(1)
	if f.refresh == nil {
		return nil
	}
	return f.refresh(ctx)
}

func (f *fakeAdapter) LoadAllDevices() ([]devices.Device, error) {
	if f.loadAllDevices == nil {
		return nil, errors.New("not configured")
	}
	return f.loadAllDevices()
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
		loadAllDevices: func() ([]devices.Device, error) {
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
	if adapter.startLoopCalls.Load() != 1 {
		t.Fatalf("expected discovery loop to start once, got %d", adapter.startLoopCalls.Load())
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
		loadAllDevices: func() ([]devices.Device, error) {
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
		loadAllDevices: func() ([]devices.Device, error) {
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
		loadAllDevices: func() ([]devices.Device, error) {
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

var (
	testChromecast = devices.Device{Name: "Living Room TV", Addr: "http://192.168.1.20:8009", Type: "Chromecast"}
	testDLNA       = devices.Device{Name: "Bedroom TV", Addr: "http://192.168.1.30:1400/dmr", Type: "DLNA"}
)

func allowAllReachable(t *testing.T) {
	t.Helper()
	orig := isReachableAddress
	t.Cleanup(func() { isReachableAddress = orig })
	isReachableAddress = func(string, time.Duration) bool { return true }
}

// slowDLNAAdapter models a cold start: the Chromecast is cached at once, and
// the DLNA renderer only once the first full scan finishes after scanDelay.
func slowDLNAAdapter(scanDelay time.Duration) *fakeAdapter {
	var scanned atomic.Bool
	return &fakeAdapter{
		refresh: func(ctx context.Context) error {
			select {
			case <-time.After(scanDelay):
				scanned.Store(true)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		loadAllDevices: func() ([]devices.Device, error) {
			if scanned.Load() {
				return []devices.Device{testChromecast, testDLNA}, nil
			}
			return []devices.Device{testChromecast}, nil
		},
	}
}

func TestListLocalHardware_ColdStartWaitsForFirstFullScan(t *testing.T) {
	allowAllReachable(t)
	svc := NewService(slowDLNAAdapter(150*time.Millisecond), context.Background())

	items, err := svc.ListLocalHardware(context.Background(), 2500, true)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected the slower DLNA device alongside the Chromecast, got %+v", items)
	}
}

func TestListLocalHardware_ColdStartTimeoutReturnsPartialList(t *testing.T) {
	allowAllReachable(t)
	svc := NewService(slowDLNAAdapter(time.Hour), context.Background())

	started := time.Now()
	items, err := svc.ListLocalHardware(context.Background(), 200, true)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("expected the request timeout to bound the warm-up wait, elapsed=%s", elapsed)
	}
	if len(items) != 1 || items[0].Name != testChromecast.Name {
		t.Fatalf("expected the cached Chromecast when the first scan is late, got %+v", items)
	}
}

func TestListLocalHardware_WarmCacheSkipsWait(t *testing.T) {
	allowAllReachable(t)
	adapter := slowDLNAAdapter(50 * time.Millisecond)
	svc := NewService(adapter, context.Background())
	svc.Start()
	svc.Start()
	time.Sleep(150 * time.Millisecond)

	started := time.Now()
	items, err := svc.ListLocalHardware(context.Background(), 2500, true)
	if err != nil {
		t.Fatalf("list local hardware: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("expected a warm cache to answer at once, elapsed=%s", elapsed)
	}
	if len(items) != 2 {
		t.Fatalf("expected both devices, got %+v", items)
	}
	if got := adapter.startLoopCalls.Load(); got != 1 {
		t.Fatalf("expected discovery to start once, got %d", got)
	}
}

func TestRefresh_WaitsForFreshScanWithinTimeout(t *testing.T) {
	adapter := &fakeAdapter{refresh: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	svc := NewService(adapter, context.Background())

	started := time.Now()
	err := svc.Refresh(context.Background(), 50)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the refresh to stop at its timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("expected refresh to be bounded, elapsed=%s", elapsed)
	}
	// One call from the startup warm-up, one from Refresh.
	if got := adapter.refreshCalls.Load(); got != 2 {
		t.Fatalf("expected 2 refresh calls, got %d", got)
	}
}
