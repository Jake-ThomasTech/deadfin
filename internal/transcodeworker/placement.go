// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Placement (#616, docs/spikes/transcode-workers.md §3.8): a server-wide
// mode, ordered rules over worker pools, a queue and one fallback chain.

// Mode is the server-wide placement mode.
type Mode string

// The placement modes. ModeLocal is the default.
const (
	ModeLocal        Mode = "local"
	ModeLocalFirst   Mode = "local-first"
	ModeWorkersFirst Mode = "workers-first"
	ModeWorkersOnly  Mode = "workers-only"
)

// Strategy chooses among the workers that fit.
type Strategy string

// The strategies. StrategyPack is the default.
const (
	// StrategyPack fills the oldest worker first, so idle workers can
	// scale away.
	StrategyPack Strategy = "pack"
	// StrategySpread takes the least loaded worker.
	StrategySpread Strategy = "spread"
)

// DefaultQueueTimeout is how long a workers-only job waits for a worker.
const DefaultQueueTimeout = 10 * time.Second

// JobFacts are the playback facts that rules match.
type JobFacts = ffmpeg.JobFacts

// Policy is the operator's placement configuration.
type Policy struct {
	Mode         Mode     `json:"mode"`
	DefaultPools []string `json:"defaultPools,omitempty"`
	// LocalLimit is the local-first spill point; 0 never spills.
	LocalLimit int `json:"localLimit,omitempty"`
	// QueueTimeout is the workers-only wait; nil is DefaultQueueTimeout.
	QueueTimeout *time.Duration `json:"queueTimeout,omitempty"`
	// CPUFallback enables the local software encode at the end of the
	// chain; nil is on.
	CPUFallback *bool    `json:"cpuFallback,omitempty"`
	Strategy    Strategy `json:"strategy,omitempty"`
	Rules       []Rule   `json:"rules,omitempty"`
	// WarmSlots is the free capacity per pool that the scaling signal asks
	// for beyond the demand (#617); a pool without an entry gets
	// DefaultWarmSlots. AllPools names the whole fleet.
	WarmSlots map[string]int `json:"warmSlots,omitempty"`
}

// DefaultWarmSlots is the warm capacity of a pool without a WarmSlots entry.
const DefaultWarmSlots = 1

// AllPools is the scaling-signal name of the whole fleet.
const AllPools = "*"

// warmSlots returns the warm capacity of pool.
func (policy Policy) warmSlots(pool string) int {
	if value, ok := policy.WarmSlots[pool]; ok {
		return value
	}
	return DefaultWarmSlots
}

// Rule is one ordered placement rule. The first match wins.
type Rule struct {
	Name  string `json:"name,omitempty"`
	Match Match  `json:"match"`
	// Pools sends a match to a worker in any of these pools. Local runs
	// it on the server instead.
	Pools []string `json:"pools,omitempty"`
	Local bool     `json:"local,omitempty"`
	// QueueTimeout and CPUFallback override the policy values.
	QueueTimeout *time.Duration `json:"queueTimeout,omitempty"`
	CPUFallback  *bool          `json:"cpuFallback,omitempty"`
}

// Match selects jobs. Every set field must match (AND); a list matches any
// of its values (OR); an unset field matches anything.
type Match struct {
	SourceCodec   []string `json:"sourceCodec,omitempty"`
	SourceProfile []string `json:"sourceProfile,omitempty"`
	// BitDepthMin and BitDepthMax bound the source bit depth; 0 is unset.
	BitDepthMin  int      `json:"bitDepthMin,omitempty"`
	BitDepthMax  int      `json:"bitDepthMax,omitempty"`
	HDR          []string `json:"hdr,omitempty"`
	NeedsToneMap *bool    `json:"needsToneMap,omitempty"`
	TargetCodec  []string `json:"targetCodec,omitempty"`
}

// WorkerSnapshot is one worker as placement sees it for one job.
type WorkerSnapshot struct {
	ID    string
	Pools []string
	// Fits reports that the worker has the capabilities the argv needs.
	Fits bool
	// Tier is the hardware tier of the worker's argv (#622, #854): 2 for
	// a hardware decode and encode, 1 for one of them, 0 for software.
	// Placement prefers a higher tier.
	Tier int
	// InputMode is shared, stream or auto. Shared reports that a path map
	// of the worker covers the source.
	InputMode  string
	Shared     bool
	ActiveJobs int
	MaxJobs    int
	// Since is the registration time; pack fills the oldest worker first.
	Since time.Time
	// Closed reports a draining or quarantined worker: it takes no job
	// (#617).
	Closed bool
}

// LocalState is the server side of a placement.
type LocalState struct {
	// ActiveJobs is the number of ffmpeg families running on the server.
	ActiveJobs int
	// GPU reports that the job's argv uses a server hardware family.
	GPU bool
	// Tier is the hardware tier of the server's argv (#854).
	Tier int
	// RemoteOnly marks a job that the server cannot encode (#854): it
	// goes to a worker or waits for one, and never runs locally.
	RemoteOnly bool
}

// DecisionKind is the outcome of Place.
type DecisionKind int

// The decisions.
const (
	DecideLocalCPU DecisionKind = iota
	DecideLocalGPU
	DecideWorker
	DecideQueue
	DecideRefuse
)

func (kind DecisionKind) String() string {
	switch kind {
	case DecideLocalGPU:
		return "local_gpu"
	case DecideWorker:
		return "worker"
	case DecideQueue:
		return "queue"
	case DecideRefuse:
		return "refused"
	}
	return "local_cpu"
}

// Fallback reasons.
const (
	ReasonNoWorker     = "no-worker"
	ReasonPoolEmpty    = "pool-empty"
	ReasonQueueTimeout = "queue-timeout"
)

// Decision is where a job runs.
type Decision struct {
	Kind     DecisionKind
	WorkerID string
	// Rule names the matched rule; empty for the mode default.
	Rule  string
	Pools []string
	// Reason is set on a fallback: no-worker, pool-empty or queue-timeout.
	Reason string
	// Fallback reports a chain step taken because no worker fit, as
	// opposed to a job that the policy keeps local.
	Fallback bool
	// QueueTimeout and Then describe a queue: wait up to QueueTimeout for
	// a worker of Pools, then take Then.
	QueueTimeout time.Duration
	Then         *Decision
	// Tier is the hardware tier of the argv where the job runs (#854).
	Tier int
}

// Place decides where one job runs. It is pure: no store, clock or
// network.
func Place(facts JobFacts, policy Policy, fleet []WorkerSnapshot, local LocalState) Decision {
	runLocal := func(rule string) Decision {
		if local.GPU {
			return Decision{Kind: DecideLocalGPU, Rule: rule, Tier: local.Tier}
		}
		return Decision{Kind: DecideLocalCPU, Rule: rule, Tier: local.Tier}
	}
	mode := policy.Mode
	if local.RemoteOnly {
		// The server cannot encode the job: every mode waits for a
		// worker, as workers-only does (#854).
		mode = ModeWorkersOnly
	}
	switch mode {
	case ModeLocalFirst:
		if policy.LocalLimit <= 0 || local.ActiveJobs < policy.LocalLimit {
			return runLocal("")
		}
	case ModeWorkersFirst, ModeWorkersOnly:
	default:
		return runLocal("")
	}

	pools := policy.DefaultPools
	cpu := (policy.CPUFallback == nil || *policy.CPUFallback) && !local.RemoteOnly
	var queue time.Duration
	if mode == ModeWorkersOnly {
		queue = DefaultQueueTimeout
		if policy.QueueTimeout != nil {
			queue = *policy.QueueTimeout
		}
	}
	ruleName := ""
	if rule, ok := matchRule(facts, policy.Rules, local.RemoteOnly); ok {
		ruleName = rule.Name
		if rule.CPUFallback != nil && !local.RemoteOnly {
			cpu = *rule.CPUFallback
		}
		if rule.Local {
			decision := runLocal(ruleName)
			if !local.GPU && !cpu {
				decision = Decision{Kind: DecideRefuse, Rule: ruleName}
			}
			return decision
		}
		if len(rule.Pools) > 0 {
			pools = rule.Pools
		}
		if rule.QueueTimeout != nil {
			queue = *rule.QueueTimeout
		}
	}
	chain := func(reason string) Decision {
		decision := Decision{Kind: DecideRefuse, Rule: ruleName, Pools: pools, Reason: reason, Fallback: true}
		switch {
		case local.RemoteOnly:
		case local.GPU:
			decision.Kind, decision.Tier = DecideLocalGPU, local.Tier
		case cpu:
			decision.Kind, decision.Tier = DecideLocalCPU, local.Tier
		}
		return decision
	}

	// The tier orders the workers inside the pools that the mode and the
	// rules chose (#854): a worker that runs more of the job in hardware
	// wins, then the strategy breaks the tie.
	var best *WorkerSnapshot
	inPool := false
	for index := range fleet {
		worker := &fleet[index]
		if !inPools(worker.Pools, pools) {
			continue
		}
		inPool = true
		if worker.Closed || !worker.Fits || worker.ActiveJobs >= worker.MaxJobs || (worker.InputMode == store.TranscodeWorkerInputShared && !worker.Shared) {
			continue
		}
		if best == nil || worker.Tier > best.Tier || worker.Tier == best.Tier && better(policy.Strategy, worker, best) {
			best = worker
		}
	}
	if best != nil {
		return Decision{Kind: DecideWorker, WorkerID: best.ID, Rule: ruleName, Pools: pools, Tier: best.Tier}
	}
	if queue > 0 {
		then := chain(ReasonQueueTimeout)
		return Decision{Kind: DecideQueue, Rule: ruleName, Pools: pools, QueueTimeout: queue, Then: &then}
	}
	if inPool {
		return chain(ReasonNoWorker)
	}
	return chain(ReasonPoolEmpty)
}

// matchRule returns the first rule that matches facts. A remote-only job
// skips the local rules: the server cannot encode it (#854).
func matchRule(facts JobFacts, rules []Rule, remoteOnly bool) (Rule, bool) {
	for _, rule := range rules {
		if remoteOnly && rule.Local {
			continue
		}
		if rule.Match.matches(facts) {
			return rule, true
		}
	}
	return Rule{}, false
}

func (match Match) matches(facts JobFacts) bool {
	switch {
	case len(match.SourceCodec) > 0 && !contains(match.SourceCodec, facts.SourceCodec):
		return false
	case len(match.SourceProfile) > 0 && !contains(match.SourceProfile, facts.SourceProfile):
		return false
	case match.BitDepthMin > 0 && facts.BitDepth < match.BitDepthMin:
		return false
	case match.BitDepthMax > 0 && facts.BitDepth > match.BitDepthMax:
		return false
	case len(match.HDR) > 0 && !contains(match.HDR, facts.HDR):
		return false
	case match.NeedsToneMap != nil && *match.NeedsToneMap != facts.NeedsToneMap:
		return false
	case len(match.TargetCodec) > 0 && !contains(match.TargetCodec, facts.TargetCodec):
		return false
	}
	return true
}

// inPools reports a worker in any of pools; no pools means any worker.
func inPools(labels, pools []string) bool {
	if len(pools) == 0 {
		return true
	}
	for _, pool := range pools {
		if contains(labels, pool) {
			return true
		}
	}
	return false
}

func better(strategy Strategy, left, right *WorkerSnapshot) bool {
	if strategy == StrategySpread {
		l, r := left.ActiveJobs*right.MaxJobs, right.ActiveJobs*left.MaxJobs
		if l != r {
			return l < r
		}
	}
	if !left.Since.Equal(right.Since) {
		return left.Since.Before(right.Since)
	}
	return left.ID < right.ID
}

// NormalizePolicy validates policy and returns it in canonical form, with
// the pool labels that no known worker carries as warnings. An unknown
// pool is not an error: with autoscale a pool can be empty.
func NormalizePolicy(policy Policy, knownPools []string) (Policy, []string, error) {
	if policy.Mode == "" {
		policy.Mode = ModeLocal
	}
	switch policy.Mode {
	case ModeLocal, ModeLocalFirst, ModeWorkersFirst, ModeWorkersOnly:
	default:
		return Policy{}, nil, fmt.Errorf("mode %q is not local, local-first, workers-first or workers-only", policy.Mode)
	}
	if policy.Strategy == "" {
		policy.Strategy = StrategyPack
	}
	if policy.Strategy != StrategyPack && policy.Strategy != StrategySpread {
		return Policy{}, nil, fmt.Errorf("strategy %q is not pack or spread", policy.Strategy)
	}
	if policy.LocalLimit < 0 {
		return Policy{}, nil, errors.New("localLimit must not be negative")
	}
	if policy.QueueTimeout != nil && (*policy.QueueTimeout < 0 || *policy.QueueTimeout > maxQueueTimeout) {
		return Policy{}, nil, fmt.Errorf("queueTimeout must be between 0 and %s", maxQueueTimeout)
	}
	known := make(map[string]bool, len(knownPools))
	for _, pool := range knownPools {
		known[strings.ToLower(pool)] = true
	}
	var warnings []string
	unknown := func(pools []string) []string {
		pools = store.NormalizeTranscodeWorkerPools(pools)
		for _, pool := range pools {
			if !known[pool] {
				warnings = append(warnings, fmt.Sprintf("no worker carries pool %q", pool))
				known[pool] = true
			}
		}
		return pools
	}
	policy.DefaultPools = unknown(policy.DefaultPools)
	rules := make([]Rule, 0, len(policy.Rules))
	for index, rule := range policy.Rules {
		label := rule.Name
		if label == "" {
			label = fmt.Sprintf("#%d", index+1)
		}
		if rule.Local && len(rule.Pools) > 0 {
			return Policy{}, nil, fmt.Errorf("rule %s sets both local and pools", label)
		}
		if rule.QueueTimeout != nil && (*rule.QueueTimeout < 0 || *rule.QueueTimeout > maxQueueTimeout) {
			return Policy{}, nil, fmt.Errorf("rule %s: queueTimeout must be between 0 and %s", label, maxQueueTimeout)
		}
		match := rule.Match
		if match.BitDepthMin < 0 || match.BitDepthMax < 0 || (match.BitDepthMax > 0 && match.BitDepthMin > match.BitDepthMax) {
			return Policy{}, nil, fmt.Errorf("rule %s: bit depth bounds are invalid", label)
		}
		for _, value := range match.HDR {
			switch strings.ToLower(value) {
			case "sdr", "hdr10", "hdr10plus", "hlg", "dovi":
			default:
				return Policy{}, nil, fmt.Errorf("rule %s: hdr %q is not sdr, hdr10, hdr10plus, hlg or dovi", label, value)
			}
		}
		match.SourceCodec = lowerAll(match.SourceCodec)
		match.SourceProfile = lowerAll(match.SourceProfile)
		match.HDR = lowerAll(match.HDR)
		match.TargetCodec = lowerAll(match.TargetCodec)
		rule.Match = match
		rule.Pools = unknown(rule.Pools)
		rules = append(rules, rule)
	}
	policy.Rules = rules
	if len(policy.WarmSlots) > 0 {
		warm := make(map[string]int, len(policy.WarmSlots))
		for pool, slots := range policy.WarmSlots {
			if slots < 0 || slots > maxWorkerJobs*16 {
				return Policy{}, nil, fmt.Errorf("warmSlots of pool %q must be between 0 and %d", pool, maxWorkerJobs*16)
			}
			if pool = strings.ToLower(strings.TrimSpace(pool)); pool != AllPools {
				if normalized := store.NormalizeTranscodeWorkerPools([]string{pool}); len(normalized) == 1 {
					pool = normalized[0]
				} else {
					return Policy{}, nil, fmt.Errorf("warmSlots names an invalid pool %q", pool)
				}
			}
			warm[pool] = slots
		}
		policy.WarmSlots = warm
	}
	sort.Strings(warnings)
	return policy, warnings, nil
}

// maxQueueTimeout stays below the runner's start bound.
const maxQueueTimeout = time.Minute

func lowerAll(values []string) []string {
	var out []string
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// DecodeStoredPolicy decodes the result of store.TranscodeWorkerStore.Policy.
// No stored policy (store.ErrNotFound) is the default policy: every job
// runs locally.
func DecodeStoredPolicy(encoded []byte, err error) (Policy, error) {
	if errors.Is(err, store.ErrNotFound) {
		return Policy{Mode: ModeLocal, Strategy: StrategyPack}, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("read placement policy: %w", err)
	}
	var policy Policy
	if err := json.Unmarshal(encoded, &policy); err != nil {
		return Policy{}, fmt.Errorf("decode placement policy: %w", err)
	}
	normalized, _, err := NormalizePolicy(policy, nil)
	if err != nil {
		return Policy{}, fmt.Errorf("stored placement policy: %w", err)
	}
	return normalized, nil
}
