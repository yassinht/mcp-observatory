// Package probe connects to one MCP endpoint and records what it served.
//
// It deliberately does NOT use an MCP client SDK. An SDK is built to talk to
// well-behaved servers: it validates, normalizes, and rejects. An observatory
// needs the opposite -- a server that returns a tool with no name, a duplicate
// name, or a 40KB description is producing exactly the observation worth
// keeping, and a conformant client would throw it away or error out. So the
// transport is hand-rolled JSON-RPC and every response body is retained as
// received bytes.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxBody caps a single response so one hostile endpoint cannot fill the disk.
	maxBody = 8 << 20
	// maxPages caps tools/list pagination so a server cannot loop us forever.
	maxPages = 50
)

type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeAuth        Outcome = "auth_required"
	OutcomeRPCError    Outcome = "rpc_error"
	OutcomeUnreachable Outcome = "unreachable"
	OutcomeTimeout     Outcome = "timeout"
	OutcomeProtocol    Outcome = "protocol_error"
)

// Result is what one probe attempt learned. RawPages holds the exact response
// bytes; everything else is a convenience view derived from them.
type Result struct {
	Transport       string
	Handshake       string
	HTTPStatus      int
	Outcome         Outcome
	FailReason      string
	ProtocolVersion string
	ServerInfo      json.RawMessage
	InitRaw         []byte
	RawPages        [][]byte
	ToolPages       [][]json.RawMessage
	PageCount       int
	Truncated       bool // hit maxPages with a cursor still outstanding
	Elapsed         time.Duration
}

type Config struct {
	UserAgent string
	Timeout   time.Duration
	// ProtocolVersions are tried in order until one is accepted. Hardcoding a
	// single version silently biased the week-0 sample against servers on a
	// different spec revision.
	ProtocolVersions []string
}

func DefaultConfig(ua string) Config {
	return Config{
		UserAgent: ua,
		Timeout:   20 * time.Second,
		ProtocolVersions: []string{
			"2025-06-18",
			"2025-03-26",
			"2024-11-05",
		},
	}
}

type Prober struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Prober {
	return &Prober{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
			// Cap redirects; an endpoint that bounces us more than a few times is
			// not serving MCP.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
	}
}

// Probe tries the transport the registry declared first, then the other one.
// Which transport actually worked is itself data worth recording: the registry's
// own declaration is frequently wrong.
func (p *Prober) Probe(ctx context.Context, endpoint, declaredType string) *Result {
	start := time.Now()
	order := []string{"streamable-http", "sse"}
	if strings.EqualFold(declaredType, "sse") {
		order = []string{"sse", "streamable-http"}
	}

	var first *Result
	for _, transport := range order {
		var r *Result
		if transport == "sse" {
			r = p.probeSSE(ctx, endpoint)
		} else {
			r = p.probeStreamable(ctx, endpoint)
		}
		r.Transport = transport
		if r.Outcome == OutcomeOK || r.Outcome == OutcomeAuth {
			r.Elapsed = time.Since(start)
			return r
		}
		if first == nil {
			first = r
		}
		// Only fall back to the other transport when the endpoint answered and we
		// disagreed about the protocol. If it did not answer at all -- no TCP, no
		// TLS, no response before the deadline -- a different MCP transport will
		// not change that, and trying costs another full timeout.
		//
		// This matters more than it sounds. Dead endpoints were costing 60s each
		// (20s streamable + 40s SSE) and, because each host is probed serially,
		// a handful of dead hosts stretched the tail of a census by two hours.
		if r.Outcome == OutcomeTimeout || r.Outcome == OutcomeUnreachable {
			break
		}
	}
	first.Elapsed = time.Since(start)
	return first
}

// ---------------------------------------------------------------- streamable

func (p *Prober) probeStreamable(ctx context.Context, endpoint string) *Result {
	r := &Result{Outcome: OutcomeUnreachable}

	var sessionID string
	var accepted string

	// Version negotiation: try each until one does not fail on version grounds.
	var initBody []byte
	var initResp *rpcResponse
	for _, ver := range p.cfg.ProtocolVersions {
		body, resp, status, err := p.post(ctx, endpoint, sessionID, ver, initializeReq(ver))
		r.HTTPStatus = status
		if err != nil {
			r.Outcome = classifyNetErr(err)
			r.FailReason = trim(err.Error())
			return r
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			r.Outcome = OutcomeAuth
			r.FailReason = fmt.Sprintf("http %d", status)
			return r
		}
		initBody = body
		if resp == nil {
			r.Outcome = OutcomeProtocol
			r.FailReason = fmt.Sprintf("no jsonrpc response to initialize (http %d)", status)
			continue
		}
		if resp.Error != nil {
			// Only keep retrying if it smells like a version disagreement.
			if isVersionError(resp.Error) {
				continue
			}
			r.Outcome = OutcomeRPCError
			r.FailReason = fmt.Sprintf("initialize rpc error %d: %s", resp.Error.Code, trim(resp.Error.Message))
			return r
		}
		initResp, accepted = resp, ver
		sessionID = resp.sessionID
		break
	}
	r.InitRaw = initBody
	if initResp == nil {
		if r.Outcome == OutcomeUnreachable {
			r.Outcome = OutcomeProtocol
		}
		if r.FailReason == "" {
			r.FailReason = "initialize rejected at every protocol version"
		}
		return r
	}

	// The server reports the version it actually chose; record that, not ours.
	var initResult struct {
		ProtocolVersion string          `json:"protocolVersion"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	_ = json.Unmarshal(initResp.Result, &initResult)
	r.ProtocolVersion = initResult.ProtocolVersion
	if r.ProtocolVersion == "" {
		r.ProtocolVersion = accepted
	}
	r.ServerInfo = initResult.ServerInfo
	r.Handshake = "initialize"

	// Notification; failure here is not fatal for enumeration.
	_, _, _, _ = p.post(ctx, endpoint, sessionID, r.ProtocolVersion, initializedNote())

	// tools/list, following nextCursor. The week-0 probe read only the first
	// page, which understates any server that paginates.
	cursor := ""
	var id int64 = 2
	for page := 0; page < maxPages; page++ {
		body, resp, status, err := p.post(ctx, endpoint, sessionID, r.ProtocolVersion, toolsListReqID(id, cursor))
		r.HTTPStatus = status
		if err != nil {
			r.Outcome = classifyNetErr(err)
			r.FailReason = trim(err.Error())
			return r
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			r.Outcome = OutcomeAuth
			r.FailReason = fmt.Sprintf("tools/list http %d", status)
			return r
		}
		r.RawPages = append(r.RawPages, body)
		if resp == nil {
			r.Outcome = OutcomeProtocol
			r.FailReason = fmt.Sprintf("no jsonrpc response to tools/list (http %d)", status)
			return r
		}
		if resp.Error != nil {
			r.Outcome = OutcomeRPCError
			r.FailReason = fmt.Sprintf("tools/list rpc error %d: %s", resp.Error.Code, trim(resp.Error.Message))
			return r
		}
		var lr struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp.Result, &lr); err != nil {
			r.Outcome = OutcomeProtocol
			r.FailReason = "tools/list result is not an object with tools[]"
			return r
		}
		r.ToolPages = append(r.ToolPages, lr.Tools)
		r.PageCount = page + 1
		if lr.NextCursor == "" {
			r.Outcome = OutcomeOK
			return r
		}
		if lr.NextCursor == cursor {
			// A server repeating its cursor would loop us forever.
			r.Outcome = OutcomeProtocol
			r.FailReason = "tools/list returned a repeating cursor"
			return r
		}
		cursor = lr.NextCursor
		id++
	}
	r.Truncated = true
	r.Outcome = OutcomeOK
	return r
}

// ---------------------------------------------------------------- legacy SSE

// probeSSE speaks the pre-streamable transport:
//
//	GET  <url>            -> event stream
//	<-   event: endpoint  -> names a POST channel
//	POST <endpoint> ...   -> replies arrive back on the open stream
//
// Omitting this transport in week 0 cost ten percentage points of measured
// reachability and would have produced a false "do not build" verdict.
func (p *Prober) probeSSE(ctx context.Context, endpoint string) *Result {
	r := &Result{Outcome: OutcomeUnreachable}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout*2)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		r.FailReason = trim(err.Error())
		return r
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "text/event-stream")

	// A client without the global timeout: the stream stays open by design, so
	// http.Client.Timeout would kill it mid-handshake. The context still bounds it.
	streamClient := &http.Client{Transport: p.client.Transport}
	res, err := streamClient.Do(req)
	if err != nil {
		r.Outcome = classifyNetErr(err)
		r.FailReason = trim(err.Error())
		return r
	}
	defer res.Body.Close()
	r.HTTPStatus = res.StatusCode

	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		r.Outcome = OutcomeAuth
		r.FailReason = fmt.Sprintf("http %d", res.StatusCode)
		return r
	}
	if res.StatusCode != http.StatusOK {
		r.FailReason = fmt.Sprintf("sse open failed: http %d", res.StatusCode)
		return r
	}

	events := make(chan sseEvent, 64)
	go readSSE(res.Body, events)

	postURL := ""
	inbox := map[int64]*rpcResponse{}

	// waitFor pumps the stream until pred is satisfied or the context expires.
	waitFor := func(pred func() bool) bool {
		if pred() {
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return false
			case ev, ok := <-events:
				if !ok {
					return false
				}
				if ev.name == "endpoint" {
					if u, uerr := url.Parse(strings.TrimSpace(ev.data)); uerr == nil {
						if base, berr := url.Parse(endpoint); berr == nil {
							postURL = base.ResolveReference(u).String()
						}
					}
				} else {
					var m rpcResponse
					if json.Unmarshal([]byte(ev.data), &m) == nil && m.ID != nil {
						m.raw = []byte(ev.data)
						inbox[*m.ID] = &m
					}
				}
				if pred() {
					return true
				}
			}
		}
	}

	if !waitFor(func() bool { return postURL != "" }) {
		r.Outcome = OutcomeProtocol
		r.FailReason = "no endpoint event on sse stream"
		return r
	}

	postOnly := func(payload []byte) error {
		rq, rerr := http.NewRequestWithContext(ctx, http.MethodPost, postURL, bytes.NewReader(payload))
		if rerr != nil {
			return rerr
		}
		rq.Header.Set("User-Agent", p.cfg.UserAgent)
		rq.Header.Set("Content-Type", "application/json")
		rs, derr := p.client.Do(rq)
		if derr != nil {
			return derr
		}
		defer rs.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(rs.Body, maxBody))
		r.HTTPStatus = rs.StatusCode
		return nil
	}

	if err := postOnly(initializeReq("2024-11-05")); err != nil {
		r.Outcome = classifyNetErr(err)
		r.FailReason = trim(err.Error())
		return r
	}
	if !waitFor(func() bool { _, ok := inbox[1]; return ok }) {
		r.Outcome = OutcomeProtocol
		r.FailReason = "no initialize response on sse stream"
		return r
	}
	if e := inbox[1].Error; e != nil {
		r.Outcome = OutcomeRPCError
		r.FailReason = fmt.Sprintf("initialize rpc error %d: %s", e.Code, trim(e.Message))
		return r
	}
	r.Handshake = "sse+initialize"
	r.InitRaw = inbox[1].raw
	var ir struct {
		ProtocolVersion string          `json:"protocolVersion"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	_ = json.Unmarshal(inbox[1].Result, &ir)
	r.ProtocolVersion, r.ServerInfo = ir.ProtocolVersion, ir.ServerInfo

	_ = postOnly(initializedNote())

	cursor := ""
	var id int64 = 2
	for page := 0; page < maxPages; page++ {
		if err := postOnly(toolsListReqID(id, cursor)); err != nil {
			r.Outcome = classifyNetErr(err)
			r.FailReason = trim(err.Error())
			return r
		}
		wanted := id
		if !waitFor(func() bool { _, ok := inbox[wanted]; return ok }) {
			r.Outcome = OutcomeProtocol
			r.FailReason = "no tools/list response on sse stream"
			return r
		}
		msg := inbox[wanted]
		r.RawPages = append(r.RawPages, msg.raw)
		if msg.Error != nil {
			r.Outcome = OutcomeRPCError
			r.FailReason = fmt.Sprintf("tools/list rpc error %d: %s", msg.Error.Code, trim(msg.Error.Message))
			return r
		}
		var lr struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(msg.Result, &lr); err != nil {
			r.Outcome = OutcomeProtocol
			r.FailReason = "tools/list result is not an object with tools[]"
			return r
		}
		r.ToolPages = append(r.ToolPages, lr.Tools)
		r.PageCount = page + 1
		if lr.NextCursor == "" || lr.NextCursor == cursor {
			r.Outcome = OutcomeOK
			return r
		}
		cursor = lr.NextCursor
		id++
	}
	r.Truncated = true
	r.Outcome = OutcomeOK
	return r
}
