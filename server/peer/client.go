package peer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/pohcee/procy-server/server/dispatch"
)

// Client makes peer calls to device processes. It keeps one HTTP/2 connection pool per node.
type Client struct {
	auth *Auth

	mu      sync.Mutex
	clients map[string]*http.Client // by node id + address
}

// NewClient returns a peer client authenticating with auth.
func NewClient(auth *Auth) *Client {
	return &Client{auth: auth, clients: map[string]*http.Client{}}
}

// callTimeout bounds a unary peer call (the client's own answer is bounded by the local dispatcher's timeouts).
const callTimeout = 5 * time.Minute

func (c *Client) httpClient(loc *Location) *http.Client {
	key := loc.NodeID + "\x00" + loc.PeerAddress
	c.mu.Lock()
	defer c.mu.Unlock()
	if hc := c.clients[key]; hc != nil {
		return hc
	}
	var rt http.RoundTripper
	if c.auth.certs == nil {
		rt = &http.Transport{IdleConnTimeout: 90 * time.Second}
	} else {
		rt = &http2.Transport{TLSClientConfig: c.auth.clientTLS(loc.NodeID), ReadIdleTimeout: 30 * time.Second}
	}
	hc := &http.Client{Transport: rt}
	c.clients[key] = hc
	return hc
}

func (c *Client) post(ctx context.Context, loc *Location, path string, body io.Reader, header http.Header) (*http.Response, error) {
	if loc.PeerAddress == "" {
		return nil, fmt.Errorf("node %q holds the client but takes no peer calls (no [cluster] peer_address)", loc.NodeID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.auth.scheme()+"://"+loc.PeerAddress+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	c.auth.addSecret(req)
	resp, err := c.httpClient(loc).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("peer call to node %s: %w", loc.NodeID, err)
		}
		// the node holding the session is gone (its lease hasn't run out yet): for the caller the client is offline
		return nil, fmt.Errorf("%w: node %s holding it is unreachable: %v", dispatch.ErrClientNotConnected, loc.NodeID, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("peer call to node %s: %s: %s", loc.NodeID, resp.Status, bytes.TrimSpace(b))
	}
	return resp, nil
}

// call makes a unary call and decodes its result into out (when not nil).
func (c *Client) call(ctx context.Context, loc *Location, t target, op string, args, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req := callRequest{target: t, Op: op}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		req.Args = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, loc, pathCall, bytes.NewReader(b), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var cr callResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCallBody)).Decode(&cr); err != nil {
		return fmt.Errorf("peer call to node %s: bad answer: %w", loc.NodeID, err)
	}
	if cr.Error != nil {
		return cr.Error.err()
	}
	if out != nil && cr.Result != nil {
		return json.Unmarshal(cr.Result, out)
	}
	return nil
}

// jobStream is an open run-job answer.
type jobStream struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

func (s *jobStream) next() (*frame, error) {
	if !s.sc.Scan() {
		if err := s.sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	var f frame
	if err := json.Unmarshal(s.sc.Bytes(), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *jobStream) close() { s.body.Close() }

// maxFrame bounds one run-job frame (a job's whole result is one frame).
const maxFrame = 64 << 20

func (c *Client) runJob(ctx context.Context, loc *Location, req runJobRequest) (*jobStream, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, loc, pathRunJob, bytes.NewReader(b), nil)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxFrame)
	return &jobStream{body: resp.Body, sc: sc}, nil
}

func (c *Client) upload(ctx context.Context, loc *Location, req uploadRequest, file io.Reader) (*uploadResponse, error) {
	h, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set(headerUpload, string(h))
	header.Set("Content-Type", "application/octet-stream")
	resp, err := c.post(ctx, loc, pathUpload, file, header)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ur uploadResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCallBody)).Decode(&ur); err != nil {
		return nil, fmt.Errorf("peer upload to node %s: bad answer: %w", loc.NodeID, err)
	}
	return &ur, nil
}
