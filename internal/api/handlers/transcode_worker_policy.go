// SPDX-License-Identifier: GPL-2.0-only

package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/api/render"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// policy returns the live placement policy (#616).
func (h transcodeWorkerAdmin) policy(w http.ResponseWriter, r *http.Request) {
	_ = render.JSON(w, r, http.StatusOK, policyDTO(h.hub.Policy()))
}

// setPolicy validates, stores and applies a placement policy. The response
// is the normalized policy with its warnings, for example a pool that no
// worker carries.
func (h transcodeWorkerAdmin) setPolicy(w http.ResponseWriter, r *http.Request) {
	var request dto.TranscodeWorkerPolicy
	if err := bind.JSONBody(r, &request); err != nil {
		renderProblem(w, r, http.StatusBadRequest, "invalid placement policy")
		return
	}
	pools, err := h.knownPools(r)
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "list transcode workers")
		return
	}
	policy, warnings, err := transcodeworker.NormalizePolicy(policyFromDTO(request), pools)
	if err != nil {
		renderProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "encode placement policy")
		return
	}
	if err := h.workers.SetPolicy(r.Context(), encoded, h.clock.Now()); err != nil {
		renderProblem(w, r, http.StatusInternalServerError, "store placement policy")
		return
	}
	h.hub.SetPolicy(policy)
	response := policyDTO(policy)
	response.Warnings = warnings
	_ = render.JSON(w, r, http.StatusOK, response)
}

// knownPools returns the pools of the enrolled and the connected workers.
func (h transcodeWorkerAdmin) knownPools(r *http.Request) ([]string, error) {
	stored, err := h.workers.Workers(r.Context())
	if err != nil {
		return nil, err
	}
	var pools []string
	for _, worker := range stored {
		if worker.Revoked.IsZero() {
			pools = append(pools, worker.Pools...)
		}
	}
	for _, info := range h.hub.Workers() {
		pools = append(pools, info.Pools...)
	}
	return pools, nil
}

func secondsPointer(value *time.Duration) *int {
	if value == nil {
		return nil
	}
	seconds := int(*value / time.Second)
	return &seconds
}

func durationPointer(seconds *int) *time.Duration {
	if seconds == nil {
		return nil
	}
	value := time.Duration(*seconds) * time.Second
	return &value
}

func policyDTO(policy transcodeworker.Policy) dto.TranscodeWorkerPolicy {
	result := dto.TranscodeWorkerPolicy{
		Mode: string(policy.Mode), DefaultPools: append([]string{}, policy.DefaultPools...), LocalLimit: policy.LocalLimit,
		QueueTimeoutSeconds: secondsPointer(policy.QueueTimeout), CPUFallback: policy.CPUFallback,
		Strategy: string(policy.Strategy), Rules: make([]dto.TranscodeWorkerPolicyRule, 0, len(policy.Rules)),
		WarmSlots: map[string]int{},
	}
	for pool, slots := range policy.WarmSlots {
		result.WarmSlots[pool] = slots
	}
	if result.Mode == "" {
		result.Mode = string(transcodeworker.ModeLocal)
	}
	if result.Strategy == "" {
		result.Strategy = string(transcodeworker.StrategyPack)
	}
	for _, rule := range policy.Rules {
		match := rule.Match
		result.Rules = append(result.Rules, dto.TranscodeWorkerPolicyRule{
			Name: rule.Name, Pools: append([]string{}, rule.Pools...), Local: rule.Local,
			QueueTimeoutSeconds: secondsPointer(rule.QueueTimeout), CPUFallback: rule.CPUFallback,
			Match: dto.TranscodeWorkerPolicyMatch{
				SourceCodec: append([]string{}, match.SourceCodec...), SourceProfile: append([]string{}, match.SourceProfile...),
				BitDepthMin: match.BitDepthMin, BitDepthMax: match.BitDepthMax, HDR: append([]string{}, match.HDR...),
				NeedsToneMap: match.NeedsToneMap, TargetCodec: append([]string{}, match.TargetCodec...),
			},
		})
	}
	return result
}

func policyFromDTO(value dto.TranscodeWorkerPolicy) transcodeworker.Policy {
	policy := transcodeworker.Policy{
		Mode: transcodeworker.Mode(value.Mode), DefaultPools: value.DefaultPools, LocalLimit: value.LocalLimit,
		QueueTimeout: durationPointer(value.QueueTimeoutSeconds), CPUFallback: value.CPUFallback,
		Strategy: transcodeworker.Strategy(value.Strategy), WarmSlots: value.WarmSlots,
	}
	for _, rule := range value.Rules {
		match := rule.Match
		policy.Rules = append(policy.Rules, transcodeworker.Rule{
			Name: rule.Name, Pools: rule.Pools, Local: rule.Local,
			QueueTimeout: durationPointer(rule.QueueTimeoutSeconds), CPUFallback: rule.CPUFallback,
			Match: transcodeworker.Match{
				SourceCodec: match.SourceCodec, SourceProfile: match.SourceProfile,
				BitDepthMin: match.BitDepthMin, BitDepthMax: match.BitDepthMax, HDR: match.HDR,
				NeedsToneMap: match.NeedsToneMap, TargetCodec: match.TargetCodec,
			},
		})
	}
	return policy
}
