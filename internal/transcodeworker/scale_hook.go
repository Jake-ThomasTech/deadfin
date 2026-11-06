// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Scale hook (#623, docs/spikes/transcode-workers.md §3.9): the server asks
// an outside scaler for capacity. It POSTs the signals of one pool when the
// pool's slotsWanted changes, at most once per interval per pool, and at
// once when a job queues or a playback is about to start a transcode.

// ScaleHookSignatureHeader carries "sha256=" and the hex HMAC-SHA256 of the
// request body with the hook secret.
const ScaleHookSignatureHeader = "Deadfin-Signature"

// DefaultScaleHookInterval is the shortest time between two calls for one
// pool, except for a call at once.
const DefaultScaleHookInterval = time.Second

// DefaultWarmupTTL is how long a PlaybackInfo transcode decision counts in
// slotsWanted while no job starts for its play session.
const DefaultWarmupTTL = time.Minute

const scaleHookTimeout = 5 * time.Second

// Scale hook reasons.
const (
	ScaleReasonChanged = "changed"
	ScaleReasonUrgent  = "queued"
)

// ScaleHook is the optional scale hook. An empty URL turns it off.
type ScaleHook struct {
	URL    string
	Secret []byte
	// Client sends the calls; nil uses a client with a 5 s timeout.
	Client *http.Client
	// Interval is the per-pool debounce; DefaultScaleHookInterval when
	// zero.
	Interval time.Duration
}

// ScaleHookCall is the JSON body of one scale hook call.
type ScaleHookCall struct {
	Pool    string      `json:"pool"`
	Reason  string      `json:"reason"`
	Signals PoolSignals `json:"signals"`
}

// SignScaleHook returns the signature header value of body.
func SignScaleHook(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// wakeScaleHook asks the hook loop for a call at once. It never blocks.
func (hub *Hub) wakeScaleHook() {
	if hub.hookWake == nil {
		return
	}
	select {
	case hub.hookWake <- struct{}{}:
	default:
	}
}

// scaleHookLoop is the hook goroutine. The hub owns it; Close stops it.
func (hub *Hub) scaleHookLoop() {
	defer close(hub.hookDone)
	hook := hub.config.ScaleHook
	interval := hook.Interval
	if interval <= 0 {
		interval = DefaultScaleHookInterval
	}
	client := hook.Client
	if client == nil {
		client = &http.Client{Timeout: scaleHookTimeout}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-hub.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	sent := make(map[string]int)
	sentAt := make(map[string]time.Time)
	urgent := true
	for {
		scaling := hub.Scaling()
		now := hub.config.Clock.Now()
		names := make([]string, 0, len(scaling.Pools))
		for name := range scaling.Pools {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			signals := scaling.Pools[name]
			previous, known := sent[name]
			if known && previous == signals.SlotsWanted {
				continue
			}
			reason := ScaleReasonChanged
			if urgent && known && signals.SlotsWanted > previous {
				reason = ScaleReasonUrgent
			} else if known && now.Sub(sentAt[name]) < interval {
				continue
			}
			if err := hub.callScaleHook(ctx, client, ScaleHookCall{Pool: name, Reason: reason, Signals: signals}); err != nil {
				if ctx.Err() != nil {
					return
				}
				hub.logger().Warn("transcode scale hook call failed", "pool", name, "error", err.Error())
				continue
			}
			sent[name], sentAt[name] = signals.SlotsWanted, now
		}
		timer := hub.config.Clock.NewTimer(interval)
		hub.observe(Event{Kind: EventScaleHookWait})
		select {
		case <-hub.stop:
			timer.Stop()
			return
		case <-hub.hookWake:
			timer.Stop()
			urgent = true
		case <-timer.C():
			urgent = false
		}
	}
}

func (hub *Hub) callScaleHook(ctx context.Context, client *http.Client, call ScaleHookCall) error {
	body, err := json.Marshal(call)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, scaleHookTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, hub.config.ScaleHook.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(ScaleHookSignatureHeader, SignScaleHook(hub.config.ScaleHook.Secret, body))
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("receiver answered %s", response.Status)
	}
	hub.observe(Event{Kind: EventScaleHook, Detail: call.Pool + ":" + call.Reason})
	return nil
}

// Anticipate counts a transcode that a PlaybackInfo response announced for
// playSessionID in slotsWanted, until its job starts or the warm-up TTL
// ends (#623). Placement mode local counts nothing.
func (hub *Hub) Anticipate(playSessionID string) {
	if playSessionID == "" {
		return
	}
	hub.mu.Lock()
	if hub.closed || hub.policy.Mode == ModeLocal || hub.policy.Mode == "" {
		hub.mu.Unlock()
		return
	}
	ttl := hub.config.WarmupTTL
	if ttl <= 0 {
		ttl = DefaultWarmupTTL
	}
	_, known := hub.warmups[playSessionID]
	hub.warmups[playSessionID] = hub.config.Clock.Now().Add(ttl)
	hub.mu.Unlock()
	if !known {
		hub.wakeScaleHook()
	}
}

// warmingLocked counts the live warm-ups and removes the expired ones.
func (hub *Hub) warmingLocked(now time.Time) int {
	for id, expires := range hub.warmups {
		if !now.Before(expires) {
			delete(hub.warmups, id)
		}
	}
	return len(hub.warmups)
}
