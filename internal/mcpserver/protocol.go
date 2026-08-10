package mcpserver

import (
	"bytes"
	"encoding/json"
)

// Protocol revisions this server speaks. The modern revision carries version,
// identity and capabilities as per-request `_meta`; the legacy revision
// establishes them once through the `initialize` handshake. Both are served
// concurrently: the era is selected per request, not per process.
const (
	protocolVersionModern = "2026-07-28"
	protocolVersionLegacy = "2024-11-05"
)

var supportedProtocolVersions = []string{protocolVersionModern, protocolVersionLegacy}

// metaServerInfo is the reserved `_meta` key carrying server identity on every
// result. The corresponding request-side keys are spelled out in the struct
// tags on requestMeta.
const metaServerInfo = "io.modelcontextprotocol/serverInfo"

const (
	// resultTypeComplete is the only result type this server produces: it never
	// returns `input_required`, having no sampling or elicitation needs.
	resultTypeComplete = "complete"

	// cacheScopePublic is correct for every cacheable result here: the tool list
	// is compiled in, identical for all callers, and stdio servers carry no
	// authorization context to vary it by.
	cacheScopePublic = "public"

	// staticListTTLMs is the freshness hint for the tool list and discovery
	// result. Both are static for the lifetime of the process.
	staticListTTLMs = 3600000
)

// errUnsupportedProtocolVersion is the code assigned by the 2026-07-28 schema.
const errUnsupportedProtocolVersion = -32022

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// unsupportedVersionData is the `data` payload of UnsupportedProtocolVersionError,
// telling the client which versions it can retry with.
type unsupportedVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// requestMeta holds the per-request protocol fields a modern client sends in
// `params._meta`. ClientInfo is deliberately absent: it is optional, purely
// informational, and the spec warns against letting it influence behavior.
type requestMeta struct {
	ProtocolVersion    *string         `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

func (m *requestMeta) isModern() bool {
	return m != nil && m.ProtocolVersion != nil
}

func (m *requestMeta) hasClientCapabilities() bool {
	if m == nil {
		return false
	}
	trimmed := bytes.TrimSpace(m.ClientCapabilities)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// decodeRequestMeta extracts `params._meta`. Absent params or an absent `_meta`
// yield a nil meta rather than an error: that is how a legacy request looks.
func decodeRequestMeta(params json.RawMessage) (*requestMeta, error) {
	if len(bytes.TrimSpace(params)) == 0 {
		return nil, nil
	}

	var envelope struct {
		Meta *requestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return nil, err
	}

	return envelope.Meta, nil
}

func isSupportedProtocolVersion(version string) bool {
	for _, supported := range supportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// initializeResult is the legacy handshake reply. It intentionally carries no
// `resultType` or `_meta` server identity: those belong to the modern revision,
// and a legacy client is served exactly the shape its own revision defines.
type initializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ServerInfo      map[string]string `json:"serverInfo"`
	Instructions    string            `json:"instructions,omitempty"`
}

type discoverResult struct {
	ResultType        string         `json:"resultType"`
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      map[string]any `json:"capabilities"`
	Instructions      string         `json:"instructions,omitempty"`
	TTLMs             int            `json:"ttlMs"`
	CacheScope        string         `json:"cacheScope"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

type toolsListResult struct {
	ResultType string         `json:"resultType"`
	Tools      []tool         `json:"tools"`
	TTLMs      int            `json:"ttlMs"`
	CacheScope string         `json:"cacheScope"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolCallResult struct {
	ResultType        string         `json:"resultType"`
	Content           []toolContent  `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
