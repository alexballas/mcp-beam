package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"go2tv.app/mcp-beam/internal/domain"
)

const serverInstructions = "Use tools/list to inspect available tools."

const (
	defaultDiscoveryTimeoutMS = 5000
	minDiscoveryTimeoutMS     = 100
	defaultTorrentTimeoutMS   = 60000
	minTorrentTimeoutMS       = 1000
	maxTorrentTimeoutMS       = 120000
	transcodeAuto             = "auto"
	transcodeAlways           = "always"
	transcodeNever            = "never"
	inflightDrainTimeout      = 2 * time.Second

	// maxSeekSeconds bounds every seconds-valued seek mode. A year is far
	// beyond any real media duration, and staying well inside int32 keeps the
	// float -> int conversions in handleSeekBeamingCall safe even where int is
	// 32 bits.
	maxSeekSeconds = 365 * 24 * 60 * 60
)

type LocalHardwareLister interface {
	ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error)
}

type BeamController interface {
	InspectTorrent(ctx context.Context, req domain.InspectTorrentRequest) (*domain.InspectTorrentResult, error)
	BeamMedia(ctx context.Context, req domain.BeamRequest) (*domain.BeamResult, error)
	StopBeaming(ctx context.Context, req domain.StopRequest) (*domain.StopResult, error)
	PlayBeaming(ctx context.Context, req domain.PlaybackControlRequest) (*domain.PlaybackControlResult, error)
	PauseBeaming(ctx context.Context, req domain.PlaybackControlRequest) (*domain.PlaybackControlResult, error)
	SeekBeaming(ctx context.Context, req domain.SeekRequest) (*domain.SeekResult, error)
	GetBeamingStatus(ctx context.Context, req domain.StatusRequest) (*domain.StatusResult, error)
}

type Server struct {
	in                  *bufio.Reader
	out                 *bufio.Writer
	serverName          string
	serverVersion       string
	logger              *slog.Logger
	useJSONLineOutput   bool
	outputModeLocked    bool
	tools               []tool
	localHardwareLister LocalHardwareLister
	beamController      BeamController
	sendMu              sync.Mutex
	inflightTools       sync.WaitGroup
	inflightMu          sync.Mutex
	inflightCalls       map[string]*inflightToolCall

	// resultMeta is the server identity stamped into each modern result. Name
	// and version are immutable after New, so it is built once.
	resultMeta map[string]any
}

// requestHandler retains one request's era until its response is sent, even
// when other requests arrive while its tool call is running.
type requestHandler struct {
	*Server
	modern bool
	call   *inflightToolCall
}

// inflightToolCall is owned by one tools/call goroutine. Registry operations,
// cancellation and the transition to sending a response share inflightMu.
type inflightToolCall struct {
	ctx        context.Context
	cancel     context.CancelFunc
	cancelled  bool
	responding bool
}

type Config struct {
	ServerName          string
	ServerVersion       string
	Logger              *slog.Logger
	LocalHardwareLister LocalHardwareLister
	BeamController      BeamController
}

func New(in io.Reader, out io.Writer, cfg Config) *Server {
	if cfg.ServerName == "" {
		cfg.ServerName = "mcp-beam"
	}
	if cfg.ServerVersion == "" {
		cfg.ServerVersion = "dev"
	}

	return &Server{
		in:                  bufio.NewReader(in),
		out:                 bufio.NewWriter(out),
		serverName:          cfg.ServerName,
		serverVersion:       cfg.ServerVersion,
		logger:              cfg.Logger,
		tools:               staticTools(),
		localHardwareLister: cfg.LocalHardwareLister,
		beamController:      cfg.BeamController,
		inflightCalls:       make(map[string]*inflightToolCall),
		resultMeta: map[string]any{
			metaServerInfo: map[string]string{
				"name":    cfg.ServerName,
				"version": cfg.ServerVersion,
			},
		},
	}
}

func (s *Server) Run(ctx context.Context) error {
	// EOF first grants the established response-drain grace period below.
	// Every exit then cancels work that has not finished during that grace.
	defer s.cancelInflightCalls()
	for {
		select {
		case <-ctx.Done():
			s.logLifecycle(slog.LevelInfo, "mcp_context_done", slog.String("reason", ctx.Err().Error()))
			return ctx.Err()
		default:
		}

		s.logLifecycle(slog.LevelDebug, "mcp_read_wait")
		payload, jsonLineInput, err := readMessage(s.in)
		if err != nil {
			if err == io.EOF {
				s.waitForInflightTools()
				s.logLifecycle(slog.LevelInfo, "mcp_stream_eof")
				return nil
			}
			s.logLifecycle(slog.LevelError, "mcp_read_error", slog.String("error", err.Error()))
			return err
		}
		if !s.outputModeLocked {
			s.useJSONLineOutput = jsonLineInput
			s.outputModeLocked = true
			s.logLifecycle(
				slog.LevelDebug,
				"mcp_output_mode",
				slog.String("mode", map[bool]string{true: "jsonline", false: "framed"}[jsonLineInput]),
			)
		}
		s.logLifecycle(slog.LevelDebug, "mcp_message_received", slog.Int("bytes", len(payload)))

		if err := s.handle(ctx, payload); err != nil {
			s.logLifecycle(slog.LevelError, "mcp_handle_error", slog.String("error", err.Error()))
			return err
		}
	}
}

func (s *Server) handle(ctx context.Context, payload []byte) error {
	startedAt := time.Now()

	var req request
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logCall("parse", "", "", startedAt, "-32700")
		return s.send(response{
			JSONRPC: "2.0",
			Error: &responseError{
				Code:    -32700,
				Message: "parse error",
			},
		})
	}

	if len(req.ID) == 0 {
		if (req.JSONRPC == "" || req.JSONRPC == "2.0") && req.Method == "notifications/cancelled" {
			s.handleCancellation(req.Params)
		}
		return nil
	}

	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		s.logCall(req.Method, "", "", startedAt, "-32600")
		return s.send(response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &responseError{
				Code:    -32600,
				Message: "invalid request",
			},
		})
	}

	meta, err := decodeRequestMeta(req.Params)
	if err != nil {
		s.logCall(req.Method, "", "", startedAt, "-32602")
		return s.send(response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &responseError{
				Code:    -32602,
				Message: "invalid params",
			},
		})
	}

	// Era selection. An `initialize` request always selects legacy semantics,
	// even if it carries modern `_meta`. `server/discover` exists only in the
	// modern revision, so it is always validated. Everything else is validated
	// only once the client has identified itself as modern -- a legacy client
	// omits `_meta` entirely and must keep working unchanged.
	if req.Method != "initialize" && (meta.isModern() || req.Method == "server/discover") {
		if errResp := validateRequestMeta(meta); errResp != nil {
			s.logCall(req.Method, "", "", startedAt, strconv.Itoa(errResp.Code))
			return s.send(response{JSONRPC: "2.0", ID: req.ID, Error: errResp})
		}
	}

	// Having survived validation, the request is modern only if it carried
	// modern `_meta`, or is the modern-only `server/discover`. An `initialize`
	// handshake selects legacy semantics however it is dressed, and a request
	// that identified itself as neither is served the legacy shape: it cannot
	// be a valid modern request, which must always carry `_meta`.
	handler := requestHandler{
		Server: s,
		modern: req.Method == "server/discover" || (req.Method != "initialize" && meta.isModern()),
	}

	switch req.Method {
	case "initialize":
		s.logCall("initialize", "", "", startedAt, "")
		return handler.send(response{JSONRPC: "2.0", ID: req.ID, Result: initializeResult{
			ProtocolVersion: protocolVersionLegacy,
			Capabilities:    serverCapabilities(),
			ServerInfo: map[string]string{
				"name":    s.serverName,
				"version": s.serverVersion,
			},
			Instructions: serverInstructions,
		}})
	case "server/discover":
		s.logCall("server/discover", "", "", startedAt, "")
		return handler.send(response{JSONRPC: "2.0", ID: req.ID, Result: discoverResult{
			SupportedVersions: supportedProtocolVersions,
			Capabilities:      serverCapabilities(),
			Instructions:      serverInstructions,
		}})
	case "tools/list":
		s.logCall("tools/list", "", "", startedAt, "")
		return handler.send(response{JSONRPC: "2.0", ID: req.ID, Result: toolsListResult{Tools: s.tools}})
	case "tools/call":
		toolID := append(json.RawMessage(nil), req.ID...)
		toolParams := append(json.RawMessage(nil), req.Params...)
		key, call, err := s.registerToolCall(ctx, toolID)
		if err != nil {
			s.logCall("tools/call", "", "", startedAt, "-32600")
			return handler.send(response{
				JSONRPC: "2.0", ID: req.ID,
				Error: &responseError{Code: -32600, Message: err.Error()},
			})
		}
		handler.call = call
		s.inflightTools.Add(1)
		go func(callID, rawParams json.RawMessage) {
			defer s.inflightTools.Done()
			defer s.finishToolCall(key, call)
			if call.ctx.Err() != nil {
				return
			}
			if err := handler.handleToolCall(call.ctx, callID, rawParams); err != nil {
				s.logLifecycle(slog.LevelError, "mcp_tool_call_error", slog.String("error", err.Error()))
			}
		}(toolID, toolParams)
		return nil
	default:
		s.logCall(req.Method, "", "", startedAt, "-32601")
		return handler.send(response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &responseError{
				Code:    -32601,
				Message: "method not found",
			},
		})
	}
}

// requestIDKey preserves the distinction between string and integer IDs and
// normalizes string escapes, so a cancellation matches the original ID even
// when its JSON string was encoded differently. MCP IDs are strings or integers.
func requestIDKey(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] == '"' {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", false
		}
		return "string:" + id, true
	}
	digits := raw
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) == 0 || (len(digits) > 1 && digits[0] == '0') {
		return "", false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	if bytes.Equal(digits, []byte("0")) {
		return "integer:0", true
	}
	return "integer:" + string(raw), true
}

func (s *Server) registerToolCall(ctx context.Context, id json.RawMessage) (string, *inflightToolCall, error) {
	key, ok := requestIDKey(id)
	if !ok {
		return "", nil, fmt.Errorf("invalid request ID")
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, exists := s.inflightCalls[key]; exists {
		return "", nil, fmt.Errorf("request ID is already in flight")
	}
	callCtx, cancel := context.WithCancel(ctx)
	call := &inflightToolCall{ctx: callCtx, cancel: cancel}
	if s.inflightCalls == nil {
		s.inflightCalls = make(map[string]*inflightToolCall)
	}
	s.inflightCalls[key] = call
	return key, call, nil
}

func (s *Server) finishToolCall(key string, call *inflightToolCall) {
	s.inflightMu.Lock()
	if s.inflightCalls[key] == call {
		delete(s.inflightCalls, key)
	}
	s.inflightMu.Unlock()
	call.cancel()
}

func (s *Server) handleCancellation(rawParams json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    *string         `json:"reason,omitempty"`
		Meta      json.RawMessage `json:"_meta,omitempty"`
	}
	if err := decodeStrict(rawParams, &params); err != nil {
		return
	}
	key, ok := requestIDKey(params.RequestID)
	if !ok {
		return
	}
	s.inflightMu.Lock()
	call := s.inflightCalls[key]
	if call == nil || call.responding || call.cancelled {
		s.inflightMu.Unlock()
		return
	}
	call.cancelled = true
	// Cancel while holding the registry lock so a concurrent completion cannot
	// expose this ID for reuse before this particular call has been cancelled.
	call.cancel()
	s.inflightMu.Unlock()
	reason := ""
	if params.Reason != nil {
		reason = *params.Reason
	}
	s.logLifecycle(slog.LevelInfo, "mcp_request_cancelled", slog.String("request_id", key), slog.String("reason", reason))
}

func (s *Server) cancelInflightCalls() {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	for _, call := range s.inflightCalls {
		call.cancelled = true
		call.cancel()
	}
	clear(s.inflightCalls)
}

func (s requestHandler) handleToolCall(ctx context.Context, id json.RawMessage, rawParams json.RawMessage) error {
	startedAt := time.Now()

	params, err := decodeToolCallParams(rawParams)
	if err != nil {
		return s.sendInvalidParams("tools/call", "", "", startedAt, id)
	}

	switch params.Name {
	case "list_local_hardware":
		return s.handleListLocalHardwareCall(ctx, id, params.Arguments)
	case "beam_media":
		return s.handleBeamMediaCall(ctx, id, params.Arguments)
	case "inspect_torrent":
		return s.handleInspectTorrentCall(ctx, id, params.Arguments)
	case "stop_beaming":
		return s.handleStopBeamingCall(ctx, id, params.Arguments)
	case "play_beaming":
		return s.handlePlaybackControlCall(ctx, id, params.Arguments, "play_beaming")
	case "pause_beaming":
		return s.handlePlaybackControlCall(ctx, id, params.Arguments, "pause_beaming")
	case "seek_beaming":
		return s.handleSeekBeamingCall(ctx, id, params.Arguments)
	case "get_beaming_status":
		return s.handleGetBeamingStatusCall(ctx, id, params.Arguments)
	default:
		s.logCall(params.Name, "", "", startedAt, "TOOL_NOT_FOUND")
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result: toolErrorResult(
				"TOOL_NOT_FOUND",
				fmt.Sprintf("unknown tool: %s", params.Name),
			),
		})
	}
}

// isProtocolCallParam reports whether a top-level `tools/call` param belongs to
// the protocol rather than to the tool's arguments. Clients that omit
// `arguments` fall through to the flattening path below, which must not fold
// these into the argument object: strict decoding would then reject the call as
// carrying unknown fields. `inputResponses` and `requestState` arrive on multi
// round-trip retries, which this server never asks for but may still receive.
func isProtocolCallParam(key string) bool {
	switch key {
	case "name", "_meta", "inputResponses", "requestState":
		return true
	default:
		return false
	}
}

func decodeToolCallParams(raw json.RawMessage) (toolsCallParams, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return toolsCallParams{}, err
	}

	nameRaw, ok := payload["name"]
	if !ok {
		return toolsCallParams{}, fmt.Errorf("missing tool name")
	}

	var name string
	if err := json.Unmarshal(nameRaw, &name); err != nil {
		return toolsCallParams{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return toolsCallParams{}, fmt.Errorf("missing tool name")
	}

	arguments, ok := payload["arguments"]
	if !ok {
		flattened := map[string]json.RawMessage{}
		for key, value := range payload {
			if isProtocolCallParam(key) {
				continue
			}
			flattened[key] = value
		}
		if len(flattened) > 0 {
			normalized, err := json.Marshal(flattened)
			if err != nil {
				return toolsCallParams{}, err
			}
			arguments = normalized
		}
	}

	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage("{}")
	}

	return toolsCallParams{
		Name:      name,
		Arguments: arguments,
	}, nil
}

func (s requestHandler) handleInspectTorrentCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()
	if s.beamController == nil {
		return s.sendToolInternalError("inspect_torrent", "", "", startedAt, id, "beam controller is not configured")
	}
	var args struct {
		Source    string `json:"source"`
		TimeoutMS *int   `json:"timeout_ms,omitempty"`
	}
	if err := decodeStrict(rawArgs, &args); err != nil {
		return s.sendInvalidParams("inspect_torrent", "", "", startedAt, id)
	}
	args.Source = strings.TrimSpace(args.Source)
	timeoutMS := defaultTorrentTimeoutMS
	if args.TimeoutMS != nil {
		timeoutMS = *args.TimeoutMS
	}
	if args.Source == "" || timeoutMS < minTorrentTimeoutMS || timeoutMS > maxTorrentTimeoutMS {
		return s.sendInvalidParams("inspect_torrent", "", "", startedAt, id)
	}
	result, err := s.beamController.InspectTorrent(ctx, domain.InspectTorrentRequest{
		Source:    args.Source,
		TimeoutMS: timeoutMS,
	})
	if err != nil {
		s.logCall("inspect_torrent", "", "", startedAt, toolErrorCode(err))
		return s.send(response{JSONRPC: "2.0", ID: id, Result: toolErrorResultFromError(err)})
	}
	s.logCall("inspect_torrent", "", "", startedAt, "")
	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content:           []toolContent{{Type: "text", Text: formatTorrentFiles(result.Files)}},
			StructuredContent: result,
		},
	})
}

func (s requestHandler) handleBeamMediaCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()

	if s.beamController == nil {
		return s.sendToolInternalError("beam_media", "", "", startedAt, id, "beam controller is not configured")
	}

	var args struct {
		Source                string  `json:"source"`
		TargetDevice          string  `json:"target_device"`
		Transcode             *string `json:"transcode,omitempty"`
		SubtitlesPath         *string `json:"subtitles_path,omitempty"`
		StartSeconds          *int    `json:"start_seconds,omitempty"`
		TorrentFileIndex      *int    `json:"torrent_file_index,omitempty"`
		SubtitleMode          *string `json:"subtitle_mode,omitempty"`
		EmbeddedSubtitleTrack *int    `json:"embedded_subtitle_track,omitempty"`
	}
	if err := decodeStrict(rawArgs, &args); err != nil {
		return s.sendInvalidParams("beam_media", "", "", startedAt, id)
	}

	args.Source = strings.TrimSpace(args.Source)
	args.TargetDevice = strings.TrimSpace(args.TargetDevice)
	if args.Source == "" || args.TargetDevice == "" {
		return s.sendInvalidParams("beam_media", args.TargetDevice, "", startedAt, id)
	}

	transcode := transcodeAuto
	if args.Transcode != nil {
		transcode = strings.ToLower(strings.TrimSpace(*args.Transcode))
	}
	if transcode != transcodeAuto && transcode != transcodeAlways && transcode != transcodeNever {
		return s.sendInvalidParams("beam_media", args.TargetDevice, "", startedAt, id)
	}

	subtitlesPath := ""
	if args.SubtitlesPath != nil {
		subtitlesPath = strings.TrimSpace(*args.SubtitlesPath)
	}
	if args.StartSeconds != nil && *args.StartSeconds < 0 {
		return s.sendInvalidParams("beam_media", args.TargetDevice, "", startedAt, id)
	}
	if (args.TorrentFileIndex != nil && *args.TorrentFileIndex < 0) ||
		(args.EmbeddedSubtitleTrack != nil && *args.EmbeddedSubtitleTrack < 0) {
		return s.sendInvalidParams("beam_media", args.TargetDevice, "", startedAt, id)
	}
	subtitleMode := "auto"
	if args.SubtitleMode != nil {
		subtitleMode = strings.ToLower(strings.TrimSpace(*args.SubtitleMode))
	}
	switch subtitleMode {
	case "auto", "receiver", "burn", "off":
	default:
		return s.sendInvalidParams("beam_media", args.TargetDevice, "", startedAt, id)
	}

	result, err := s.beamController.BeamMedia(ctx, domain.BeamRequest{
		Source:                args.Source,
		TargetDevice:          args.TargetDevice,
		Transcode:             transcode,
		SubtitlesPath:         subtitlesPath,
		StartSeconds:          args.StartSeconds,
		TorrentFileIndex:      args.TorrentFileIndex,
		SubtitleMode:          subtitleMode,
		EmbeddedSubtitleTrack: args.EmbeddedSubtitleTrack,
	})
	if err != nil {
		s.logCall("beam_media", args.TargetDevice, "", startedAt, toolErrorCode(err))
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result:  toolErrorResultFromError(err),
		})
	}
	s.logCall("beam_media", result.DeviceID, result.SessionID, startedAt, "")

	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{
				{
					Type: "text",
					Text: fmt.Sprintf("Beam started on device %s (session %s).", result.DeviceID, result.SessionID),
				},
			},
			StructuredContent: result,
		},
	})
}

func (s requestHandler) handleStopBeamingCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()

	if s.beamController == nil {
		return s.sendToolInternalError("stop_beaming", "", "", startedAt, id, "beam controller is not configured")
	}

	var args struct {
		TargetDevice *string `json:"target_device,omitempty"`
		SessionID    *string `json:"session_id,omitempty"`
	}
	if err := decodeStrict(rawArgs, &args); err != nil {
		return s.sendInvalidParams("stop_beaming", "", "", startedAt, id)
	}

	targetDevice := ""
	sessionID := ""
	if args.TargetDevice != nil {
		targetDevice = strings.TrimSpace(*args.TargetDevice)
	}
	if args.SessionID != nil {
		sessionID = strings.TrimSpace(*args.SessionID)
	}
	if targetDevice == "" && sessionID == "" {
		return s.sendInvalidParams("stop_beaming", targetDevice, sessionID, startedAt, id)
	}

	result, err := s.beamController.StopBeaming(ctx, domain.StopRequest{
		TargetDevice: targetDevice,
		SessionID:    sessionID,
	})
	if err != nil {
		s.logCall("stop_beaming", targetDevice, sessionID, startedAt, toolErrorCode(err))
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result:  toolErrorResultFromError(err),
		})
	}
	s.logCall("stop_beaming", result.DeviceID, result.StoppedSessionID, startedAt, "")

	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{
				{
					Type: "text",
					Text: fmt.Sprintf("Stopped beaming session %s.", result.StoppedSessionID),
				},
			},
			StructuredContent: result,
		},
	})
}

func (s requestHandler) handlePlaybackControlCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage, toolName string) error {
	startedAt := time.Now()

	if s.beamController == nil {
		return s.sendToolInternalError(toolName, "", "", startedAt, id, "beam controller is not configured")
	}

	var args struct {
		TargetDevice *string `json:"target_device,omitempty"`
		SessionID    *string `json:"session_id,omitempty"`
	}
	if err := decodeStrict(rawArgs, &args); err != nil {
		return s.sendInvalidParams(toolName, "", "", startedAt, id)
	}

	targetDevice := ""
	sessionID := ""
	if args.TargetDevice != nil {
		targetDevice = strings.TrimSpace(*args.TargetDevice)
	}
	if args.SessionID != nil {
		sessionID = strings.TrimSpace(*args.SessionID)
	}
	if targetDevice == "" && sessionID == "" {
		return s.sendInvalidParams(toolName, targetDevice, sessionID, startedAt, id)
	}

	req := domain.PlaybackControlRequest{
		TargetDevice: targetDevice,
		SessionID:    sessionID,
	}
	var result *domain.PlaybackControlResult
	var err error
	switch toolName {
	case "play_beaming":
		result, err = s.beamController.PlayBeaming(ctx, req)
	case "pause_beaming":
		result, err = s.beamController.PauseBeaming(ctx, req)
	default:
		return s.sendToolInternalError(toolName, targetDevice, sessionID, startedAt, id, "unknown playback control tool")
	}
	if err != nil {
		s.logCall(toolName, targetDevice, sessionID, startedAt, toolErrorCode(err))
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result:  toolErrorResultFromError(err),
		})
	}
	s.logCall(toolName, result.DeviceID, result.SessionID, startedAt, "")

	verb := "Resumed"
	if toolName == "pause_beaming" {
		verb = "Paused"
	}
	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{
				{
					Type: "text",
					Text: fmt.Sprintf("%s beaming session %s.", verb, result.SessionID),
				},
			},
			StructuredContent: result,
		},
	})
}

func (s requestHandler) handleSeekBeamingCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()

	if s.beamController == nil {
		return s.sendToolInternalError("seek_beaming", "", "", startedAt, id, "beam controller is not configured")
	}

	var args struct {
		TargetDevice *string  `json:"target_device,omitempty"`
		SessionID    *string  `json:"session_id,omitempty"`
		Mode         *string  `json:"mode,omitempty"`
		Value        *float64 `json:"value,omitempty"`
	}
	if err := decodeStrict(rawArgs, &args); err != nil {
		return s.sendInvalidParams("seek_beaming", "", "", startedAt, id)
	}

	targetDevice := ""
	sessionID := ""
	if args.TargetDevice != nil {
		targetDevice = strings.TrimSpace(*args.TargetDevice)
	}
	if args.SessionID != nil {
		sessionID = strings.TrimSpace(*args.SessionID)
	}

	if targetDevice == "" && sessionID == "" {
		return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
	}

	if args.Mode == nil || args.Value == nil {
		return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
	}

	seekReq := domain.SeekRequest{
		TargetDevice: targetDevice,
		SessionID:    sessionID,
	}
	value := *args.Value

	// Bound the magnitude before the float -> int conversions below: a value
	// beyond int range converts to a large negative number, which delta_seconds
	// does not range-check and would clamp into a seek back to the start.
	// Written as a range test so that a NaN, which compares false against
	// everything, is rejected here rather than converted.
	if !(value >= -maxSeekSeconds && value <= maxSeekSeconds) {
		return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
	}

	switch *args.Mode {
	case "absolute_seconds":
		if value < 0 {
			return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
		}
		seconds := int(math.Round(value))
		seekReq.PositionSeconds = &seconds
	case "percent":
		if value < 0 || value > 100 {
			return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
		}
		seekReq.PositionPercent = &value
	case "from_end_seconds":
		if value < 0 {
			return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
		}
		seconds := int(math.Round(value))
		seekReq.FromEndSeconds = &seconds
	case "delta_seconds":
		seconds := int(math.Round(value))
		seekReq.DeltaSeconds = &seconds
	default:
		return s.sendInvalidParams("seek_beaming", targetDevice, sessionID, startedAt, id)
	}

	result, err := s.beamController.SeekBeaming(ctx, seekReq)
	if err != nil {
		s.logCall("seek_beaming", targetDevice, sessionID, startedAt, toolErrorCode(err))
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result:  toolErrorResultFromError(err),
		})
	}
	s.logCall("seek_beaming", result.DeviceID, result.SessionID, startedAt, "")

	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{
				{
					Type: "text",
					Text: fmt.Sprintf("Seeked session %s to %d second(s).", result.SessionID, result.ResolvedPositionSeconds),
				},
			},
			StructuredContent: result,
		},
	})
}

func (s requestHandler) handleGetBeamingStatusCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()

	if s.beamController == nil {
		return s.sendToolInternalError("get_beaming_status", "", "", startedAt, id, "beam controller is not configured")
	}

	var args struct {
		TargetDevice *string `json:"target_device,omitempty"`
		SessionID    *string `json:"session_id,omitempty"`
	}
	if err := decodeStrict(rawArgs, &args); err != nil {
		return s.sendInvalidParams("get_beaming_status", "", "", startedAt, id)
	}

	targetDevice := ""
	sessionID := ""
	if args.TargetDevice != nil {
		targetDevice = strings.TrimSpace(*args.TargetDevice)
	}
	if args.SessionID != nil {
		sessionID = strings.TrimSpace(*args.SessionID)
	}
	if targetDevice == "" && sessionID == "" {
		return s.sendInvalidParams("get_beaming_status", targetDevice, sessionID, startedAt, id)
	}

	result, err := s.beamController.GetBeamingStatus(ctx, domain.StatusRequest{
		TargetDevice: targetDevice,
		SessionID:    sessionID,
	})
	if err != nil {
		s.logCall("get_beaming_status", targetDevice, sessionID, startedAt, toolErrorCode(err))
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result:  toolErrorResultFromError(err),
		})
	}
	s.logCall("get_beaming_status", result.DeviceID, result.SessionID, startedAt, "")

	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{
				{
					Type: "text",
					Text: formatBeamingStatusText(result),
				},
			},
			StructuredContent: result,
		},
	})
}

func (s requestHandler) handleListLocalHardwareCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()

	if s.localHardwareLister == nil {
		return s.sendToolInternalError("list_local_hardware", "", "", startedAt, id, "discovery service is not configured")
	}

	timeoutMS := defaultDiscoveryTimeoutMS
	includeUnreachable := false
	if len(rawArgs) > 0 {
		var args struct {
			TimeoutMS          *int  `json:"timeout_ms,omitempty"`
			IncludeUnreachable *bool `json:"include_unreachable,omitempty"`
		}
		if err := decodeStrict(rawArgs, &args); err != nil {
			return s.sendInvalidParams("list_local_hardware", "", "", startedAt, id)
		}
		if args.TimeoutMS != nil {
			if *args.TimeoutMS < minDiscoveryTimeoutMS {
				return s.sendInvalidParams("list_local_hardware", "", "", startedAt, id)
			}
			timeoutMS = *args.TimeoutMS
		}
		if args.IncludeUnreachable != nil {
			includeUnreachable = *args.IncludeUnreachable
		}
	}
	s.logLifecycle(
		slog.LevelDebug,
		"list_local_hardware_request",
		slog.Int("timeout_ms", timeoutMS),
		slog.Bool("include_unreachable", includeUnreachable),
	)

	devices, err := s.localHardwareLister.ListLocalHardware(ctx, timeoutMS, includeUnreachable)
	if err != nil {
		s.logCall("list_local_hardware", "", "", startedAt, "INTERNAL_ERROR")
		return s.send(response{
			JSONRPC: "2.0",
			ID:      id,
			Result:  toolErrorResult("INTERNAL_ERROR", err.Error()),
		})
	}
	s.logLifecycle(slog.LevelDebug, "list_local_hardware_result", slog.Int("discovered_count", len(devices)))
	s.logCall("list_local_hardware", "", "", startedAt, "")
	summaryText := fmt.Sprintf("Discovered %d device(s).", len(devices))
	if len(devices) > 0 {
		summaryText += "\n" + formatDiscoveredDevices(devices)
	}

	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{
				{
					Type: "text",
					Text: summaryText,
				},
			},
			StructuredContent: map[string]any{
				"count":   len(devices),
				"devices": devices,
			},
		},
	})
}

// validateRequestMeta enforces the per-request protocol fields the modern
// revision requires. The version is checked before the remaining fields so that
// a client speaking a revision this server does not know is told to downgrade
// rather than being told its request is malformed by this revision's rules.
func validateRequestMeta(meta *requestMeta) *responseError {
	if !meta.isModern() {
		return &responseError{Code: -32602, Message: "invalid params"}
	}

	if !isSupportedProtocolVersion(*meta.ProtocolVersion) {
		return &responseError{
			Code:    errUnsupportedProtocolVersion,
			Message: "Unsupported protocol version",
			Data: unsupportedVersionData{
				Supported: supportedProtocolVersions,
				Requested: *meta.ProtocolVersion,
			},
		}
	}

	if !meta.hasClientCapabilities() {
		return &responseError{Code: -32602, Message: "invalid params"}
	}

	return nil
}

func serverCapabilities() map[string]any {
	return map[string]any{
		"tools": map[string]any{
			"listChanged": false,
		},
	}
}

// finalizeResult stamps the fields every modern result carries: the mandatory
// `resultType`, the cache hints required of a CacheableResult, and the
// recommended server identity. Applying it centrally in the send path is what
// guarantees no handler can omit them.
//
// On a legacy exchange results are handed back untouched, so a legacy client
// sees only its own revision's shape: the modern-only fields are all omitempty
// and stay absent.
func (s requestHandler) finalizeResult(result any) any {
	if !s.modern {
		return result
	}

	ttl := staticListTTLMs

	switch typed := result.(type) {
	case discoverResult:
		typed.ResultType = resultTypeComplete
		typed.TTLMs = &ttl
		typed.CacheScope = cacheScopePublic
		typed.Meta = s.resultMeta
		return typed
	case toolsListResult:
		typed.ResultType = resultTypeComplete
		typed.TTLMs = &ttl
		typed.CacheScope = cacheScopePublic
		typed.Meta = s.resultMeta
		return typed
	case toolCallResult:
		typed.ResultType = resultTypeComplete
		typed.Meta = s.resultMeta
		return typed
	default:
		return result
	}
}

func decodeStrict(raw json.RawMessage, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("invalid JSON payload")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid JSON payload")
	}
	return nil
}

func (s requestHandler) sendInvalidParams(method, deviceID, sessionID string, startedAt time.Time, id json.RawMessage) error {
	s.logCall(method, deviceID, sessionID, startedAt, "-32602")
	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &responseError{
			Code:    -32602,
			Message: "invalid params",
		},
	})
}

func (s requestHandler) sendToolInternalError(method, deviceID, sessionID string, startedAt time.Time, id json.RawMessage, message string) error {
	s.logCall(method, deviceID, sessionID, startedAt, "INTERNAL_ERROR")
	return s.send(response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  toolErrorResult("INTERNAL_ERROR", message),
	})
}

func toolErrorResult(code, message string) toolCallResult {
	return toolCallResult{
		Content: []toolContent{
			{
				Type: "text",
				Text: fmt.Sprintf("%s: %s", code, message),
			},
		},
		StructuredContent: map[string]any{
			"error": map[string]string{
				"code":    code,
				"message": message,
			},
		},
		IsError: true,
	}
}

func toolErrorResultFromError(err error) toolCallResult {
	var tErr *domain.ToolError
	if errors.As(err, &tErr) && tErr != nil {
		result := toolErrorResult(tErr.Code, tErr.Message)
		structured := map[string]any{
			"error": map[string]any{
				"code":    tErr.Code,
				"message": tErr.Message,
			},
		}
		if len(tErr.Limitations) > 0 {
			structured["error"].(map[string]any)["limitations"] = tErr.Limitations
		}
		if len(tErr.SuggestedFixes) > 0 {
			structured["error"].(map[string]any)["suggested_fixes"] = tErr.SuggestedFixes
		}
		if len(tErr.Details) > 0 {
			structured["error"].(map[string]any)["details"] = tErr.Details
		}
		result.StructuredContent = structured
		return result
	}

	return toolErrorResult("INTERNAL_ERROR", err.Error())
}

func toolErrorCode(err error) string {
	var tErr *domain.ToolError
	if errors.As(err, &tErr) && tErr != nil && strings.TrimSpace(tErr.Code) != "" {
		return tErr.Code
	}
	return "INTERNAL_ERROR"
}

func (s *Server) logCall(method, deviceID, sessionID string, startedAt time.Time, errorCode string) {
	if s == nil || s.logger == nil {
		return
	}
	level := slog.LevelInfo
	if strings.TrimSpace(errorCode) != "" {
		level = slog.LevelError
	}

	s.logger.Log(
		context.Background(),
		level,
		"mcp_call",
		slog.String("method", strings.TrimSpace(method)),
		slog.String("device_id", strings.TrimSpace(deviceID)),
		slog.String("session_id", strings.TrimSpace(sessionID)),
		slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
		slog.String("error_code", strings.TrimSpace(errorCode)),
	)
}

func (s requestHandler) send(resp response) error {
	if s.call != nil {
		s.inflightMu.Lock()
		// A cancelled request produces no response. Marking the send transition
		// under the same lock makes a late cancellation harmless.
		if s.call.cancelled || s.call.ctx.Err() != nil {
			s.inflightMu.Unlock()
			return nil
		}
		s.call.responding = true
		s.inflightMu.Unlock()
	}
	resp.Result = s.finalizeResult(resp.Result)
	return s.Server.send(resp)
}

func (s *Server) send(resp response) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	encoded, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	s.logLifecycle(slog.LevelDebug, "mcp_send", slog.Int("bytes", len(encoded)))
	if s.useJSONLineOutput {
		return writeJSONLineMessage(s.out, encoded)
	}
	return writeFramedMessage(s.out, encoded)
}

func (s *Server) waitForInflightTools() {
	done := make(chan struct{})
	go func() {
		s.inflightTools.Wait()
		close(done)
	}()

	timer := time.NewTimer(inflightDrainTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		s.logLifecycle(
			slog.LevelWarn,
			"mcp_inflight_drain_timeout",
			slog.Int64("waited_ms", inflightDrainTimeout.Milliseconds()),
		)
	}
}

func (s *Server) logLifecycle(level slog.Level, msg string, attrs ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Log(context.Background(), level, msg, attrs...)
}

func formatDiscoveredDevices(devices []domain.Device) string {
	var out strings.Builder
	for i, dev := range devices {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(
			&out,
			"%d. id=%s name=%s protocol=%s address=%s",
			i+1,
			strings.TrimSpace(dev.ID),
			strings.TrimSpace(dev.Name),
			strings.TrimSpace(dev.Protocol),
			strings.TrimSpace(dev.Address),
		)
	}
	return out.String()
}

func formatTorrentFiles(files []domain.TorrentFile) string {
	if len(files) == 0 {
		return "Torrent contains no supported media files to beam."
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Torrent contains %d supported media files. Pass the desired original index as beam_media.arguments.torrent_file_index.", len(files))
	for _, file := range files {
		fmt.Fprintf(&out, "\nindex=%d name=%q size_bytes=%d", file.Index, file.Name, file.Size)
	}
	return out.String()
}

func formatBeamingStatusText(result *domain.StatusResult) string {
	if result == nil {
		return "No beaming status available."
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Session %s on device %s is %s.", result.SessionID, result.DeviceID, result.State)
	if result.PositionSeconds != nil {
		fmt.Fprintf(&out, " Position %.0fs", *result.PositionSeconds)
		if result.DurationSeconds != nil {
			fmt.Fprintf(&out, " of %.0fs", *result.DurationSeconds)
		}
		out.WriteByte('.')
	}
	if strings.TrimSpace(result.Title) != "" {
		fmt.Fprintf(&out, " Title: %s.", strings.TrimSpace(result.Title))
	}
	if result.Torrent != nil {
		fmt.Fprintf(&out, " Torrent file %s: %.1f%% downloaded (%d/%d bytes).",
			result.Torrent.FileName, result.Torrent.ProgressPercent,
			result.Torrent.DownloadedBytes, result.Torrent.TotalBytes)
	}
	return out.String()
}

func staticTools() []tool {
	return []tool{
		{
			Name:        "list_local_hardware",
			Description: "Discover Chromecast, Smart TVs, and DLNA/UPnP media renderers on the local network. Always call this first to find available 'target_device' IDs or names before attempting to cast media.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"timeout_ms": map[string]any{
						"type":        "integer",
						"minimum":     100,
						"default":     defaultDiscoveryTimeoutMS,
						"description": "Discovery timeout in milliseconds. Increase this if devices are slow to respond.",
					},
					"include_unreachable": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "Include devices that fail immediate reachability checks. Useful if a known device is temporarily sleeping.",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        "inspect_torrent",
			Description: "Inspect files in a local .torrent file or magnet URI before casting. Returns file indices, names, and sizes without starting playback. When multiple supported media files are present, choose an index and pass it as torrent_file_index to beam_media.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source": map[string]any{
						"type":        "string",
						"description": "Absolute local path to a .torrent file or a magnet URI. Magnet metadata retrieval may take time depending on available peers.",
					},
					"timeout_ms": map[string]any{
						"type":        "integer",
						"minimum":     minTorrentTimeoutMS,
						"maximum":     maxTorrentTimeoutMS,
						"default":     defaultTorrentTimeoutMS,
						"description": "Timeout in milliseconds for torrent preparation and metadata retrieval.",
					},
				},
				"required":             []string{"source"},
				"additionalProperties": false,
			},
		},
		{
			Name: "beam_media",
			Description: "Cast or stream media (video, audio, etc.) to a selected local Smart TV, Chromecast, or UPnP/DLNA device. " +
				"You must provide a valid target_device ID or name. Local .torrent files and magnet URIs are supported; torrents with multiple supported media files require an explicit torrent_file_index from inspect_torrent. Torrent metadata retrieval has a 60-second timeout; subsequent media preparation and device startup can take additional time. " +
				"Returns a session_id accepted by the other beaming tools. Sessions live only in this server process: they are lost if it restarts, " +
				"and are reclaimed automatically after about 10 minutes idle or stalled, 90 minutes paused, or 24 hours in total. " +
				"If a session_id is no longer recognized, start a new beam rather than retrying with it.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source": map[string]any{
						"type":        "string",
						"description": "Absolute local media path, HTTP/HTTPS media URL, absolute local .torrent path, or magnet URI.",
					},
					"target_device": map[string]any{
						"type":        "string",
						"description": "The target device ID or exact name. Obtain this by calling 'list_local_hardware' first.",
					},
					"transcode": map[string]any{
						"type":        "string",
						"default":     "auto",
						"enum":        []string{"auto", "always", "never"},
						"description": "Whether to transcode the media on the fly. Defaults to 'auto'.",
					},
					"subtitles_path": map[string]any{
						"type":        "string",
						"description": "Optional absolute local path to an external subtitle file (e.g., .srt, .vtt). Takes precedence over sidecar and embedded subtitles.",
					},
					"torrent_file_index": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "File index returned by inspect_torrent. Required when multiple supported media files are present; optional when there is exactly one supported media file.",
					},
					"subtitle_mode": map[string]any{
						"type":        "string",
						"enum":        []string{"auto", "receiver", "burn", "off"},
						"default":     "auto",
						"description": "Subtitle rendering: auto selects a compatible path; receiver sends subtitles to the device and cannot render bitmap tracks; burn embeds subtitles in video and implies transcoding (incompatible with transcode=never); off disables subtitles.",
					},
					"embedded_subtitle_track": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Zero-based subtitle track index in a local media file; unavailable for torrent sources. External subtitles_path takes precedence.",
					},
					"start_seconds": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Optional start offset in seconds from the beginning of media playback.",
					},
				},
				"required":             []string{"source", "target_device"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "get_beaming_status",
			Description: "Get current playback status for an active beaming session, including state, position, duration, title, content type, and torrent download progress when available.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_device": map[string]any{
						"type":        "string",
						"description": "The device ID or exact name of the session target.",
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "The unique session ID to inspect. This is returned by a successful 'beam_media' call.",
					},
				},
				"minProperties":        1,
				"additionalProperties": false,
			},
		},
		{
			Name:        "play_beaming",
			Description: "Resume active playback on a selected device or session.",
			InputSchema: playbackControlInputSchema(
				"The device ID or exact name of the device to resume playback on.",
				"The unique session ID to resume. This is returned by a successful 'beam_media' call.",
			),
		},
		{
			Name:        "pause_beaming",
			Description: "Pause active playback on a selected device or session.",
			InputSchema: playbackControlInputSchema(
				"The device ID or exact name of the device to pause playback on.",
				"The unique session ID to pause. This is returned by a successful 'beam_media' call.",
			),
		},
		{
			Name:        "seek_beaming",
			Description: "Seek active playback on a selected device or session. Identify the target with session_id or target_device, then set 'mode' to choose how to seek and 'value' to the amount. Provide exactly one mode and one value.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_device": map[string]any{
						"type":        "string",
						"description": "The device ID or exact name of the session target.",
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "The unique session ID to seek. This is returned by a successful 'beam_media' call.",
					},
					"mode": map[string]any{
						"type": "string",
						"enum": []string{"absolute_seconds", "percent", "from_end_seconds", "delta_seconds"},
						"description": "How to interpret 'value': " +
							"'absolute_seconds' jumps to a timestamp measured from the start; " +
							"'percent' jumps to a percentage of the total duration; " +
							"'from_end_seconds' jumps to a point measured back from the end; " +
							"'delta_seconds' skips relative to the current position. " +
							"For an exact timestamp use 'absolute_seconds'; for skip/rewind use 'delta_seconds'.",
					},
					"value": map[string]any{
						"type":    "number",
						"minimum": -maxSeekSeconds,
						"maximum": maxSeekSeconds,
						"description": "The seek amount, interpreted by 'mode'. " +
							"absolute_seconds: whole seconds from the start, 0 or greater (e.g. 4140 for 1:09:00). " +
							"percent: 0 to 100 (e.g. 50 for the middle). " +
							"from_end_seconds: whole seconds before the end, 0 or greater (e.g. 10 for ten seconds before the end). " +
							"delta_seconds: relative seconds, positive to skip ahead or negative to rewind (e.g. 30 or -10).",
					},
				},
				"required":             []string{"mode", "value"},
				"minProperties":        3,
				"additionalProperties": false,
			},
		},
		{
			Name:        "stop_beaming",
			Description: "Stop, or halt active media playback/casting on a selected device or session.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_device": map[string]any{
						"type":        "string",
						"description": "The device ID or exact name of the device to stop playing on.",
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "The unique session ID to stop. This is returned by a successful 'beam_media' call.",
					},
				},
				"minProperties":        1,
				"additionalProperties": false,
			},
		},
	}
}

func playbackControlInputSchema(targetDescription, sessionDescription string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target_device": map[string]any{
				"type":        "string",
				"description": targetDescription,
			},
			"session_id": map[string]any{
				"type":        "string",
				"description": sessionDescription,
			},
		},
		"minProperties":        1,
		"additionalProperties": false,
	}
}
