package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go2tv.app/mcp-beam/internal/domain"
)

type fakeLocalHardwareLister struct {
	timeoutMS          int
	includeUnreachable bool
	devices            []domain.Device
	err                error
}

type fakeBeamController struct {
	beamReq    domain.BeamRequest
	beamResult *domain.BeamResult
	beamErr    error
	stopReq    domain.StopRequest
	stopResult *domain.StopResult
	stopErr    error
}

func (f *fakeBeamController) BeamMedia(ctx context.Context, req domain.BeamRequest) (*domain.BeamResult, error) {
	f.beamReq = req
	return f.beamResult, f.beamErr
}

func (f *fakeBeamController) StopBeaming(ctx context.Context, req domain.StopRequest) (*domain.StopResult, error) {
	f.stopReq = req
	return f.stopResult, f.stopErr
}

func (f *fakeLocalHardwareLister) ListLocalHardware(ctx context.Context, timeoutMS int, includeUnreachable bool) ([]domain.Device, error) {
	f.timeoutMS = timeoutMS
	f.includeUnreachable = includeUnreachable
	return f.devices, f.err
}

func TestInitializeAndToolsList(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})

	srv := New(input, output, Config{ServerName: "mcp-beam", ServerVersion: "1.0.0-test"})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	if responses[0]["id"].(float64) != 1 {
		t.Fatalf("initialize response id mismatch: %#v", responses[0]["id"])
	}

	initResult := responses[0]["result"].(map[string]any)
	if initResult["protocolVersion"].(string) == "" {
		t.Fatal("protocolVersion must not be empty")
	}

	if responses[1]["id"].(float64) != 2 {
		t.Fatalf("tools/list response id mismatch: %#v", responses[1]["id"])
	}

	toolResult := responses[1]["result"].(map[string]any)
	tools := toolResult["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
}

func TestInitializeJSONLineRequest(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := input.Write(append(payload, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}

	srv := New(input, output, Config{ServerName: "mcp-beam", ServerVersion: "1.0.0-test"})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d", len(lines))
	}

	resp := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["id"].(float64) != 1 {
		t.Fatalf("initialize response id mismatch: %#v", resp["id"])
	}
}

func TestUnknownMethod(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      "abc",
		"method":  "does/not/exist",
	})

	srv := New(input, output, Config{})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	errObj := responses[0]["error"].(map[string]any)
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("expected -32601, got %v", errObj["code"])
	}
}

func TestInvalidRequestJSONRPCVersion(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)

	writeRequest(t, input, map[string]any{
		"jsonrpc": "1.0",
		"id":      "badver",
		"method":  "tools/list",
	})

	srv := New(input, output, Config{})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	errObj := responses[0]["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected -32600, got %v", errObj["code"])
	}
}

func TestToolsCallListLocalHardware(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	lister := &fakeLocalHardwareLister{
		devices: []domain.Device{
			{ID: "dev_a", Name: "Bedroom TV", Protocol: "dlna"},
			{ID: "dev_b", Name: "Living Room TV", Protocol: "chromecast"},
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "list_local_hardware",
			"arguments": map[string]any{
				"timeout_ms":          3000,
				"include_unreachable": true,
			},
		},
	})

	srv := New(input, output, Config{LocalHardwareLister: lister})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	if responses[0]["id"].(float64) != 3 {
		t.Fatalf("tools/call response id mismatch: %#v", responses[0]["id"])
	}

	result := responses[0]["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	devices := structured["devices"].([]any)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	if lister.timeoutMS != 3000 {
		t.Fatalf("expected timeout 3000, got %d", lister.timeoutMS)
	}
	if !lister.includeUnreachable {
		t.Fatal("expected include_unreachable=true to be forwarded")
	}
}

func TestToolsCallListLocalHardwareAllowsMetaField(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	lister := &fakeLocalHardwareLister{
		devices: []domain.Device{
			{ID: "dev_a", Name: "Bedroom TV", Protocol: "dlna"},
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      30,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "list_local_hardware",
			"_meta": map[string]any{
				"progressToken": "tok_1",
			},
			"arguments": map[string]any{
				"timeout_ms":          3100,
				"include_unreachable": true,
			},
		},
	})

	srv := New(input, output, Config{LocalHardwareLister: lister})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0]["error"] != nil {
		t.Fatalf("expected successful tools/call, got error: %#v", responses[0]["error"])
	}
	if lister.timeoutMS != 3100 {
		t.Fatalf("expected timeout 3100, got %d", lister.timeoutMS)
	}
	if !lister.includeUnreachable {
		t.Fatal("expected include_unreachable=true to be forwarded")
	}
}

func TestToolsCallListLocalHardwareSupportsFlattenedArguments(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	lister := &fakeLocalHardwareLister{
		devices: []domain.Device{
			{ID: "dev_a", Name: "Bedroom TV", Protocol: "dlna"},
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      31,
		"method":  "tools/call",
		"params": map[string]any{
			"name":                "list_local_hardware",
			"timeout_ms":          3200,
			"include_unreachable": true,
		},
	})

	srv := New(input, output, Config{LocalHardwareLister: lister})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0]["error"] != nil {
		t.Fatalf("expected successful tools/call, got error: %#v", responses[0]["error"])
	}
	if lister.timeoutMS != 3200 {
		t.Fatalf("expected timeout 3200, got %d", lister.timeoutMS)
	}
	if !lister.includeUnreachable {
		t.Fatal("expected include_unreachable=true to be forwarded")
	}
}

func TestToolsCallListLocalHardwareClientFixtureMatrix(t *testing.T) {
	type fixture struct {
		Name    string         `json:"name"`
		Request map[string]any `json:"request"`
		Expect  struct {
			TimeoutMS          int  `json:"timeout_ms"`
			IncludeUnreachable bool `json:"include_unreachable"`
		} `json:"expect"`
	}

	entries, err := os.ReadDir("testdata/client-fixtures")
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one client fixture")
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join("testdata/client-fixtures", entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}

		var f fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("unmarshal fixture %s: %v", path, err)
		}

		t.Run(f.Name, func(t *testing.T) {
			input := bytes.NewBuffer(nil)
			output := bytes.NewBuffer(nil)
			lister := &fakeLocalHardwareLister{
				devices: []domain.Device{
					{ID: "dev_a", Name: "Living Room TV", Protocol: "chromecast"},
				},
			}

			writeRequest(t, input, f.Request)

			srv := New(input, output, Config{LocalHardwareLister: lister})
			if err := srv.Run(context.Background()); err != nil {
				t.Fatalf("run server: %v", err)
			}

			responses := readResponses(t, output.Bytes())
			if len(responses) != 1 {
				t.Fatalf("expected 1 response, got %d", len(responses))
			}
			if responses[0]["error"] != nil {
				t.Fatalf("expected successful tools/call, got error: %#v", responses[0]["error"])
			}

			if lister.timeoutMS != f.Expect.TimeoutMS {
				t.Fatalf("expected timeout %d, got %d", f.Expect.TimeoutMS, lister.timeoutMS)
			}
			if lister.includeUnreachable != f.Expect.IncludeUnreachable {
				t.Fatalf("expected include_unreachable=%t, got %t", f.Expect.IncludeUnreachable, lister.includeUnreachable)
			}
		})
	}
}

func TestToolsCallListLocalHardwareInvalidParams(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	lister := &fakeLocalHardwareLister{}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "list_local_hardware",
			"arguments": map[string]any{
				"timeout_ms": 99,
			},
		},
	})

	srv := New(input, output, Config{LocalHardwareLister: lister})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	errObj := responses[0]["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("expected -32602, got %v", errObj["code"])
	}
}

func TestToolsCallBeamMedia(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	controller := &fakeBeamController{
		beamResult: &domain.BeamResult{
			OK:          true,
			SessionID:   "sess_123",
			DeviceID:    "dev_1",
			MediaURL:    "http://127.0.0.1:3500/media",
			Transcoding: false,
			Warnings:    []string{},
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "beam_media",
			"arguments": map[string]any{
				"source":        "/tmp/video.mp4",
				"target_device": "dev_1",
				"transcode":     "never",
			},
		},
	})

	srv := New(input, output, Config{BeamController: controller})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	if structured["session_id"].(string) != "sess_123" {
		t.Fatalf("unexpected session_id: %v", structured["session_id"])
	}

	if controller.beamReq.Source != "/tmp/video.mp4" {
		t.Fatalf("unexpected source forwarded: %s", controller.beamReq.Source)
	}
	if controller.beamReq.TargetDevice != "dev_1" {
		t.Fatalf("unexpected target forwarded: %s", controller.beamReq.TargetDevice)
	}
	if controller.beamReq.Transcode != "never" {
		t.Fatalf("unexpected transcode forwarded: %s", controller.beamReq.Transcode)
	}
}

func TestToolsCallBeamMediaJSONLine(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	controller := &fakeBeamController{
		beamResult: &domain.BeamResult{
			OK:          true,
			SessionID:   "sess_json_1",
			DeviceID:    "dev_json_1",
			MediaURL:    "http://127.0.0.1:3500/media",
			Transcoding: false,
			Warnings:    []string{},
		},
	}

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      55,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "beam_media",
			"arguments": map[string]any{
				"source":        "/tmp/video.mp4",
				"target_device": "dev_json_1",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := input.Write(append(payload, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}

	srv := New(input, output, Config{BeamController: controller})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d", len(lines))
	}
	resp := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["id"].(float64) != 55 {
		t.Fatalf("tools/call response id mismatch: %#v", resp["id"])
	}
	result := resp["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	if structured["session_id"].(string) != "sess_json_1" {
		t.Fatalf("unexpected session_id: %v", structured["session_id"])
	}
}

func TestToolsCallBeamMediaStructuredLog(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	logOutput := bytes.NewBuffer(nil)
	logger := slog.New(slog.NewJSONHandler(logOutput, nil))

	controller := &fakeBeamController{
		beamResult: &domain.BeamResult{
			OK:          true,
			SessionID:   "sess_123",
			DeviceID:    "dev_1",
			MediaURL:    "http://127.0.0.1:3500/media",
			Transcoding: false,
			Warnings:    []string{},
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "beam_media",
			"arguments": map[string]any{
				"source":        "/tmp/video.mp4",
				"target_device": "dev_1",
			},
		},
	})

	srv := New(input, output, Config{BeamController: controller, Logger: logger})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(logOutput.String()), "\n")
	var logEntry map[string]any
	for _, line := range lines {
		candidate := map[string]any{}
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("unmarshal log line: %v", err)
		}
		if candidate["msg"] == "mcp_call" {
			logEntry = candidate
			break
		}
	}
	if len(logEntry) == 0 {
		t.Fatalf("missing mcp_call log entry; got %d total log line(s)", len(lines))
	}

	if logEntry["level"] != "INFO" {
		t.Fatalf("expected INFO level, got %v", logEntry["level"])
	}
	if logEntry["method"] != "beam_media" {
		t.Fatalf("unexpected method: %v", logEntry["method"])
	}
	if logEntry["device_id"] != "dev_1" {
		t.Fatalf("unexpected device_id: %v", logEntry["device_id"])
	}
	if logEntry["session_id"] != "sess_123" {
		t.Fatalf("unexpected session_id: %v", logEntry["session_id"])
	}
	if _, ok := logEntry["duration_ms"]; !ok {
		t.Fatal("expected duration_ms field")
	}
	if logEntry["error_code"] != "" {
		t.Fatalf("expected empty error_code, got %v", logEntry["error_code"])
	}
}

func TestToolsCallStopBeaming(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	controller := &fakeBeamController{
		stopResult: &domain.StopResult{
			OK:               true,
			StoppedSessionID: "sess_123",
			DeviceID:         "dev_1",
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "stop_beaming",
			"arguments": map[string]any{
				"session_id": "sess_123",
			},
		},
	})

	srv := New(input, output, Config{BeamController: controller})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	if structured["stopped_session_id"].(string) != "sess_123" {
		t.Fatalf("unexpected stopped_session_id: %v", structured["stopped_session_id"])
	}
	if controller.stopReq.SessionID != "sess_123" {
		t.Fatalf("unexpected stop request session: %s", controller.stopReq.SessionID)
	}
}

func TestToolsCallStopBeamingJSONLine(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	controller := &fakeBeamController{
		stopResult: &domain.StopResult{
			OK:               true,
			StoppedSessionID: "sess_json_stop",
			DeviceID:         "dev_json_stop",
		},
	}

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      77,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "stop_beaming",
			"arguments": map[string]any{
				"session_id": "sess_json_stop",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := input.Write(append(payload, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}

	srv := New(input, output, Config{BeamController: controller})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d", len(lines))
	}
	resp := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["id"].(float64) != 77 {
		t.Fatalf("tools/call response id mismatch: %#v", resp["id"])
	}
	result := resp["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	if structured["stopped_session_id"].(string) != "sess_json_stop" {
		t.Fatalf("unexpected stopped_session_id: %v", structured["stopped_session_id"])
	}
}

func TestToolsCallStopBeamingInvalidParams(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "stop_beaming",
			"arguments": map[string]any{},
		},
	})

	srv := New(input, output, Config{BeamController: &fakeBeamController{}})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	errObj := responses[0]["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("expected -32602, got %v", errObj["code"])
	}
}

func TestToolsCallBeamMediaToolErrorIncludesDetails(t *testing.T) {
	input := bytes.NewBuffer(nil)
	output := bytes.NewBuffer(nil)
	controller := &fakeBeamController{
		beamErr: &domain.ToolError{
			Code:    "UNSUPPORTED_URL_PATTERN",
			Message: "localhost and loopback URL hosts are blocked by default",
			Limitations: []domain.Limitation{
				{Code: "URL_LOOPBACK_BLOCKED", Message: "blocked"},
			},
			SuggestedFixes: []string{"use a LAN URL"},
			Details: map[string]any{
				"host": "127.0.0.1",
			},
		},
	}

	writeRequest(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "beam_media",
			"arguments": map[string]any{
				"source":        "http://127.0.0.1/video.mp4",
				"target_device": "dev_1",
				"transcode":     "never",
			},
		},
	})

	srv := New(input, output, Config{BeamController: controller})
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run server: %v", err)
	}

	responses := readResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	result := responses[0]["result"].(map[string]any)
	if !result["isError"].(bool) {
		t.Fatal("expected isError=true")
	}
	structured := result["structuredContent"].(map[string]any)
	errObj := structured["error"].(map[string]any)
	if errObj["code"].(string) != "UNSUPPORTED_URL_PATTERN" {
		t.Fatalf("unexpected error code: %v", errObj["code"])
	}
	details, ok := errObj["details"].(map[string]any)
	if !ok {
		t.Fatal("expected details object")
	}
	if details["host"].(string) != "127.0.0.1" {
		t.Fatalf("unexpected host detail: %v", details["host"])
	}
}

func TestDecodeStrictRejectsTrailingJSON(t *testing.T) {
	var payload struct {
		Value string `json:"value"`
	}

	err := decodeStrict(json.RawMessage(`{"value":"ok"}{"value":"extra"}`), &payload)
	if err == nil {
		t.Fatal("expected error for trailing JSON payload")
	}
}

func writeRequest(t *testing.T, w io.Writer, req map[string]any) {
	t.Helper()

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	if _, err := w.Write([]byte("Content-Length: ")); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := w.Write([]byte(strconv.Itoa(len(payload)))); err != nil {
		t.Fatalf("write length: %v", err)
	}
	if _, err := w.Write([]byte("\r\n\r\n")); err != nil {
		t.Fatalf("write separator: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

func readResponses(t *testing.T, output []byte) []map[string]any {
	t.Helper()

	reader := bufio.NewReader(bytes.NewReader(output))
	var responses []map[string]any
	for {
		msg, _, err := readMessage(reader)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read response: %v", err)
		}

		resp := map[string]any{}
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		responses = append(responses, resp)
	}

	return responses
}
