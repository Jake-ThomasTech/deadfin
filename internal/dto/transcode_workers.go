// SPDX-License-Identifier: GPL-2.0-only

package dto

// TranscodeWorker is one enrolled transcode worker on the native admin API
// (/deadfin/v1/transcode-workers, #615). Capabilities and ActiveJobs are
// set while the worker is connected.
type TranscodeWorker struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
	// Static marks a worker that connected with a token from the server
	// environment. It has no stored identity, so it cannot be edited or
	// revoked here; remove its token from the environment instead.
	Static       bool                         `json:"Static"`
	JoinTokenID  string                       `json:"JoinTokenId"`
	InputMode    string                       `json:"InputMode"`
	Pools        []string                     `json:"Pools"`
	Connected    bool                         `json:"Connected"`
	Created      Time                         `json:"Created"`
	LastSeen     Time                         `json:"LastSeen"`
	Revoked      *Time                        `json:"Revoked,omitempty"`
	Capabilities *TranscodeWorkerCapabilities `json:"Capabilities,omitempty"`
	ActiveJobs   int                          `json:"ActiveJobs"`
}

// TranscodeWorkerCapabilities is what a connected worker reported.
type TranscodeWorkerCapabilities struct {
	HWAccels      []string `json:"HwAccels"`
	Encoders      []string `json:"Encoders"`
	FFmpegVersion string   `json:"FfmpegVersion"`
	MaxJobs       int      `json:"MaxJobs"`
}

// TranscodeWorkerUpdate changes the operator-owned fields of a worker. A nil
// field keeps its value.
type TranscodeWorkerUpdate struct {
	Name      *string   `json:"Name"`
	InputMode *string   `json:"InputMode"`
	Pools     *[]string `json:"Pools"`
}

// TranscodeWorkerJoinToken is a join token without its secret.
type TranscodeWorkerJoinToken struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	Fingerprint string `json:"Fingerprint"`
	Expires     *Time  `json:"Expires,omitempty"`
	UsesLeft    *int   `json:"UsesLeft,omitempty"`
	Created     Time   `json:"Created"`
	Revoked     *Time  `json:"Revoked,omitempty"`
	Usable      bool   `json:"Usable"`
}

// CreateTranscodeWorkerJoinToken is the create request. Without
// ExpiresInSeconds the token does not expire; without Uses it has no use
// limit. Fingerprint pins the server key the worker sees.
type CreateTranscodeWorkerJoinToken struct {
	Name             string `json:"Name"`
	ExpiresInSeconds int64  `json:"ExpiresInSeconds"`
	Uses             int    `json:"Uses"`
	Fingerprint      string `json:"Fingerprint"`
}

// CreatedTranscodeWorkerJoinToken carries the token string. The server
// keeps only a derived key, so this response is the only copy.
type CreatedTranscodeWorkerJoinToken struct {
	TranscodeWorkerJoinToken
	Token string `json:"Token"`
}

// TranscodeWorkerPins is the set of extra server key fingerprints that
// pinned workers accept, for a certificate key rotation.
type TranscodeWorkerPins struct {
	Fingerprints []string `json:"Fingerprints"`
}

// TranscodeWorkerPlainHTTP is the plain-HTTP opt-in for the worker routes
// (#664). AllowPlainHttp is the value in effect. Configured is the
// dashboard setting, EncodingOptions.AllowPlainHttpTranscodeWorkers.
// SetByEnvironment is true when DEADFIN_TRANSCODE_WORKERS_ALLOW_HTTP is set;
// it then wins over Configured.
type TranscodeWorkerPlainHTTP struct {
	AllowPlainHttp   bool `json:"AllowPlainHttp"`
	Configured       bool `json:"Configured"`
	SetByEnvironment bool `json:"SetByEnvironment"`
}

// TranscodeWorkerPolicy is the transcode placement policy (#616,
// docs/spikes/transcode-workers.md §3.8). Durations are whole seconds.
// Warnings is set only in the PUT response.
type TranscodeWorkerPolicy struct {
	Mode                string                      `json:"Mode"`
	DefaultPools        []string                    `json:"DefaultPools"`
	LocalLimit          int                         `json:"LocalLimit"`
	QueueTimeoutSeconds *int                        `json:"QueueTimeoutSeconds"`
	CPUFallback         *bool                       `json:"CpuFallback"`
	Strategy            string                      `json:"Strategy"`
	Rules               []TranscodeWorkerPolicyRule `json:"Rules"`
	// WarmSlots is the warm capacity per pool for the scaling signals;
	// "*" is the whole fleet, and a pool without an entry gets 1.
	WarmSlots map[string]int `json:"WarmSlots"`
	Warnings  []string       `json:"Warnings,omitempty"`
}

// TranscodeWorkerPolicyRule is one ordered placement rule.
type TranscodeWorkerPolicyRule struct {
	Name                string                     `json:"Name"`
	Match               TranscodeWorkerPolicyMatch `json:"Match"`
	Pools               []string                   `json:"Pools"`
	Local               bool                       `json:"Local"`
	QueueTimeoutSeconds *int                       `json:"QueueTimeoutSeconds"`
	CPUFallback         *bool                      `json:"CpuFallback"`
}

// TranscodeWorkerPolicyMatch selects the jobs of a rule.
type TranscodeWorkerPolicyMatch struct {
	SourceCodec   []string `json:"SourceCodec"`
	SourceProfile []string `json:"SourceProfile"`
	BitDepthMin   int      `json:"BitDepthMin"`
	BitDepthMax   int      `json:"BitDepthMax"`
	HDR           []string `json:"Hdr"`
	NeedsToneMap  *bool    `json:"NeedsToneMap"`
	TargetCodec   []string `json:"TargetCodec"`
}

// TranscodeWorkerScalingToken is a new scaling token. It is shown once.
type TranscodeWorkerScalingToken struct {
	Token string `json:"Token"`
}
