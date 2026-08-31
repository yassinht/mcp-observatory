package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// rpcResponse is a permissive view of a JSON-RPC reply. Fields the spec
// requires are optional here on purpose: a server that omits "jsonrpc" is
// non-conformant, and recording that is the job.
type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`

	raw       []byte // exact bytes this message arrived as
	sessionID string
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func initializeReq(version string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":` + jsonStr(version) + `,` +
		`"capabilities":{},` +
		`"clientInfo":{"name":"mcp-observatory","version":"0.1.0"}}}`)
}

func initializedNote() []byte {
	return []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func toolsListReqID(id int64, cursor string) []byte {
	params := `{}`
	if cursor != "" {
		params = `{"cursor":` + jsonStr(cursor) + `}`
	}
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":%s}`, id, params))
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// post sends one JSON-RPC request over streamable HTTP and returns the exact
// response bytes alongside the first message that carries an id.
func (p *Prober) post(ctx context.Context, endpoint, sessionID, version string, payload []byte) ([]byte, *rpcResponse, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Content-Type", "application/json")
	// Both forms must be advertised: the spec lets the server pick either.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	res, err := p.client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return body, nil, res.StatusCode, err
	}

	msgs := parseMaybeSSE(body, res.Header.Get("Content-Type"))
	sid := res.Header.Get("Mcp-Session-Id")
	if sid == "" {
		sid = sessionID
	}
	for _, m := range msgs {
		if m.ID != nil {
			m.sessionID = sid
			return body, m, res.StatusCode, nil
		}
	}
	return body, nil, res.StatusCode, nil
}

// parseMaybeSSE handles the two shapes a streamable-HTTP reply can take: a
// plain JSON body, or an SSE stream carrying one or more data: frames.
func parseMaybeSSE(body []byte, contentType string) []*rpcResponse {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		var out []*rpcResponse
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimRight(line, "\r")
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(line[5:])
			if data == "" {
				continue
			}
			var m rpcResponse
			if json.Unmarshal([]byte(data), &m) == nil {
				m.raw = []byte(data)
				out = append(out, &m)
			}
		}
		return out
	}
	var m rpcResponse
	if json.Unmarshal(body, &m) == nil {
		m.raw = body
		return []*rpcResponse{&m}
	}
	return nil
}

// ---------------------------------------------------------------- sse reader

type sseEvent struct {
	name string
	data string
}

// readSSE turns a live event stream into events. Per the SSE spec a single
// event may carry several data: lines, which are joined with newlines --
// getting this wrong truncates any JSON-RPC message a server chose to split.
func readSSE(body io.Reader, out chan<- sseEvent) {
	defer close(out)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxBody)

	name := ""
	var data []string
	flush := func() {
		if len(data) == 0 {
			name = ""
			return
		}
		out <- sseEvent{name: name, data: strings.Join(data, "\n")}
		name = ""
		data = nil
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line[5:], " "), " "))
		}
	}
	flush()
}

// ---------------------------------------------------------------- classify

func classifyNetErr(err error) Outcome {
	if err == nil {
		return OutcomeOK
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeTimeout
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return OutcomeTimeout
	}
	if strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout") {
		return OutcomeTimeout
	}
	return OutcomeUnreachable
}

// isVersionError guesses whether an initialize failure is a protocol-version
// disagreement rather than a real refusal, so the caller knows to retry with an
// older revision instead of writing the server off.
func isVersionError(e *rpcError) bool {
	m := strings.ToLower(e.Message + " " + string(e.Data))
	return strings.Contains(m, "protocol version") ||
		strings.Contains(m, "protocolversion") ||
		strings.Contains(m, "unsupported version") ||
		strings.Contains(m, "version mismatch")
}

func trim(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
