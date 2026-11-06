// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

// Hardware placement (#622, docs/spikes/transcode-workers.md §3.2): place
// first, then build the argv. A worker that reports its encoding
// capabilities gets an argv built for its own ffmpeg and hardware; any
// other worker gets the server's argv when it can run it.

// workerSpec is the worker form of one job for one worker profile.
type workerSpec struct {
	spec JobSpec
	need requirement
	// hardware reports an argv that uses a hardware family.
	hardware bool
	// tier counts the stages of the argv that run in hardware (#854).
	tier int
	err  error
}

// jobPlan holds the worker forms of one job, built once per worker
// profile. The caller holds hub.mu.
type jobPlan struct {
	request ffmpeg.RemoteHLSJob
	server  workerSpec
	built   map[string]workerSpec
}

func newJobPlan(request ffmpeg.RemoteHLSJob) *jobPlan {
	plan := &jobPlan{request: request, built: make(map[string]workerSpec)}
	plan.server = specFor(request, request.Args)
	return plan
}

func specFor(request ffmpeg.RemoteHLSJob, args []string) workerSpec {
	spec, need, err := buildJobSpecArgs(request, args)
	return workerSpec{spec: spec, need: need, hardware: need.hardware(), tier: need.tier(), err: err}
}

// forWorker returns the worker form of the job for caps, whose profile key
// is profile.
func (plan *jobPlan) forWorker(caps Capabilities, profile string) workerSpec {
	if caps.Encoding == nil || plan.request.Build == nil {
		return plan.server
	}
	if built, ok := plan.built[profile]; ok {
		return built
	}
	var host encoding.HostHardware
	if caps.Hardware != nil {
		host = *caps.Hardware
	}
	var built workerSpec
	if args, err := plan.request.Build(*caps.Encoding, host); err != nil {
		built = workerSpec{err: fmt.Errorf("build the worker argv: %w", err)}
	} else {
		built = specFor(plan.request, args)
	}
	plan.built[profile] = built
	return built
}

// fits reports whether conn can take the job now, and the hardware tier
// of its argv there. The caller holds hub.mu.
func (plan *jobPlan) fits(conn *workerConn) (fits bool, tier int) {
	// The argv of a remote-only job encodes a codec that the server
	// cannot (#854): only a worker that builds its own argv can run it.
	if plan.request.RemoteOnly && (conn.caps.Encoding == nil || plan.request.Build == nil) {
		return false, 0
	}
	form := plan.forWorker(conn.caps, conn.profile)
	if form.err != nil || !form.need.satisfiedBy(conn.caps) {
		return false, 0
	}
	if form.hardware && conn.hardwareJobs() >= conn.caps.hardwareSessions() {
		return false, 0
	}
	return true, form.tier
}

// profileKey identifies the inputs of a worker's argv: its encoding
// capabilities and hardware. Workers with one profile share one argv.
func profileKey(caps Capabilities) string {
	if caps.Encoding == nil {
		return ""
	}
	encoded, _ := json.Marshal(struct {
		Encoding *encoding.Capabilities
		Hardware *encoding.HostHardware
	}{caps.Encoding, caps.Hardware})
	return string(encoded)
}

// hardwareSessions is the limit of the jobs whose argv uses the hardware
// family.
func (caps Capabilities) hardwareSessions() int {
	if caps.HardwareSessions > 0 && caps.HardwareSessions < caps.MaxJobs {
		return caps.HardwareSessions
	}
	return caps.MaxJobs
}

// hardwareJobs counts the worker's jobs whose argv uses the hardware
// family. The caller holds hub.mu.
func (conn *workerConn) hardwareJobs() int {
	count := 0
	for _, job := range conn.jobs {
		if job.hardware {
			count++
		}
	}
	return count
}

// hardwareEncoderSuffixes name the hardware video encoders.
var hardwareEncoderSuffixes = []string{"_qsv", "_vaapi", "_nvenc", "_videotoolbox", "_amf", "_v4l2m2m", "_rkmpp"}

// hardwareDecoderSuffixes name the hardware video decoders.
var hardwareDecoderSuffixes = append([]string{"_cuvid"}, hardwareEncoderSuffixes...)

// hardware reports an argv that opens a hardware device or encoder.
func (need requirement) hardware() bool {
	return len(need.hwaccels) > 0 || need.hardwareEncode()
}

// The hardware tiers (#854): the count of the stages of an argv, decode
// and encode, that run in hardware.
const (
	TierSoftware = 0
	TierEncode   = 1
	TierFull     = 2
)

// tier is the hardware tier of the argv. Placement prefers a higher tier.
func (need requirement) tier() int {
	tier := TierSoftware
	if need.hwdecode {
		tier++
	}
	if need.hardwareEncode() {
		tier++
	}
	return tier
}

// hardwareEncode reports an argv that names a hardware video encoder.
func (need requirement) hardwareEncode() bool {
	for _, encoder := range need.encoders {
		if hasSuffix(encoder, hardwareEncoderSuffixes) {
			return true
		}
	}
	return false
}

func hasSuffix(name string, suffixes []string) bool {
	name = strings.ToLower(name)
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// validateHardware checks a worker's reported hardware.
func validateHardware(caps Capabilities) error {
	if caps.HardwareSessions < 0 || caps.HardwareSessions > maxWorkerJobs {
		return errors.New("invalid hardware session limit")
	}
	if caps.Hardware == nil {
		return nil
	}
	switch caps.Hardware.Type {
	case "", encoding.HardwareAccelerationNone, encoding.HardwareAccelerationQSV, encoding.HardwareAccelerationVAAPI, encoding.HardwareAccelerationNVENC:
	default:
		return fmt.Errorf("hardware type %q is not qsv, vaapi or nvenc", caps.Hardware.Type)
	}
	for _, device := range []string{caps.Hardware.VaapiDevice, caps.Hardware.QsvDevice} {
		if device != "" && (!strings.HasPrefix(device, "/dev/") || strings.Contains(device, "..") || strings.ContainsAny(device, ":,= '\"\\")) {
			return fmt.Errorf("render node %q is not a device path", device)
		}
	}
	return nil
}

// hardwareType names the worker's hardware family for logs.
func hardwareType(caps Capabilities) string {
	if caps.Hardware == nil || caps.Hardware.Type == "" {
		return "none"
	}
	return caps.Hardware.Type
}
