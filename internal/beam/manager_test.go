package beam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/httphandlers"
	"go2tv.app/go2tv/v2/soapcalls"
	"go2tv.app/go2tv/v2/utils"
	"go2tv.app/mcp-beam/internal/adapters"
	"go2tv.app/mcp-beam/internal/domain"
)

type fakeDiscovery struct {
	devices       []domain.Device
	err           error
	calls         int
	timeoutCalls  []int
	includeCalls  []bool
	devicesByCall [][]domain.Device
	errsByCall    []error
}

func (f *fakeDiscovery) ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error) {
	f.timeoutCalls = append(f.timeoutCalls, timeoutMS)
	f.includeCalls = append(f.includeCalls, includeUnreachable)
	callIdx := f.calls
	f.calls++

	if len(f.devicesByCall) > 0 || len(f.errsByCall) > 0 {
		devIdx := callIdx
		if devIdx >= len(f.devicesByCall) {
			devIdx = len(f.devicesByCall) - 1
		}
		errIdx := callIdx
		if errIdx >= len(f.errsByCall) {
			errIdx = len(f.errsByCall) - 1
		}

		if len(f.errsByCall) > 0 && f.errsByCall[errIdx] != nil {
			return nil, f.errsByCall[errIdx]
		}
		if len(f.devicesByCall) > 0 {
			return append([]domain.Device{}, f.devicesByCall[devIdx]...), nil
		}
		return []domain.Device{}, nil
	}

	if f.err != nil {
		return nil, f.err
	}
	return append([]domain.Device{}, f.devices...), nil
}

type fakeCastFactory struct {
	client  *fakeCastClient
	clients []*fakeCastClient
	err     error

	mu    sync.Mutex
	calls int
}

func (f *fakeCastFactory) NewCastClient(deviceAddr string) (adapters.CastClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.clients) > 0 {
		idx := f.calls - 1
		if idx >= len(f.clients) {
			idx = len(f.clients) - 1
		}
		c := f.clients[idx]
		c.deviceAddr = deviceAddr
		return c, nil
	}
	if f.client == nil {
		f.client = &fakeCastClient{}
	}
	f.client.deviceAddr = deviceAddr
	return f.client, nil
}

type fakeCastClient struct {
	deviceAddr          string
	connectErr          error
	connectErrs         []error
	connectDelay        time.Duration
	loadErr             error
	loadErrs            []error
	loadOnExistingErr   error
	loadDelay           time.Duration
	loadBlock           <-chan struct{}
	seekErr             error
	stopErr             error
	closeErr            error
	statusErr           error
	statuses            []castprotocol.CastStatus
	loadURL             string
	loadType            string
	loadLive            bool
	loadSubtitle        string
	seekSeconds         int
	connectCalls        int
	loadCalls           int
	loadOnExistingCalls int
	seekCalls           int
	stopCalls           int
	closeCalls          int
	statusCalls         int

	mu sync.Mutex
}

func (f *fakeCastClient) Connect() error {
	if f.connectDelay > 0 {
		time.Sleep(f.connectDelay)
	}
	f.connectCalls++
	if len(f.connectErrs) > 0 {
		idx := f.connectCalls - 1
		if idx >= len(f.connectErrs) {
			idx = len(f.connectErrs) - 1
		}
		if f.connectErrs[idx] != nil {
			return f.connectErrs[idx]
		}
	}
	return f.connectErr
}

func (f *fakeCastClient) Load(mediaURL, contentType string, startTime int, duration float64, subtitleURL string, live bool) error {
	if f.loadDelay > 0 {
		time.Sleep(f.loadDelay)
	}
	if f.loadBlock != nil {
		<-f.loadBlock
	}
	f.loadCalls++
	f.loadURL = mediaURL
	f.loadType = contentType
	f.loadLive = live
	f.loadSubtitle = subtitleURL
	if len(f.loadErrs) > 0 {
		idx := f.loadCalls - 1
		if idx >= len(f.loadErrs) {
			idx = len(f.loadErrs) - 1
		}
		if f.loadErrs[idx] != nil {
			return f.loadErrs[idx]
		}
	}
	return f.loadErr
}

func (f *fakeCastClient) LoadOnExisting(mediaURL, contentType string, startTime int, duration float64, subtitleURL string, live bool) error {
	if f.loadDelay > 0 {
		time.Sleep(f.loadDelay)
	}
	if f.loadBlock != nil {
		<-f.loadBlock
	}
	f.loadOnExistingCalls++
	f.loadURL = mediaURL
	f.loadType = contentType
	f.loadLive = live
	f.loadSubtitle = subtitleURL
	return f.loadOnExistingErr
}

func (f *fakeCastClient) Stop() error {
	f.stopCalls++
	return f.stopErr
}

func (f *fakeCastClient) Seek(seconds int) error {
	f.seekCalls++
	f.seekSeconds = seconds
	return f.seekErr
}

func (f *fakeCastClient) GetStatus() (*castprotocol.CastStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statuses) > 0 {
		idx := f.statusCalls - 1
		if idx >= len(f.statuses) {
			idx = len(f.statuses) - 1
		}
		status := f.statuses[idx]
		return &status, nil
	}
	return &castprotocol.CastStatus{PlayerState: "PLAYING", CurrentTime: float32(f.statusCalls)}, nil
}

func (f *fakeCastClient) Close(stopMedia bool) error {
	f.closeCalls++
	return f.closeErr
}

type fakeDLNAFactory struct {
	payloads []*fakeDLNAPayload
	err      error

	calls int
}

func (f *fakeDLNAFactory) NewTVPayload(o *soapcalls.Options) (adapters.DLNAPayload, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.payloads) == 0 {
		return nil, fmt.Errorf("no payload configured")
	}
	idx := f.calls
	if idx >= len(f.payloads) {
		idx = len(f.payloads) - 1
	}
	p := f.payloads[idx]
	p.mediaURL = "http://" + p.listenAddr + "/media-token.mp4"
	if p.rawPayload == nil {
		p.rawPayload = &soapcalls.TVPayload{
			MediaURL:    p.mediaURL,
			CallbackURL: "http://" + p.listenAddr + "/cb-token",
		}
	} else {
		p.rawPayload.MediaURL = p.mediaURL
		p.rawPayload.CallbackURL = "http://" + p.listenAddr + "/cb-token"
	}
	f.calls++
	return p, nil
}

type fakeDLNAPayload struct {
	listenAddr string
	mediaURL   string
	rawPayload *soapcalls.TVPayload

	mu sync.Mutex

	actions   []string
	actionErr map[string]error

	transportResponses [][]string
	transportErr       error
	positionResponse   []string
	positionErr        error
	seekErr            error
	seekRelTime        string

	setContextCalls int
	ctx             context.Context

	blockPlayUntilContextDone bool
}

func (f *fakeDLNAPayload) SendtoTV(action string) error {
	f.mu.Lock()
	f.actions = append(f.actions, action)
	ctx := f.ctx
	block := f.blockPlayUntilContextDone && action == "Play1"
	actionErr := f.actionErr
	f.mu.Unlock()

	if block {
		if ctx == nil {
			return errors.New("missing payload context")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	if actionErr != nil {
		if err, ok := actionErr[action]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeDLNAPayload) GetTransportInfo() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transportErr != nil {
		return nil, f.transportErr
	}
	if len(f.transportResponses) == 0 {
		return []string{"PLAYING", "OK", "1"}, nil
	}
	resp := f.transportResponses[0]
	if len(f.transportResponses) > 1 {
		f.transportResponses = f.transportResponses[1:]
	}
	return resp, nil
}

func (f *fakeDLNAPayload) GetPositionInfo() ([]string, error) {
	if f.positionErr != nil {
		return nil, f.positionErr
	}
	if len(f.positionResponse) > 0 {
		return append([]string{}, f.positionResponse...), nil
	}
	return []string{"00:30:00", "00:00:02"}, nil
}

func (f *fakeDLNAPayload) SeekSoapCall(reltime string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seekRelTime = reltime
	return f.seekErr
}

func (f *fakeDLNAPayload) ListenAddress() string {
	return f.listenAddr
}

func (f *fakeDLNAPayload) SetContext(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setContextCalls++
	f.ctx = ctx
}

func (f *fakeDLNAPayload) MediaURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mediaURL
}

func (f *fakeDLNAPayload) SetMediaURL(mediaURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mediaURL = mediaURL
	if f.rawPayload != nil {
		f.rawPayload.MediaURL = mediaURL
	}
}

func (f *fakeDLNAPayload) RawPayload() *soapcalls.TVPayload {
	if f.rawPayload == nil {
		f.rawPayload = &soapcalls.TVPayload{MediaURL: f.mediaURL, CallbackURL: "http://" + f.listenAddr + "/cb-token"}
	}
	return f.rawPayload
}

func (f *fakeDLNAPayload) actionCount(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, a := range f.actions {
		if a == action {
			count++
		}
	}
	return count
}

type fakeServerFactory struct {
	servers []*fakeServer
}

func (f *fakeServerFactory) New(addr string) streamServer {
	s := &fakeServer{addr: addr}
	f.servers = append(f.servers, s)
	return s
}

type fakeServer struct {
	addr              string
	addCount          int
	addPaths          []string
	lastTranscodeOpts *utils.TranscodeOptions
	lastMedia         any
	startCalled       bool
	startServerCalled bool
	stopCalled        bool
	lastScreen        httphandlers.Screen
}

func (f *fakeServer) AddHandler(path string, payload *soapcalls.TVPayload, transcode *utils.TranscodeOptions, media any) {
	f.addCount++
	f.addPaths = append(f.addPaths, path)
	f.lastTranscodeOpts = transcode
	f.lastMedia = media
}

func (f *fakeServer) StartServing(serverStarted chan<- error) {
	f.startCalled = true
	serverStarted <- nil
}

func (f *fakeServer) StartServer(serverStarted chan<- error, media, subtitles any, tvpayload *soapcalls.TVPayload, screen httphandlers.Screen) {
	f.startServerCalled = true
	f.lastScreen = screen
	serverStarted <- nil
}

func (f *fakeServer) StopServer() {
	f.stopCalled = true
}

type fakeCloser struct {
	closed bool
}

func (f *fakeCloser) Close() error {
	f.closed = true
	return nil
}

func TestBeamMediaFileAndStop(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_1",
		Name:     "Living Room",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	serverFactory := &fakeServerFactory{}
	manager.serverFactory = serverFactory
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3500", nil
	}

	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_1",
		Transcode:    "never",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected beam result OK=true")
	}
	if result.SessionID == "" {
		t.Fatal("expected session ID")
	}
	if !strings.HasPrefix(result.MediaURL, "http://") {
		t.Fatalf("expected local media URL, got %s", result.MediaURL)
	}
	if castClient.loadCalls != 1 {
		t.Fatalf("expected load to be called once, got %d", castClient.loadCalls)
	}
	if castClient.loadType != "video/mp4" {
		t.Fatalf("expected content type video/mp4, got %s", castClient.loadType)
	}

	stopResult, err := manager.StopBeaming(context.Background(), domain.StopRequest{SessionID: result.SessionID})
	if err != nil {
		t.Fatalf("stop beaming: %v", err)
	}
	if !stopResult.OK {
		t.Fatal("expected stop OK=true")
	}
	if castClient.stopCalls != 1 {
		t.Fatalf("expected stop to be called once, got %d", castClient.stopCalls)
	}
	if castClient.closeCalls == 0 {
		t.Fatal("expected close to be called")
	}
	if len(serverFactory.servers) == 0 || !serverFactory.servers[0].stopCalled {
		t.Fatal("expected media server to be stopped")
	}
}

func TestSeekBeamingChromecastBySessionID(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	castClient := &fakeCastClient{}
	sess := &session{
		ID:         "sess_seek_cast",
		DeviceID:   "dev_cast",
		DeviceName: "Living Room",
		Protocol:   "chromecast",
		castClient: castClient,
	}
	manager.initializeSessionLifecycle(sess, "playing", "10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	seekPosition := 95
	result, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_cast",
		PositionSeconds: &seekPosition,
	})
	if err != nil {
		t.Fatalf("seek beaming: %v", err)
	}
	if !result.OK {
		t.Fatal("expected seek OK=true")
	}
	if result.SessionID != "sess_seek_cast" {
		t.Fatalf("unexpected session id: %s", result.SessionID)
	}
	if result.DeviceID != "dev_cast" {
		t.Fatalf("unexpected device id: %s", result.DeviceID)
	}
	if castClient.seekCalls != 1 {
		t.Fatalf("expected one seek call, got %d", castClient.seekCalls)
	}
	if castClient.seekSeconds != 95 {
		t.Fatalf("expected seek position 95, got %d", castClient.seekSeconds)
	}
	if result.RequestedMode != "absolute_seconds" {
		t.Fatalf("expected absolute_seconds, got %s", result.RequestedMode)
	}
	if result.ResolvedPositionSeconds != 95 {
		t.Fatalf("expected resolved_position_seconds=95, got %d", result.ResolvedPositionSeconds)
	}
	if result.DurationSeconds != nil {
		t.Fatalf("expected no duration for absolute seek, got %v", *result.DurationSeconds)
	}
}

func TestSeekBeamingChromecastTranscodedBySessionID(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	castClient := &fakeCastClient{}
	server := &fakeServer{addr: "127.0.0.1:3551"}
	sess := &session{
		ID:            "sess_seek_cast_tc",
		DeviceID:      "dev_cast",
		DeviceName:    "Living Room",
		Protocol:      "chromecast",
		Transcoding:   true,
		MediaURL:      "http://127.0.0.1:3551/media-old.mp4",
		mediaDuration: 200,
		castClient:    castClient,
		httpServer:    server,
		castSeekPlan: &chromecastTranscodeSeek{
			sourcePath: "/tmp/video.mp4",
			ffmpegPath: "/usr/bin/ffmpeg",
			subsPath:   "/tmp/subs.srt",
			route:      "/media-new.mp4",
		},
	}
	manager.initializeSessionLifecycle(sess, "playing", "10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	seekPosition := 95
	result, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_cast_tc",
		PositionSeconds: &seekPosition,
	})
	if err != nil {
		t.Fatalf("seek beaming: %v", err)
	}
	if !result.OK {
		t.Fatal("expected seek OK=true")
	}
	if castClient.seekCalls != 0 {
		t.Fatalf("expected native Seek to be skipped for transcoded session, got %d calls", castClient.seekCalls)
	}
	if castClient.loadOnExistingCalls != 1 {
		t.Fatalf("expected one LoadOnExisting call, got %d", castClient.loadOnExistingCalls)
	}
	if castClient.loadURL != "http://127.0.0.1:3551/media-new.mp4" {
		t.Fatalf("unexpected load URL: %s", castClient.loadURL)
	}
	if server.addCount != 1 {
		t.Fatalf("expected one handler update, got %d", server.addCount)
	}
	if len(server.addPaths) == 0 || server.addPaths[0] != "/media-new.mp4" {
		t.Fatalf("unexpected handler paths: %v", server.addPaths)
	}
	if server.lastTranscodeOpts == nil {
		t.Fatal("expected transcode options for handler update")
	}
	if server.lastTranscodeOpts.SeekSeconds != 95 {
		t.Fatalf("expected SeekSeconds=95, got %d", server.lastTranscodeOpts.SeekSeconds)
	}
	if mediaPath, ok := server.lastMedia.(string); !ok || mediaPath != "/tmp/video.mp4" {
		t.Fatalf("unexpected handler media value: %#v", server.lastMedia)
	}
}

func TestSeekBeamingDLNAByTargetDevice(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	dlnaPayload := &fakeDLNAPayload{listenAddr: "127.0.0.1:3511"}
	sess := &session{
		ID:          "sess_seek_dlna",
		DeviceID:    "dev_dlna",
		DeviceName:  "Bedroom TV",
		Protocol:    "dlna",
		dlnaPayload: dlnaPayload,
	}
	manager.initializeSessionLifecycle(sess, "playing", "00:00:10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	seekPosition := 90
	result, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		TargetDevice:    "Bedroom TV",
		PositionSeconds: &seekPosition,
	})
	if err != nil {
		t.Fatalf("seek beaming: %v", err)
	}
	if !result.OK {
		t.Fatal("expected seek OK=true")
	}
	if dlnaPayload.seekRelTime != "00:01:30" {
		t.Fatalf("expected DLNA reltime 00:01:30, got %s", dlnaPayload.seekRelTime)
	}
	if result.RequestedMode != "absolute_seconds" {
		t.Fatalf("expected absolute_seconds, got %s", result.RequestedMode)
	}
}

func TestSeekBeamingDLNATranscodedByTargetDevice(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	dlnaPayload := &fakeDLNAPayload{listenAddr: "127.0.0.1:3511"}
	dlnaPayload.rawPayload = &soapcalls.TVPayload{FFmpegSeek: 0}
	sess := &session{
		ID:          "sess_seek_dlna_tc",
		DeviceID:    "dev_dlna",
		DeviceName:  "Bedroom TV",
		Protocol:    "dlna",
		Transcoding: true,
		dlnaPayload: dlnaPayload,
	}
	manager.initializeSessionLifecycle(sess, "playing", "00:00:10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	seekPosition := 90
	result, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		TargetDevice:    "Bedroom TV",
		PositionSeconds: &seekPosition,
	})
	if err != nil {
		t.Fatalf("seek beaming: %v", err)
	}
	if !result.OK {
		t.Fatal("expected seek OK=true")
	}
	if dlnaPayload.actionCount("Play1") != 1 {
		t.Fatalf("expected Play1 action once, got %d", dlnaPayload.actionCount("Play1"))
	}
	if dlnaPayload.RawPayload().FFmpegSeek != 90 {
		t.Fatalf("expected FFmpegSeek=90, got %d", dlnaPayload.RawPayload().FFmpegSeek)
	}
	if dlnaPayload.seekRelTime != "00:01:30" {
		t.Fatalf("expected follow-up reltime 00:01:30, got %s", dlnaPayload.seekRelTime)
	}
}

func TestSeekBeamingRequiresTarget(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	seekPosition := 30
	_, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		PositionSeconds: &seekPosition,
	})
	if err == nil {
		t.Fatal("expected error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected INTERNAL_ERROR, got %s", toolErr.Code)
	}
}

func TestSeekBeamingChromecastByPercent(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	castClient := &fakeCastClient{
		statuses: []castprotocol.CastStatus{
			{PlayerState: "PLAYING", CurrentTime: 10, Duration: 200},
		},
	}
	sess := &session{
		ID:         "sess_seek_pct",
		DeviceID:   "dev_cast",
		DeviceName: "Living Room",
		Protocol:   "chromecast",
		castClient: castClient,
	}
	manager.initializeSessionLifecycle(sess, "playing", "10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	percent := 50.0
	result, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_pct",
		PositionPercent: &percent,
	})
	if err != nil {
		t.Fatalf("seek beaming: %v", err)
	}
	if castClient.seekSeconds != 100 {
		t.Fatalf("expected seek position 100, got %d", castClient.seekSeconds)
	}
	if result.RequestedMode != "percent" {
		t.Fatalf("expected requested_mode percent, got %s", result.RequestedMode)
	}
	if result.ResolvedPositionSeconds != 100 {
		t.Fatalf("expected resolved_position_seconds 100, got %d", result.ResolvedPositionSeconds)
	}
	if result.DurationSeconds == nil || *result.DurationSeconds != 200 {
		t.Fatalf("expected duration_seconds 200, got %#v", result.DurationSeconds)
	}
}

func TestSeekBeamingDLNAFromEnd(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	dlnaPayload := &fakeDLNAPayload{
		listenAddr:         "127.0.0.1:3511",
		positionResponse:   []string{"00:03:00", "00:00:10"},
		transportResponses: [][]string{{"PLAYING", "OK", "1"}},
	}
	sess := &session{
		ID:          "sess_seek_end",
		DeviceID:    "dev_dlna",
		DeviceName:  "Bedroom TV",
		Protocol:    "dlna",
		dlnaPayload: dlnaPayload,
	}
	manager.initializeSessionLifecycle(sess, "playing", "00:00:10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	fromEnd := 10
	result, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		TargetDevice:   "Bedroom TV",
		FromEndSeconds: &fromEnd,
	})
	if err != nil {
		t.Fatalf("seek beaming: %v", err)
	}
	if dlnaPayload.seekRelTime != "00:02:50" {
		t.Fatalf("expected DLNA reltime 00:02:50, got %s", dlnaPayload.seekRelTime)
	}
	if result.RequestedMode != "from_end_seconds" {
		t.Fatalf("expected requested_mode from_end_seconds, got %s", result.RequestedMode)
	}
	if result.ResolvedPositionSeconds != 170 {
		t.Fatalf("expected resolved_position_seconds 170, got %d", result.ResolvedPositionSeconds)
	}
	if result.DurationSeconds == nil || *result.DurationSeconds != 180 {
		t.Fatalf("expected duration_seconds 180, got %#v", result.DurationSeconds)
	}
}

func TestSeekBeamingRelativeFailsWhenDurationUnknown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	castClient := &fakeCastClient{
		statuses: []castprotocol.CastStatus{
			{PlayerState: "PLAYING", CurrentTime: 10, Duration: 0},
		},
	}
	sess := &session{
		ID:         "sess_seek_unknown",
		DeviceID:   "dev_cast",
		DeviceName: "Living Room",
		Protocol:   "chromecast",
		castClient: castClient,
	}
	manager.initializeSessionLifecycle(sess, "playing", "10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	percent := 50.0
	_, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_unknown",
		PositionPercent: &percent,
	})
	if err == nil {
		t.Fatal("expected error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "SEEK_DURATION_UNKNOWN" {
		t.Fatalf("expected SEEK_DURATION_UNKNOWN, got %s", toolErr.Code)
	}
}

func TestSeekBeamingRequiresExactlyOneMode(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	castClient := &fakeCastClient{}
	sess := &session{
		ID:         "sess_seek_mode",
		DeviceID:   "dev_cast",
		DeviceName: "Living Room",
		Protocol:   "chromecast",
		castClient: castClient,
	}
	manager.initializeSessionLifecycle(sess, "playing", "10")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	_, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID: "sess_seek_mode",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if toolErr, ok := err.(*domain.ToolError); !ok || toolErr.Code != "SEEK_MODE_INVALID" {
		t.Fatalf("expected SEEK_MODE_INVALID, got %#v", err)
	}

	seekPos := 10
	percent := 50.0
	_, err = manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_mode",
		PositionSeconds: &seekPos,
		PositionPercent: &percent,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if toolErr, ok := err.(*domain.ToolError); !ok || toolErr.Code != "SEEK_MODE_INVALID" {
		t.Fatalf("expected SEEK_MODE_INVALID, got %#v", err)
	}
}

func TestSeekBeamingRelativeEdgeBounds(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())

	castClient := &fakeCastClient{
		statuses: []castprotocol.CastStatus{
			{PlayerState: "PLAYING", CurrentTime: 0, Duration: 200},
			{PlayerState: "PLAYING", CurrentTime: 0, Duration: 200},
			{PlayerState: "PLAYING", CurrentTime: 0, Duration: 200},
		},
	}
	sess := &session{
		ID:         "sess_seek_bounds",
		DeviceID:   "dev_cast",
		DeviceName: "Living Room",
		Protocol:   "chromecast",
		castClient: castClient,
	}
	manager.initializeSessionLifecycle(sess, "playing", "0")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	zero := 0.0
	resultZero, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_bounds",
		PositionPercent: &zero,
	})
	if err != nil {
		t.Fatalf("seek 0%%: %v", err)
	}
	if resultZero.ResolvedPositionSeconds != 0 {
		t.Fatalf("expected 0%% to resolve to 0, got %d", resultZero.ResolvedPositionSeconds)
	}

	hundred := 100.0
	resultHundred, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:       "sess_seek_bounds",
		PositionPercent: &hundred,
	})
	if err != nil {
		t.Fatalf("seek 100%%: %v", err)
	}
	if resultHundred.ResolvedPositionSeconds != 200 {
		t.Fatalf("expected 100%% to resolve to 200, got %d", resultHundred.ResolvedPositionSeconds)
	}

	fromEnd := 999
	resultFromEnd, err := manager.SeekBeaming(context.Background(), domain.SeekRequest{
		SessionID:      "sess_seek_bounds",
		FromEndSeconds: &fromEnd,
	})
	if err != nil {
		t.Fatalf("seek from end: %v", err)
	}
	if resultFromEnd.ResolvedPositionSeconds != 0 {
		t.Fatalf("expected from-end larger than duration to resolve to 0, got %d", resultFromEnd.ResolvedPositionSeconds)
	}
}

func TestBeamMediaHLSURLDirectCast(t *testing.T) {
	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_1",
		Name:     "Living Room",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	serverFactory := &fakeServerFactory{}
	manager.serverFactory = serverFactory
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3500", nil
	}

	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "https://example.com/live/stream.m3u8",
		TargetDevice: "Living Room",
		Transcode:    "auto",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if result.MediaURL != "https://example.com/live/stream.m3u8" {
		t.Fatalf("expected direct HLS URL, got %s", result.MediaURL)
	}
	if !castClient.loadLive {
		t.Fatal("expected HLS load to be marked as live")
	}
	if len(serverFactory.servers) != 0 {
		t.Fatal("expected no local server for direct HLS URL")
	}
}

func TestBeamMediaResolveDeviceFallsBackToLongerDiscoveryTimeout(t *testing.T) {
	discovery := &fakeDiscovery{
		devicesByCall: [][]domain.Device{
			{},
			{
				{
					ID:       "dev_1",
					Name:     "Living Room TV (Chromecast)",
					Address:  "http://127.0.0.1:8009",
					Protocol: "chromecast",
				},
			},
		},
	}
	castClient := &fakeCastClient{}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3500", nil
	}

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "https://example.com/live/stream.m3u8",
		TargetDevice: "dev_1",
		Transcode:    "auto",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}

	if len(discovery.timeoutCalls) < 2 {
		t.Fatalf("expected at least 2 discovery calls, got %d", len(discovery.timeoutCalls))
	}
	if discovery.timeoutCalls[0] != defaultDiscoveryTimeoutMS {
		t.Fatalf("expected first timeout %d, got %d", defaultDiscoveryTimeoutMS, discovery.timeoutCalls[0])
	}
	if discovery.timeoutCalls[1] != fallbackDiscoveryTimeoutMS {
		t.Fatalf("expected second timeout %d, got %d", fallbackDiscoveryTimeoutMS, discovery.timeoutCalls[1])
	}
}

func TestBeamMediaResolveDeviceMatchesChromecastSuffixedName(t *testing.T) {
	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_1",
		Name:     "Living Room TV (Chromecast)",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3500", nil
	}

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "https://example.com/live/stream.m3u8",
		TargetDevice: "Living Room TV",
		Transcode:    "auto",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if castClient.loadCalls != 1 {
		t.Fatalf("expected load to be called once, got %d", castClient.loadCalls)
	}
}

func TestBeamMediaTranscodeAlwaysNeedsFFmpeg(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_1",
		Name:     "Living Room",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.lookPath = func(file string) (string, error) {
		return "", errors.New("missing")
	}
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3500", nil
	}

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_1",
		Transcode:    "always",
	})
	if err == nil {
		t.Fatal("expected error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "FFMPEG_NOT_FOUND" {
		t.Fatalf("expected FFMPEG_NOT_FOUND, got %s", toolErr.Code)
	}
	if len(toolErr.SuggestedFixes) < 3 {
		t.Fatalf("expected OS install guidance, got %v", toolErr.SuggestedFixes)
	}
	if !containsWarning(toolErr.SuggestedFixes, "Linux:") || !containsWarning(toolErr.SuggestedFixes, "macOS:") || !containsWarning(toolErr.SuggestedFixes, "Windows:") {
		t.Fatalf("expected Linux/macOS/Windows install guidance, got %v", toolErr.SuggestedFixes)
	}
}

func TestBeamMediaRetriesTransientChromecastConnect(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_retry_connect",
		Name:     "Retry TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{
		connectErrs: []error{
			errors.New("i/o timeout"),
			nil,
		},
	}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3560", nil
	}
	manager.retryBaseBackoff = time.Millisecond
	manager.retryMaxBackoff = 2 * time.Millisecond

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_retry_connect",
		Transcode:    "never",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if castClient.connectCalls != 2 {
		t.Fatalf("expected two connect attempts, got %d", castClient.connectCalls)
	}
}

func TestBeamMediaRetryExhaustionReturnsDeterministicError(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_retry_fail",
		Name:     "Retry Fail TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{
		connectErrs: []error{
			errors.New("connection reset by peer"),
			errors.New("connection reset by peer"),
			errors.New("connection reset by peer"),
		},
	}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3561", nil
	}
	manager.retryBaseBackoff = time.Millisecond
	manager.retryMaxBackoff = 2 * time.Millisecond
	manager.retryAttempts = 3

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_retry_fail",
		Transcode:    "never",
	})
	if err == nil {
		t.Fatal("expected retry exhaustion error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "DEVICE_UNREACHABLE" {
		t.Fatalf("expected DEVICE_UNREACHABLE, got %s", toolErr.Code)
	}
	if castClient.connectCalls != 3 {
		t.Fatalf("expected three connect attempts, got %d", castClient.connectCalls)
	}
}

func TestBeamMediaChromecastOperationTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_timeout_cast",
		Name:     "Timeout TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{connectDelay: 300 * time.Millisecond}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3562", nil
	}
	manager.retryAttempts = 1
	manager.beamOperationTimeout = 40 * time.Millisecond

	started := time.Now()
	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_timeout_cast",
		Transcode:    "never",
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected timeout to fail fast, elapsed=%s", elapsed)
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "DEVICE_UNREACHABLE" {
		t.Fatalf("expected DEVICE_UNREACHABLE, got %s", toolErr.Code)
	}
	if !strings.Contains(strings.ToLower(toolErr.Message), "context deadline exceeded") {
		t.Fatalf("expected context deadline message, got %q", toolErr.Message)
	}
}

func TestBeamMediaChromecastLoadDeadlineGraceAllowsLateSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_load_grace",
		Name:     "Grace TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{loadDelay: 120 * time.Millisecond}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3564", nil
	}
	manager.retryAttempts = 1
	manager.beamOperationTimeout = 40 * time.Millisecond
	manager.chromecastLoadDeadlineGrace = 300 * time.Millisecond

	started := time.Now()
	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_load_grace",
		Transcode:    "never",
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result")
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("expected to wait for delayed load completion, elapsed=%s", elapsed)
	}
	if elapsed > 650*time.Millisecond {
		t.Fatalf("expected load completion within grace window, elapsed=%s", elapsed)
	}
}

func TestBeamMediaChromecastLoadDeadlineGraceStillTimesOut(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_load_timeout",
		Name:     "Timeout Load TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	castClient := &fakeCastClient{loadDelay: 400 * time.Millisecond}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3565", nil
	}
	manager.retryAttempts = 1
	manager.beamOperationTimeout = 40 * time.Millisecond
	manager.chromecastLoadDeadlineGrace = 90 * time.Millisecond

	started := time.Now()
	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_load_timeout",
		Transcode:    "never",
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("expected timeout after grace window, elapsed=%s", elapsed)
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "PROTOCOL_ERROR" {
		t.Fatalf("expected PROTOCOL_ERROR, got %s", toolErr.Code)
	}
	if !strings.Contains(strings.ToLower(toolErr.Message), "context deadline exceeded") {
		t.Fatalf("expected context deadline message, got %q", toolErr.Message)
	}
}

func TestBeamMediaChromecastUnblocksOnPlayingFeedback(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_playing_ack",
		Name:     "Playing Ack TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	releaseLoad := make(chan struct{})
	castClient := &fakeCastClient{
		loadBlock: releaseLoad,
		statuses: []castprotocol.CastStatus{
			{PlayerState: "BUFFERING"},
			{PlayerState: "PLAYING"},
			{PlayerState: "PLAYING"},
		},
	}
	manager := NewManager(discovery, &fakeCastFactory{client: castClient}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3566", nil
	}
	manager.retryAttempts = 1
	manager.beamOperationTimeout = 2 * time.Second
	manager.chromecastLoadDeadlineGrace = 0
	manager.chromecastStatusPollEvery = 25 * time.Millisecond

	started := time.Now()
	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_playing_ack",
		Transcode:    "never",
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected beam_media to return quickly after playing feedback, elapsed=%s", elapsed)
	}

	// Release the blocked fake Load() call so the goroutine can exit in the test process.
	close(releaseLoad)
}

func TestBeamMediaDLNAOperationTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	payload := &fakeDLNAPayload{
		listenAddr:                "127.0.0.1:3563",
		blockPlayUntilContextDone: true,
	}
	manager := NewManager(&fakeDiscovery{devices: []domain.Device{{
		ID:       "dlna_timeout",
		Name:     "Timeout DLNA",
		Address:  "http://192.168.1.13:1400/device.xml",
		Protocol: "dlna",
	}}}, nil, &fakeDLNAFactory{payloads: []*fakeDLNAPayload{payload}})
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.beamOperationTimeout = 40 * time.Millisecond

	started := time.Now()
	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dlna_timeout",
		Transcode:    "never",
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("expected timeout to fail fast, elapsed=%s", elapsed)
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "PROTOCOL_ERROR" {
		t.Fatalf("expected PROTOCOL_ERROR, got %s", toolErr.Code)
	}
	if !strings.Contains(strings.ToLower(toolErr.Message), "context deadline exceeded") {
		t.Fatalf("expected context deadline message, got %q", toolErr.Message)
	}
}

func TestBeamMediaDLNARebindsPayloadContextForSessionLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	payload := &fakeDLNAPayload{listenAddr: "127.0.0.1:3571"}
	manager := NewManager(&fakeDiscovery{devices: []domain.Device{{
		ID:       "dlna_ctx",
		Name:     "Context TV",
		Address:  "http://192.168.1.15:1400/device.xml",
		Protocol: "dlna",
	}}}, nil, &fakeDLNAFactory{payloads: []*fakeDLNAPayload{payload}})
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.beamOperationTimeout = 40 * time.Millisecond

	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dlna_ctx",
		Transcode:    "never",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result")
	}

	time.Sleep(80 * time.Millisecond)

	payload.mu.Lock()
	defer payload.mu.Unlock()
	if payload.setContextCalls < 2 {
		t.Fatalf("expected payload context to be rebound for session lifecycle, got %d setContext calls", payload.setContextCalls)
	}
	if payload.ctx == nil {
		t.Fatal("expected payload context to be set")
	}
	if payload.ctx.Err() != nil {
		t.Fatalf("expected long-lived payload context, got err=%v", payload.ctx.Err())
	}
}

func TestBeamMediaRejectsLoopbackURLByDefault(t *testing.T) {
	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_loopback",
		Name:     "Loopback TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	manager := NewManager(discovery, &fakeCastFactory{client: &fakeCastClient{}}, nil)
	defer manager.Close(context.Background())

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "http://127.0.0.1/video.mp4",
		TargetDevice: "dev_loopback",
		Transcode:    "never",
	})
	if err == nil {
		t.Fatal("expected loopback URL policy error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "UNSUPPORTED_URL_PATTERN" {
		t.Fatalf("expected UNSUPPORTED_URL_PATTERN, got %s", toolErr.Code)
	}
	if len(toolErr.Limitations) == 0 || toolErr.Limitations[0].Code != "URL_LOOPBACK_BLOCKED" {
		t.Fatalf("expected loopback limitation details, got %+v", toolErr.Limitations)
	}
}

func TestBeamMediaStrictPathPolicyBlocked(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_path_policy",
		Name:     "Policy TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	manager := NewManager(discovery, &fakeCastFactory{client: &fakeCastClient{}}, nil)
	defer manager.Close(context.Background())
	manager.strictPathPolicy = true
	manager.allowedPathPrefixes = []string{filepath.Join(tmpDir, "allowed")}

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_path_policy",
		Transcode:    "never",
	})
	if err == nil {
		t.Fatal("expected strict path policy error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "FILE_NOT_READABLE" {
		t.Fatalf("expected FILE_NOT_READABLE, got %s", toolErr.Code)
	}
	if len(toolErr.Limitations) == 0 || toolErr.Limitations[0].Code != "PATH_POLICY_BLOCKED" {
		t.Fatalf("expected path policy limitation details, got %+v", toolErr.Limitations)
	}
}

func TestBeamMediaBindPolicyRejectsWildcard(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_bind_policy",
		Name:     "Bind Policy TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	manager := NewManager(discovery, &fakeCastFactory{client: &fakeCastClient{}}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "0.0.0.0:3570", nil
	}

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_bind_policy",
		Transcode:    "never",
	})
	if err == nil {
		t.Fatal("expected bind policy error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "PROTOCOL_ERROR" {
		t.Fatalf("expected PROTOCOL_ERROR, got %s", toolErr.Code)
	}
	if len(toolErr.Limitations) == 0 || toolErr.Limitations[0].Code != "BIND_WILDCARD_BLOCKED" {
		t.Fatalf("expected bind limitation details, got %+v", toolErr.Limitations)
	}
}

func TestBeamMediaUnsupportedProtocolHasActionableLimitation(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "sample.mp4")
	if err := os.WriteFile(mediaPath, []byte("not-real-media"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_unsupported_protocol",
		Name:     "Unknown Protocol TV",
		Address:  "http://127.0.0.1:8009",
		Protocol: "airplay",
	}}}
	manager := NewManager(discovery, &fakeCastFactory{client: &fakeCastClient{}}, nil)
	defer manager.Close(context.Background())

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dev_unsupported_protocol",
		Transcode:    "never",
	})
	if err == nil {
		t.Fatal("expected unsupported protocol error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "UNSUPPORTED_SOURCE_FOR_PROTOCOL" {
		t.Fatalf("expected UNSUPPORTED_SOURCE_FOR_PROTOCOL, got %s", toolErr.Code)
	}
	if len(toolErr.Limitations) == 0 || toolErr.Limitations[0].Code != "PROTOCOL_UNSUPPORTED" {
		t.Fatalf("expected protocol limitation details, got %+v", toolErr.Limitations)
	}
}

func TestBeamMediaDLNAFileAndStopWithHybridMonitor(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPath := filepath.Join(tmpDir, "movie.mp4")
	if err := os.WriteFile(mediaPath, []byte("sample"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	dlnaPayload := &fakeDLNAPayload{
		listenAddr:         "127.0.0.1:3510",
		transportResponses: [][]string{{"PLAYING", "OK", "1"}},
		positionResponse:   []string{"00:10:00", "00:00:03"},
	}
	manager := NewManager(&fakeDiscovery{devices: []domain.Device{{
		ID:       "dlna_1",
		Name:     "Living Room TV",
		Address:  "http://192.168.1.10:1400/device.xml",
		Protocol: "dlna",
	}}}, nil, &fakeDLNAFactory{payloads: []*fakeDLNAPayload{dlnaPayload}})
	defer manager.Close(context.Background())
	serverFactory := &fakeServerFactory{}
	manager.serverFactory = serverFactory
	manager.dlnaPollEvery = 15 * time.Millisecond

	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPath,
		TargetDevice: "dlna_1",
		Transcode:    "never",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result")
	}
	if result.SessionID == "" {
		t.Fatal("expected session ID")
	}
	if dlnaPayload.actionCount("Play1") != 1 {
		t.Fatalf("expected Play1 exactly once, got %d", dlnaPayload.actionCount("Play1"))
	}
	if len(serverFactory.servers) != 1 || !serverFactory.servers[0].startServerCalled {
		t.Fatal("expected DLNA StartServer to be used")
	}

	sess := waitForSession(t, manager, result.SessionID)
	if serverFactory.servers[0].lastScreen == nil {
		t.Fatal("expected callback screen to be wired")
	}
	serverFactory.servers[0].lastScreen.EmitMsg("Stopped")

	waitForCondition(t, 300*time.Millisecond, func() bool {
		sess.stateMu.Lock()
		defer sess.stateMu.Unlock()
		return sess.callbackSeen && sess.pollingSeen && sess.lastDLNAState != ""
	})

	stopResult, err := manager.StopBeaming(context.Background(), domain.StopRequest{SessionID: result.SessionID})
	if err != nil {
		t.Fatalf("stop beaming: %v", err)
	}
	if !stopResult.OK {
		t.Fatal("expected stop OK=true")
	}
	if dlnaPayload.actionCount("Stop") != 1 {
		t.Fatalf("expected Stop exactly once, got %d", dlnaPayload.actionCount("Stop"))
	}
	if !serverFactory.servers[0].stopCalled {
		t.Fatal("expected DLNA server stop")
	}
}

func TestBeamMediaDLNAURLDirectThenProxyFallback(t *testing.T) {
	direct := &fakeDLNAPayload{
		listenAddr: "127.0.0.1:3520",
		actionErr: map[string]error{
			"Play1": errors.New("dmr rejected direct URI"),
		},
	}
	proxy := &fakeDLNAPayload{listenAddr: "127.0.0.1:3521"}
	factory := &fakeDLNAFactory{payloads: []*fakeDLNAPayload{direct, proxy}}

	manager := NewManager(&fakeDiscovery{devices: []domain.Device{{
		ID:       "dlna_1",
		Name:     "Bedroom TV",
		Address:  "http://192.168.1.11:1400/device.xml",
		Protocol: "dlna",
	}}}, nil, factory)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.prepareURLMedia = func(ctx context.Context, sourceURL string) (any, string, error) {
		return io.NopCloser(strings.NewReader("video-bytes")), "video/mp4", nil
	}

	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "https://example.com/video.mp4",
		TargetDevice: "dlna_1",
		Transcode:    "auto",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result")
	}
	if factory.calls != 2 {
		t.Fatalf("expected two payload attempts (direct + proxy), got %d", factory.calls)
	}
	if direct.actionCount("Play1") != 1 {
		t.Fatalf("expected direct Play1 once, got %d", direct.actionCount("Play1"))
	}
	if proxy.actionCount("Play1") != 1 {
		t.Fatalf("expected proxy Play1 once, got %d", proxy.actionCount("Play1"))
	}
	if !containsWarning(result.Warnings, "falling back") {
		t.Fatalf("expected fallback warning, got %v", result.Warnings)
	}
}

func TestBeamMediaDLNAURLDirectAttemptTimeoutFallsBack(t *testing.T) {
	direct := &fakeDLNAPayload{
		listenAddr:                "127.0.0.1:3522",
		blockPlayUntilContextDone: true,
	}
	proxy := &fakeDLNAPayload{listenAddr: "127.0.0.1:3523"}
	factory := &fakeDLNAFactory{payloads: []*fakeDLNAPayload{direct, proxy}}

	manager := NewManager(&fakeDiscovery{devices: []domain.Device{{
		ID:       "dlna_1",
		Name:     "Bedroom TV",
		Address:  "http://192.168.1.12:1400/device.xml",
		Protocol: "dlna",
	}}}, nil, factory)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.prepareURLMedia = func(ctx context.Context, sourceURL string) (any, string, error) {
		return io.NopCloser(strings.NewReader("video-bytes")), "video/mp4", nil
	}
	manager.dlnaDirectURLAttemptTimeout = 25 * time.Millisecond

	startedAt := time.Now()
	result, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "https://example.com/video.mp4",
		TargetDevice: "dlna_1",
		Transcode:    "auto",
	})
	if err != nil {
		t.Fatalf("beam media: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result")
	}
	if time.Since(startedAt) > 750*time.Millisecond {
		t.Fatalf("expected fast fallback after direct timeout, elapsed=%s", time.Since(startedAt))
	}
	if factory.calls != 2 {
		t.Fatalf("expected two payload attempts (direct + proxy), got %d", factory.calls)
	}
	if direct.actionCount("Play1") != 1 {
		t.Fatalf("expected direct Play1 once, got %d", direct.actionCount("Play1"))
	}
	if proxy.actionCount("Play1") != 1 {
		t.Fatalf("expected proxy Play1 once, got %d", proxy.actionCount("Play1"))
	}
	if !containsWarning(result.Warnings, "falling back") {
		t.Fatalf("expected fallback warning, got %v", result.Warnings)
	}
}

func TestBeamMediaDLNARejectsHLSURL(t *testing.T) {
	manager := NewManager(&fakeDiscovery{devices: []domain.Device{{
		ID:       "dlna_1",
		Name:     "Bedroom TV",
		Address:  "http://192.168.1.11:1400/device.xml",
		Protocol: "dlna",
	}}}, nil, &fakeDLNAFactory{payloads: []*fakeDLNAPayload{{listenAddr: "127.0.0.1:3530"}}})
	defer manager.Close(context.Background())

	_, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       "https://example.com/live/stream.m3u8",
		TargetDevice: "dlna_1",
		Transcode:    "auto",
	})
	if err == nil {
		t.Fatal("expected error")
	}

	toolErr, ok := err.(*domain.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.Code != "UNSUPPORTED_SOURCE_FOR_PROTOCOL" {
		t.Fatalf("expected UNSUPPORTED_SOURCE_FOR_PROTOCOL, got %s", toolErr.Code)
	}
	if len(toolErr.Limitations) == 0 {
		t.Fatal("expected limitations details")
	}
	if len(toolErr.SuggestedFixes) == 0 {
		t.Fatal("expected suggested fixes")
	}
}

func TestBeamMediaReplacesActiveSessionPerDevice(t *testing.T) {
	tmpDir := t.TempDir()
	mediaPathOne := filepath.Join(tmpDir, "one.mp4")
	mediaPathTwo := filepath.Join(tmpDir, "two.mp4")
	if err := os.WriteFile(mediaPathOne, []byte("one"), 0o600); err != nil {
		t.Fatalf("write media one: %v", err)
	}
	if err := os.WriteFile(mediaPathTwo, []byte("two"), 0o600); err != nil {
		t.Fatalf("write media two: %v", err)
	}

	discovery := &fakeDiscovery{devices: []domain.Device{{
		ID:       "dev_1",
		Name:     "Living Room",
		Address:  "http://127.0.0.1:8009",
		Protocol: "chromecast",
	}}}
	clientOne := &fakeCastClient{}
	clientTwo := &fakeCastClient{}
	manager := NewManager(discovery, &fakeCastFactory{clients: []*fakeCastClient{clientOne, clientTwo}}, nil)
	defer manager.Close(context.Background())
	manager.serverFactory = &fakeServerFactory{}
	manager.listenAddressForDevice = func(deviceAddress string) (string, error) {
		return "127.0.0.1:3550", nil
	}

	first, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPathOne,
		TargetDevice: "dev_1",
		Transcode:    "never",
	})
	if err != nil {
		t.Fatalf("first beam media: %v", err)
	}
	second, err := manager.BeamMedia(context.Background(), domain.BeamRequest{
		Source:       mediaPathTwo,
		TargetDevice: "dev_1",
		Transcode:    "never",
	})
	if err != nil {
		t.Fatalf("second beam media: %v", err)
	}
	if first.SessionID == second.SessionID {
		t.Fatal("expected second beam to allocate a new session ID")
	}

	if clientOne.stopCalls != 1 {
		t.Fatalf("expected first client to be stopped once, got %d", clientOne.stopCalls)
	}
	if clientOne.closeCalls == 0 {
		t.Fatal("expected first client to be closed")
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.sessionsByID) != 1 {
		t.Fatalf("expected exactly one active session, got %d", len(manager.sessionsByID))
	}
	if manager.sessionByDeviceID["dev_1"] != second.SessionID {
		t.Fatalf("expected active session to be %s, got %s", second.SessionID, manager.sessionByDeviceID["dev_1"])
	}
	if _, ok := manager.sessionsByID[first.SessionID]; ok {
		t.Fatal("expected first session to be removed")
	}
}

func TestCleanupSweepIdleSession(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())
	manager.idleCleanupAfter = 40 * time.Millisecond
	manager.pausedCleanupAfter = time.Hour
	manager.maxSessionAge = time.Hour

	client := &fakeCastClient{statuses: []castprotocol.CastStatus{{PlayerState: "IDLE"}}}
	server := &fakeServer{}
	sess := &session{
		ID:         "sess_idle",
		DeviceID:   "dev_idle",
		DeviceName: "Idle TV",
		Protocol:   "chromecast",
		castClient: client,
		httpServer: server,
	}
	manager.initializeSessionLifecycle(sess, "idle", "")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	waitForCondition(t, 400*time.Millisecond, func() bool {
		manager.cleanupSweep()
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, ok := manager.sessionsByID[sess.ID]
		return !ok
	})

	if client.stopCalls != 1 {
		t.Fatalf("expected idle session stop once, got %d", client.stopCalls)
	}
	if !server.stopCalled {
		t.Fatal("expected idle session server stop")
	}
}

func TestCleanupSweepPausedSession(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())
	manager.idleCleanupAfter = time.Hour
	manager.pausedCleanupAfter = 40 * time.Millisecond
	manager.maxSessionAge = time.Hour

	client := &fakeCastClient{statuses: []castprotocol.CastStatus{{PlayerState: "PAUSED"}}}
	sess := &session{
		ID:         "sess_paused",
		DeviceID:   "dev_paused",
		DeviceName: "Paused TV",
		Protocol:   "chromecast",
		castClient: client,
		httpServer: &fakeServer{},
	}
	manager.initializeSessionLifecycle(sess, "paused", "")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	waitForCondition(t, 400*time.Millisecond, func() bool {
		manager.cleanupSweep()
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, ok := manager.sessionsByID[sess.ID]
		return !ok
	})

	if client.stopCalls != 1 {
		t.Fatalf("expected paused session stop once, got %d", client.stopCalls)
	}
}

func TestCleanupSweepMaxSessionAge(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())
	manager.idleCleanupAfter = time.Hour
	manager.pausedCleanupAfter = time.Hour
	manager.maxSessionAge = 20 * time.Millisecond

	client := &fakeCastClient{statuses: []castprotocol.CastStatus{{PlayerState: "BUFFERING"}}}
	sess := &session{
		ID:         "sess_old",
		DeviceID:   "dev_old",
		DeviceName: "Old TV",
		Protocol:   "chromecast",
		castClient: client,
		httpServer: &fakeServer{},
	}
	manager.initializeSessionLifecycle(sess, "buffering", "")
	sess.stateMu.Lock()
	sess.createdAt = time.Now().Add(-50 * time.Millisecond)
	sess.stateMu.Unlock()
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	manager.cleanupSweep()

	manager.mu.Lock()
	_, ok := manager.sessionsByID[sess.ID]
	manager.mu.Unlock()
	if ok {
		t.Fatal("expected max-age session to be cleaned up")
	}
	if client.stopCalls != 1 {
		t.Fatalf("expected max-age session stop once, got %d", client.stopCalls)
	}
}

func TestCleanupSweepKeepsPlayingSessionWithProgress(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	defer manager.Close(context.Background())
	manager.idleCleanupAfter = 40 * time.Millisecond
	manager.pausedCleanupAfter = time.Hour
	manager.maxSessionAge = time.Hour

	statuses := make([]castprotocol.CastStatus, 0, 64)
	for i := 0; i < 64; i++ {
		statuses = append(statuses, castprotocol.CastStatus{PlayerState: "PLAYING", CurrentTime: float32(i)})
	}
	client := &fakeCastClient{statuses: statuses}
	sess := &session{
		ID:         "sess_playing",
		DeviceID:   "dev_playing",
		DeviceName: "Movie TV",
		Protocol:   "chromecast",
		castClient: client,
		httpServer: &fakeServer{},
	}
	manager.initializeSessionLifecycle(sess, "playing", "0")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	deadline := time.Now().Add(180 * time.Millisecond)
	for time.Now().Before(deadline) {
		manager.cleanupSweep()
		manager.mu.Lock()
		_, ok := manager.sessionsByID[sess.ID]
		manager.mu.Unlock()
		if !ok {
			t.Fatal("expected playing session with progress to stay active")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerCloseTeardownStopsSessions(t *testing.T) {
	manager := NewManager(nil, nil, nil)

	client := &fakeCastClient{}
	server := &fakeServer{}
	closer := &fakeCloser{}
	sess := &session{
		ID:           "sess_close",
		DeviceID:     "dev_close",
		DeviceName:   "Close TV",
		Protocol:     "chromecast",
		castClient:   client,
		httpServer:   server,
		sourceCloser: closer,
	}
	manager.initializeSessionLifecycle(sess, "playing", "5")
	if _, stored := manager.storeSession(sess); !stored {
		t.Fatal("expected session to be stored")
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("close manager: %v", err)
	}

	if client.stopCalls != 1 {
		t.Fatalf("expected close to stop playback once, got %d", client.stopCalls)
	}
	if client.closeCalls == 0 {
		t.Fatal("expected close to close cast client")
	}
	if !server.stopCalled {
		t.Fatal("expected close to stop server")
	}
	if !closer.closed {
		t.Fatal("expected close to close source stream")
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.sessionsByID) != 0 {
		t.Fatalf("expected no sessions after close, got %d", len(manager.sessionsByID))
	}
}

func waitForSession(t *testing.T, manager *Manager, sessionID string) *session {
	t.Helper()
	var out *session
	waitForCondition(t, 300*time.Millisecond, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		out = manager.sessionsByID[sessionID]
		return out != nil
	})
	return out
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func containsWarning(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

var _ adapters.CastClient = (*fakeCastClient)(nil)
var _ adapters.CastFactory = (*fakeCastFactory)(nil)
var _ adapters.DLNAFactory = (*fakeDLNAFactory)(nil)
var _ adapters.DLNAPayload = (*fakeDLNAPayload)(nil)
