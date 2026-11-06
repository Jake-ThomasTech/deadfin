// SPDX-License-Identifier: GPL-2.0-only

// Package edge is the stream edge of D21 (#619): a small process, `deadfin
// edge`, that keeps HLS segments playable while the server process
// restarts. Transcode workers mirror each completed segment to the edge;
// the edge serves a segment request from that copy when the request carries
// a valid playback grant, and forwards every other request to the server.
// It also serves static direct streams from the media library (#621) with
// the direct grants that the server adds to PlaybackInfo responses.
// The server signs the grants into the segment URLs it renders, so client
// URLs keep their paths. The edge reads no user database: a grant is an
// HMAC over its claims with a key that the server and the edge share.
package edge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

const (
	grantPrefix = "dfe1"
	// GrantQuery is the query parameter that carries a read grant on a
	// segment URL.
	GrantQuery = "DeadfinEdge"
	// GrantHeader carries an upload grant on a worker's mirror request.
	GrantHeader = "X-Deadfin-Edge-Grant"
	// MinKeyLength is the shortest shared key the edge accepts.
	MinKeyLength = 16

	kindRead   = "r"
	kindUpload = "u"
	kindDirect = "d"
)

var (
	// ErrGrant reports a grant that is malformed, forged, or for another
	// use.
	ErrGrant = errors.New("edge grant is not valid")
	// ErrGrantExpired reports a grant past its expiry.
	ErrGrantExpired = errors.New("edge grant has expired")
)

// Family names one HLS family: Stem is the playlist file stem, and Pattern
// is the segment file name with a "%d" index directive.
type Family struct {
	Stem    string
	Pattern string
}

// FileName is the file name of segment index.
func (family Family) FileName(index int) string {
	before, after, _ := strings.Cut(family.Pattern, "%d")
	return before + strconv.Itoa(index) + after
}

// Owns reports whether name is a file of the family: a segment, or another
// file that shares the playlist stem, such as the fMP4 init segment. The
// playlist itself is not mirrored.
func (family Family) Owns(name string) bool {
	if !validName(name) || strings.HasSuffix(name, ".m3u8") {
		return false
	}
	before, after, _ := strings.Cut(family.Pattern, "%d")
	if strings.HasPrefix(name, before) && strings.HasSuffix(name, after) && len(name) > len(before)+len(after) {
		digits := strings.TrimPrefix(name[len(before):len(name)-len(after)], "-")
		if digits != "" && strings.Trim(digits, "0123456789") == "" {
			return true
		}
	}
	return family.Stem != "" && strings.HasPrefix(name, family.Stem)
}

func (family Family) valid() bool {
	return validName(family.Stem) && validName(strings.Replace(family.Pattern, "%d", "0", 1)) &&
		strings.Count(family.Pattern, "%") == 1 && strings.Contains(family.Pattern, "%d")
}

// ReadGrant authorizes reading the segments of one family for one item and
// play session.
type ReadGrant struct {
	ItemID        dto.UUID
	PlaySessionID string
	Family        Family
	Expires       time.Time
}

// UploadGrant authorizes a worker to mirror the files of one family.
type UploadGrant struct {
	Family  Family
	Expires time.Time
}

// DirectGrant authorizes reading one media source file by range for one
// item (#621). PlaySessionID is empty when the grant was issued to an API
// key with no user; such a grant is not a PlaySessionId credential.
type DirectGrant struct {
	ItemID        dto.UUID
	MediaSourceID string
	PlaySessionID string
	// Path is the server's path of the source file; Size is its length in
	// bytes, or 0 when it is not known.
	Path      string
	Size      int64
	Container string
	Expires   time.Time
}

type claims struct {
	Kind          string `json:"k"`
	ItemID        string `json:"i,omitempty"`
	PlaySessionID string `json:"s,omitempty"`
	Stem          string `json:"f,omitempty"`
	Pattern       string `json:"p,omitempty"`
	Source        string `json:"m,omitempty"`
	Path          string `json:"a,omitempty"`
	Size          int64  `json:"z,omitempty"`
	Container     string `json:"c,omitempty"`
	Expires       int64  `json:"e"`
}

// Signer signs and verifies grants with the shared key.
type Signer struct {
	key [sha256.Size]byte
}

// NewSigner derives the HMAC key from the shared secret.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < MinKeyLength {
		return nil, errors.New("edge key is shorter than 16 bytes")
	}
	return &Signer{key: sha256.Sum256([]byte("deadfin-edge-grant\x00" + secret))}, nil
}

// SignRead returns the token of grant.
func (signer *Signer) SignRead(grant ReadGrant) string {
	return signer.sign(claims{Kind: kindRead, ItemID: grant.ItemID.String(), PlaySessionID: grant.PlaySessionID,
		Stem: grant.Family.Stem, Pattern: grant.Family.Pattern, Expires: grant.Expires.Unix()})
}

// SignUpload returns the token of grant.
func (signer *Signer) SignUpload(grant UploadGrant) string {
	return signer.sign(claims{Kind: kindUpload, Stem: grant.Family.Stem, Pattern: grant.Family.Pattern, Expires: grant.Expires.Unix()})
}

// SignDirect returns the token of grant.
func (signer *Signer) SignDirect(grant DirectGrant) string {
	return signer.sign(claims{Kind: kindDirect, ItemID: grant.ItemID.String(), PlaySessionID: grant.PlaySessionID,
		Source: grant.MediaSourceID, Path: grant.Path, Size: grant.Size, Container: grant.Container, Expires: grant.Expires.Unix()})
}

// VerifyDirect checks a direct grant at now.
func (signer *Signer) VerifyDirect(token string, now time.Time) (DirectGrant, error) {
	c, err := signer.verify(token, kindDirect, now)
	if err != nil {
		return DirectGrant{}, err
	}
	item, err := dto.ParseUUID(c.ItemID)
	if err != nil {
		return DirectGrant{}, ErrGrant
	}
	return DirectGrant{ItemID: item, MediaSourceID: c.Source, PlaySessionID: c.PlaySessionID, Path: c.Path,
		Size: c.Size, Container: c.Container, Expires: time.Unix(c.Expires, 0)}, nil
}

// VerifyRead checks a read grant at now.
func (signer *Signer) VerifyRead(token string, now time.Time) (ReadGrant, error) {
	c, err := signer.verify(token, kindRead, now)
	if err != nil {
		return ReadGrant{}, err
	}
	item, err := dto.ParseUUID(c.ItemID)
	if err != nil {
		return ReadGrant{}, ErrGrant
	}
	return ReadGrant{ItemID: item, PlaySessionID: c.PlaySessionID, Family: Family{Stem: c.Stem, Pattern: c.Pattern}, Expires: time.Unix(c.Expires, 0)}, nil
}

// VerifyUpload checks an upload grant at now.
func (signer *Signer) VerifyUpload(token string, now time.Time) (UploadGrant, error) {
	c, err := signer.verify(token, kindUpload, now)
	if err != nil {
		return UploadGrant{}, err
	}
	return UploadGrant{Family: Family{Stem: c.Stem, Pattern: c.Pattern}, Expires: time.Unix(c.Expires, 0)}, nil
}

func (signer *Signer) sign(c claims) string {
	payload, _ := json.Marshal(c)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return grantPrefix + "." + encoded + "." + base64.RawURLEncoding.EncodeToString(signer.mac(encoded))
}

func (signer *Signer) mac(encoded string) []byte {
	mac := hmac.New(sha256.New, signer.key[:])
	mac.Write([]byte(grantPrefix + "." + encoded))
	return mac.Sum(nil)
}

func (signer *Signer) verify(token, kind string, now time.Time) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != grantPrefix {
		return claims{}, ErrGrant
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, signer.mac(parts[1])) {
		return claims{}, ErrGrant
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims{}, ErrGrant
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil || c.Kind != kind || !c.valid() {
		return claims{}, ErrGrant
	}
	if !now.Before(time.Unix(c.Expires, 0)) {
		return claims{}, ErrGrantExpired
	}
	return c, nil
}

func (c claims) valid() bool {
	if c.Kind == kindDirect {
		return c.Source != "" && strings.HasPrefix(c.Path, "/") && c.Size >= 0
	}
	return Family{Stem: c.Stem, Pattern: c.Pattern}.valid()
}

// validName accepts a portable file name with no path separator.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || name[0] == '.' {
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
