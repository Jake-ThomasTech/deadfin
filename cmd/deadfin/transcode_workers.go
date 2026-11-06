// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/transcodeworker"
)

// Transcode workers (#614, #615, D21, docs/spikes/transcode-workers.md).
// Two setups exist. GitOps: envWorkerTokens on the server, and the server
// URL and one of those tokens on each worker. Dashboard pairing: the admin
// API creates a join token that enrolls a worker identity, which the worker
// keeps in its state directory.
const (
	envWorkerTokens    = "DEADFIN_TRANSCODE_WORKER_TOKENS"
	envWorkerAllowHTTP = "DEADFIN_TRANSCODE_WORKERS_ALLOW_HTTP"
	envWorkerMode      = "DEADFIN_TRANSCODE_WORKER_MODE"
	envScalingTokens   = "DEADFIN_TRANSCODE_SCALING_TOKENS"
	envJobLease        = "DEADFIN_TRANSCODE_JOB_LEASE"
	envWorkerToken     = "DEADFIN_WORKER_TOKEN"
	envWorkerServer    = "DEADFIN_WORKER_SERVER"
	envWorkerEphemeral = "DEADFIN_WORKER_EPHEMERAL"
	envScaleHookURL    = "DEADFIN_TRANSCODE_SCALE_HOOK_URL"
	envScaleHookSecret = "DEADFIN_TRANSCODE_SCALE_HOOK_SECRET"
)

// scaleHookMinSecret is the shortest scale hook secret the server accepts.
const scaleHookMinSecret = 16

// transcodeScaleHook reads the optional scale hook (#623). With no URL the
// hook is off. A URL without a secret is ignored with a warning; a short
// secret or a URL that is not http or https is an error.
func transcodeScaleHook(getenv func(string) string, logger *slog.Logger) (transcodeworker.ScaleHook, error) {
	target, secret := strings.TrimSpace(getenv(envScaleHookURL)), strings.TrimSpace(getenv(envScaleHookSecret))
	if target == "" {
		return transcodeworker.ScaleHook{}, nil
	}
	if secret == "" {
		logger.Warn("transcode scale hook URL is set without a secret; the hook is off", "setting", envScaleHookURL, "missing", envScaleHookSecret)
		return transcodeworker.ScaleHook{}, nil
	}
	if len(secret) < scaleHookMinSecret {
		return transcodeworker.ScaleHook{}, fmt.Errorf("%s: the secret is shorter than %d bytes", envScaleHookSecret, scaleHookMinSecret)
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return transcodeworker.ScaleHook{}, fmt.Errorf("%s: %s is not an http or https URL", envScaleHookURL, target)
	}
	return transcodeworker.ScaleHook{URL: target, Secret: []byte(secret)}, nil
}

// transcodeWorkerEnvironment reads the server-side worker settings: the
// static tokens (comma-separated) and the plain-HTTP opt-in, nil when the
// variable is not set.
func transcodeWorkerEnvironment(getenv func(string) string) (tokens []transcodeworker.Token, allowHTTP *bool, err error) {
	for _, value := range strings.Split(getenv(envWorkerTokens), ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		token, err := transcodeworker.ParseToken(value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", envWorkerTokens, err)
		}
		tokens = append(tokens, token)
	}
	if value := strings.TrimSpace(getenv(envWorkerAllowHTTP)); value != "" {
		allow, err := strconv.ParseBool(value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %q is not a boolean", envWorkerAllowHTTP, value)
		}
		allowHTTP = &allow
	}
	return tokens, allowHTTP, nil
}

// transcodeWorkerPlainHTTP resolves the plain-HTTP opt-in for the worker
// routes (#664): DEADFIN_TRANSCODE_WORKERS_ALLOW_HTTP when it is set, else
// the dashboard setting in the encoding configuration. The hub reads it on
// each worker request, so a change applies without a restart.
type transcodeWorkerPlainHTTP struct {
	environment *bool
	manager     *config.Manager
	logger      *slog.Logger
	// on is the last value that allowed returned; a change to true logs
	// the warning once.
	on atomic.Bool
}

func newTranscodeWorkerPlainHTTP(environment *bool, manager *config.Manager, logger *slog.Logger) *transcodeWorkerPlainHTTP {
	setting := &transcodeWorkerPlainHTTP{environment: environment, manager: manager, logger: logger}
	setting.allowed()
	return setting
}

// allowed reports the value in effect and logs a warning when it turns on.
func (p *transcodeWorkerPlainHTTP) allowed() bool {
	current := p.setting()
	if current.AllowPlainHttp && !p.on.Swap(true) {
		source := "EncodingOptions.AllowPlainHttpTranscodeWorkers"
		if current.SetByEnvironment {
			source = envWorkerAllowHTTP
		}
		p.logger.Warn("transcode workers may connect over plain HTTP; media and segments cross the network unencrypted", "setting", source)
	}
	if !current.AllowPlainHttp {
		p.on.Store(false)
	}
	return current.AllowPlainHttp
}

// setting returns the value in effect and its sources, for the admin API.
func (p *transcodeWorkerPlainHTTP) setting() dto.TranscodeWorkerPlainHTTP {
	var result dto.TranscodeWorkerPlainHTTP
	if configured := p.manager.Encoding().AllowPlainHttpTranscodeWorkers; configured != nil {
		result.Configured = *configured
	}
	result.AllowPlainHttp = result.Configured
	if p.environment != nil {
		result.SetByEnvironment = true
		result.AllowPlainHttp = *p.environment
	}
	return result
}

// transcodeWorkerPolicy returns the placement policy at startup (#616): the
// stored policy, or the default (every job runs locally). A set
// DEADFIN_TRANSCODE_WORKER_MODE replaces its mode, so a GitOps install needs
// no dashboard. A later policy PUT replaces the policy until the restart.
func transcodeWorkerPolicy(ctx context.Context, workers *store.TranscodeWorkerStore, getenv func(string) string) (transcodeworker.Policy, error) {
	policy, err := transcodeworker.DecodeStoredPolicy(workers.Policy(ctx))
	if err != nil {
		return transcodeworker.Policy{}, err
	}
	if mode := strings.TrimSpace(getenv(envWorkerMode)); mode != "" {
		policy.Mode = transcodeworker.Mode(mode)
		if policy, _, err = transcodeworker.NormalizePolicy(policy, nil); err != nil {
			return transcodeworker.Policy{}, fmt.Errorf("%s: %w", envWorkerMode, err)
		}
	}
	return policy, nil
}

// transcodeScalingTokens returns the static scaling tokens (#617): a
// comma-separated list of "dfs1." tokens, for a GitOps scraper or
// autoscaler. Each token reads only the metrics and the scaling signals.
func transcodeScalingTokens(getenv func(string) string) ([]string, error) {
	var tokens []string
	for _, value := range strings.Split(getenv(envScalingTokens), ",") {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if !strings.HasPrefix(value, "dfs1.") || len(value) < 32 {
			return nil, fmt.Errorf("%s: a token is not a dfs1. scaling token of at least 32 characters", envScalingTokens)
		}
		tokens = append(tokens, value)
	}
	return tokens, nil
}

// transcodeJobLease returns the job lease (#618): how long a worker keeps a
// job running without a server connection, so the job survives a server
// restart. "0" turns job records off: a job stops with its connection.
func transcodeJobLease(getenv func(string) string) (time.Duration, error) {
	value := strings.TrimSpace(getenv(envJobLease))
	if value == "" {
		return transcodeworker.DefaultLease, nil
	}
	if value == "0" {
		return 0, nil
	}
	lease, err := time.ParseDuration(value)
	if err != nil || lease < 0 {
		return 0, fmt.Errorf("%s: %q is not a duration such as 60s", envJobLease, value)
	}
	return lease, nil
}

// keptTranscodeFamilies returns the families of the job records with a live
// lease (#618), so the startup wipe keeps their files for the adoption. It
// deletes the expired records first.
func keptTranscodeFamilies(ctx context.Context, workers *store.TranscodeWorkerStore, now time.Time) ([]ffmpeg.KeptFamily, error) {
	if err := workers.DeleteExpiredJobs(ctx, now); err != nil {
		return nil, err
	}
	jobs, err := workers.LiveJobs(ctx, now)
	if err != nil {
		return nil, err
	}
	kept := make([]ffmpeg.KeptFamily, 0, len(jobs))
	for _, job := range jobs {
		kept = append(kept, ffmpeg.KeptFamily{Playlist: job.Playlist, SegmentPattern: job.SegmentPattern})
	}
	return kept, nil
}

type pathMapFlag []transcodeworker.PathMap

func (f *pathMapFlag) String() string { return fmt.Sprint(*f) }

func (f *pathMapFlag) Set(value string) error {
	server, local, ok := strings.Cut(value, "=")
	if !ok || !filepath.IsAbs(server) || !filepath.IsAbs(local) {
		return errors.New("path map must be /server/prefix=/local/prefix")
	}
	*f = append(*f, transcodeworker.PathMap{Server: filepath.Clean(server), Local: filepath.Clean(local)})
	return nil
}

// runWorker is "deadfin worker", the transcode worker agent. "deadfin worker
// fingerprint" prints the pin of a certificate for a join token.
func runWorker(args []string) {
	if len(args) > 0 && args[0] == "scaling-token" {
		token, err := transcodeworker.NewScalingToken()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(token)
		return
	}
	if len(args) > 0 && args[0] == "fingerprint" {
		fingerprint, err := workerFingerprintCommand(args[1:])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(fingerprint)
		return
	}
	flags := flag.NewFlagSet("worker", flag.ExitOnError)
	server := flags.String("server", os.Getenv(envWorkerServer), "server base URL, including any base path (https://host/jellyfin; http only on loopback)")
	tokenFile := flags.String("token-file", "", "file holding the token (default: "+envWorkerToken+")")
	stateDir := flags.String("state-dir", "", "directory that keeps the identity a dashboard join token enrolls")
	hostname, _ := os.Hostname()
	name := flags.String("name", hostname, "worker name shown to the server")
	workDir := flags.String("work-dir", filepath.Join(os.TempDir(), "deadfin-worker"), "scratch directory for job output")
	ffmpegPath := flags.String("ffmpeg", "ffmpeg", "ffmpeg executable")
	maxJobs := flags.Int("max-jobs", 1, "concurrent job limit (set to the hardware session limit)")
	hwaccels := flags.String("hwaccel", "", "comma-separated hardware families this worker can open (qsv,vaapi,cuda); default: the families of -hw-type")
	hwType := flags.String("hw-type", "", "hardware family the server builds this worker's argv for: qsv, vaapi or nvenc; empty is software")
	renderNode := flags.String("render-node", "/dev/dri/renderD128", "render node of -hw-type qsv or vaapi on this host")
	hwDecode := flags.String("hw-decode", "", "comma-separated codecs to decode in hardware; default: the server's setting")
	hwSessions := flags.Int("hw-sessions", 0, "concurrent jobs that use the hardware family (for example the NVENC session limit); 0 is -max-jobs")
	pools := flags.String("pool", "", "comma-separated pool labels proposed at enrollment; the server owns them after")
	inputMode := flags.String("input-mode", store.TranscodeWorkerInputAuto, "media input: shared (read the mapped mount), stream (fetch from the server) or auto; a dashboard-enrolled worker proposes it once")
	drainGrace := flags.Duration("drain-grace", 25*time.Second, "after SIGTERM, how long the worker waits for the server to hand its jobs off before it stops them")
	maxHeld := flags.Int("max-held-segments", 64, "segments a job keeps on disk while the server is away; at the limit ffmpeg pauses")
	ephemeral := flags.Bool("ephemeral", envBool(os.Getenv(envWorkerEphemeral)), "run without durable state, such as a Deployment replica: enroll with the join token at each start; the server deletes the identity when the connection ends (default: "+envWorkerEphemeral+")")
	insecure := flags.Bool("insecure", false, "allow plain http to a non-loopback server (the server must allow it too)")
	var maps pathMapFlag
	flags.Var(&maps, "path-map", "server media prefix mapped to a local mount, /server=/local (repeatable)")
	_ = flags.Parse(args)
	if !store.ValidTranscodeWorkerInputMode(*inputMode) {
		log.Fatalf("-input-mode %q is not shared, stream or auto", *inputMode)
	}

	tokenValue := os.Getenv(envWorkerToken)
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatal(err)
		}
		tokenValue = string(data)
	}
	var token transcodeworker.Token
	if strings.TrimSpace(tokenValue) != "" {
		parsed, err := transcodeworker.ParseToken(tokenValue)
		if err != nil {
			log.Fatal(err)
		}
		token = parsed
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	probed, err := ffmpeg.ProbeCapabilities(ctx, *ffmpegPath)
	if err != nil {
		log.Fatalf("probe ffmpeg: %v", err)
	}
	capabilities := transcodeworker.Capabilities{Encoders: probed.Encoders, FFmpegVersion: probed.Version.String(), MaxJobs: *maxJobs, InputMode: *inputMode, HardwareSessions: *hwSessions}
	hardware, err := workerHardware(*hwType, *renderNode, splitList(*hwDecode))
	if err != nil {
		log.Fatal(err)
	}
	encodingCaps := probeWorkerEncoding(ctx, *ffmpegPath, probed, hardware)
	capabilities.Encoding = &encodingCaps
	if hardware.Type != "" {
		capabilities.Hardware = &hardware
	}
	capabilities.HWAccels = splitList(*hwaccels)
	if len(capabilities.HWAccels) == 0 {
		capabilities.HWAccels = hardwareFamilies(hardware.Type, encodingCaps.HWAccels)
	}
	for _, pool := range strings.Split(*pools, ",") {
		if pool = strings.TrimSpace(pool); pool != "" {
			capabilities.Pools = append(capabilities.Pools, pool)
		}
	}
	agent, err := transcodeworker.NewAgent(transcodeworker.AgentConfig{
		ServerURL: *server, StateDir: *stateDir, JoinToken: token, Name: *name, FFmpegPath: *ffmpegPath, WorkDir: *workDir,
		Capabilities: capabilities, PathMaps: maps, Clock: clock.NewReal(), PKeyPause: probed.PKeyPause,
		AllowInsecure: *insecure, Logger: slog.Default(), MaxHeldSegments: *maxHeld, Ephemeral: *ephemeral,
	})
	if err != nil {
		log.Fatal(err)
	}
	done := make(chan struct{})
	go drainOnSignal(signals, agent, *drainGrace, clock.NewReal(), cancel, done)
	err = agent.Run(ctx)
	close(done)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// drainOnSignal drains the agent on the first signal (#617): the server
// hands its jobs off, and Run returns when they are gone. A second signal or
// the grace period stops the agent at once. It returns when done closes.
func drainOnSignal(signals <-chan os.Signal, agent *transcodeworker.Agent, grace time.Duration, clk clock.Clock, cancel context.CancelFunc, done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-signals:
	}
	slog.Info("transcode worker stopping: handing jobs off to other hosts", "grace", grace)
	agent.Drain()
	timer := clk.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-signals:
		cancel()
	case <-timer.C():
		slog.Warn("transcode worker drain grace ended; stopping the remaining jobs")
		cancel()
	}
}

// workerFingerprintCommand prints the key fingerprint of a PEM certificate
// (the certificate the worker sees: the server's own, or the ingress
// certificate behind a proxy). The admin API takes it as the Fingerprint of
// a join token or as an extra pin.
func workerFingerprintCommand(args []string) (string, error) {
	flags := flag.NewFlagSet("worker fingerprint", flag.ContinueOnError)
	certificate := flags.String("cert", "", "PEM certificate whose key the worker pins")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if *certificate == "" {
		return "", errors.New("deadfin worker fingerprint: -cert is required")
	}
	data, err := os.ReadFile(*certificate)
	if err != nil {
		return "", err
	}
	fingerprint, err := pemCertificateFingerprint(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", *certificate, err)
	}
	return fingerprint, nil
}

// pemCertificateFingerprint returns the token fingerprint of the first
// certificate (the leaf) in a PEM bundle.
func pemCertificateFingerprint(data []byte) (string, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return "", errors.New("no PEM certificate found")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		return transcodeworker.CertificateFingerprint(certificate), nil
	}
}

// workerHardware validates the worker's hardware flags (#622).
func workerHardware(hwType, renderNode string, decode []string) (encoding.HostHardware, error) {
	hardware := encoding.HostHardware{Type: strings.ToLower(strings.TrimSpace(hwType)), DecodingCodecs: decode}
	switch hardware.Type {
	case "", encoding.HardwareAccelerationNone:
		return encoding.HostHardware{}, nil
	case encoding.HardwareAccelerationQSV, encoding.HardwareAccelerationVAAPI:
		if !strings.HasPrefix(renderNode, "/dev/") {
			return encoding.HostHardware{}, fmt.Errorf("-render-node %q is not a device path", renderNode)
		}
		hardware.VaapiDevice, hardware.QsvDevice = renderNode, renderNode
	case encoding.HardwareAccelerationNVENC:
	default:
		return encoding.HostHardware{}, fmt.Errorf("-hw-type %q is not qsv, vaapi or nvenc", hwType)
	}
	return hardware, nil
}

// probeWorkerEncoding runs the server's encoding probes on the worker: the
// NVENC encoders that open for nvenc, and the VAAPI driver for vaapi.
func probeWorkerEncoding(ctx context.Context, ffmpegPath string, probed ffmpeg.Capabilities, hardware encoding.HostHardware) encoding.Capabilities {
	var nvidia ffmpeg.NvidiaCapabilities
	if hardware.Type == encoding.HardwareAccelerationNVENC {
		probe, err := ffmpeg.ProbeNvidia(ctx, ffmpegPath, probed)
		if err != nil {
			slog.Warn("NVIDIA probe failed; NVENC stays off on this worker", "error", err.Error())
		}
		nvidia = probe
	}
	caps := ffmpeg.EncodingCapabilities(probed, nvidia)
	if hardware.Type == encoding.HardwareAccelerationVAAPI {
		driver, err := ffmpeg.ProbeVaapiDriver(ctx, ffmpegPath, hardware.VaapiDevice)
		if err != nil {
			slog.Warn("VAAPI driver probe failed; VAAPI stays off on this worker", "device", hardware.VaapiDevice, "error", err.Error())
		}
		caps.VaapiDriver = driver
	}
	return caps
}

// hardwareFamilies are the device families that an argv for hwType opens,
// as far as the worker's ffmpeg supports them.
func hardwareFamilies(hwType string, probed []string) []string {
	var wanted []string
	switch hwType {
	case encoding.HardwareAccelerationQSV:
		wanted = []string{"qsv", "vaapi", "opencl"}
	case encoding.HardwareAccelerationVAAPI:
		wanted = []string{"vaapi", "opencl", "vulkan"}
	case encoding.HardwareAccelerationNVENC:
		wanted = []string{"cuda", "opencl", "vulkan"}
	}
	var families []string
	for _, family := range wanted {
		if containsCapability(probed, family) {
			families = append(families, family)
		}
	}
	return families
}

func splitList(value string) []string {
	var values []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

// envBool reads a boolean variable; anything that is not a boolean is
// false.
func envBool(value string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && parsed
}
