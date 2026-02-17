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
	"strings"
	"time"

	"go2tv.app/mcp-beam/internal/domain"
)

const protocolVersion = "2024-11-05"
const (
	defaultDiscoveryTimeoutMS = 5000
	minDiscoveryTimeoutMS     = 100
	transcodeAuto             = "auto"
	transcodeAlways           = "always"
	transcodeNever            = "never"
)

type LocalHardwareLister interface {
	ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error)
}

type BeamController interface {
	BeamMedia(ctx context.Context, req domain.BeamRequest) (*domain.BeamResult, error)
	StopBeaming(ctx context.Context, req domain.StopRequest) (*domain.StopResult, error)
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
	}
}

func (s *Server) Run(ctx context.Context) error {
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

	switch req.Method {
	case "initialize":
		s.logCall("initialize", "", "", startedAt, "")
		return s.send(response{JSONRPC: "2.0", ID: req.ID, Result: initializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities: map[string]any{
				"tools": map[string]any{
					"listChanged": false,
				},
			},
			ServerInfo: map[string]string{
				"name":    s.serverName,
				"version": s.serverVersion,
			},
			Instructions: "Use tools/list to inspect available tools.",
		}})
	case "tools/list":
		s.logCall("tools/list", "", "", startedAt, "")
		return s.send(response{JSONRPC: "2.0", ID: req.ID, Result: toolsListResult{Tools: s.tools}})
	case "tools/call":
		return s.handleToolCall(ctx, req.ID, req.Params)
	default:
		s.logCall(req.Method, "", "", startedAt, "-32601")
		return s.send(response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &responseError{
				Code:    -32601,
				Message: "method not found",
			},
		})
	}
}

func (s *Server) handleToolCall(ctx context.Context, id json.RawMessage, rawParams json.RawMessage) error {
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
	case "stop_beaming":
		return s.handleStopBeamingCall(ctx, id, params.Arguments)
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
			if key == "name" || key == "_meta" {
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

func (s *Server) handleBeamMediaCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
	startedAt := time.Now()

	if s.beamController == nil {
		return s.sendToolInternalError("beam_media", "", "", startedAt, id, "beam controller is not configured")
	}

	var args struct {
		Source        string  `json:"source"`
		TargetDevice  string  `json:"target_device"`
		Transcode     *string `json:"transcode,omitempty"`
		SubtitlesPath *string `json:"subtitles_path,omitempty"`
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

	result, err := s.beamController.BeamMedia(ctx, domain.BeamRequest{
		Source:        args.Source,
		TargetDevice:  args.TargetDevice,
		Transcode:     transcode,
		SubtitlesPath: subtitlesPath,
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

func (s *Server) handleStopBeamingCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
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

func (s *Server) handleListLocalHardwareCall(ctx context.Context, id json.RawMessage, rawArgs json.RawMessage) error {
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

func (s *Server) sendInvalidParams(method, deviceID, sessionID string, startedAt time.Time, id json.RawMessage) error {
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

func (s *Server) sendToolInternalError(method, deviceID, sessionID string, startedAt time.Time, id json.RawMessage, message string) error {
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

func (s *Server) send(resp response) error {
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
			Name:        "beam_media",
			Description: "Cast or stream media (video, audio, etc.) to a selected local Smart TV, Chromecast, or UPnP/DLNA device. You must provide a valid target_device ID or name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source": map[string]any{
						"type":        "string",
						"description": "The absolute local file path (e.g., /home/user/movie.mp4) or a valid HTTP/HTTPS URL of the media to cast.",
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
						"description": "Optional absolute local file path to a subtitle file (e.g., .srt, .vtt) to load with the media.",
					},
				},
				"required":             []string{"source", "target_device"},
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
				"anyOf": []map[string]any{
					{"required": []string{"target_device"}},
					{"required": []string{"session_id"}},
				},
				"additionalProperties": false,
			},
		},
	}
}
