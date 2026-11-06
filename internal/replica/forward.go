// SPDX-License-Identifier: GPL-2.0-only

package replica

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// Forward headers. A replica that forwards a request signs it with the
// replica secret; the receiver takes the client's transport peer and TLS
// flag from the signed headers.
const (
	HeaderFrom      = "Deadfin-Replica-From"
	HeaderPeer      = "Deadfin-Replica-Peer"
	HeaderTLS       = "Deadfin-Replica-Tls"
	HeaderTime      = "Deadfin-Replica-Time"
	HeaderSignature = "Deadfin-Replica-Signature"
	// HeaderScope "local" asks the receiver to serve the request itself,
	// with no routing to the owner (#663).
	HeaderScope = "Deadfin-Replica-Scope"
	// HeaderMoved marks a 421 answer: the receiver does not own the key
	// of the forwarded request.
	HeaderMoved = "Deadfin-Replica-Moved"

	// forwardSkew is the largest clock difference a signature accepts.
	forwardSkew = 30 * time.Second
	// maxReplayBody is the largest request body that a forward keeps, so
	// that it can send the request again after a 421.
	maxReplayBody = 1 << 20
)

var signedHeaders = []string{HeaderFrom, HeaderPeer, HeaderTLS, HeaderTime, HeaderSignature, HeaderScope}

// Signer signs and checks forwards with the replica secret.
type Signer struct {
	key   []byte
	clock clock.Clock
}

// NewSigner returns a signer with key.
func NewSigner(key []byte, c clock.Clock) (*Signer, error) {
	if len(key) < 16 || c == nil {
		return nil, errors.New("replica: the forward key must have at least 16 bytes, and the signer needs a clock")
	}
	return &Signer{key: append([]byte(nil), key...), clock: c}, nil
}

// Forwarded is the signed origin of a forwarded request.
type Forwarded struct {
	From string
	Peer string
	TLS  bool
	// Local is a request that the receiver serves itself: a part of a
	// cluster-wide answer, or a command for a session that it owns.
	Local bool
}

func (signer *Signer) signature(method, requestURI string, forwarded Forwarded, timestamp string) string {
	mac := hmac.New(sha256.New, signer.key)
	tlsFlag := "0"
	if forwarded.TLS {
		tlsFlag = "1"
	}
	parts := []string{"deadfin-replica-forward", method, requestURI, forwarded.From, forwarded.Peer, tlsFlag, timestamp}
	if forwarded.Local {
		// Only a local-scope forward signs the scope, so that a forward
		// without it keeps the signature of an older replica.
		parts = append(parts, "local")
	}
	for _, part := range parts {
		_, _ = io.WriteString(mac, part)
		_, _ = mac.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Sign sets the forward headers on header.
func (signer *Signer) Sign(header http.Header, method, requestURI string, forwarded Forwarded) {
	timestamp := strconv.FormatInt(signer.clock.Now().Unix(), 10)
	header.Set(HeaderFrom, forwarded.From)
	header.Set(HeaderPeer, forwarded.Peer)
	if forwarded.TLS {
		header.Set(HeaderTLS, "1")
	} else {
		header.Set(HeaderTLS, "0")
	}
	header.Set(HeaderTime, timestamp)
	if forwarded.Local {
		header.Set(HeaderScope, "local")
	} else {
		header.Del(HeaderScope)
	}
	header.Set(HeaderSignature, signer.signature(method, requestURI, forwarded, timestamp))
}

// Verify checks the forward headers of request.
func (signer *Signer) Verify(request *http.Request) (Forwarded, bool) {
	signature := request.Header.Get(HeaderSignature)
	if signature == "" {
		return Forwarded{}, false
	}
	forwarded := Forwarded{From: request.Header.Get(HeaderFrom), Peer: request.Header.Get(HeaderPeer), TLS: request.Header.Get(HeaderTLS) == "1",
		Local: request.Header.Get(HeaderScope) == "local"}
	timestamp := request.Header.Get(HeaderTime)
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || forwarded.From == "" || forwarded.Peer == "" {
		return Forwarded{}, false
	}
	skew := signer.clock.Now().Sub(time.Unix(seconds, 0))
	if skew > forwardSkew || skew < -forwardSkew {
		return Forwarded{}, false
	}
	want := signer.signature(request.Method, request.RequestURI, forwarded, timestamp)
	if !hmac.Equal([]byte(signature), []byte(want)) {
		return Forwarded{}, false
	}
	return forwarded, true
}

type forwardedKey struct{}

// ForwardedFrom returns the origin of a request that a peer forwarded.
func ForwardedFrom(ctx context.Context) (Forwarded, bool) {
	forwarded, ok := ctx.Value(forwardedKey{}).(Forwarded)
	return forwarded, ok
}

// Accept wraps the server's whole handler. A request with a valid forward
// signature gets the transport peer and the TLS flag of the client's first
// connection, and is marked as forwarded. The forward headers never reach
// next.
func (signer *Signer) Accept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get(HeaderSignature) == "" && request.Header.Get(HeaderFrom) == "" {
			next.ServeHTTP(w, request)
			return
		}
		forwarded, ok := signer.Verify(request)
		request = request.Clone(request.Context())
		for _, name := range signedHeaders {
			request.Header.Del(name)
		}
		if ok {
			request.RemoteAddr = forwarded.Peer
			if forwarded.TLS && request.TLS == nil {
				request.TLS = &tls.ConnectionState{}
			} else if !forwarded.TLS {
				request.TLS = nil
			}
			request = request.WithContext(context.WithValue(request.Context(), forwardedKey{}, forwarded))
		}
		next.ServeHTTP(w, request)
	})
}

// forwardOutcome is the result of one forward.
type forwardOutcome int

const (
	// forwardDone: the peer's answer reached the client.
	forwardDone forwardOutcome = iota
	// forwardMoved: the peer answered 421; nothing reached the client.
	forwardMoved
	// forwardFailed: the peer could not be reached; nothing reached the
	// client.
	forwardFailed
)

var errMoved = errors.New("replica: the peer does not own the request")

// forwarder sends a request to a peer and copies the answer back.
type forwarder struct {
	signer    *Signer
	from      string
	transport http.RoundTripper
}

// newTransport returns the transport between replicas.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0,
		ExpectContinueTimeout: time.Second,
	}
}

// replayableBody reads at most maxReplayBody bytes of the request body. It
// returns the body for each attempt and whether a second attempt can send
// the same body.
func replayableBody(request *http.Request) (func() io.ReadCloser, bool, error) {
	if request.Body == nil || request.Body == http.NoBody {
		return func() io.ReadCloser { return http.NoBody }, true, nil
	}
	buffer, err := io.ReadAll(io.LimitReader(request.Body, maxReplayBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(buffer) <= maxReplayBody {
		return func() io.ReadCloser { return io.NopCloser(bytes.NewReader(buffer)) }, true, nil
	}
	rest := request.Body
	used := false
	return func() io.ReadCloser {
		if used {
			return http.NoBody
		}
		used = true
		return struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buffer), rest), rest}
	}, false, nil
}

// forward sends request to peer. The answer reaches w only when the peer
// owns the request; otherwise forward writes nothing and reports why.
func (f *forwarder) forward(w http.ResponseWriter, request *http.Request, peerURL string, body io.ReadCloser) forwardOutcome {
	return f.forwardScoped(w, request, peerURL, body, false)
}

// forwardScoped is forward; local asks the peer to serve request itself.
func (f *forwarder) forwardScoped(w http.ResponseWriter, request *http.Request, peerURL string, body io.ReadCloser, local bool) forwardOutcome {
	target, err := url.Parse(peerURL)
	if err != nil || target.Host == "" {
		return forwardFailed
	}
	outcome := forwardDone
	original := request
	proxy := &httputil.ReverseProxy{
		Rewrite: func(proxied *httputil.ProxyRequest) {
			out := proxied.Out
			path, rawQuery, _ := strings.Cut(original.RequestURI, "?")
			out.URL = &url.URL{Scheme: target.Scheme, Host: target.Host, Path: strings.TrimSuffix(target.Path, "/") + path, RawQuery: rawQuery}
			if unescaped, err := url.PathUnescape(out.URL.Path); err == nil {
				out.URL.RawPath = out.URL.Path
				out.URL.Path = unescaped
			}
			out.Host = original.Host
			// The receiver walks the client's own X-Forwarded chain
			// from the signed transport peer.
			copyForwardedHeaders(out.Header, original.Header)
			f.signer.Sign(out.Header, original.Method, original.RequestURI, Forwarded{From: f.from, Peer: original.RemoteAddr, TLS: original.TLS != nil, Local: local})
		},
		Transport:     f.transport,
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode == http.StatusMisdirectedRequest && response.Header.Get(HeaderMoved) != "" {
				return errMoved
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			switch {
			case errors.Is(err, errMoved):
				outcome = forwardMoved
			case errors.Is(err, context.Canceled):
				// The client went away.
				outcome = forwardDone
			default:
				outcome = forwardFailed
			}
		},
	}
	attempt := request.Clone(request.Context())
	attempt.Body = body
	proxy.ServeHTTP(w, attempt)
	return outcome
}

// copyForwardedHeaders keeps the client's own X-Forwarded chain, which the
// receiver walks from the signed transport peer, and drops forward headers
// that the client sent.
func copyForwardedHeaders(out, original http.Header) {
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
		if values, ok := original[name]; ok {
			out[name] = append([]string(nil), values...)
		}
	}
	for _, name := range signedHeaders {
		out.Del(name)
	}
}

// writeMoved answers a forwarded request whose key this replica does not
// own.
func writeMoved(w http.ResponseWriter) {
	w.Header().Set(HeaderMoved, "1")
	http.Error(w, http.StatusText(http.StatusMisdirectedRequest), http.StatusMisdirectedRequest)
}

// writeUnavailable answers a request whose owner cannot be reached yet.
func writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}
