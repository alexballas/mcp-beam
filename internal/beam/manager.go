package beam

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alex/mcp-beam/internal/adapters"
	"github.com/alex/mcp-beam/internal/domain"
	"go2tv.app/go2tv/v2/httphandlers"
	"go2tv.app/go2tv/v2/soapcalls"
	"go2tv.app/go2tv/v2/utils"
)

const (
	defaultDiscoveryTimeoutMS  = 2500
	fallbackDiscoveryTimeoutMS = 12000

	transcodeAuto   = "auto"
	transcodeAlways = "always"
	transcodeNever  = "never"

	dlnaPollInterval      = 4 * time.Second
	dlnaMonitorStopWait   = 500 * time.Millisecond
	dlnaCallbackQueueSize = 16

	defaultIdleCleanupAfter   = 10 * time.Minute
	defaultPausedCleanupAfter = 90 * time.Minute
	defaultMaxSessionAge      = 24 * time.Hour
	defaultCleanupSweepEvery  = 5 * time.Second

	defaultRetryAttempts    = 3
	defaultRetryBaseBackoff = 120 * time.Millisecond
	defaultRetryMaxBackoff  = 800 * time.Millisecond
)

type deviceLister interface {
	ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error)
}

type streamServer interface {
	AddHandler(path string, payload *soapcalls.TVPayload, transcode *utils.TranscodeOptions, media any)
	StartServing(serverStarted chan<- error)
	StartServer(serverStarted chan<- error, media, subtitles any, tvpayload *soapcalls.TVPayload, screen httphandlers.Screen)
	StopServer()
}

type streamServerFactory interface {
	New(addr string) streamServer
}

type go2TVStreamServerFactory struct{}

func (go2TVStreamServerFactory) New(addr string) streamServer {
	return httphandlers.NewServer(addr)
}

type Manager struct {
	discovery              deviceLister
	castFactory            adapters.CastFactory
	dlnaFactory            adapters.DLNAFactory
	serverFactory          streamServerFactory
	lookPath               func(file string) (string, error)
	listenAddressForDevice func(deviceAddress string) (string, error)
	prepareURLMedia        func(ctx context.Context, sourceURL string) (any, string, error)

	dlnaPollEvery time.Duration

	idleCleanupAfter   time.Duration
	pausedCleanupAfter time.Duration
	maxSessionAge      time.Duration
	cleanupSweepEvery  time.Duration
	now                func() time.Time

	strictPathPolicy    bool
	allowedPathPrefixes []string
	allowLoopbackURLs   bool
	allowWildcardBind   bool
	redactPaths         bool

	retryAttempts    int
	retryBaseBackoff time.Duration
	retryMaxBackoff  time.Duration

	logf func(format string, args ...any)

	cleanupLoopCancel context.CancelFunc
	cleanupLoopDone   chan struct{}
	closeOnce         sync.Once
	closeErr          error

	mu                sync.Mutex
	sessionsByID      map[string]*session
	sessionByDeviceID map[string]string
	closed            bool
}

type session struct {
	ID          string
	DeviceID    string
	DeviceName  string
	MediaURL    string
	Transcoding bool
	Warnings    []string
	Protocol    string

	castClient   adapters.CastClient
	dlnaPayload  adapters.DLNAPayload
	httpServer   streamServer
	sourceCloser io.Closer

	monitorCancel context.CancelFunc
	monitorDone   chan struct{}
	callbackCh    <-chan string

	stateMu          sync.Mutex
	lastDLNAState    string
	lastDLNAPosition string
	callbackSeen     bool
	pollingSeen      bool

	createdAt         time.Time
	lastObservedAt    time.Time
	lastStateChangeAt time.Time
	lastProgressAt    time.Time
	lastPosition      string
	normalizedState   string

	closeOnce sync.Once
}

type preparedPlayback struct {
	mediaURL      string
	mediaType     string
	subtitleURL   string
	live          bool
	transcoding   bool
	mediaDuration float64
	warnings      []string
	httpServer    streamServer
	sourceCloser  io.Closer
}

type preparedDLNA struct {
	mediaURL     string
	transcoding  bool
	warnings     []string
	httpServer   streamServer
	sourceCloser io.Closer
	payload      adapters.DLNAPayload
	callbackCh   <-chan string
}

func NewManager(discovery deviceLister, castFactory adapters.CastFactory, dlnaFactory adapters.DLNAFactory) *Manager {
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	allowedPathPrefixes := parseAllowedPathPrefixes(os.Getenv("MCP_BEAM_ALLOWED_PATH_PREFIXES"))
	manager := &Manager{
		discovery:              discovery,
		castFactory:            castFactory,
		dlnaFactory:            dlnaFactory,
		serverFactory:          go2TVStreamServerFactory{},
		lookPath:               exec.LookPath,
		listenAddressForDevice: utils.URLtoListenIPandPort,
		prepareURLMedia:        utils.PrepareURLMedia,
		dlnaPollEvery:          dlnaPollInterval,
		idleCleanupAfter:       defaultIdleCleanupAfter,
		pausedCleanupAfter:     defaultPausedCleanupAfter,
		maxSessionAge:          defaultMaxSessionAge,
		cleanupSweepEvery:      defaultCleanupSweepEvery,
		now:                    time.Now,
		strictPathPolicy:       boolEnv("MCP_BEAM_STRICT_PATH_POLICY", false),
		allowedPathPrefixes:    allowedPathPrefixes,
		allowLoopbackURLs:      boolEnv("MCP_BEAM_ALLOW_LOOPBACK_URLS", false),
		allowWildcardBind:      boolEnv("MCP_BEAM_ALLOW_WILDCARD_BIND", false),
		redactPaths:            boolEnv("MCP_BEAM_REDACT_PATHS", true),
		retryAttempts:          defaultRetryAttempts,
		retryBaseBackoff:       defaultRetryBaseBackoff,
		retryMaxBackoff:        defaultRetryMaxBackoff,
		logf:                   func(string, ...any) {},
		cleanupLoopCancel:      cleanupCancel,
		cleanupLoopDone:        make(chan struct{}),
		sessionsByID:           map[string]*session{},
		sessionByDeviceID:      map[string]string{},
	}
	go manager.runCleanupLoop(cleanupCtx)
	return manager
}

func (m *Manager) BeamMedia(ctx context.Context, req domain.BeamRequest) (*domain.BeamResult, error) {
	if m.discovery == nil {
		return nil, toolError("INTERNAL_ERROR", "beam manager is not configured")
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}

	mode := normalizeTranscodeMode(req.Transcode)
	if mode == "" {
		return nil, toolError("INTERNAL_ERROR", "invalid transcode mode")
	}

	device, err := m.resolveDevice(ctx, req.TargetDevice)
	if err != nil {
		return nil, err
	}

	switch device.Protocol {
	case "chromecast":
		return m.beamChromecast(ctx, req, device, mode)
	case "dlna":
		return m.beamDLNA(ctx, req, device, mode)
	default:
		return nil, unsupportedProtocolError(device.Protocol)
	}
}

func (m *Manager) StopBeaming(_ context.Context, req domain.StopRequest) (*domain.StopResult, error) {
	if req.SessionID == "" && req.TargetDevice == "" {
		return nil, toolError("INTERNAL_ERROR", "either session_id or target_device is required")
	}

	sess := m.takeSession(req)
	if sess == nil {
		return nil, toolError("DEVICE_NOT_FOUND", "no active session matches the provided target")
	}

	if err := shutdownSession(sess, true); err != nil {
		return nil, toolError("PROTOCOL_ERROR", err.Error())
	}

	return &domain.StopResult{
		OK:               true,
		StoppedSessionID: sess.ID,
		DeviceID:         sess.DeviceID,
	}, nil
}

func (m *Manager) beamChromecast(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*domain.BeamResult, error) {
	if m.castFactory == nil {
		return nil, toolError("INTERNAL_ERROR", "Chromecast adapter is not configured")
	}

	client, err := m.castFactory.NewCastClient(device.Address)
	if err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to create Chromecast client: %v", err))
	}
	if err := m.withRetry(ctx, "chromecast_connect", func() error {
		return client.Connect()
	}); err != nil {
		_ = client.Close(true)
		return nil, toolError("DEVICE_UNREACHABLE", fmt.Sprintf("failed to connect to Chromecast device: %v", err))
	}

	playback, err := m.preparePlayback(ctx, req, device, mode)
	if err != nil {
		_ = client.Close(true)
		return nil, err
	}

	if err := m.withRetry(ctx, "chromecast_load", func() error {
		return client.Load(playback.mediaURL, playback.mediaType, 0, playback.mediaDuration, playback.subtitleURL, playback.live)
	}); err != nil {
		cleanupPrepared(playback)
		_ = client.Close(true)
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start Chromecast playback: %v", err))
	}

	sess := &session{
		ID:           newSessionID(),
		DeviceID:     device.ID,
		DeviceName:   device.Name,
		MediaURL:     playback.mediaURL,
		Transcoding:  playback.transcoding,
		Warnings:     playback.warnings,
		Protocol:     "chromecast",
		castClient:   client,
		httpServer:   playback.httpServer,
		sourceCloser: playback.sourceCloser,
	}
	m.initializeSessionLifecycle(sess, "buffering", "")
	replaced, stored := m.storeSession(sess)
	if !stored {
		_ = shutdownSession(sess, true)
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	_ = shutdownSession(replaced, true)

	return &domain.BeamResult{
		OK:          true,
		SessionID:   sess.ID,
		DeviceID:    sess.DeviceID,
		MediaURL:    sess.MediaURL,
		Transcoding: sess.Transcoding,
		Warnings:    append([]string{}, sess.Warnings...),
	}, nil
}

func (m *Manager) beamDLNA(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*domain.BeamResult, error) {
	if m.dlnaFactory == nil {
		return nil, toolError("INTERNAL_ERROR", "DLNA adapter is not configured")
	}
	if m.serverFactory == nil {
		return nil, toolError("INTERNAL_ERROR", "stream server factory is not configured")
	}

	source := strings.TrimSpace(req.Source)
	if source == "" {
		return nil, toolError("UNSUPPORTED_MEDIA", "source is empty")
	}

	isURL := false
	if parsed, err := url.Parse(source); err == nil && parsed.Scheme != "" {
		isURL = true
	}

	var prepared *preparedDLNA
	var err error
	if isURL {
		prepared, err = m.prepareDLNAURLPlayback(ctx, req, device, mode)
	} else {
		prepared, err = m.prepareDLNAFilePlayback(ctx, req, device, mode)
	}
	if err != nil {
		return nil, err
	}

	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	sess := &session{
		ID:            newSessionID(),
		DeviceID:      device.ID,
		DeviceName:    device.Name,
		MediaURL:      prepared.mediaURL,
		Transcoding:   prepared.transcoding,
		Warnings:      append([]string{}, prepared.warnings...),
		Protocol:      "dlna",
		dlnaPayload:   prepared.payload,
		httpServer:    prepared.httpServer,
		sourceCloser:  prepared.sourceCloser,
		monitorCancel: monitorCancel,
		monitorDone:   make(chan struct{}),
		callbackCh:    prepared.callbackCh,
	}
	m.initializeSessionLifecycle(sess, "buffering", "")

	if m.dlnaPollEvery <= 0 {
		m.dlnaPollEvery = dlnaPollInterval
	}
	go m.runDLNAStateMonitor(monitorCtx, sess)

	replaced, stored := m.storeSession(sess)
	if !stored {
		_ = shutdownSession(sess, true)
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	_ = shutdownSession(replaced, true)

	return &domain.BeamResult{
		OK:          true,
		SessionID:   sess.ID,
		DeviceID:    sess.DeviceID,
		MediaURL:    sess.MediaURL,
		Transcoding: sess.Transcoding,
		Warnings:    append([]string{}, sess.Warnings...),
	}, nil
}

func (m *Manager) resolveDevice(ctx context.Context, target string) (*domain.Device, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, toolError("DEVICE_NOT_FOUND", "target_device is empty")
	}

	timeouts := []int{defaultDiscoveryTimeoutMS, fallbackDiscoveryTimeoutMS}
	for i, timeoutMS := range timeouts {
		if i > 0 && timeoutMS == timeouts[i-1] {
			continue
		}

		devs, err := m.discovery.ListLocalHardware(ctx, timeoutMS, true)
		if err != nil {
			return nil, toolError("INTERNAL_ERROR", fmt.Sprintf("device discovery failed: %v", err))
		}
		if matched := matchTargetDevice(devs, target); matched != nil {
			return matched, nil
		}
	}

	return nil, toolError("DEVICE_NOT_FOUND", fmt.Sprintf("device not found: %s", target))
}

func matchTargetDevice(devices []domain.Device, target string) *domain.Device {
	target = strings.TrimSpace(target)
	normalizedTarget := normalizeDeviceTarget(target)

	for i := range devices {
		if strings.TrimSpace(devices[i].ID) == target {
			return &devices[i]
		}
	}
	for i := range devices {
		if strings.TrimSpace(devices[i].Name) == target {
			return &devices[i]
		}
	}
	for i := range devices {
		if strings.EqualFold(strings.TrimSpace(devices[i].ID), target) {
			return &devices[i]
		}
		if strings.EqualFold(strings.TrimSpace(devices[i].Name), target) {
			return &devices[i]
		}
		if normalizeDeviceTarget(devices[i].Name) == normalizedTarget {
			return &devices[i]
		}
	}
	return nil
}

func normalizeDeviceTarget(v string) string {
	normalized := strings.ToLower(strings.TrimSpace(v))
	if idx := strings.LastIndex(normalized, " ("); idx > 0 && strings.HasSuffix(normalized, ")") {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	return normalized
}

func (m *Manager) preparePlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*preparedPlayback, error) {
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return nil, toolError("UNSUPPORTED_MEDIA", "source is empty")
	}

	if parsed, err := url.Parse(source); err == nil && parsed.Scheme != "" {
		return m.prepareURLPlayback(ctx, req, device, mode)
	}
	return m.prepareFilePlayback(req, device, mode)
}

func (m *Manager) prepareFilePlayback(req domain.BeamRequest, device *domain.Device, mode string) (*preparedPlayback, error) {
	source, err := m.validateLocalFilePath(req.Source, "source")
	if err != nil {
		return nil, err
	}

	mediaType := detectFileMediaType(source)
	transcoding, warnings, ffmpegPath, err := m.computeTranscodingForFile(mode, mediaType, source)
	if err != nil {
		return nil, err
	}

	listenAddr, server, err := m.newStreamServer(device.Address)
	if err != nil {
		return nil, err
	}

	route := mediaRouteFor(source)
	var tcOpts *utils.TranscodeOptions
	mediaDuration := 0.0
	if transcoding {
		tcOpts = &utils.TranscodeOptions{
			FFmpegPath:   ffmpegPath,
			SubsPath:     validatedSubtitlePath(req.SubtitlesPath),
			SeekSeconds:  0,
			SubtitleSize: utils.SubtitleSizeMedium,
		}
		mediaType = "video/mp4"
		if duration, durationErr := utils.DurationForMediaSeconds(ffmpegPath, source); durationErr == nil {
			mediaDuration = duration
		} else {
			warnings = append(warnings, "failed to determine media duration for transcoded stream")
		}
	}

	server.AddHandler(route, nil, tcOpts, source)

	subtitleURL, subtitleWarnings, subtitleErr := m.addSubtitleSidecar(server, listenAddr, req.SubtitlesPath, transcoding)
	if subtitleErr != nil {
		cleanupPrepared(&preparedPlayback{httpServer: server})
		return nil, subtitleErr
	}
	warnings = append(warnings, subtitleWarnings...)

	if err := startStreamServer(server); err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start media server: %v", err))
	}

	return &preparedPlayback{
		mediaURL:      "http://" + listenAddr + route,
		mediaType:     mediaType,
		subtitleURL:   subtitleURL,
		live:          false,
		transcoding:   transcoding,
		mediaDuration: mediaDuration,
		warnings:      warnings,
		httpServer:    server,
	}, nil
}

func (m *Manager) prepareURLPlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*preparedPlayback, error) {
	sourceURL := strings.TrimSpace(req.Source)
	if _, err := m.validateSourceURLPolicy(sourceURL); err != nil {
		return nil, err
	}

	if utils.IsHLSStream(sourceURL, "") {
		if mode == transcodeAlways {
			return nil, toolError("TRANSCODE_REQUIRED", "transcode=always is not supported for direct HLS URL casting")
		}
		return &preparedPlayback{
			mediaURL:    sourceURL,
			mediaType:   "application/vnd.apple.mpegurl",
			subtitleURL: "",
			live:        true,
			warnings:    []string{},
		}, nil
	}

	var preparedMedia any
	var mediaType string
	err := m.withRetry(ctx, "prepare_url_media", func() error {
		var callErr error
		preparedMedia, mediaType, callErr = m.prepareURLMedia(ctx, sourceURL)
		return callErr
	})
	if err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to stream source URL: %v", err))
	}
	if mediaType == "" || mediaType == "/" {
		mediaType = "application/octet-stream"
	}

	warnings := []string{}
	transcoding := false
	ffmpegPath := ""
	if mode == transcodeAlways {
		if strings.Contains(mediaType, "video") {
			ffmpegPath, err = m.requireFFmpeg()
			if err != nil {
				if closer, ok := preparedMedia.(io.Closer); ok {
					_ = closer.Close()
				}
				return nil, err
			}
			transcoding = true
		} else {
			warnings = append(warnings, "transcode=always ignored for non-video URL source")
		}
	} else if mode == transcodeAuto && strings.Contains(mediaType, "video") {
		warnings = append(warnings, "auto transcode for URL sources defaults to direct stream")
	}

	listenAddr, server, err := m.newStreamServer(device.Address)
	if err != nil {
		if closer, ok := preparedMedia.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, err
	}

	route := mediaRouteFor(sourceURL)
	var tcOpts *utils.TranscodeOptions
	if transcoding {
		tcOpts = &utils.TranscodeOptions{
			FFmpegPath:   ffmpegPath,
			SubsPath:     validatedSubtitlePath(req.SubtitlesPath),
			SeekSeconds:  0,
			SubtitleSize: utils.SubtitleSizeMedium,
		}
		mediaType = "video/mp4"
	}

	server.AddHandler(route, nil, tcOpts, preparedMedia)

	subtitleURL, subtitleWarnings, subtitleErr := m.addSubtitleSidecar(server, listenAddr, req.SubtitlesPath, transcoding)
	if subtitleErr != nil {
		cleanupPrepared(&preparedPlayback{httpServer: server, sourceCloser: asCloser(preparedMedia)})
		return nil, subtitleErr
	}
	warnings = append(warnings, subtitleWarnings...)

	if err := startStreamServer(server); err != nil {
		cleanupPrepared(&preparedPlayback{httpServer: server, sourceCloser: asCloser(preparedMedia)})
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start media server: %v", err))
	}

	return &preparedPlayback{
		mediaURL:     "http://" + listenAddr + route,
		mediaType:    mediaType,
		subtitleURL:  subtitleURL,
		live:         true,
		transcoding:  transcoding,
		warnings:     warnings,
		httpServer:   server,
		sourceCloser: asCloser(preparedMedia),
	}, nil
}

func (m *Manager) prepareDLNAFilePlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*preparedDLNA, error) {
	source, err := m.validateLocalFilePath(req.Source, "source")
	if err != nil {
		return nil, err
	}

	subtitlesPath, err := m.validateDLNASubtitles(req.SubtitlesPath)
	if err != nil {
		return nil, err
	}

	mediaType := detectFileMediaType(source)
	transcoding, warnings, ffmpegPath, err := m.computeTranscodingForDLNA(mode, mediaType)
	if err != nil {
		return nil, err
	}

	prepared, err := m.startDLNAServerAndPlay(ctx, device, source, mediaType, source, subtitlesPath, transcoding, ffmpegPath, "")
	if err != nil {
		return nil, err
	}
	prepared.warnings = append(prepared.warnings, warnings...)
	return prepared, nil
}

func (m *Manager) prepareDLNAURLPlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*preparedDLNA, error) {
	sourceURL := strings.TrimSpace(req.Source)
	if _, err := m.validateSourceURLPolicy(sourceURL); err != nil {
		return nil, err
	}

	if utils.IsHLSStream(sourceURL, "") {
		return nil, dlnaHLSUnsupportedError()
	}

	subtitlesPath, err := m.validateDLNASubtitles(req.SubtitlesPath)
	if err != nil {
		return nil, err
	}

	warnings := []string{}
	if mode != transcodeAlways {
		directType := detectURLMediaType(sourceURL)
		direct, directErr := m.startDLNAServerAndPlay(ctx, device, []byte("dlna-direct-url-placeholder"), directType, sourceURL, subtitlesPath, false, "", sourceURL)
		if directErr == nil {
			direct.warnings = append(direct.warnings, warnings...)
			return direct, nil
		}
		warnings = append(warnings, "direct DLNA URL playback failed; falling back to local proxy")
	}

	var preparedMedia any
	var mediaType string
	err = m.withRetry(ctx, "prepare_url_media", func() error {
		var callErr error
		preparedMedia, mediaType, callErr = m.prepareURLMedia(ctx, sourceURL)
		return callErr
	})
	if err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to stream source URL: %v", err))
	}

	if mediaType == "" || mediaType == "/" {
		mediaType = detectURLMediaType(sourceURL)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
	}

	transcoding, tcWarnings, ffmpegPath, err := m.computeTranscodingForDLNA(mode, mediaType)
	if err != nil {
		if closer := asCloser(preparedMedia); closer != nil {
			_ = closer.Close()
		}
		return nil, err
	}
	warnings = append(warnings, tcWarnings...)

	prepared, err := m.startDLNAServerAndPlay(ctx, device, preparedMedia, mediaType, sourceURL, subtitlesPath, transcoding, ffmpegPath, "")
	if err != nil {
		if closer := asCloser(preparedMedia); closer != nil {
			_ = closer.Close()
		}
		return nil, err
	}
	prepared.warnings = append(prepared.warnings, warnings...)
	prepared.sourceCloser = asCloser(preparedMedia)
	return prepared, nil
}

func (m *Manager) startDLNAServerAndPlay(
	ctx context.Context,
	device *domain.Device,
	media any,
	mediaType string,
	mediaPath string,
	subtitlesPath string,
	transcoding bool,
	ffmpegPath string,
	overrideMediaURL string,
) (*preparedDLNA, error) {
	payload, err := m.newDLNAPayload(ctx, device, mediaPath, mediaType, subtitlesPath, transcoding, ffmpegPath)
	if err != nil {
		return nil, err
	}

	server := m.serverFactory.New(payload.ListenAddress())
	serverStarted := make(chan error, 1)
	callbackStateCh := make(chan string, dlnaCallbackQueueSize)
	screen := &dlnaMonitorScreen{stateCh: callbackStateCh}

	go server.StartServer(serverStarted, media, subtitlesPath, payload.RawPayload(), screen)
	if err := <-serverStarted; err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start DLNA media server: %v", err))
	}

	if strings.TrimSpace(overrideMediaURL) != "" {
		payload.SetMediaURL(strings.TrimSpace(overrideMediaURL))
	}

	if err := m.withRetry(ctx, "dlna_play", func() error {
		return payload.SendtoTV("Play1")
	}); err != nil {
		server.StopServer()
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start DLNA playback: %v", err))
	}

	return &preparedDLNA{
		mediaURL:    payload.MediaURL(),
		transcoding: transcoding,
		warnings:    []string{},
		httpServer:  server,
		payload:     payload,
		callbackCh:  callbackStateCh,
	}, nil
}

func (m *Manager) newDLNAPayload(
	ctx context.Context,
	device *domain.Device,
	mediaPath string,
	mediaType string,
	subtitlesPath string,
	transcoding bool,
	ffmpegPath string,
) (adapters.DLNAPayload, error) {
	if strings.TrimSpace(mediaPath) == "" {
		return nil, toolError("UNSUPPORTED_MEDIA", "source is empty")
	}

	subsForPayload := strings.TrimSpace(subtitlesPath)
	if subsForPayload == "" {
		subsForPayload = ""
	}

	payload, err := m.dlnaFactory.NewTVPayload(&soapcalls.Options{
		Ctx:            ctx,
		DMR:            device.Address,
		Media:          mediaPath,
		Subs:           subsForPayload,
		Mtype:          mediaType,
		Transcode:      transcoding,
		Seek:           !transcoding,
		FFmpegPath:     ffmpegPath,
		FFmpegSubsPath: validatedSubtitlePath(subtitlesPath),
		FFmpegSeek:     0,
	})
	if err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to initialize DLNA payload: %v", err))
	}

	payload.SetContext(ctx)
	return payload, nil
}

func (m *Manager) runDLNAStateMonitor(ctx context.Context, sess *session) {
	defer close(sess.monitorDone)
	if sess == nil || sess.dlnaPayload == nil {
		return
	}

	m.pollDLNAState(sess)

	ticker := time.NewTicker(m.dlnaPollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-sess.callbackCh:
			if !ok {
				sess.callbackCh = nil
				continue
			}
			state := normalizeDLNAState(update)
			if state != "" {
				sess.recordDLNAState(state, "", true, m.now())
			}
		case <-ticker.C:
			m.pollDLNAState(sess)
		}
	}
}

func (m *Manager) pollDLNAState(sess *session) {
	if sess == nil || sess.dlnaPayload == nil {
		return
	}

	transport, err := sess.dlnaPayload.GetTransportInfo()
	if err != nil {
		return
	}

	state := normalizeDLNATransport(transport)
	position := ""
	if state == "playing" {
		if p, pErr := sess.dlnaPayload.GetPositionInfo(); pErr == nil && len(p) >= 2 {
			position = strings.TrimSpace(p[1])
		}
	}

	sess.recordDLNAState(state, position, false, m.now())
}

func (s *session) recordDLNAState(state, position string, fromCallback bool, observedAt time.Time) {
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.lastDLNAState = state
	if strings.TrimSpace(position) != "" {
		s.lastDLNAPosition = strings.TrimSpace(position)
	}
	if fromCallback {
		s.callbackSeen = true
	} else {
		s.pollingSeen = true
	}
	s.recordObservationLocked(state, position, observedAt)
}

func (m *Manager) computeTranscodingForFile(mode, mediaType, source string) (bool, []string, string, error) {
	warnings := []string{}
	if !strings.Contains(mediaType, "video") {
		if mode == transcodeAlways {
			warnings = append(warnings, "transcode=always ignored for non-video source")
		}
		return false, warnings, "", nil
	}

	if mode == transcodeNever {
		return false, warnings, "", nil
	}

	ffmpegPath, lookupErr := m.lookPath("ffmpeg")
	if lookupErr != nil {
		if mode == transcodeAlways {
			return false, nil, "", ffmpegNotFoundError()
		}
		warnings = append(warnings, "ffmpeg unavailable; auto transcode disabled")
		return false, warnings, "", nil
	}

	if mode == transcodeAlways {
		return true, warnings, ffmpegPath, nil
	}

	codecInfo, err := utils.GetMediaCodecInfo(ffmpegPath, source)
	if err != nil {
		warnings = append(warnings, "codec probe failed; auto transcode disabled")
		return false, warnings, ffmpegPath, nil
	}

	if utils.IsChromecastCompatible(codecInfo) {
		return false, warnings, ffmpegPath, nil
	}
	warnings = append(warnings, "media is not Chromecast-compatible; enabling auto transcode")
	return true, warnings, ffmpegPath, nil
}

func (m *Manager) computeTranscodingForDLNA(mode, mediaType string) (bool, []string, string, error) {
	warnings := []string{}
	if !strings.Contains(strings.ToLower(mediaType), "video") {
		if mode == transcodeAlways {
			warnings = append(warnings, "transcode=always ignored for non-video source")
		}
		return false, warnings, "", nil
	}

	switch mode {
	case transcodeNever:
		return false, warnings, "", nil
	case transcodeAuto:
		return false, warnings, "", nil
	case transcodeAlways:
		ffmpegPath, err := m.requireFFmpeg()
		if err != nil {
			return false, nil, "", err
		}
		return true, warnings, ffmpegPath, nil
	default:
		return false, nil, "", toolError("INTERNAL_ERROR", "invalid transcode mode")
	}
}

func (m *Manager) requireFFmpeg() (string, error) {
	ffmpegPath, err := m.lookPath("ffmpeg")
	if err != nil {
		return "", ffmpegNotFoundError()
	}
	return ffmpegPath, nil
}

func (m *Manager) newStreamServer(deviceAddress string) (string, streamServer, error) {
	if m.listenAddressForDevice == nil {
		return "", nil, toolError("INTERNAL_ERROR", "listen address resolver is not configured")
	}

	listenAddr, err := m.listenAddressForDevice(deviceAddress)
	if err != nil {
		return "", nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to select media listen address: %v", err))
	}
	if err := m.validateBindAddress(listenAddr); err != nil {
		return "", nil, err
	}

	if m.serverFactory == nil {
		return "", nil, toolError("INTERNAL_ERROR", "stream server factory is not configured")
	}
	return listenAddr, m.serverFactory.New(listenAddr), nil
}

func (m *Manager) addSubtitleSidecar(server streamServer, listenAddr, subtitlesPath string, transcoding bool) (string, []string, error) {
	warnings := []string{}
	if strings.TrimSpace(subtitlesPath) == "" {
		return "", warnings, nil
	}
	if transcoding {
		return "", warnings, nil
	}

	validatedPath, err := m.validateLocalFilePath(subtitlesPath, "subtitles_path")
	if err != nil {
		return "", warnings, err
	}
	subtitlesPath = validatedPath

	route := "/subs-" + randomToken(8) + ".vtt"
	ext := strings.ToLower(filepath.Ext(subtitlesPath))
	switch ext {
	case ".srt":
		webvttData, err := utils.ConvertSRTtoWebVTT(subtitlesPath)
		if err != nil {
			warnings = append(warnings, "failed to convert SRT subtitles to WebVTT")
			return "", warnings, nil
		}
		server.AddHandler(route, nil, nil, webvttData)
		return "http://" + listenAddr + route, warnings, nil
	case ".vtt":
		server.AddHandler(route, nil, nil, subtitlesPath)
		return "http://" + listenAddr + route, warnings, nil
	default:
		warnings = append(warnings, "unsupported subtitle format; expected .srt or .vtt")
		return "", warnings, nil
	}
}

func (m *Manager) withRetry(ctx context.Context, operation string, call func() error) error {
	if call == nil {
		return errors.New("retry call is nil")
	}

	attempts := m.retryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	baseBackoff := m.retryBaseBackoff
	if baseBackoff < 0 {
		baseBackoff = 0
	}
	maxBackoff := m.retryMaxBackoff
	if maxBackoff < baseBackoff {
		maxBackoff = baseBackoff
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := call()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt >= attempts || !isTransientNetworkError(err) {
			break
		}

		backoff := backoffForAttempt(baseBackoff, maxBackoff, attempt)
		m.safeLogf("retrying operation=%s attempt=%d/%d backoff=%s err=%s", operation, attempt+1, attempts, backoff, err.Error())
		if waitErr := waitForBackoff(ctx, backoff); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

func backoffForAttempt(base, max time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	backoff := base
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if max > 0 && backoff >= max {
			return max
		}
	}
	if max > 0 && backoff > max {
		return max
	}
	return backoff
}

func waitForBackoff(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	msg := strings.ToLower(err.Error())
	transientPatterns := []string{
		"timeout",
		"temporar",
		"connection reset",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"i/o timeout",
		"network is unreachable",
		"no route to host",
		"tls handshake timeout",
	}
	for _, pattern := range transientPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

func (m *Manager) safeLogf(format string, args ...any) {
	if m == nil || m.logf == nil {
		return
	}
	m.logf(format, args...)
}

func (m *Manager) takeSession(req domain.StopRequest) *session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if req.SessionID != "" {
		sess, ok := m.sessionsByID[req.SessionID]
		if !ok {
			return nil
		}
		delete(m.sessionsByID, req.SessionID)
		delete(m.sessionByDeviceID, sess.DeviceID)
		return sess
	}

	for id, sess := range m.sessionsByID {
		if sess.DeviceID == req.TargetDevice || sess.DeviceName == req.TargetDevice {
			delete(m.sessionsByID, id)
			delete(m.sessionByDeviceID, sess.DeviceID)
			return sess
		}
	}

	return nil
}

func (m *Manager) initializeSessionLifecycle(sess *session, state, position string) {
	if sess == nil {
		return
	}
	now := m.now()
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	sess.createdAt = now
	sess.recordObservationLocked(normalizeDLNAState(state), position, now)
}

func (m *Manager) storeSession(sess *session) (*session, bool) {
	if sess == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false
	}

	var replaced *session
	if oldID, ok := m.sessionByDeviceID[sess.DeviceID]; ok {
		replaced = m.sessionsByID[oldID]
		delete(m.sessionsByID, oldID)
	}
	m.sessionsByID[sess.ID] = sess
	m.sessionByDeviceID[sess.DeviceID] = sess.ID
	return replaced, true
}

func (m *Manager) snapshotSessions() []*session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session, 0, len(m.sessionsByID))
	for _, sess := range m.sessionsByID {
		out = append(out, sess)
	}
	return out
}

func (m *Manager) detachSessionByID(sessionID string) *session {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	sess := m.sessionsByID[sessionID]
	if sess == nil {
		return nil
	}
	delete(m.sessionsByID, sessionID)
	delete(m.sessionByDeviceID, sess.DeviceID)
	return sess
}

func (m *Manager) detachAllSessions() []*session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session, 0, len(m.sessionsByID))
	for id, sess := range m.sessionsByID {
		out = append(out, sess)
		delete(m.sessionsByID, id)
		delete(m.sessionByDeviceID, sess.DeviceID)
	}
	return out
}

func (m *Manager) runCleanupLoop(ctx context.Context) {
	defer close(m.cleanupLoopDone)

	sweepEvery := m.cleanupSweepEvery
	if sweepEvery <= 0 {
		sweepEvery = defaultCleanupSweepEvery
	}
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cleanupSweep()
		}
	}
}

func (m *Manager) cleanupSweep() {
	now := m.now()
	sessions := m.snapshotSessions()
	for _, sess := range sessions {
		if sess == nil {
			continue
		}

		m.observeSession(sess, now)
		if !m.shouldCleanupSession(sess, now) {
			continue
		}
		detached := m.detachSessionByID(sess.ID)
		_ = shutdownSession(detached, true)
	}
}

func (m *Manager) observeSession(sess *session, observedAt time.Time) {
	if sess == nil {
		return
	}

	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return
		}
		status, err := sess.castClient.GetStatus()
		if err != nil || status == nil {
			return
		}
		state := normalizeCastState(status.PlayerState)
		position := ""
		if state == "playing" {
			position = strconv.FormatInt(int64(status.CurrentTime), 10)
		}
		sess.stateMu.Lock()
		sess.recordObservationLocked(state, position, observedAt)
		sess.stateMu.Unlock()
	case "dlna":
		// DLNA state is fed by the dedicated polling+callback monitor.
	default:
		return
	}
}

func (m *Manager) shouldCleanupSession(sess *session, now time.Time) bool {
	if sess == nil {
		return false
	}

	idleAfter := m.idleCleanupAfter
	pausedAfter := m.pausedCleanupAfter
	maxAge := m.maxSessionAge

	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()

	createdAt := sess.createdAt
	if createdAt.IsZero() {
		createdAt = now
		sess.createdAt = createdAt
	}
	if maxAge > 0 && now.Sub(createdAt) >= maxAge {
		return true
	}

	state := strings.TrimSpace(sess.normalizedState)
	if state == "" {
		state = "idle"
	}

	lastStateAt := sess.lastStateChangeAt
	if lastStateAt.IsZero() {
		lastStateAt = createdAt
	}

	switch state {
	case "playing":
		if idleAfter <= 0 || strings.TrimSpace(sess.lastPosition) == "" {
			return false
		}
		lastProgressAt := sess.lastProgressAt
		if lastProgressAt.IsZero() {
			lastProgressAt = lastStateAt
		}
		return now.Sub(lastProgressAt) >= idleAfter
	case "buffering":
		return false
	case "paused":
		if pausedAfter <= 0 {
			return false
		}
		return now.Sub(lastStateAt) >= pausedAfter
	default:
		if idleAfter <= 0 {
			return false
		}
		return now.Sub(lastStateAt) >= idleAfter
	}
}

func (s *session) recordObservationLocked(state, position string, observedAt time.Time) {
	state = strings.TrimSpace(state)
	if state == "" {
		state = "idle"
	}
	position = strings.TrimSpace(position)

	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if s.createdAt.IsZero() {
		s.createdAt = observedAt
	}
	if s.normalizedState != state {
		s.normalizedState = state
		s.lastStateChangeAt = observedAt
	}
	if position != "" {
		if s.lastPosition != position {
			s.lastProgressAt = observedAt
			s.lastPosition = position
		}
	}
	if s.lastObservedAt.IsZero() || observedAt.After(s.lastObservedAt) {
		s.lastObservedAt = observedAt
	}
}

func normalizeCastState(state string) string {
	return normalizeDLNAState(state)
}

func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()

		if m.cleanupLoopCancel != nil {
			m.cleanupLoopCancel()
		}

		if m.cleanupLoopDone != nil {
			select {
			case <-m.cleanupLoopDone:
			case <-ctx.Done():
				m.closeErr = ctx.Err()
				return
			}
		}

		sessions := m.detachAllSessions()
		var errs []string
		for _, sess := range sessions {
			if err := shutdownSession(sess, true); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			m.closeErr = errors.New(strings.Join(errs, "; "))
		}
	})

	return m.closeErr
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func shutdownSession(sess *session, stopMedia bool) error {
	if sess == nil {
		return nil
	}

	var shutdownErr error
	sess.closeOnce.Do(func() {
		if sess.monitorCancel != nil {
			sess.monitorCancel()
		}
		if sess.monitorDone != nil {
			select {
			case <-sess.monitorDone:
			case <-time.After(dlnaMonitorStopWait):
			}
		}

		var errs []string
		if sess.castClient != nil {
			if stopMedia {
				if err := sess.castClient.Stop(); err != nil {
					errs = append(errs, fmt.Sprintf("stop: %v", err))
				}
			}
			if err := sess.castClient.Close(true); err != nil {
				errs = append(errs, fmt.Sprintf("close: %v", err))
			}
		}
		if sess.dlnaPayload != nil && stopMedia {
			if err := sess.dlnaPayload.SendtoTV("Stop"); err != nil {
				errs = append(errs, fmt.Sprintf("stop: %v", err))
			}
		}
		if sess.httpServer != nil {
			sess.httpServer.StopServer()
		}
		if sess.sourceCloser != nil {
			_ = sess.sourceCloser.Close()
		}

		if len(errs) > 0 {
			shutdownErr = errors.New(strings.Join(errs, "; "))
		}
	})
	return shutdownErr
}

func cleanupPrepared(p *preparedPlayback) {
	if p == nil {
		return
	}
	if p.httpServer != nil {
		p.httpServer.StopServer()
	}
	if p.sourceCloser != nil {
		_ = p.sourceCloser.Close()
	}
}

func startStreamServer(server streamServer) error {
	serverStarted := make(chan error, 1)
	go server.StartServing(serverStarted)
	return <-serverStarted
}

func detectFileMediaType(source string) string {
	mediaType, err := utils.GetMimeDetailsFromPath(source)
	if err == nil && mediaType != "" && mediaType != "/" && mediaType != "application/octet-stream" {
		return mediaType
	}

	ext := strings.ToLower(filepath.Ext(source))
	if ext != "" {
		if guessed := mime.TypeByExtension(ext); guessed != "" {
			parts := strings.Split(guessed, ";")
			return strings.TrimSpace(parts[0])
		}
	}

	return "application/octet-stream"
}

func detectURLMediaType(sourceURL string) string {
	ext := mediaExt(sourceURL)
	if ext == "" {
		return "application/octet-stream"
	}
	guessed := mime.TypeByExtension(ext)
	if guessed == "" {
		return "application/octet-stream"
	}
	parts := strings.Split(guessed, ";")
	if len(parts) == 0 {
		return "application/octet-stream"
	}
	return strings.TrimSpace(parts[0])
}

func mediaRouteFor(source string) string {
	ext := mediaExt(source)
	if ext == "" {
		ext = ".bin"
	}
	return "/media-" + randomToken(8) + ext
}

func mediaExt(source string) string {
	if parsed, err := url.Parse(source); err == nil && parsed.Path != "" {
		ext := strings.ToLower(path.Ext(parsed.Path))
		if isSafeExt(ext) {
			return ext
		}
	}

	ext := strings.ToLower(filepath.Ext(source))
	if isSafeExt(ext) {
		return ext
	}
	return ""
}

func isSafeExt(ext string) bool {
	if ext == "" || len(ext) > 16 || !strings.HasPrefix(ext, ".") {
		return false
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func asCloser(v any) io.Closer {
	if c, ok := v.(io.Closer); ok {
		return c
	}
	return nil
}

func validatedSubtitlePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (m *Manager) validateDLNASubtitles(path string) (string, error) {
	return m.validateLocalFilePath(path, "subtitles_path")
}

func (m *Manager) validateLocalFilePath(pathValue, fieldName string) (string, error) {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return "", nil
	}
	if !filepath.IsAbs(pathValue) {
		return "", toolError("FILE_NOT_READABLE", fmt.Sprintf("%s must be an absolute local file path", fieldName))
	}

	info, err := os.Stat(pathValue)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", toolError("FILE_NOT_FOUND", fmt.Sprintf("file not found: %s", pathValue))
		}
		return "", toolError("FILE_NOT_READABLE", fmt.Sprintf("unable to read file: %v", err))
	}
	if info.IsDir() {
		return "", toolError("FILE_NOT_READABLE", fmt.Sprintf("%s must be a file, not a directory", fieldName))
	}

	cleaned := filepath.Clean(pathValue)
	if !m.strictPathPolicy {
		return cleaned, nil
	}

	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", pathPolicyBlockedError(fieldName)
	}
	if !filepath.IsAbs(resolved) {
		return "", pathPolicyBlockedError(fieldName)
	}
	if !m.pathAllowed(resolved) {
		m.safeLogf("path policy blocked field=%s path=%s", fieldName, m.logPath(resolved))
		return "", pathPolicyBlockedError(fieldName)
	}
	return filepath.Clean(resolved), nil
}

func (m *Manager) pathAllowed(pathValue string) bool {
	if !m.strictPathPolicy {
		return true
	}
	if len(m.allowedPathPrefixes) == 0 {
		return false
	}

	cleanPath := filepath.Clean(pathValue)
	for _, prefix := range m.allowedPathPrefixes {
		if prefix == "" {
			continue
		}
		rel, err := filepath.Rel(prefix, cleanPath)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return true
		}
	}
	return false
}

func (m *Manager) validateSourceURLPolicy(sourceURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil {
		return nil, unsupportedURLPatternError("source URL is invalid", "URL_PARSE_INVALID")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, unsupportedURLPatternError("source URL must use http or https", "URL_SCHEME_UNSUPPORTED")
	}

	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return nil, unsupportedURLPatternError("source URL must include a host", "URL_HOST_MISSING")
	}
	if !m.allowLoopbackURLs && isLoopbackHost(host) {
		return nil, loopbackURLBlockedError(host)
	}
	return u, nil
}

func (m *Manager) validateBindAddress(listenAddr string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return toolError("PROTOCOL_ERROR", fmt.Sprintf("invalid media bind address: %q", listenAddr))
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if m.allowWildcardBind {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return bindPolicyBlockedError(listenAddr)
	}
	return nil
}

func (m *Manager) logPath(pathValue string) string {
	if !m.redactPaths {
		return pathValue
	}
	if filepath.IsAbs(pathValue) {
		return "<redacted-path>"
	}
	return pathValue
}

func normalizeTranscodeMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return transcodeAuto
	}
	switch mode {
	case transcodeAuto, transcodeAlways, transcodeNever:
		return mode
	default:
		return ""
	}
}

func normalizeDLNAState(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	switch s {
	case "playing":
		return "playing"
	case "paused", "paused_playback":
		return "paused"
	case "stopped", "no_media_present":
		return "stopped"
	case "buffering", "transitioning":
		return "buffering"
	default:
		return s
	}
}

func normalizeDLNATransport(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return normalizeDLNAState(v[0])
}

type dlnaMonitorScreen struct {
	stateCh chan<- string
}

func (d *dlnaMonitorScreen) EmitMsg(msg string) {
	if d == nil || d.stateCh == nil {
		return
	}

	select {
	case d.stateCh <- msg:
	default:
	}
}

func (d *dlnaMonitorScreen) Fini() {}

func (d *dlnaMonitorScreen) SetMediaType(string) {}

func toolError(code, message string) *domain.ToolError {
	return &domain.ToolError{Code: code, Message: message}
}

func unsupportedProtocolError(protocol string) *domain.ToolError {
	p := strings.TrimSpace(protocol)
	if p == "" {
		p = "unknown"
	}
	return &domain.ToolError{
		Code:    "UNSUPPORTED_SOURCE_FOR_PROTOCOL",
		Message: fmt.Sprintf("target device protocol %q is not supported", p),
		Limitations: []domain.Limitation{
			{
				Code:    "PROTOCOL_UNSUPPORTED",
				Message: "Only Chromecast and DLNA/UPnP device protocols are supported in v1.",
			},
		},
		SuggestedFixes: []string{
			"Run list_local_hardware and choose a Chromecast or DLNA/UPnP target.",
		},
		Details: map[string]any{
			"protocol": p,
		},
	}
}

func unsupportedURLPatternError(message, limitationCode string) *domain.ToolError {
	return &domain.ToolError{
		Code:    "UNSUPPORTED_URL_PATTERN",
		Message: message,
		Limitations: []domain.Limitation{
			{
				Code:    limitationCode,
				Message: message,
			},
		},
		SuggestedFixes: []string{
			"Use an absolute local file path, or an http/https URL with a routable host.",
		},
	}
}

func loopbackURLBlockedError(host string) *domain.ToolError {
	return &domain.ToolError{
		Code:    "UNSUPPORTED_URL_PATTERN",
		Message: "localhost and loopback URL hosts are blocked by default",
		Limitations: []domain.Limitation{
			{
				Code:    "URL_LOOPBACK_BLOCKED",
				Message: "URL host resolves to localhost/loopback and is blocked by default policy.",
			},
		},
		SuggestedFixes: []string{
			"Use a URL hosted on another machine reachable by the target device.",
			"Use a local file source so mcp-beam serves media on LAN.",
			"Set MCP_BEAM_ALLOW_LOOPBACK_URLS=true only for trusted local testing.",
		},
		Details: map[string]any{
			"host": host,
		},
	}
}

func dlnaHLSUnsupportedError() *domain.ToolError {
	return &domain.ToolError{
		Code:    "UNSUPPORTED_SOURCE_FOR_PROTOCOL",
		Message: "HLS .m3u8 URLs are Chromecast-only by default in v1",
		Limitations: []domain.Limitation{{
			Code:    "HLS_M3U8_URL_UNSUPPORTED",
			Message: "DLNA rendering path does not support .m3u8 URLs in v1.",
		}},
		SuggestedFixes: []string{
			"Use a Chromecast target for .m3u8 URLs.",
			"Use a non-HLS URL or local file for DLNA targets.",
		},
		Details: map[string]any{
			"protocol": "dlna",
			"source":   "hls_m3u8_url",
		},
	}
}

func bindPolicyBlockedError(listenAddr string) *domain.ToolError {
	return &domain.ToolError{
		Code:    "PROTOCOL_ERROR",
		Message: "bind policy rejected wildcard media listener address",
		Limitations: []domain.Limitation{{
			Code:    "BIND_WILDCARD_BLOCKED",
			Message: "Binding media server to wildcard interfaces is blocked by default.",
		}},
		SuggestedFixes: []string{
			"Use a concrete LAN IP bind address selected for the target route.",
			"Set MCP_BEAM_ALLOW_WILDCARD_BIND=true only when wildcard binding is explicitly desired.",
		},
		Details: map[string]any{
			"listen_address": listenAddr,
		},
	}
}

func pathPolicyBlockedError(fieldName string) *domain.ToolError {
	return &domain.ToolError{
		Code:    "FILE_NOT_READABLE",
		Message: fmt.Sprintf("%s is blocked by strict path policy", fieldName),
		Limitations: []domain.Limitation{{
			Code:    "PATH_POLICY_BLOCKED",
			Message: "Strict path policy allows only configured local path prefixes.",
		}},
		SuggestedFixes: []string{
			"Move media/subtitles under an allowed local directory.",
			"Set MCP_BEAM_ALLOWED_PATH_PREFIXES to include required path prefixes.",
			"Disable strict mode with MCP_BEAM_STRICT_PATH_POLICY=false if appropriate.",
		},
		Details: map[string]any{
			"field": fieldName,
		},
	}
}

func ffmpegNotFoundError() *domain.ToolError {
	return &domain.ToolError{
		Code:    "FFMPEG_NOT_FOUND",
		Message: "ffmpeg is required for transcoding but was not found in PATH",
		Limitations: []domain.Limitation{{
			Code:    "FFMPEG_BINARY_MISSING",
			Message: "Transcoding requires the ffmpeg binary to be available in PATH.",
		}},
		SuggestedFixes: []string{
			"Linux: install ffmpeg with your package manager (for example: sudo apt install ffmpeg).",
			"macOS: install ffmpeg with Homebrew (brew install ffmpeg).",
			"Windows: install ffmpeg and add ffmpeg.exe to PATH, then verify with `where ffmpeg`.",
		},
		Details: map[string]any{
			"binary": "ffmpeg",
		},
	}
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func boolEnv(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseAllowedPathPrefixes(raw string) []string {
	items := strings.Split(raw, ",")
	out := make([]string, 0, len(items))
	for _, item := range items {
		p := strings.TrimSpace(item)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			continue
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

func newSessionID() string {
	return "sess_" + randomToken(8)
}

func randomToken(bytesLen int) string {
	if bytesLen <= 0 {
		bytesLen = 8
	}
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf)
}
