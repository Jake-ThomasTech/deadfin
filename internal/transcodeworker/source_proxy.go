// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// sourceProxy serves one job's streamed source to the worker's ffmpeg on
// loopback (#616). It adds the job bearer token, so the token never appears
// in the ffmpeg argv or the worker's process list. The URL path is random
// and valid for this job only. A read that the server interrupts, as a
// server restart does, resumes at the next byte when the server answers
// again (#618), so ffmpeg never sees a short source.
type sourceProxy struct {
	config   sourceProxyConfig
	ctx      context.Context
	listener net.Listener
	server   *http.Server
	done     chan struct{}
	url      string
}

// sourceProxyConfig configures a source proxy.
type sourceProxyConfig struct {
	Client   *http.Client
	Upstream string
	Token    string
	Clock    clock.Clock
	// RetryDelay is the pause before a read is tried again.
	RetryDelay time.Duration
	Logger     *slog.Logger
}

// startSourceProxy listens on loopback and forwards range reads to
// the upstream with the token. Reads stop retrying when ctx ends. Close
// stops the proxy.
func startSourceProxy(ctx context.Context, config sourceProxyConfig) (*sourceProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret, err := randomID()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	path := "/" + secret + "/source"
	proxy := &sourceProxy{
		config:   config,
		ctx:      ctx,
		listener: listener,
		done:     make(chan struct{}),
		url:      "http://" + listener.Addr().String() + path,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, proxy.serve)
	proxy.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer close(proxy.done)
		if err := proxy.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return
		}
	}()
	return proxy, nil
}

// byteRange is a parsed "bytes=start-" or "bytes=start-end" header; end is
// -1 when open.
type byteRange struct {
	start, end int64
}

func parseByteRange(header string) (byteRange, bool) {
	if header == "" {
		return byteRange{end: -1}, true
	}
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return byteRange{}, false
	}
	first, last, found := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(first, 10, 64)
	if !found || err != nil || start < 0 {
		return byteRange{}, false
	}
	if last == "" {
		return byteRange{start: start, end: -1}, true
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < start {
		return byteRange{}, false
	}
	return byteRange{start: start, end: end}, true
}

func (r byteRange) header(offset int64) string {
	if r.end < 0 {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	return fmt.Sprintf("bytes=%d-%d", offset, r.end)
}

// retryableStatus is an answer of a restarting server or its proxy.
func retryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// clientWriter records a failed write to ffmpeg, which ends the read.
type clientWriter struct {
	writer io.Writer
	err    error
}

func (c *clientWriter) Write(data []byte) (int, error) {
	count, err := c.writer.Write(data)
	if err != nil {
		c.err = err
	}
	return count, err
}

func (proxy *sourceProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requested, resumable := parseByteRange(r.Header.Get("Range"))
	offset := requested.start
	headerSent := false
	client := &clientWriter{writer: w}
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !proxy.pause(r.Context()) {
			if !headerSent {
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
			}
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, proxy.config.Upstream, nil)
		if err != nil {
			http.Error(w, "bad upstream", http.StatusBadGateway)
			return
		}
		request.Header.Set("Authorization", "Bearer "+proxy.config.Token)
		switch {
		case headerSent || (resumable && r.Header.Get("Range") != ""):
			request.Header.Set("Range", requested.header(offset))
		case r.Header.Get("Range") != "":
			request.Header.Set("Range", r.Header.Get("Range"))
		}
		if value := r.Header.Get("If-Range"); value != "" && !headerSent {
			request.Header.Set("If-Range", value)
		}
		response, err := proxy.config.Client.Do(request)
		if err != nil {
			if !resumable {
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			continue
		}
		if retryableStatus(response.StatusCode) && resumable {
			_ = response.Body.Close()
			continue
		}
		if !headerSent {
			for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
				if value := response.Header.Get(name); value != "" {
					w.Header().Set(name, value)
				}
			}
			w.WriteHeader(response.StatusCode)
			headerSent = true
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
				_, _ = io.Copy(client, response.Body)
				_ = response.Body.Close()
				return
			}
		} else if response.StatusCode != http.StatusPartialContent {
			// The server cannot resume this read.
			_ = response.Body.Close()
			return
		}
		copied, err := io.Copy(client, response.Body)
		_ = response.Body.Close()
		offset += copied
		if err == nil || client.err != nil || !resumable || r.Method == http.MethodHead {
			return
		}
		proxy.config.Logger.Info("transcode worker source read interrupted; resuming", "offset", offset, "error", err.Error())
	}
}

// pause waits RetryDelay. It reports false when the read or the job ends.
func (proxy *sourceProxy) pause(request context.Context) bool {
	if proxy.config.Clock == nil {
		return false
	}
	timer := proxy.config.Clock.NewTimer(proxy.config.RetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C():
		return true
	case <-request.Done():
	case <-proxy.ctx.Done():
	}
	return false
}

// Close stops the proxy and waits for its serve loop.
func (proxy *sourceProxy) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.server.Shutdown(ctx); err != nil {
		_ = proxy.server.Close()
	}
	<-proxy.done
}
