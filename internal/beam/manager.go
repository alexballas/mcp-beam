package beam

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
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

	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/httphandlers"
	"go2tv.app/go2tv/v2/mediasource"
	"go2tv.app/go2tv/v2/metadata"
	"go2tv.app/go2tv/v2/soapcalls"
	"go2tv.app/go2tv/v2/torrentstream"
	"go2tv.app/go2tv/v2/utils"
	"go2tv.app/mcp-beam/internal/adapters"
	"go2tv.app/mcp-beam/internal/domain"
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

	defaultBeamOperationTimeout        = 12 * time.Second
	defaultChromecastLoadDeadlineGrace = 4 * time.Second
	defaultChromecastStatusPollEvery   = 250 * time.Millisecond

	seekModeAbsoluteSeconds = "absolute_seconds"
	seekModePercent         = "percent"
	seekModeFromEndSeconds  = "from_end_seconds"
	seekModeDeltaSeconds    = "delta_seconds"
)

type deviceLister interface {
	ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error)
}

type streamServer interface {
	AddHandler(path string, payload *soapcalls.TVPayload, transcode *utils.TranscodeOptions, media any)
	SetDLNATranscodeSeek(payload *soapcalls.TVPayload, seconds int, subsPath string, embedded *utils.EmbeddedSubtitle, subtitleRoute string, subtitles any)
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
	openTorrent            func(context.Context, string) (torrentSession, error)
	torrentContext         context.Context
	torrentCancel          context.CancelFunc

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

	retryAttempts    int
	retryBaseBackoff time.Duration
	retryMaxBackoff  time.Duration

	beamOperationTimeout        time.Duration
	chromecastLoadDeadlineGrace time.Duration
	chromecastStatusPollEvery   time.Duration

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
	Title       string
	ContentType string
	Transcoding bool
	Warnings    []string
	Protocol    string

	mediaDuration float64

	castClient   adapters.CastClient
	dlnaPayload  adapters.DLNAPayload
	httpServer   streamServer
	sourceCloser io.Closer
	castSeekPlan *chromecastTranscodeSeek
	dlnaSeekPlan *dlnaSubtitleSeek
	torrent      *torrentPlayback
	loadCancel   context.CancelFunc
	seekOffset   int
	timelineMu   sync.RWMutex
	playbackMu   sync.Mutex
	closed       bool
	seekCancelMu sync.Mutex
	seekCancel   context.CancelFunc
	seekClosing  bool

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

type chromecastTranscodeSeek struct {
	urlSource            bool
	sourcePath           string
	ffmpegPath           string
	subsPath             string
	route                string
	subtitleMode         string
	embeddedTrack        *int
	torrentSource        mediasource.Source
	torrentSubtitleRoute string
}

type preparedPlayback struct {
	mediaURL           string
	mediaType          string
	title              string
	subtitleURL        string
	torrentSubtitleURL string
	live               bool
	transcoding        bool
	mediaDuration      float64
	warnings           []string
	httpServer         streamServer
	sourceCloser       io.Closer
	castSeekPlan       *chromecastTranscodeSeek
}

type preparedDLNA struct {
	mediaURL      string
	title         string
	contentType   string
	transcoding   bool
	warnings      []string
	httpServer    streamServer
	sourceCloser  io.Closer
	payload       adapters.DLNAPayload
	callbackCh    <-chan string
	mediaDuration float64
	dlnaSeekPlan  *dlnaSubtitleSeek
}

func NewManager(discovery deviceLister, castFactory adapters.CastFactory, dlnaFactory adapters.DLNAFactory) *Manager {
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	torrentCtx, torrentCancel := context.WithCancel(context.Background())
	allowedPathPrefixes := parseAllowedPathPrefixes(os.Getenv("MCP_BEAM_ALLOWED_PATH_PREFIXES"))
	manager := &Manager{
		discovery:              discovery,
		castFactory:            castFactory,
		dlnaFactory:            dlnaFactory,
		serverFactory:          go2TVStreamServerFactory{},
		lookPath:               exec.LookPath,
		listenAddressForDevice: utils.URLtoListenIPandPort,
		prepareURLMedia:        utils.PrepareURLMedia,
		openTorrent: func(ctx context.Context, source string) (torrentSession, error) {
			return torrentstream.Open(ctx, source)
		},
		torrentContext:              torrentCtx,
		torrentCancel:               torrentCancel,
		dlnaPollEvery:               dlnaPollInterval,
		idleCleanupAfter:            defaultIdleCleanupAfter,
		pausedCleanupAfter:          defaultPausedCleanupAfter,
		maxSessionAge:               defaultMaxSessionAge,
		cleanupSweepEvery:           defaultCleanupSweepEvery,
		now:                         time.Now,
		strictPathPolicy:            boolEnv("MCP_BEAM_STRICT_PATH_POLICY", false),
		allowedPathPrefixes:         allowedPathPrefixes,
		allowLoopbackURLs:           boolEnv("MCP_BEAM_ALLOW_LOOPBACK_URLS", false),
		allowWildcardBind:           boolEnv("MCP_BEAM_ALLOW_WILDCARD_BIND", false),
		retryAttempts:               defaultRetryAttempts,
		retryBaseBackoff:            defaultRetryBaseBackoff,
		retryMaxBackoff:             defaultRetryMaxBackoff,
		beamOperationTimeout:        defaultBeamOperationTimeout,
		chromecastLoadDeadlineGrace: defaultChromecastLoadDeadlineGrace,
		chromecastStatusPollEvery:   defaultChromecastStatusPollEvery,
		cleanupLoopCancel:           cleanupCancel,
		cleanupLoopDone:             make(chan struct{}),
		sessionsByID:                map[string]*session{},
		sessionByDeviceID:           map[string]string{},
	}
	go manager.runCleanupLoop(cleanupCtx)
	return manager
}

func (m *Manager) BeamMedia(ctx context.Context, req domain.BeamRequest) (*domain.BeamResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.discovery == nil {
		return nil, toolError("INTERNAL_ERROR", "beam manager is not configured")
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	if req.StartSeconds != nil && *req.StartSeconds < 0 {
		return nil, toolError("INTERNAL_ERROR", "start_seconds must be >= 0")
	}

	mode := normalizeTranscodeMode(req.Transcode)
	if mode == "" {
		return nil, toolError("INTERNAL_ERROR", "invalid transcode mode")
	}

	req.SubtitleMode = strings.ToLower(strings.TrimSpace(req.SubtitleMode))
	if req.SubtitleMode == "" {
		req.SubtitleMode = "auto"
	}
	switch req.SubtitleMode {
	case "auto", "receiver", "burn", "off":
	default:
		return nil, toolError("INVALID_ARGUMENT", "invalid subtitle_mode")
	}
	if req.SubtitleMode == "burn" {
		if mode == transcodeNever {
			return nil, toolError("TRANSCODE_REQUIRED", "subtitle_mode=burn requires transcoding")
		}
		mode = transcodeAlways
	}
	if req.EmbeddedSubtitleTrack != nil && *req.EmbeddedSubtitleTrack < 0 {
		return nil, toolError("INVALID_ARGUMENT", "embedded_subtitle_track must be non-negative")
	}
	if req.TorrentFileIndex != nil && !isTorrentInput(req.Source) {
		return nil, toolError("INVALID_ARGUMENT", "torrent_file_index requires a magnet URI or .torrent file")
	}
	if req.EmbeddedSubtitleTrack != nil && strings.TrimSpace(req.SubtitlesPath) == "" && req.SubtitleMode != "off" && !isTorrentInput(req.Source) {
		if parsed, err := url.Parse(req.Source); err == nil && parsed.Scheme != "" {
			return nil, toolError("INVALID_ARGUMENT", "embedded_subtitle_track requires a local media file")
		}
	}
	deviceCtx, cancelDevice := m.operationContext(ctx)
	device, err := m.resolveDevice(deviceCtx, req.TargetDevice)
	cancelDevice()
	if err != nil {
		return nil, err
	}
	var torrent *torrentPlayback
	if isTorrentInput(req.Source) {
		torrent, err = m.prepareTorrent(ctx, req)
		if err != nil {
			return nil, err
		}
		req.Source = torrent.path
	}
	transferred := false
	defer func() {
		if torrent != nil && !transferred {
			_ = torrent.Close()
		}
	}()

	var result *domain.BeamResult
	switch device.Protocol {
	case "chromecast":
		result, err = m.beamChromecast(ctx, req, device, mode, torrent)
	case "dlna":
		result, err = m.beamDLNA(ctx, req, device, mode, torrent)
	default:
		return nil, unsupportedProtocolError(device.Protocol)
	}
	transferred = err == nil
	return result, err
}

func (m *Manager) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.beamOperationTimeout > 0 {
		return context.WithTimeout(ctx, m.beamOperationTimeout)
	}
	return context.WithCancel(ctx)
}

func (m *Manager) preparationContext(ctx context.Context, torrent *torrentPlayback) (context.Context, context.CancelFunc) {
	if torrent != nil {
		return context.WithTimeout(ctx, time.Minute)
	}
	return m.operationContext(ctx)
}

func (m *Manager) StopBeaming(_ context.Context, req domain.StopRequest) (*domain.StopResult, error) {
	if req.SessionID == "" && req.TargetDevice == "" {
		return nil, toolError("INTERNAL_ERROR", "either session_id or target_device is required")
	}

	sess := m.takeSession(req)
	if sess == nil {
		return nil, toolError("DEVICE_NOT_FOUND", "no active session matches the provided target")
	}

	shutdown := shutdownSessionWithReport(sess, true)
	if err := shutdown.stopErr(); err != nil {
		return nil, toolError("PROTOCOL_ERROR", err.Error())
	}

	return &domain.StopResult{
		OK:               true,
		StoppedSessionID: sess.ID,
		DeviceID:         sess.DeviceID,
		Warnings:         shutdown.warnings(),
	}, nil
}

func (m *Manager) PlayBeaming(ctx context.Context, req domain.PlaybackControlRequest) (*domain.PlaybackControlResult, error) {
	return m.controlPlayback(ctx, req, "play")
}

func (m *Manager) PauseBeaming(ctx context.Context, req domain.PlaybackControlRequest) (*domain.PlaybackControlResult, error) {
	return m.controlPlayback(ctx, req, "pause")
}

func (m *Manager) controlPlayback(ctx context.Context, req domain.PlaybackControlRequest, action string) (*domain.PlaybackControlResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	if req.SessionID == "" && req.TargetDevice == "" {
		return nil, toolError("INTERNAL_ERROR", "either session_id or target_device is required")
	}

	sess := m.findSessionByTarget(req.SessionID, req.TargetDevice)
	if sess == nil {
		return nil, toolError("DEVICE_NOT_FOUND", "no active session matches the provided target")
	}

	state := ""
	switch action {
	case "play":
		if err := m.playSession(ctx, sess); err != nil {
			return nil, err
		}
		state = "playing"
	case "pause":
		if err := m.pauseSession(ctx, sess); err != nil {
			return nil, err
		}
		state = "paused"
	default:
		return nil, toolError("INTERNAL_ERROR", "invalid playback control action")
	}

	observedAt := m.now()
	sess.stateMu.Lock()
	sess.recordObservationLocked(state, "", observedAt)
	if action == "play" {
		sess.lastProgressAt = observedAt
	}
	sess.stateMu.Unlock()

	return &domain.PlaybackControlResult{
		OK:        true,
		SessionID: sess.ID,
		DeviceID:  sess.DeviceID,
		State:     state,
	}, nil
}

func (m *Manager) playSession(ctx context.Context, sess *session) error {
	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return toolError("INTERNAL_ERROR", "chromecast session is not configured")
		}
		if err := m.withRetry(ctx, func() error {
			return sess.castClient.Play()
		}); err != nil {
			return toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to resume Chromecast playback: %v", err))
		}
	case "dlna":
		if sess.dlnaPayload == nil {
			return toolError("INTERNAL_ERROR", "dlna session is not configured")
		}
		if err := m.withRetry(ctx, func() error {
			return sess.dlnaPayload.SendtoTV("Play")
		}); err != nil {
			return toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to resume DLNA playback: %v", err))
		}
	default:
		return unsupportedProtocolError(sess.Protocol)
	}
	return nil
}

func (m *Manager) pauseSession(ctx context.Context, sess *session) error {
	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return toolError("INTERNAL_ERROR", "chromecast session is not configured")
		}
		if err := m.withRetry(ctx, func() error {
			return sess.castClient.Pause()
		}); err != nil {
			return toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to pause Chromecast playback: %v", err))
		}
	case "dlna":
		if sess.dlnaPayload == nil {
			return toolError("INTERNAL_ERROR", "dlna session is not configured")
		}
		if err := m.withRetry(ctx, func() error {
			return sess.dlnaPayload.SendtoTV("Pause")
		}); err != nil {
			return toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to pause DLNA playback: %v", err))
		}
	default:
		return unsupportedProtocolError(sess.Protocol)
	}
	return nil
}

func (m *Manager) SeekBeaming(ctx context.Context, req domain.SeekRequest) (*domain.SeekResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	if req.SessionID == "" && req.TargetDevice == "" {
		return nil, toolError("INTERNAL_ERROR", "either session_id or target_device is required")
	}

	modeCount := 0
	if req.PositionSeconds != nil {
		if *req.PositionSeconds < 0 {
			return nil, seekPositionInvalidError("position_seconds must be >= 0")
		}
		modeCount++
	}
	if req.PositionPercent != nil {
		if *req.PositionPercent < 0 || *req.PositionPercent > 100 {
			return nil, seekPositionInvalidError("position_percent must be in range [0, 100]")
		}
		modeCount++
	}
	if req.FromEndSeconds != nil {
		if *req.FromEndSeconds < 0 {
			return nil, seekPositionInvalidError("from_end_seconds must be >= 0")
		}
		modeCount++
	}
	if req.DeltaSeconds != nil {
		modeCount++
	}
	if modeCount != 1 {
		return nil, seekModeInvalidError("exactly one seek mode is required")
	}

	sess := m.findSession(req)
	if sess == nil {
		return nil, toolError("DEVICE_NOT_FOUND", "no active session matches the provided target")
	}

	resolvedSeconds, durationSeconds, mode, err := m.resolveSeekPosition(ctx, sess, req)
	if err != nil {
		return nil, err
	}

	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return nil, toolError("INTERNAL_ERROR", "chromecast session is not configured")
		}

		if sess.Transcoding {
			if err := m.seekChromecastTranscoded(ctx, sess, resolvedSeconds); err != nil {
				return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to seek Chromecast transcoded playback: %v", err))
			}
		} else {
			if err := m.withRetry(ctx, func() error {
				return sess.castClient.Seek(resolvedSeconds)
			}); err != nil {
				return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to seek Chromecast playback: %v", err))
			}
		}

		observedAt := m.now()
		observedPosition := resolvedSeconds
		if sess.Transcoding {
			observedPosition = 0
		}
		sess.stateMu.Lock()
		sess.recordObservationLocked("playing", strconv.Itoa(observedPosition), observedAt)
		sess.stateMu.Unlock()
	case "dlna":
		if sess.dlnaPayload == nil {
			return nil, toolError("INTERNAL_ERROR", "dlna session is not configured")
		}

		reltime := utils.SecondsToClockTime(resolvedSeconds)

		if sess.Transcoding {
			if err := m.seekDLNATranscoded(ctx, sess, resolvedSeconds); err != nil {
				return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to seek DLNA transcoded playback: %v", err))
			}
		} else {
			if err := m.withRetry(ctx, func() error {
				return sess.dlnaPayload.SeekSoapCall(reltime)
			}); err != nil {
				return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to seek DLNA playback: %v", err))
			}
		}

		observedAt := m.now()
		if sess.Transcoding {
			reltime = utils.SecondsToClockTime(0)
		}
		sess.stateMu.Lock()
		sess.lastDLNAPosition = reltime
		sess.recordObservationLocked("playing", reltime, observedAt)
		sess.stateMu.Unlock()
	default:
		return nil, unsupportedProtocolError(sess.Protocol)
	}

	var durationPtr *float64
	if durationSeconds > 0 {
		duration := durationSeconds
		durationPtr = &duration
	}

	return &domain.SeekResult{
		OK:                      true,
		SessionID:               sess.ID,
		DeviceID:                sess.DeviceID,
		PositionSeconds:         resolvedSeconds,
		RequestedMode:           mode,
		ResolvedPositionSeconds: resolvedSeconds,
		DurationSeconds:         durationPtr,
	}, nil
}

func (m *Manager) GetBeamingStatus(ctx context.Context, req domain.StatusRequest) (*domain.StatusResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	if req.SessionID == "" && req.TargetDevice == "" {
		return nil, toolError("INTERNAL_ERROR", "either session_id or target_device is required")
	}

	sess := m.findStatusSession(req)
	if sess == nil {
		return nil, toolError("DEVICE_NOT_FOUND", "no active session matches the provided target")
	}

	result := statusResultFromSession(sess)
	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return nil, toolError("INTERNAL_ERROR", "chromecast session is not configured")
		}

		var status *castprotocol.CastStatus
		if err := m.withRetry(ctx, func() error {
			var err error
			status, err = sess.castClient.GetStatus()
			return err
		}); err != nil {
			return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to query Chromecast playback status: %v", err))
		}
		if status == nil {
			return nil, toolError("PROTOCOL_ERROR", "failed to query Chromecast playback status: empty status")
		}

		applyCastStatus(result, status, sess.mediaDuration)
		positionText := ""
		if result.PositionSeconds != nil {
			positionText = strconv.Itoa(int(math.Round(*result.PositionSeconds)))
		}
		sess.stateMu.Lock()
		sess.recordObservationLocked(result.State, positionText, m.now())
		sess.stateMu.Unlock()
	case "dlna":
		if sess.dlnaPayload == nil {
			return nil, toolError("INTERNAL_ERROR", "dlna session is not configured")
		}

		var transport []string
		if err := m.withRetry(ctx, func() error {
			var err error
			transport, err = sess.dlnaPayload.GetTransportInfo()
			return err
		}); err != nil {
			return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to query DLNA playback transport: %v", err))
		}
		if state := normalizeDLNATransport(transport); state != "" {
			result.State = state
		}

		var positionInfo []string
		positionErr := m.withRetry(ctx, func() error {
			var err error
			positionInfo, err = sess.dlnaPayload.GetPositionInfo()
			return err
		})
		if positionErr == nil {
			applyDLNAPositionInfo(result, positionInfo)
		}

		positionText := ""
		if len(positionInfo) >= 2 {
			positionText = strings.TrimSpace(positionInfo[1])
		}
		sess.recordDLNAState(result.State, positionText, false, m.now())
	default:
		return nil, unsupportedProtocolError(sess.Protocol)
	}

	if result.PositionSeconds != nil {
		position, duration := sess.sourceTimeline(*result.PositionSeconds, valueOrZero(result.DurationSeconds))
		result.PositionSeconds = float64Ptr(position)
		if duration > 0 {
			result.DurationSeconds = float64Ptr(duration)
		}
	} else if sess.Transcoding && sess.mediaDuration > 0 {
		result.DurationSeconds = float64Ptr(sess.mediaDuration)
	}
	return result, nil
}

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func (s *session) sourceTimeline(position, duration float64) (float64, float64) {
	if !s.Transcoding {
		return position, duration
	}
	s.timelineMu.RLock()
	offset := s.seekOffset
	s.timelineMu.RUnlock()
	position += float64(offset)
	if s.mediaDuration > 0 {
		duration = s.mediaDuration
	} else if duration > 0 {
		duration += float64(offset)
	}
	return position, duration
}

func (s *session) streamURL() string {
	s.timelineMu.RLock()
	defer s.timelineMu.RUnlock()
	return s.MediaURL
}

func (s *session) setTimeline(mediaURL string, offset int) {
	s.timelineMu.Lock()
	defer s.timelineMu.Unlock()
	s.MediaURL = mediaURL
	s.seekOffset = offset
}

func (m *Manager) seekDLNATranscoded(ctx context.Context, sess *session, resolvedSeconds int) error {
	if sess == nil || sess.dlnaPayload == nil {
		return errors.New("dlna session is not configured")
	}

	sess.playbackMu.Lock()
	defer sess.playbackMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancelSeek := context.WithCancel(ctx)
	sess.seekCancelMu.Lock()
	if sess.seekClosing {
		sess.seekCancelMu.Unlock()
		cancelSeek()
		return errors.New("session is closing")
	}
	sess.seekCancel = cancelSeek
	sess.seekCancelMu.Unlock()
	defer func() {
		sess.seekCancelMu.Lock()
		sess.seekCancel = nil
		sess.seekCancelMu.Unlock()
		cancelSeek()
	}()
	if sess.closed {
		return errors.New("session is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw := sess.dlnaPayload.RawPayload()
	if raw == nil {
		return errors.New("dlna payload details are unavailable")
	}
	if err := m.refreshDLNASeekSubtitles(ctx, sess, resolvedSeconds); err != nil {
		return err
	}

	// Best effort: stop first to force a clean transport restart on strict renderers.
	_ = m.withRetryAwait(ctx, func() error {
		return sess.dlnaPayload.SendtoTVContext(ctx, "Stop")
	})

	if err := m.withRetryAwait(ctx, func() error {
		return sess.dlnaPayload.SendtoTVContext(ctx, "Play1")
	}); err != nil {
		return err
	}

	// FFmpeg has already skipped to the requested source position. The new
	// stream starts at zero; another renderer seek would skip the offset twice.
	sess.setTimeline(sess.streamURL(), resolvedSeconds)
	return nil
}

func (m *Manager) beamChromecast(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string, torrent *torrentPlayback) (*domain.BeamResult, error) {
	if m.castFactory == nil {
		return nil, toolError("INTERNAL_ERROR", "Chromecast adapter is not configured")
	}

	client, err := m.castFactory.NewCastClient(device.Address)
	if err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to create Chromecast client: %v", err))
	}
	prepareCtx, cancelPrepare := m.preparationContext(ctx, torrent)
	playback, err := m.preparePlayback(prepareCtx, req, device, mode, torrent)
	cancelPrepare()
	if err != nil {
		_ = client.ForceClose()
		return nil, err
	}
	startupCtx, cancelStartup := m.operationContext(ctx)
	defer cancelStartup()
	if err := m.withRetry(startupCtx, func() error {
		return client.ConnectContext(startupCtx)
	}); err != nil {
		cleanupPrepared(playback)
		_ = client.ForceClose()
		return nil, toolError("DEVICE_UNREACHABLE", fmt.Sprintf("failed to connect to Chromecast device: %v", err))
	}

	loadResultCh := make(chan error, 1)
	loadCtx, handoff, cancelLoad := m.chromecastLoadContext(startupCtx)
	defer handoff()
	startTime := 0
	if req.StartSeconds != nil && !playback.transcoding {
		startTime = *req.StartSeconds
	}
	go func() {
		loadResultCh <- client.LoadMediaContext(loadCtx, castprotocol.LoadRequest{
			MediaURL: playback.mediaURL, ContentType: playback.mediaType,
			Metadata: metadata.Media{Title: playback.title}, StartTime: startTime,
			Duration:    remainingDuration(playback.mediaDuration, startSeconds(req.StartSeconds), playback.transcoding),
			SubtitleURL: playback.subtitleURL, TorrentSubtitleURL: playback.torrentSubtitleURL, Live: playback.live,
		})
	}()

	if err := m.waitForChromecastPlaybackStart(startupCtx, client, loadResultCh); err != nil {
		cancelLoad()
		cleanupPrepared(playback)
		_ = client.ForceClose()
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start Chromecast playback: %v", err))
	}
	handoff()

	sess := &session{
		ID:            newSessionID(),
		DeviceID:      device.ID,
		DeviceName:    device.Name,
		MediaURL:      playback.mediaURL,
		Title:         playback.title,
		ContentType:   playback.mediaType,
		Transcoding:   playback.transcoding,
		Warnings:      playback.warnings,
		Protocol:      "chromecast",
		mediaDuration: playback.mediaDuration,
		castClient:    client,
		httpServer:    playback.httpServer,
		sourceCloser:  combineClosers(playback.sourceCloser, torrent),
		castSeekPlan:  playback.castSeekPlan,
		torrent:       torrent,
		loadCancel:    cancelLoad,
	}
	if playback.transcoding {
		sess.seekOffset = startSeconds(req.StartSeconds)
	}
	if err := ctx.Err(); err != nil {
		_ = shutdownSession(sess, true)
		return nil, err
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
		Torrent:     torrent.status(),
	}, nil
}

func remainingDuration(duration float64, offset int, transcoding bool) float64 {
	if !transcoding || duration <= 0 {
		return duration
	}
	return math.Max(0, duration-float64(offset))
}

func (m *Manager) waitForChromecastPlaybackStart(ctx context.Context, client adapters.CastClient, loadResultCh <-chan error) error {
	if loadResultCh == nil {
		return errors.New("load result channel is nil")
	}

	pollEvery := m.chromecastStatusPollEvery
	if pollEvery <= 0 {
		pollEvery = defaultChromecastStatusPollEvery
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	var graceTimer *time.Timer
	var graceC <-chan time.Time
	ctxDone := ctx.Done()
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()

	for {
		select {
		case err := <-loadResultCh:
			return err
		case <-ticker.C:
			status, err := client.GetStatus()
			if err != nil || status == nil {
				continue
			}
			if normalizeCastState(status.PlayerState) == "playing" {
				return nil
			}
		case <-ctxDone:
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) || m.chromecastLoadDeadlineGrace <= 0 {
				return ctx.Err()
			}
			graceTimer = time.NewTimer(m.chromecastLoadDeadlineGrace)
			graceC = graceTimer.C
			ctxDone = nil
		case <-graceC:
			return context.DeadlineExceeded
		}
	}
}

func (m *Manager) beamDLNA(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string, torrent *torrentPlayback) (*domain.BeamResult, error) {
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
	prepareCtx, cancelPrepare := m.preparationContext(ctx, torrent)
	defer cancelPrepare()
	if isURL {
		prepared, err = m.prepareDLNAURLPlayback(prepareCtx, req, device, mode)
	} else {
		prepared, err = m.prepareDLNAFilePlayback(prepareCtx, req, device, mode, torrent)
	}
	if err != nil {
		return nil, err
	}

	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	prepared.payload.SetContext(monitorCtx)
	sess := &session{
		ID:            newSessionID(),
		DeviceID:      device.ID,
		DeviceName:    device.Name,
		MediaURL:      prepared.mediaURL,
		Title:         prepared.title,
		ContentType:   prepared.contentType,
		Transcoding:   prepared.transcoding,
		Warnings:      append([]string{}, prepared.warnings...),
		Protocol:      "dlna",
		mediaDuration: prepared.mediaDuration,
		dlnaPayload:   prepared.payload,
		httpServer:    prepared.httpServer,
		sourceCloser:  combineClosers(prepared.sourceCloser, torrent),
		torrent:       torrent,
		dlnaSeekPlan:  prepared.dlnaSeekPlan,
		monitorCancel: monitorCancel,
		monitorDone:   make(chan struct{}),
		callbackCh:    prepared.callbackCh,
	}
	if prepared.transcoding {
		sess.seekOffset = startSeconds(req.StartSeconds)
	}
	if err := ctx.Err(); err != nil {
		close(sess.monitorDone)
		_ = shutdownSession(sess, true)
		return nil, err
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
		Torrent:     torrent.status(),
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

func (m *Manager) runDLNAStateMonitor(ctx context.Context, sess *session) {
	if sess == nil {
		return
	}
	if sess.monitorDone != nil {
		defer close(sess.monitorDone)
	}
	if sess.dlnaPayload == nil {
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

func (m *Manager) computeTranscodingForFile(ctx context.Context, mode, mediaType, source string) (bool, []string, string, error) {
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

	codecInfo, err := utils.GetMediaCodecInfoContext(ctx, ffmpegPath, source)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil, "", ctx.Err()
		}
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

func (m *Manager) withRetry(ctx context.Context, call func() error) error {
	return m.withRetryRunner(ctx, call)
}

func (m *Manager) withRetryRunner(ctx context.Context, call func() error) error {
	return m.withRetryUsing(ctx, call, runCallWithContext)
}

// withRetryAwait waits for context-aware operations to finish before returning,
// so cancellation cannot leave control work running after releasing session locks.
func (m *Manager) withRetryAwait(ctx context.Context, call func() error) error {
	return m.withRetryUsing(ctx, call, func(ctx context.Context, call func() error) error {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		err := call()
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	})
}

func (m *Manager) withRetryUsing(ctx context.Context, call func() error, run func(context.Context, func() error) error) error {
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
		err := run(ctx, call)
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if attempt >= attempts || !isTransientNetworkError(err) {
			break
		}

		backoff := backoffForAttempt(baseBackoff, maxBackoff, attempt)
		if waitErr := waitForBackoff(ctx, backoff); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

func runCallWithContext(ctx context.Context, call func() error) error {
	if ctx == nil {
		return call()
	}

	done := make(chan error, 1)
	go func() {
		done <- call()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
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
	if errors.As(err, &netErr) && netErr.Timeout() {
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

func (m *Manager) findSession(req domain.SeekRequest) *session {
	return m.findSessionByTarget(req.SessionID, req.TargetDevice)
}

func (m *Manager) findStatusSession(req domain.StatusRequest) *session {
	return m.findSessionByTarget(req.SessionID, req.TargetDevice)
}

func (m *Manager) findSessionByTarget(sessionID, targetDevice string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sessionID != "" {
		return m.sessionsByID[sessionID]
	}

	target := strings.TrimSpace(targetDevice)
	if target == "" {
		return nil
	}

	for _, sess := range m.sessionsByID {
		if sess == nil {
			continue
		}
		if sess.DeviceID == target || sess.DeviceName == target {
			return sess
		}
		if strings.EqualFold(strings.TrimSpace(sess.DeviceID), target) ||
			strings.EqualFold(strings.TrimSpace(sess.DeviceName), target) {
			return sess
		}
	}

	return nil
}

func statusResultFromSession(sess *session) *domain.StatusResult {
	mediaURL := sess.streamURL()
	result := &domain.StatusResult{
		OK:          true,
		SessionID:   sess.ID,
		DeviceID:    sess.DeviceID,
		DeviceName:  sess.DeviceName,
		Protocol:    sess.Protocol,
		State:       "unknown",
		Title:       strings.TrimSpace(sess.Title),
		ContentType: strings.TrimSpace(sess.ContentType),
		MediaURL:    strings.TrimSpace(mediaURL),
		Transcoding: sess.Transcoding,
		Warnings:    append([]string{}, sess.Warnings...),
		Torrent:     sess.torrent.status(),
	}

	if sess.mediaDuration > 0 {
		result.DurationSeconds = float64Ptr(sess.mediaDuration)
	}

	sess.stateMu.Lock()
	state := strings.TrimSpace(sess.normalizedState)
	position := strings.TrimSpace(sess.lastPosition)
	sess.stateMu.Unlock()

	if state != "" {
		result.State = state
	}
	if parsed := parseSessionPositionSeconds(sess.Protocol, position); parsed != nil {
		result.PositionSeconds = parsed
	}

	return result
}

func applyCastStatus(result *domain.StatusResult, status *castprotocol.CastStatus, fallbackDuration float64) {
	if result == nil || status == nil {
		return
	}

	if state := normalizeCastState(status.PlayerState); state != "" {
		result.State = state
	}
	position := float64(status.CurrentTime)
	if position < 0 {
		position = 0
	}
	result.PositionSeconds = float64Ptr(position)

	duration := float64(status.Duration)
	if duration <= 0 {
		duration = fallbackDuration
	}
	if duration > 0 {
		result.DurationSeconds = float64Ptr(duration)
	}

	if title := strings.TrimSpace(status.MediaTitle); title != "" {
		result.Title = title
	}
	if contentType := strings.TrimSpace(status.ContentType); contentType != "" {
		result.ContentType = contentType
	}
}

func applyDLNAPositionInfo(result *domain.StatusResult, positionInfo []string) {
	if result == nil {
		return
	}
	if len(positionInfo) >= 1 {
		if duration := clockTimeSecondsPtr(positionInfo[0], false); duration != nil {
			result.DurationSeconds = duration
		}
	}
	if len(positionInfo) >= 2 {
		if position := clockTimeSecondsPtr(positionInfo[1], true); position != nil {
			result.PositionSeconds = position
		}
	}
}

func parseSessionPositionSeconds(protocol, position string) *float64 {
	position = strings.TrimSpace(position)
	if position == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(protocol), "dlna") {
		return clockTimeSecondsPtr(position, true)
	}
	seconds, err := strconv.ParseFloat(position, 64)
	if err != nil || seconds < 0 {
		return nil
	}
	return float64Ptr(seconds)
}

func clockTimeSecondsPtr(value string, allowZero bool) *float64 {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "NOT_IMPLEMENTED") {
		return nil
	}
	seconds, err := utils.ClockTimeToSeconds(value)
	if err != nil || seconds < 0 || (!allowZero && seconds == 0) {
		return nil
	}
	return float64Ptr(float64(seconds))
}

func float64Ptr(v float64) *float64 {
	return &v
}

func (m *Manager) resolveSeekPosition(ctx context.Context, sess *session, req domain.SeekRequest) (resolvedSeconds int, durationSeconds float64, mode string, err error) {
	if req.PositionSeconds != nil {
		return *req.PositionSeconds, 0, seekModeAbsoluteSeconds, nil
	}

	if req.DeltaSeconds != nil {
		current, duration, err := m.sessionPositionSeconds(ctx, sess)
		if err != nil {
			return 0, 0, "", err
		}

		resolved := current + *req.DeltaSeconds
		if duration > 0 {
			// A duration large enough to overflow the conversion yields a
			// negative ceiling, so clamp to the floor afterwards rather than
			// before: otherwise the ceiling could drag resolved below zero and
			// send a negative position to the device.
			maxSeconds := int(math.Round(duration))
			if resolved > maxSeconds {
				resolved = maxSeconds
			}
		}
		if resolved < 0 {
			resolved = 0
		}
		return resolved, duration, seekModeDeltaSeconds, nil
	}

	duration, err := m.sessionDurationSeconds(ctx, sess)
	if err != nil {
		return 0, 0, "", err
	}
	if duration <= 0 {
		return 0, 0, "", seekDurationUnknownError()
	}

	if req.PositionPercent != nil {
		resolved := int(math.Round(duration * (*req.PositionPercent) / 100.0))
		if resolved < 0 {
			resolved = 0
		}
		return resolved, duration, seekModePercent, nil
	}

	if req.FromEndSeconds != nil {
		target := duration - float64(*req.FromEndSeconds)
		if target < 0 {
			target = 0
		}
		resolved := int(math.Round(target))
		if resolved < 0 {
			resolved = 0
		}
		return resolved, duration, seekModeFromEndSeconds, nil
	}

	return 0, 0, "", seekModeInvalidError("exactly one seek mode is required")
}

func (m *Manager) sessionDurationSeconds(ctx context.Context, sess *session) (float64, error) {
	if sess == nil {
		return 0, seekDurationUnknownError()
	}
	if sess.Transcoding && sess.mediaDuration > 0 {
		return sess.mediaDuration, nil
	}

	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return 0, toolError("INTERNAL_ERROR", "chromecast session is not configured")
		}

		var statusDuration float64
		if err := m.withRetry(ctx, func() error {
			status, err := sess.castClient.GetStatus()
			if err != nil {
				return err
			}
			if status == nil || status.Duration <= 0 {
				statusDuration = 0
				return nil
			}
			statusDuration = float64(status.Duration)
			return nil
		}); err != nil {
			return 0, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to query Chromecast playback status: %v", err))
		}

		if statusDuration <= 0 {
			if sess.mediaDuration > 0 {
				return sess.mediaDuration, nil
			}
			return 0, nil
		}
		_, duration := sess.sourceTimeline(0, statusDuration)
		return duration, nil
	case "dlna":
		if sess.dlnaPayload == nil {
			return 0, toolError("INTERNAL_ERROR", "dlna session is not configured")
		}

		var position []string
		if err := m.withRetry(ctx, func() error {
			p, err := sess.dlnaPayload.GetPositionInfo()
			if err != nil {
				return err
			}
			position = p
			return nil
		}); err != nil {
			return 0, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to query DLNA playback position: %v", err))
		}
		if len(position) == 0 {
			return 0, nil
		}

		durationSeconds, err := utils.ClockTimeToSeconds(strings.TrimSpace(position[0]))
		if err != nil || durationSeconds <= 0 {
			return 0, nil
		}
		_, duration := sess.sourceTimeline(0, float64(durationSeconds))
		return duration, nil
	default:
		return 0, unsupportedProtocolError(sess.Protocol)
	}
}

func (m *Manager) sessionPositionSeconds(ctx context.Context, sess *session) (int, float64, error) {
	if sess == nil {
		return 0, 0, toolError("DEVICE_NOT_FOUND", "no active session matches the provided target")
	}

	switch sess.Protocol {
	case "chromecast":
		if sess.castClient == nil {
			return 0, 0, toolError("INTERNAL_ERROR", "chromecast session is not configured")
		}

		var status *castprotocol.CastStatus
		if err := m.withRetry(ctx, func() error {
			var err error
			status, err = sess.castClient.GetStatus()
			return err
		}); err != nil {
			return 0, 0, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to query Chromecast playback status: %v", err))
		}
		if status == nil {
			return 0, 0, toolError("PROTOCOL_ERROR", "failed to query Chromecast playback status: empty status")
		}

		position := int(math.Round(float64(status.CurrentTime)))
		if position < 0 {
			position = 0
		}
		duration := float64(status.Duration)
		if duration <= 0 && sess.mediaDuration > 0 {
			duration = sess.mediaDuration
		}
		originalPosition, originalDuration := sess.sourceTimeline(float64(position), duration)
		return int(math.Round(originalPosition)), originalDuration, nil
	case "dlna":
		if sess.dlnaPayload == nil {
			return 0, 0, toolError("INTERNAL_ERROR", "dlna session is not configured")
		}

		var positionInfo []string
		if err := m.withRetry(ctx, func() error {
			var err error
			positionInfo, err = sess.dlnaPayload.GetPositionInfo()
			return err
		}); err != nil {
			return 0, 0, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to query DLNA playback position: %v", err))
		}
		if len(positionInfo) < 2 {
			return 0, 0, toolError("PROTOCOL_ERROR", "failed to query DLNA playback position: missing current position")
		}

		current, err := utils.ClockTimeToSeconds(strings.TrimSpace(positionInfo[1]))
		if err != nil || current < 0 {
			return 0, 0, toolError("PROTOCOL_ERROR", "failed to parse DLNA current playback position")
		}

		duration := 0.0
		if len(positionInfo) > 0 {
			if seconds, err := utils.ClockTimeToSeconds(strings.TrimSpace(positionInfo[0])); err == nil && seconds > 0 {
				duration = float64(seconds)
			}
		}
		originalPosition, originalDuration := sess.sourceTimeline(float64(current), duration)
		return int(math.Round(originalPosition)), originalDuration, nil
	default:
		return 0, 0, unsupportedProtocolError(sess.Protocol)
	}
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
		if m.torrentCancel != nil {
			defer m.torrentCancel()
		}
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
			m.closeErr = errors.Join(m.closeErr, errors.New(strings.Join(errs, "; ")))
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
	return shutdownSessionWithReport(sess, stopMedia).err()
}

type sessionShutdownReport struct {
	stopErrors      []string
	cleanupWarnings []string
}

func (r sessionShutdownReport) err() error {
	errs := make([]string, 0, len(r.stopErrors)+len(r.cleanupWarnings))
	errs = append(errs, r.stopErrors...)
	errs = append(errs, r.cleanupWarnings...)
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

func (r sessionShutdownReport) stopErr() error {
	if len(r.stopErrors) == 0 {
		return nil
	}
	return errors.New(strings.Join(r.stopErrors, "; "))
}

func (r sessionShutdownReport) warnings() []string {
	if len(r.cleanupWarnings) == 0 {
		return nil
	}
	return append([]string{}, r.cleanupWarnings...)
}

func shutdownSessionWithReport(sess *session, stopMedia bool) sessionShutdownReport {
	var report sessionShutdownReport
	if sess == nil {
		return report
	}

	sess.closeOnce.Do(func() {
		sess.seekCancelMu.Lock()
		sess.seekClosing = true
		cancelSeek := sess.seekCancel
		sess.seekCancelMu.Unlock()
		if cancelSeek != nil {
			cancelSeek()
		}
		if sess.loadCancel != nil {
			sess.loadCancel()
		}
		sess.playbackMu.Lock()
		defer sess.playbackMu.Unlock()
		sess.closed = true
		if sess.castClient != nil {
			if stopMedia {
				if err := sess.castClient.Stop(); err != nil {
					report.stopErrors = append(report.stopErrors, fmt.Sprintf("stop: %v", err))
				}
			}
			if err := sess.castClient.Close(true); err != nil {
				report.cleanupWarnings = append(report.cleanupWarnings, fmt.Sprintf("close: %v", err))
			}
		}
		if sess.dlnaPayload != nil && stopMedia {
			if warning, err := stopDLNAPayload(sess.dlnaPayload); err != nil {
				report.stopErrors = append(report.stopErrors, fmt.Sprintf("stop: %v", err))
			} else if warning != "" {
				report.cleanupWarnings = append(report.cleanupWarnings, warning)
			}
		}
		if sess.monitorCancel != nil {
			sess.monitorCancel()
		}
		if sess.monitorDone != nil {
			select {
			case <-sess.monitorDone:
			case <-time.After(dlnaMonitorStopWait):
			}
		}
		if sess.httpServer != nil {
			sess.httpServer.StopServer()
		}
		if sess.sourceCloser != nil {
			if err := sess.sourceCloser.Close(); err != nil {
				report.cleanupWarnings = append(report.cleanupWarnings, fmt.Sprintf("source close: %v", err))
			}
		}
	})
	return report
}

func stopDLNAPayload(payload adapters.DLNAPayload) (string, error) {
	if payload == nil {
		return "", nil
	}

	err := payload.SendtoTV("Stop")
	if err == nil {
		return "", nil
	}
	if !isDLNAUnsubscribeError(err) {
		return "", err
	}
	if stopErr := payload.StopPlayback(); stopErr != nil {
		return "", fmt.Errorf("%v; fallback stop after unsubscribe cleanup failure: %w", err, stopErr)
	}
	return fmt.Sprintf("stop cleanup: %v", err), nil
}

func isDLNAUnsubscribeError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unsubscribe")
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

func mediaTitleFor(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if parsed, err := url.Parse(source); err == nil && parsed.Scheme != "" {
		if base := path.Base(strings.TrimSpace(parsed.Path)); base != "" && base != "." && base != "/" {
			return base
		}
		return strings.TrimSpace(parsed.Host)
	}
	return strings.TrimSpace(filepath.Base(source))
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

func (m *Manager) resolveSubtitlePathForFile(sourcePath, requestedSubtitlesPath string) (string, error) {
	requestedSubtitlesPath = strings.TrimSpace(requestedSubtitlesPath)
	if requestedSubtitlesPath != "" {
		return m.validateLocalFilePath(requestedSubtitlesPath, "subtitles_path")
	}

	base := strings.TrimSuffix(sourcePath, filepath.Ext(sourcePath))
	candidates := []string{base + ".srt", base + ".vtt", base + ".ass", base + ".ssa"}
	for _, candidate := range candidates {
		validatedPath, err := m.validateLocalFilePath(candidate, "subtitles_path")
		if err == nil && validatedPath != "" {
			return validatedPath, nil
		}
		var toolErr *domain.ToolError
		if errors.As(err, &toolErr) && toolErr.Code == "FILE_NOT_FOUND" {
			continue
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

func startSeconds(startAt *int) int {
	if startAt == nil || *startAt <= 0 {
		return 0
	}
	return *startAt
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

func seekModeInvalidError(message string) *domain.ToolError {
	return &domain.ToolError{
		Code:    "SEEK_MODE_INVALID",
		Message: message,
	}
}

func seekPositionInvalidError(message string) *domain.ToolError {
	return &domain.ToolError{
		Code:    "SEEK_POSITION_INVALID",
		Message: message,
	}
}

func seekDurationUnknownError() *domain.ToolError {
	return &domain.ToolError{
		Code:    "SEEK_DURATION_UNKNOWN",
		Message: "duration required for relative seek mode",
		SuggestedFixes: []string{
			"use absolute position_seconds",
			"wait until playback metadata is available",
		},
	}
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
