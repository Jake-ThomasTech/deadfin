// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"testing"
	"time"
)

func TestPlace(t *testing.T) {
	yes, no := true, false
	seconds := func(n int) *time.Duration { d := time.Duration(n) * time.Second; return &d }
	t0 := testEpoch
	worker := func(id string, pools []string, active, max int, since time.Duration) WorkerSnapshot {
		return WorkerSnapshot{ID: id, Pools: pools, Fits: true, InputMode: "auto", ActiveJobs: active, MaxJobs: max, Since: t0.Add(since)}
	}
	hevc10 := JobFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "hdr10", NeedsToneMap: true, TargetCodec: "h264"}
	h264 := JobFacts{SourceCodec: "h264", SourceProfile: "high", BitDepth: 8, HDR: "sdr", TargetCodec: "h264"}
	av1 := JobFacts{SourceCodec: "av1", SourceProfile: "main", BitDepth: 8, HDR: "sdr", TargetCodec: "h264"}
	// tiered is the fleet of #854: b is the older worker, in pool igpu;
	// a is in pool arc. Each has four slots.
	tiered := func(bTier, bActive, aTier, aActive int) []WorkerSnapshot {
		b := worker("b", []string{"igpu"}, bActive, 4, 0)
		a := worker("a", []string{"arc"}, aActive, 4, time.Minute)
		b.Tier, a.Tier = bTier, aTier
		return []WorkerSnapshot{a, b}
	}
	closed := func(fleet []WorkerSnapshot, id string) []WorkerSnapshot {
		for index := range fleet {
			fleet[index].Closed = fleet[index].ID == id
		}
		return fleet
	}
	fleet := []WorkerSnapshot{
		worker("old", []string{"qsv"}, 1, 2, 0),
		worker("new", []string{"qsv", "big-gpu"}, 0, 2, time.Minute),
		worker("nv", []string{"nvenc"}, 0, 1, 2*time.Minute),
	}
	rules := []Rule{
		{Name: "10bit-hevc", Match: Match{SourceCodec: []string{"av1", "hevc"}, BitDepthMin: 10}, Pools: []string{"big-gpu"}},
		{Name: "tonemap", Match: Match{NeedsToneMap: &yes}, Pools: []string{"nvenc", "qsv"}},
		{Name: "dovi-local", Match: Match{HDR: []string{"dovi"}}, Local: true},
		{Name: "hevc-target", Match: Match{TargetCodec: []string{"hevc"}}, Pools: []string{"nvenc"}, QueueTimeout: seconds(20), CPUFallback: &no},
	}
	cases := []struct {
		name   string
		facts  JobFacts
		policy Policy
		fleet  []WorkerSnapshot
		local  LocalState
		want   Decision
	}{
		// Modes with no matching rule.
		{name: "local mode ignores workers and rules", facts: hevc10, policy: Policy{Rules: rules}, fleet: fleet, want: Decision{Kind: DecideLocalCPU}},
		{name: "local mode uses the local GPU", facts: h264, policy: Policy{Mode: ModeLocal}, fleet: fleet, local: LocalState{GPU: true}, want: Decision{Kind: DecideLocalGPU}},
		{name: "local-first at 0 never spills", facts: h264, policy: Policy{Mode: ModeLocalFirst}, fleet: fleet, local: LocalState{ActiveJobs: 9}, want: Decision{Kind: DecideLocalCPU}},
		{name: "local-first below the limit", facts: h264, policy: Policy{Mode: ModeLocalFirst, LocalLimit: 2}, fleet: fleet, local: LocalState{ActiveJobs: 1}, want: Decision{Kind: DecideLocalCPU}},
		{name: "local-first at the limit spills", facts: h264, policy: Policy{Mode: ModeLocalFirst, LocalLimit: 2}, fleet: fleet, local: LocalState{ActiveJobs: 2}, want: Decision{Kind: DecideWorker, WorkerID: "old"}},
		{name: "workers-first packs the oldest free worker", facts: h264, policy: Policy{Mode: ModeWorkersFirst}, fleet: fleet, want: Decision{Kind: DecideWorker, WorkerID: "old"}},
		{name: "spread takes the least loaded", facts: h264, policy: Policy{Mode: ModeWorkersFirst, Strategy: StrategySpread}, fleet: fleet, want: Decision{Kind: DecideWorker, WorkerID: "new"}},
		{name: "default pools", facts: h264, policy: Policy{Mode: ModeWorkersFirst, DefaultPools: []string{"nvenc"}}, fleet: fleet, want: Decision{Kind: DecideWorker, WorkerID: "nv", Pools: []string{"nvenc"}}},
		{name: "workers-first with no worker takes the chain at once", facts: h264, policy: Policy{Mode: ModeWorkersFirst}, want: Decision{Kind: DecideLocalCPU, Reason: ReasonPoolEmpty, Fallback: true}},
		{name: "workers-only queues with the default timeout", facts: h264, policy: Policy{Mode: ModeWorkersOnly}, want: Decision{Kind: DecideQueue, QueueTimeout: DefaultQueueTimeout,
			Then: &Decision{Kind: DecideLocalCPU, Reason: ReasonQueueTimeout, Fallback: true}}},
		{name: "workers-only queue then local GPU", facts: h264, policy: Policy{Mode: ModeWorkersOnly, QueueTimeout: seconds(3)}, local: LocalState{GPU: true}, want: Decision{Kind: DecideQueue, QueueTimeout: 3 * time.Second,
			Then: &Decision{Kind: DecideLocalGPU, Reason: ReasonQueueTimeout, Fallback: true}}},
		{name: "workers-only without cpu fallback refuses after the queue", facts: h264, policy: Policy{Mode: ModeWorkersOnly, CPUFallback: &no}, want: Decision{Kind: DecideQueue, QueueTimeout: DefaultQueueTimeout,
			Then: &Decision{Kind: DecideRefuse, Reason: ReasonQueueTimeout, Fallback: true}}},
		{name: "full pool is no-worker", facts: h264, policy: Policy{Mode: ModeWorkersFirst, DefaultPools: []string{"qsv"}}, fleet: []WorkerSnapshot{worker("old", []string{"qsv"}, 2, 2, 0)},
			want: Decision{Kind: DecideLocalCPU, Pools: []string{"qsv"}, Reason: ReasonNoWorker, Fallback: true}},
		{name: "a worker that lacks capabilities does not fit", facts: h264, policy: Policy{Mode: ModeWorkersFirst}, fleet: []WorkerSnapshot{{ID: "w", Fits: false, MaxJobs: 1}},
			want: Decision{Kind: DecideLocalCPU, Reason: ReasonNoWorker, Fallback: true}},
		{name: "a shared worker without a covering map does not fit", facts: h264, policy: Policy{Mode: ModeWorkersFirst}, fleet: []WorkerSnapshot{{ID: "w", Fits: true, InputMode: "shared", MaxJobs: 1}},
			want: Decision{Kind: DecideLocalCPU, Reason: ReasonNoWorker, Fallback: true}},
		{name: "a shared worker with a covering map fits", facts: h264, policy: Policy{Mode: ModeWorkersFirst}, fleet: []WorkerSnapshot{{ID: "w", Fits: true, InputMode: "shared", Shared: true, MaxJobs: 1}},
			want: Decision{Kind: DecideWorker, WorkerID: "w"}},

		// Rules: first match wins, every field, AND/OR, several pools,
		// per-rule overrides.
		{name: "first match wins", facts: hevc10, policy: Policy{Mode: ModeWorkersFirst, Rules: rules}, fleet: fleet, want: Decision{Kind: DecideWorker, WorkerID: "new", Rule: "10bit-hevc", Pools: []string{"big-gpu"}}},
		{name: "AND across fields: 8-bit hevc skips the bit-depth rule", facts: JobFacts{SourceCodec: "hevc", BitDepth: 8, HDR: "hdr10", NeedsToneMap: true, TargetCodec: "h264"}, policy: Policy{Mode: ModeWorkersFirst, Rules: rules}, fleet: fleet,
			want: Decision{Kind: DecideWorker, WorkerID: "old", Rule: "tonemap", Pools: []string{"nvenc", "qsv"}}},
		{name: "several pools: any of them", facts: JobFacts{SourceCodec: "h264", HDR: "hdr10", NeedsToneMap: true}, policy: Policy{Mode: ModeWorkersFirst, Rules: rules},
			fleet: []WorkerSnapshot{worker("nv", []string{"nvenc"}, 0, 1, 0)}, want: Decision{Kind: DecideWorker, WorkerID: "nv", Rule: "tonemap", Pools: []string{"nvenc", "qsv"}}},
		{name: "local rule", facts: JobFacts{SourceCodec: "hevc", BitDepth: 8, HDR: "dovi"}, policy: Policy{Mode: ModeWorkersOnly, Rules: rules}, fleet: fleet, want: Decision{Kind: DecideLocalCPU, Rule: "dovi-local"}},
		{name: "local rule without cpu fallback or GPU refuses", facts: JobFacts{HDR: "dovi"}, policy: Policy{Mode: ModeWorkersOnly, CPUFallback: &no, Rules: rules}, want: Decision{Kind: DecideRefuse, Rule: "dovi-local"}},
		{name: "per-rule queue and cpu overrides in workers-first", facts: JobFacts{SourceCodec: "h264", HDR: "sdr", TargetCodec: "hevc"}, policy: Policy{Mode: ModeWorkersFirst, Rules: rules}, fleet: []WorkerSnapshot{worker("nv", []string{"nvenc"}, 1, 1, 0)},
			want: Decision{Kind: DecideQueue, Rule: "hevc-target", Pools: []string{"nvenc"}, QueueTimeout: 20 * time.Second,
				Then: &Decision{Kind: DecideRefuse, Rule: "hevc-target", Pools: []string{"nvenc"}, Reason: ReasonQueueTimeout, Fallback: true}}},
		{name: "no rule matches: mode default", facts: h264, policy: Policy{Mode: ModeWorkersFirst, Rules: rules[:1]}, fleet: fleet, want: Decision{Kind: DecideWorker, WorkerID: "old"}},
		{name: "chain order: GPU before CPU", facts: h264, policy: Policy{Mode: ModeWorkersFirst, DefaultPools: []string{"none"}}, local: LocalState{GPU: true},
			want: Decision{Kind: DecideLocalGPU, Pools: []string{"none"}, Reason: ReasonPoolEmpty, Fallback: true}},

		// Hardware tiers (#854): B is older and idle, A runs the whole job
		// in hardware.
		{name: "pack prefers tier 2 over an older tier 1", facts: av1, policy: Policy{Mode: ModeWorkersFirst}, fleet: tiered(1, 0, 2, 0),
			want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "spread prefers tier 2 over an idle tier 1", facts: av1, policy: Policy{Mode: ModeWorkersFirst, Strategy: StrategySpread}, fleet: tiered(1, 0, 2, 3),
			want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "tier 1 over software", facts: av1, policy: Policy{Mode: ModeWorkersFirst}, fleet: tiered(0, 0, 1, 0),
			want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierEncode}},
		{name: "equal tiers: pack takes the oldest", facts: av1, policy: Policy{Mode: ModeWorkersFirst}, fleet: tiered(2, 0, 2, 0),
			want: Decision{Kind: DecideWorker, WorkerID: "b", Tier: TierFull}},
		{name: "equal tiers: spread takes the least loaded", facts: av1, policy: Policy{Mode: ModeWorkersFirst, Strategy: StrategySpread}, fleet: tiered(2, 2, 2, 1),
			want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "tier 2 busy falls back to tier 1", facts: av1, policy: Policy{Mode: ModeWorkersOnly}, fleet: tiered(1, 0, 2, 4),
			want: Decision{Kind: DecideWorker, WorkerID: "b", Tier: TierEncode}},
		{name: "tier 2 draining falls back to tier 1", facts: av1, policy: Policy{Mode: ModeWorkersOnly}, fleet: closed(tiered(1, 0, 2, 0), "a"),
			want: Decision{Kind: DecideWorker, WorkerID: "b", Tier: TierEncode}},
		{name: "a rule picks the pool before the tier", facts: av1, policy: Policy{Mode: ModeWorkersFirst, Rules: []Rule{{Name: "igpu", Match: Match{SourceCodec: []string{"av1"}}, Pools: []string{"igpu"}}}},
			fleet: tiered(1, 0, 2, 0), want: Decision{Kind: DecideWorker, WorkerID: "b", Rule: "igpu", Pools: []string{"igpu"}, Tier: TierEncode}},
		{name: "local-first at its limit takes the tier 2 worker over a tier 1 server", facts: av1, policy: Policy{Mode: ModeLocalFirst, LocalLimit: 1}, fleet: tiered(1, 0, 2, 0),
			local: LocalState{ActiveJobs: 1, GPU: true, Tier: TierEncode}, want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "workers-first takes the tier 2 worker over a tier 1 server", facts: av1, policy: Policy{Mode: ModeWorkersFirst}, fleet: tiered(1, 0, 2, 0),
			local: LocalState{GPU: true, Tier: TierEncode}, want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "a local decision carries the server tier", facts: av1, policy: Policy{Mode: ModeLocal}, fleet: tiered(1, 0, 2, 0),
			local: LocalState{GPU: true, Tier: TierFull}, want: Decision{Kind: DecideLocalGPU, Tier: TierFull}},

		// Remote-only jobs (#854) never run locally.
		{name: "remote-only in local mode takes a worker", facts: av1, policy: Policy{Mode: ModeLocal}, fleet: tiered(1, 0, 2, 0), local: LocalState{RemoteOnly: true},
			want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "remote-only below the local-first limit takes a worker", facts: av1, policy: Policy{Mode: ModeLocalFirst, LocalLimit: 4}, fleet: tiered(1, 0, 2, 0), local: LocalState{RemoteOnly: true},
			want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "remote-only without a worker queues and then refuses", facts: av1, policy: Policy{Mode: ModeWorkersFirst}, local: LocalState{RemoteOnly: true},
			want: Decision{Kind: DecideQueue, QueueTimeout: DefaultQueueTimeout, Then: &Decision{Kind: DecideRefuse, Reason: ReasonQueueTimeout, Fallback: true}}},
		{name: "remote-only uses the policy queue timeout", facts: av1, policy: Policy{Mode: ModeWorkersFirst, QueueTimeout: seconds(3)}, local: LocalState{RemoteOnly: true},
			want: Decision{Kind: DecideQueue, QueueTimeout: 3 * time.Second, Then: &Decision{Kind: DecideRefuse, Reason: ReasonQueueTimeout, Fallback: true}}},
		{name: "remote-only skips a local rule", facts: av1, policy: Policy{Mode: ModeWorkersFirst, Rules: []Rule{{Name: "keep", Match: Match{SourceCodec: []string{"av1"}}, Local: true}}},
			fleet: tiered(1, 0, 2, 0), local: LocalState{RemoteOnly: true}, want: Decision{Kind: DecideWorker, WorkerID: "a", Tier: TierFull}},
		{name: "remote-only ignores the cpu fallback", facts: av1, policy: Policy{Mode: ModeWorkersOnly, CPUFallback: &yes}, local: LocalState{RemoteOnly: true},
			want: Decision{Kind: DecideQueue, QueueTimeout: DefaultQueueTimeout, Then: &Decision{Kind: DecideRefuse, Reason: ReasonQueueTimeout, Fallback: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Place(tc.facts, tc.policy, tc.fleet, tc.local)
			if !decisionEqual(got, tc.want) {
				t.Fatalf("Place = %+v (then %+v), want %+v (then %+v)", got, got.Then, tc.want, tc.want.Then)
			}
		})
	}
}

func TestMatchFields(t *testing.T) {
	yes := true
	facts := JobFacts{SourceCodec: "hevc", SourceProfile: "main10", BitDepth: 10, HDR: "hlg", NeedsToneMap: true, TargetCodec: "h264"}
	for name, tc := range map[string]struct {
		match Match
		want  bool
	}{
		"empty matches anything":   {Match{}, true},
		"source codec any of":      {Match{SourceCodec: []string{"av1", "hevc"}}, true},
		"source codec miss":        {Match{SourceCodec: []string{"av1"}}, false},
		"profile":                  {Match{SourceProfile: []string{"main10"}}, true},
		"profile miss":             {Match{SourceProfile: []string{"main"}}, false},
		"bit depth min":            {Match{BitDepthMin: 10}, true},
		"bit depth min miss":       {Match{BitDepthMin: 12}, false},
		"bit depth max miss":       {Match{BitDepthMax: 8}, false},
		"hdr":                      {Match{HDR: []string{"hdr10", "hlg"}}, true},
		"hdr miss":                 {Match{HDR: []string{"dovi"}}, false},
		"tone map":                 {Match{NeedsToneMap: &yes}, true},
		"target codec":             {Match{TargetCodec: []string{"h264"}}, true},
		"target codec miss":        {Match{TargetCodec: []string{"hevc"}}, false},
		"AND: one field misses":    {Match{SourceCodec: []string{"hevc"}, TargetCodec: []string{"hevc"}}, false},
		"AND: every field matches": {Match{SourceCodec: []string{"hevc"}, BitDepthMin: 10, BitDepthMax: 10, HDR: []string{"hlg"}, NeedsToneMap: &yes}, true},
	} {
		if got := tc.match.matches(facts); got != tc.want {
			t.Errorf("%s: matches = %v, want %v", name, got, tc.want)
		}
	}
}

func TestNormalizePolicy(t *testing.T) {
	minute := 2 * time.Minute
	policy, warnings, err := NormalizePolicy(Policy{
		DefaultPools: []string{"QSV", "ghost"},
		Rules:        []Rule{{Name: "x", Match: Match{SourceCodec: []string{" HEVC "}, HDR: []string{"HDR10"}}, Pools: []string{"Big-GPU"}}},
	}, []string{"qsv"})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != ModeLocal || policy.Strategy != StrategyPack || policy.Rules[0].Match.SourceCodec[0] != "hevc" || policy.Rules[0].Match.HDR[0] != "hdr10" ||
		policy.Rules[0].Pools[0] != "big-gpu" || len(policy.DefaultPools) != 2 {
		t.Fatalf("normalized = %+v", policy)
	}
	warm, _, err := NormalizePolicy(Policy{WarmSlots: map[string]int{" GPU ": 2, "*": 0}}, nil)
	if err != nil || warm.WarmSlots["gpu"] != 2 || warm.warmSlots("*") != 0 || warm.warmSlots("other") != DefaultWarmSlots {
		t.Fatalf("warm slots = %+v, %v", warm.WarmSlots, err)
	}
	if len(warnings) != 2 || warnings[0] != `no worker carries pool "big-gpu"` || warnings[1] != `no worker carries pool "ghost"` {
		t.Fatalf("warnings = %q", warnings)
	}
	for name, bad := range map[string]Policy{
		"mode":          {Mode: "everywhere"},
		"strategy":      {Strategy: "random"},
		"local limit":   {LocalLimit: -1},
		"queue timeout": {QueueTimeout: &minute},
		"local+pools":   {Rules: []Rule{{Local: true, Pools: []string{"a"}}}},
		"hdr":           {Rules: []Rule{{Match: Match{HDR: []string{"hdr11"}}}}},
		"bit depth":     {Rules: []Rule{{Match: Match{BitDepthMin: 12, BitDepthMax: 10}}}},
		"warm slots":    {WarmSlots: map[string]int{"gpu": -1}},
		"warm pool":     {WarmSlots: map[string]int{" ": 1}},
	} {
		if _, _, err := NormalizePolicy(bad, nil); err == nil {
			t.Errorf("%s: invalid policy accepted", name)
		}
	}
}

func decisionEqual(a, b Decision) bool {
	if a.Kind != b.Kind || a.WorkerID != b.WorkerID || a.Tier != b.Tier || a.Rule != b.Rule || a.Reason != b.Reason || a.Fallback != b.Fallback || a.QueueTimeout != b.QueueTimeout || len(a.Pools) != len(b.Pools) {
		return false
	}
	for i := range a.Pools {
		if a.Pools[i] != b.Pools[i] {
			return false
		}
	}
	if (a.Then == nil) != (b.Then == nil) {
		return false
	}
	return a.Then == nil || decisionEqual(*a.Then, *b.Then)
}
