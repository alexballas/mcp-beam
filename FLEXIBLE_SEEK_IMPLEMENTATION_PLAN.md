# Flexible Seek Implementation Plan

## Goal
Support flexible seek requests beyond absolute seconds, including:
- Absolute position (`position_seconds`)
- Percentage-based position (`position_percent`)
- Relative from end (`from_end_seconds`)
- Natural-language-like shortcuts in client prompts (for example, "middle of the movie", "10 seconds from the end") by mapping them to structured tool arguments.

Also expose and use total media duration when available, so relative seeks can be resolved reliably.

## Scope
- Extend `seek_beaming` tool input model and validation.
- Add duration-aware seek resolution in beam manager.
- Add optional duration reporting to tool responses.
- Update protocol handling for Chromecast and DLNA.
- Update tests and README.

Out of scope for this phase:
- Full natural-language parser inside server (LLM/client should continue mapping NL phrasing to structured fields).
- Seeking truly live streams with no known duration.

## Proposed API Changes

### 1. `seek_beaming` input schema
Allow exactly one of these targeting fields:
- `position_seconds` (integer, >= 0)
- `position_percent` (number, 0 to 100)
- `from_end_seconds` (integer, >= 0)

Session target remains:
- `session_id` (optional)
- `target_device` (optional)
- require at least one of `session_id` or `target_device`.

Validation rules:
- Exactly one seek mode field must be provided.
- For percent mode, clamp or reject outside [0, 100]. Prefer reject for deterministic behavior.
- For from-end mode, require known duration.

### 2. `seek_beaming` response
Extend `SeekResult` with:
- `requested_mode` (`absolute_seconds`, `percent`, `from_end_seconds`)
- `resolved_position_seconds` (int)
- `duration_seconds` (optional float)

Keep existing fields for compatibility (`ok`, `session_id`, `device_id`, `position_seconds`).
`position_seconds` can remain as alias to `resolved_position_seconds` during transition.

## Duration Strategy

### Chromecast
Use `castClient.GetStatus()`:
- Duration source: `status.Duration`
- Current state source: `status.PlayerState`
- If `Duration <= 0`, treat as unknown duration.

### DLNA
Use `dlnaPayload.GetPositionInfo()`:
- Index 0 is total duration clock string (e.g. `HH:MM:SS`)
- Convert with `utils.ClockTimeToSeconds`
- If parse fails or result <= 0, treat as unknown duration.

### Unknown Duration Handling
For `position_percent` and `from_end_seconds`, return structured tool error:
- Code: `SEEK_DURATION_UNKNOWN`
- Message: duration required for relative seek mode
- Suggested fixes:
  - use absolute `position_seconds`
  - wait until playback metadata is available

## Manager Design Changes

### 1. Request model
Add a richer seek request model in `internal/domain/beam.go`:
- Optional fields for each seek mode.

### 2. Resolution path
In `SeekBeaming`:
1. Resolve active session (`session_id` or `target_device`)
2. Validate seek mode exclusivity
3. Resolve target seconds:
   - absolute: direct
   - percent: `round(duration * percent / 100)`
   - from_end: `max(duration - from_end_seconds, 0)`
4. Execute seek call via protocol adapter
5. Update session observation state
6. Return structured result with resolved target and duration (if known)

### 3. Shared helper
Add helper function:
- `resolveSeekPosition(sess, req) (resolvedSeconds int, durationSeconds float64, mode string, err error)`

This keeps protocol-specific duration retrieval in one place.

## MCP Server Changes

### 1. Handler
Update `handleSeekBeamingCall` to decode new optional fields and enforce exclusivity.

### 2. Static tool schema
Update `seek_beaming` schema in `staticTools()` with three seek modes.
Document mode exclusivity in `description`.

### 3. Backward compatibility
Keep supporting current `position_seconds` requests without changes.

## Error Model Additions
Add new tool errors where needed:
- `SEEK_DURATION_UNKNOWN`
- `SEEK_MODE_INVALID` (none or multiple seek mode fields)
- `SEEK_POSITION_INVALID` (negative or out-of-range values)

## Testing Plan

### Unit tests: `internal/beam/manager_test.go`
Add coverage for:
- Absolute seek path unchanged (Chromecast + DLNA)
- Percent seek resolution with known duration
- From-end seek resolution with known duration
- Unknown duration error for percent/from-end
- Exactly-one-mode validation
- Edge bounds: 0%, 100%, from-end larger than duration

### Unit tests: `internal/mcpserver/server_test.go`
Add coverage for:
- Valid decode of each seek mode
- Invalid params for multi-mode payload
- Invalid params for missing mode
- Invalid params for out-of-range percent
- Response includes resolved fields

### Regression tests
Ensure existing `beam_media` and `stop_beaming` behavior remains unchanged.

## Documentation Updates
Update README:
- Tool list remains `seek_beaming`
- Add examples:
  - middle (`position_percent: 50`)
  - 10 seconds from end (`from_end_seconds: 10`)
  - exact second (`position_seconds: 120`)
- Add note that relative modes require known duration.

## Rollout Plan
1. Add domain models + manager resolution helper
2. Update adapters/interfaces if needed for duration retrieval consistency
3. Update MCP handler + schema
4. Add tests
5. Update README
6. Run `go test ./...`

## Risks and Mitigations
- Risk: some devices report bogus/zero duration.
  - Mitigation: strict duration validity checks and fallback to explicit error.
- Risk: ambiguity with multiple seek fields.
  - Mitigation: enforce exactly-one-mode in server and manager.
- Risk: breaking clients expecting only `position_seconds`.
  - Mitigation: keep `position_seconds` as supported input and response field.

## Acceptance Criteria
- Can seek using absolute seconds, percent, and from-end modes.
- "Seek to middle" is supported via `position_percent: 50`.
- "Seek to 10 seconds from end" is supported via `from_end_seconds: 10`.
- Relative seeks fail gracefully with `SEEK_DURATION_UNKNOWN` when duration is unavailable.
- All tests pass and README reflects new behavior.
