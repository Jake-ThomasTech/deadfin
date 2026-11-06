// SPDX-License-Identifier: GPL-2.0-only

package replica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/store"
)

// Peer paths (#663). Only a request with a valid forward signature reaches
// them; PeerHandler answers them.
const (
	PeerPrefix = "/deadfin/replica/"
	// PathEvents takes a batch of a peer's server events.
	PathEvents = PeerPrefix + "events"
	// PathSessions answers a peer's WebSocket Sessions feed with this
	// replica's own sessions.
	PathSessions = PeerPrefix + "sessions"
)

const (
	// maxPendingEvents bounds the events that wait for the peers; the
	// oldest go first.
	maxPendingEvents = 1024
	// peerCallTimeout bounds one call to a peer.
	peerCallTimeout = 5 * time.Second
	// maxPeerBody bounds a body between replicas.
	maxPeerBody = 16 << 20
)

// PeerEndpoints answers the peers.
type PeerEndpoints struct {
	// Events takes the server events of a peer.
	Events func(ctx context.Context, events []json.RawMessage)
	// Sessions answers a query of a peer's WebSocket Sessions feed with
	// this replica's own sessions.
	Sessions func(ctx context.Context, query json.RawMessage) (any, error)
}

// PeerHandler serves the peer paths and passes every other request to next.
// It runs inside Signer.Accept: a peer path without a valid forward
// signature gets 404.
func PeerHandler(endpoints PeerEndpoints, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, PeerPrefix) {
			next.ServeHTTP(w, request)
			return
		}
		if _, forwarded := ForwardedFrom(request.Context()); !forwarded || request.Method != http.MethodPost {
			http.NotFound(w, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, maxPeerBody))
		if err != nil {
			return
		}
		switch request.URL.Path {
		case PathEvents:
			var events []json.RawMessage
			if err := json.Unmarshal(body, &events); err != nil {
				http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
				return
			}
			if endpoints.Events != nil {
				endpoints.Events(request.Context(), events)
			}
			w.WriteHeader(http.StatusNoContent)
		case PathSessions:
			if endpoints.Sessions == nil {
				http.NotFound(w, request)
				return
			}
			answer, err := endpoints.Sessions(request.Context(), body)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			encoded, err := json.Marshal(answer)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(encoded)
		default:
			http.NotFound(w, request)
		}
	})
}

// ClusterConfig configures a Cluster.
type ClusterConfig struct {
	Registry *Registry
	Signer   *Signer
	// Transport carries the calls; a new transport when nil.
	Transport http.RoundTripper
	Logger    *slog.Logger
}

// Cluster makes the answers that span every replica (#663): it sends this
// replica's server events to the peers, and gathers the peers' parts of a
// session list. It owns one sender goroutine; Close stops it.
type Cluster struct {
	config ClusterConfig
	from   string
	notify chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	pending []json.RawMessage
	dropped int
	closed  bool
}

// NewCluster returns a cluster and starts its sender.
func NewCluster(config ClusterConfig) (*Cluster, error) {
	if config.Registry == nil || config.Signer == nil {
		return nil, errors.New("replica: the cluster needs a registry and a signer")
	}
	if config.Transport == nil {
		config.Transport = newTransport()
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cluster := &Cluster{
		config: config, from: config.Registry.ID(), notify: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}),
	}
	go cluster.run(ctx)
	return cluster, nil
}

// Close stops the sender; events that wait for the peers are dropped.
func (cluster *Cluster) Close() {
	cluster.mu.Lock()
	closed := cluster.closed
	cluster.closed = true
	cluster.pending = nil
	cluster.mu.Unlock()
	if closed {
		return
	}
	cluster.cancel()
	<-cluster.done
	if transport, ok := cluster.config.Transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

// Publish queues event for every live peer. It does not wait.
func (cluster *Cluster) Publish(event any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		cluster.config.Logger.Warn("encode a server event for the peers", "error", err.Error())
		return
	}
	cluster.mu.Lock()
	if cluster.closed {
		cluster.mu.Unlock()
		return
	}
	cluster.pending = append(cluster.pending, encoded)
	if len(cluster.pending) > maxPendingEvents {
		cluster.dropped += len(cluster.pending) - maxPendingEvents
		cluster.pending = append(cluster.pending[:0], cluster.pending[len(cluster.pending)-maxPendingEvents:]...)
	}
	cluster.mu.Unlock()
	select {
	case cluster.notify <- struct{}{}:
	default:
	}
}

func (cluster *Cluster) run(ctx context.Context) {
	defer close(cluster.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-cluster.notify:
		}
		cluster.mu.Lock()
		batch, dropped := cluster.pending, cluster.dropped
		cluster.pending, cluster.dropped = nil, 0
		cluster.mu.Unlock()
		if dropped > 0 {
			cluster.config.Logger.Warn("server events for the peers dropped", "count", dropped)
		}
		if len(batch) == 0 {
			continue
		}
		body, err := json.Marshal(batch)
		if err != nil {
			continue
		}
		cluster.eachPeer(ctx, func(ctx context.Context, peer store.Replica) {
			response, err := cluster.call(ctx, peer, PathEvents, body)
			if err != nil {
				cluster.config.Logger.Warn("send server events to a peer", "replica", peer.ID, "error", err.Error())
				return
			}
			drain(response)
		})
	}
}

// eachPeer runs call for every live peer at once and waits for them.
func (cluster *Cluster) eachPeer(ctx context.Context, call func(context.Context, store.Replica)) {
	var wait sync.WaitGroup
	for _, peer := range cluster.config.Registry.Peers() {
		wait.Add(1)
		go func() {
			defer wait.Done()
			callCtx, cancel := context.WithTimeout(ctx, peerCallTimeout)
			defer cancel()
			call(callCtx, peer)
		}()
	}
	wait.Wait()
}

// call posts body to path on peer.
func (cluster *Cluster) call(ctx context.Context, peer store.Replica, path string, body []byte) (*http.Response, error) {
	target, err := url.Parse(peer.URL)
	if err != nil {
		return nil, err
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	cluster.config.Signer.Sign(request.Header, http.MethodPost, target.RequestURI(), Forwarded{From: cluster.from, Peer: cluster.from, Local: true})
	response, err := cluster.config.Transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode/100 != 2 {
		drain(response)
		return nil, errors.New("replica: peer answered " + response.Status)
	}
	return response, nil
}

// PeerSessions posts query to every live peer's PathSessions and returns
// their answers. A peer that fails is left out.
func (cluster *Cluster) PeerSessions(ctx context.Context, query any) [][]byte {
	body, err := json.Marshal(query)
	if err != nil {
		return nil
	}
	return cluster.gather(ctx, func(ctx context.Context, peer store.Replica) (*http.Response, error) {
		return cluster.call(ctx, peer, PathSessions, body)
	})
}

// Gather sends request to every live peer, for the peer to serve itself,
// and returns the bodies of their 200 answers: each is that peer's part of
// a cluster-wide answer, such as GET /Sessions. A request that a peer sent
// here to serve gathers nothing.
func (cluster *Cluster) Gather(request *http.Request) [][]byte {
	if IsLocalForward(request) {
		return nil
	}
	return cluster.gather(request.Context(), func(ctx context.Context, peer store.Replica) (*http.Response, error) {
		target, err := url.Parse(peer.URL)
		if err != nil {
			return nil, err
		}
		path, rawQuery, _ := strings.Cut(request.RequestURI, "?")
		out, err := http.NewRequestWithContext(ctx, request.Method, target.Scheme+"://"+target.Host, http.NoBody)
		if err != nil {
			return nil, err
		}
		out.URL.Path = strings.TrimSuffix(target.Path, "/") + path
		out.URL.RawQuery = rawQuery
		if unescaped, err := url.PathUnescape(out.URL.Path); err == nil {
			out.URL.RawPath, out.URL.Path = out.URL.Path, unescaped
		}
		out.Host = request.Host
		for name, values := range request.Header {
			if !strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Forwarded-") {
				out.Header[name] = append([]string(nil), values...)
			}
		}
		for _, name := range []string{"Connection", "Upgrade", "Te", "Trailer", "Transfer-Encoding", "Keep-Alive", "Proxy-Connection"} {
			out.Header.Del(name)
		}
		copyForwardedHeaders(out.Header, request.Header)
		cluster.config.Signer.Sign(out.Header, request.Method, request.RequestURI, Forwarded{From: cluster.from, Peer: request.RemoteAddr, TLS: request.TLS != nil, Local: true})
		response, err := cluster.config.Transport.RoundTrip(out)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			drain(response)
			return nil, errors.New("replica: peer answered " + response.Status)
		}
		return response, nil
	})
}

func (cluster *Cluster) gather(ctx context.Context, call func(context.Context, store.Replica) (*http.Response, error)) [][]byte {
	var mu sync.Mutex
	var bodies [][]byte
	cluster.eachPeer(ctx, func(ctx context.Context, peer store.Replica) {
		response, err := call(ctx, peer)
		if err != nil {
			if ctx.Err() == nil {
				cluster.config.Logger.Warn("read a peer's part of a cluster answer", "replica", peer.ID, "error", err.Error())
			}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, maxPeerBody))
		if err != nil {
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
	})
	return bodies
}

func drain(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxPeerBody))
	_ = response.Body.Close()
}
