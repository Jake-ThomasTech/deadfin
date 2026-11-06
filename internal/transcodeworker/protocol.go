// SPDX-License-Identifier: GPL-2.0-only

// Package transcodeworker runs live HLS transcodes on remote worker hosts
// (docs/spikes/transcode-workers.md, #614). A worker agent dials out to the
// server's main listener, authenticates with a join token, heartbeats, and
// runs the HLS ffmpeg jobs that the server's Hub places on it through
// ffmpeg.RemoteRunner. Segments and playlists flow back over authenticated
// HTTP PUTs into the server's transcode directory, so the HLS contract the
// clients see does not change. The feature is off unless the operator
// configures a join token.
package transcodeworker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// ProtocolVersion is the control-channel version. A mismatch is a reject.
const ProtocolVersion = 1

// Control message types. The worker sends hello, auth, heartbeat, drain and
// the job.* reports; the server sends challenge, enrolled (first connection with
// a join token only), welcome, pins, reject and the job.* commands.
const (
	TypeHello     = "hello"
	TypeChallenge = "challenge"
	TypeAuth      = "auth"
	TypeEnrolled  = "enrolled"
	TypeWelcome   = "welcome"
	TypePins      = "pins"
	TypeReject    = "reject"
	TypeHeartbeat = "heartbeat"
	// TypeDrain announces a worker that stops: it takes no new job, and the
	// server hands its jobs off to other hosts before it kills them (#617).
	TypeDrain      = "drain"
	TypeJobStart   = "job.start"
	TypeJobStarted = "job.started"
	TypeJobFailed  = "job.failed"
	TypeJobStdin   = "job.stdin"
	TypeJobKill    = "job.kill"
	TypeJobStderr  = "job.stderr"
	TypeJobExited  = "job.exited"
	// TypeJobsHeld lists the jobs a reconnecting worker kept running
	// (#618). The server answers each with job.adopted or job.kill.
	TypeJobsHeld   = "jobs.held"
	TypeJobAdopted = "job.adopted"
)

// Worker routes on the server's main listener, below its base URL. The
// router registers them with a worker credential (router.Route.Worker).
const (
	PathConnect = "/deadfin/v1/workers/connect"
	PathSource  = "/deadfin/v1/workers/jobs/{id}/source"
	PathFile    = "/deadfin/v1/workers/jobs/{id}/files/{name}"
	PathAsset   = "/deadfin/v1/workers/jobs/{id}/assets/{name}"
	// PathOutput appends to the output of a progressive job (#624).
	PathOutput = "/deadfin/v1/workers/jobs/{id}/output"
	pathJobs   = "/deadfin/v1/workers/jobs/"
)

// ReplicaHeader names, on a worker job request, the server replica of the
// worker's control connection (#652). Another replica forwards the request
// there.
const ReplicaHeader = "Deadfin-Replica"

// Placeholders in JobSpec.Args that the worker replaces.
const (
	PlaceholderSource = "{{source}}"
	PlaceholderOut    = "{{out}}"
)

// Message is the one control-channel envelope. Only the fields of its Type
// are set.
type Message struct {
	Type  string `json:"type"`
	Proto int    `json:"proto,omitempty"`
	// TokenID names a join token in hello; WorkerID names an enrolled
	// identity in hello, enrolled and welcome. Exactly one is set in hello.
	TokenID string `json:"tokenId,omitempty"`
	// Ephemeral marks a worker without durable state, such as a Deployment
	// replica (#623): the server deletes its identity when its connection
	// ends.
	Ephemeral    bool          `json:"ephemeral,omitempty"`
	Name         string        `json:"name,omitempty"`
	Nonce        []byte        `json:"nonce,omitempty"`
	Proof        []byte        `json:"proof,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Load         *Load         `json:"load,omitempty"`
	WorkerID     string        `json:"workerId,omitempty"`
	// WorkerSecret crosses the wire once, in enrolled, inside TLS and the
	// authenticated channel.
	WorkerSecret string `json:"workerSecret,omitempty"`
	// Pins are the extra server key fingerprints (welcome and pins).
	Pins            []string `json:"pins,omitempty"`
	HeartbeatMillis int64    `json:"heartbeatMillis,omitempty"`
	// Replica names the server replica of the connection in welcome
	// (#652). The worker sends it back in ReplicaHeader on job requests.
	Replica string   `json:"replica,omitempty"`
	Job     *JobSpec `json:"job,omitempty"`
	JobID   string   `json:"jobId,omitempty"`
	Data    []byte   `json:"data,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Held is the jobs.held payload; LeaseMillis is the renewed lease in
	// job.adopted.
	Held        []HeldJob `json:"held,omitempty"`
	LeaseMillis int64     `json:"leaseMillis,omitempty"`
}

// HeldJob is one job that a worker kept running while it had no server.
type HeldJob struct {
	JobID string `json:"jobId"`
	// NextSegment is the lowest segment index the worker has not uploaded.
	NextSegment int `json:"nextSegment"`
	// Files are the family files the worker still has to upload.
	Files []string `json:"files,omitempty"`
}

// Capabilities is what a worker reports at registration.
type Capabilities struct {
	// HWAccels lists the hardware families the worker can open: "qsv",
	// "vaapi", "cuda", "videotoolbox". Software encoding is always present.
	HWAccels []string `json:"hwaccels,omitempty"`
	// Encoders is the probed ffmpeg encoder list. Empty means "not probed".
	Encoders      []string `json:"encoders,omitempty"`
	ToneMapping   bool     `json:"toneMapping,omitempty"`
	FFmpegVersion string   `json:"ffmpegVersion,omitempty"`
	// MaxJobs is the concurrent job limit. Set it to the hardware session
	// limit (for example the NVENC session cap, #420).
	MaxJobs int `json:"maxJobs"`
	// Pools are the worker's own pool labels ("deadfin worker -pool"). They
	// seed the identity at enrollment; afterwards the operator owns them.
	Pools []string `json:"pools,omitempty"`
	// InputMode is the worker's own input mode (shared, stream or auto),
	// with the same ownership as Pools.
	InputMode string `json:"inputMode,omitempty"`
	// PathPrefixes are the server media prefixes the worker maps, so the
	// server checks a shared placement before it starts the job.
	PathPrefixes []string `json:"pathPrefixes,omitempty"`
	// Encoding is the worker's probed encoding capabilities, with the
	// projection that the server uses for its own ffmpeg (#622). With it,
	// the server builds each argv for this worker; without it, the worker
	// gets the server's argv when it can run it.
	Encoding *encoding.Capabilities `json:"encoding,omitempty"`
	// Hardware is the worker's hardware family and render nodes.
	Hardware *encoding.HostHardware `json:"hardware,omitempty"`
	// HardwareSessions bounds the jobs whose argv uses the hardware family
	// (for example the NVENC sessions of one GPU, #420); 0 is MaxJobs.
	HardwareSessions int `json:"hardwareSessions,omitempty"`
}

// Load is the heartbeat payload.
type Load struct {
	ActiveJobs int `json:"activeJobs"`
}

// JobSpec is one job placement.
type JobSpec struct {
	ID string `json:"id"`
	// Token authorizes the data plane for this job only.
	Token string `json:"token"`
	// Args is the ffmpeg argv. PlaceholderSource is the input operand;
	// PlaceholderOut prefixes every output path.
	Args []string `json:"args"`
	// SourcePath is the server path; a worker with a matching path map
	// reads the file directly. SourceURL is the server-relative streamed
	// fallback.
	SourcePath string `json:"sourcePath"`
	SourceURL  string `json:"sourceUrl"`
	SourceSize int64  `json:"sourceSize"`
	// Playlist is the playlist file name. SegmentPrefix and SegmentSuffix
	// bracket the integer index of a segment file name.
	Playlist string `json:"playlist"`
	// Output is the one output file name of a progressive job (#624). The
	// worker streams it to PathOutput while ffmpeg writes it. Playlist and
	// the segment fields are then empty.
	Output        string `json:"output,omitempty"`
	SegmentPrefix string `json:"segmentPrefix"`
	SegmentSuffix string `json:"segmentSuffix"`
	// LeaseMillis is how long the worker keeps the job running without a
	// server connection (#618). Zero stops the job when the connection
	// ends.
	LeaseMillis int64 `json:"leaseMillis,omitempty"`
	// EdgeURL is the base URL of the stream edge (#619). The worker mirrors
	// each completed family file there with the EdgeGrant upload grant, so
	// the edge serves the segments while the server restarts. Empty means
	// no edge.
	EdgeURL   string `json:"edgeUrl,omitempty"`
	EdgeGrant string `json:"edgeGrant,omitempty"`
	// Assets are the files of a subtitle burn-in (#624). The worker fetches
	// each from PathAsset before it starts ffmpeg and replaces
	// ffmpeg.RemoteAssetsPlaceholder in Args with its asset directory.
	Assets []JobAsset `json:"assets,omitempty"`
}

// JobAsset names one job asset and its size.
type JobAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// assetName matches the asset names: the subtitle and the fonts.
var assetName = regexp.MustCompile(`^(?:subtitle|font[0-9]{3})(?:\.[a-z0-9]{1,8})?$`)

// fontAsset reports an asset that belongs in the fonts directory.
func fontAsset(name string) bool { return strings.HasPrefix(name, "font") }

// Token is a parsed join token: "dfw1.<id>.<secret>.<fingerprint>". The
// fingerprint is empty when the worker verifies the server certificate
// through a CA it trusts.
type Token struct {
	ID     string
	Secret string
	// Fingerprint is the base64url SHA-256 of the server certificate's
	// SubjectPublicKeyInfo. The worker pins that key when it is set.
	Fingerprint string
}

const tokenPrefix = "dfw1"

// NewToken creates a random join token that pins fingerprint (may be empty).
func NewToken(fingerprint string) (string, error) {
	if fingerprint != "" && !ValidFingerprint(fingerprint) {
		return "", errors.New("certificate fingerprint is not a base64url SHA-256")
	}
	id := make([]byte, 6)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	return tokenPrefix + "." + encode(id) + "." + encode(secret) + "." + fingerprint, nil
}

// ParseToken splits a join token.
func ParseToken(value string) (Token, error) {
	malformed := errors.New("transcode worker token is malformed")
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != 4 || parts[0] != tokenPrefix || parts[1] == "" || len(parts[2]) < 32 {
		return Token{}, malformed
	}
	if parts[3] != "" && !ValidFingerprint(parts[3]) {
		return Token{}, malformed
	}
	return Token{ID: parts[1], Secret: parts[2], Fingerprint: parts[3]}, nil
}

// ValidFingerprint reports whether value is a base64url SHA-256, the form of
// a certificate fingerprint.
func ValidFingerprint(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

// CertificateFingerprint is the token fingerprint of a certificate: the
// base64url SHA-256 of its SubjectPublicKeyInfo.
func CertificateFingerprint(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Key derives the shared key that both sides use for the handshake proofs.
// The server stores only this key; the secret never crosses the wire.
func (t Token) Key() []byte { return deriveKey(t.Secret) }

func proof(key []byte, role string, workerNonce, serverNonce []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s\x00%d\x00", role, ProtocolVersion)
	_, _ = mac.Write(workerNonce)
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(serverNonce)
	return mac.Sum(nil)
}

func newNonce() ([]byte, error) {
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	return nonce, err
}

// newSecret is a worker identity secret: 32 random bytes, base64url.
func newSecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// validArtifactName accepts a bare file name of the family: no separators,
// no dot files, a restricted alphabet.
func validArtifactName(name string) bool {
	if name == "" || len(name) > 255 || name[0] == '.' {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
