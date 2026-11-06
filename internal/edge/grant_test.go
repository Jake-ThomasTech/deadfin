// SPDX-License-Identifier: GPL-2.0-only

package edge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
)

var testEpoch = time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)

func testSigner(t *testing.T, key string) *Signer {
	t.Helper()
	signer, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestReadGrantSignAndVerify(t *testing.T) {
	signer := testSigner(t, "shared-edge-key-0123456789")
	item := dto.UUID{0xab, 0x01}
	family := Family{Stem: "f00d", Pattern: "f00d%d.ts"}
	token := signer.SignRead(ReadGrant{ItemID: item, PlaySessionID: "play-1", Family: family, Expires: testEpoch.Add(time.Hour)})

	grant, err := signer.VerifyRead(token, testEpoch)
	if err != nil || grant.ItemID != item || grant.PlaySessionID != "play-1" || grant.Family != family || !grant.Expires.Equal(testEpoch.Add(time.Hour)) {
		t.Fatalf("VerifyRead = %+v, %v", grant, err)
	}
	if _, err := signer.VerifyRead(token, testEpoch.Add(time.Hour)); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired grant: %v", err)
	}
	if _, err := testSigner(t, "another-edge-key-0123456789").VerifyRead(token, testEpoch); !errors.Is(err, ErrGrant) {
		t.Fatalf("grant of another key: %v", err)
	}
	if _, err := signer.VerifyUpload(token, testEpoch); !errors.Is(err, ErrGrant) {
		t.Fatalf("a read grant verified as an upload grant: %v", err)
	}
	parts := strings.Split(token, ".")
	other := signer.SignRead(ReadGrant{ItemID: dto.UUID{0xcd}, Family: family, Expires: testEpoch.Add(time.Hour)})
	forged := parts[0] + "." + strings.Split(other, ".")[1] + "." + parts[2]
	if _, err := signer.VerifyRead(forged, testEpoch); !errors.Is(err, ErrGrant) {
		t.Fatalf("a payload swap verified: %v", err)
	}
	for _, bad := range []string{"", "dfe1", "dfe1.x.y", "dfe2." + parts[1] + "." + parts[2]} {
		if _, err := signer.VerifyRead(bad, testEpoch); !errors.Is(err, ErrGrant) {
			t.Fatalf("VerifyRead(%q) = %v", bad, err)
		}
	}
	if _, err := NewSigner("short"); err == nil {
		t.Fatal("a short key was accepted")
	}
}

func TestUploadGrantAndFamilyFiles(t *testing.T) {
	signer := testSigner(t, "shared-edge-key-0123456789")
	family := Family{Stem: "f00d", Pattern: "f00d%d.mp4"}
	grant, err := signer.VerifyUpload(signer.SignUpload(UploadGrant{Family: family, Expires: testEpoch.Add(time.Minute)}), testEpoch)
	if err != nil || grant.Family != family {
		t.Fatalf("VerifyUpload = %+v, %v", grant, err)
	}
	if bad := signer.SignUpload(UploadGrant{Family: Family{Stem: "../x", Pattern: "x%d.ts"}, Expires: testEpoch.Add(time.Minute)}); bad != "" {
		if _, err := signer.VerifyUpload(bad, testEpoch); !errors.Is(err, ErrGrant) {
			t.Fatalf("a grant for an unsafe family verified: %v", err)
		}
	}
	if got := family.FileName(12); got != "f00d12.mp4" {
		t.Fatalf("FileName(12) = %q", got)
	}
	if got := family.FileName(-1); got != "f00d-1.mp4" {
		t.Fatalf("FileName(-1) = %q", got)
	}
	for name, want := range map[string]bool{
		"f00d0.mp4": true, "f00d-1.mp4": true, "f00d12.mp4": true, "f00d.m3u8": false, "beef0.mp4": false,
		"../f00d0.mp4": false, ".f00d0.mp4": false, "f00dx.mp4": true,
	} {
		if got := family.Owns(name); got != want {
			t.Errorf("Owns(%q) = %v, want %v", name, got, want)
		}
	}
}
