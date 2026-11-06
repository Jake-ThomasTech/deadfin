// SPDX-License-Identifier: GPL-2.0-only

package config_test

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
)

// Source: upstream/jellyfin/MediaBrowser.Model/Configuration/EncodingOptions.cs.

func TestManagerLoadsJellyfinEncodingXMLAndReplacesDefaultArrays(t *testing.T) {
	paths := mustPaths(t)
	const source = `<?xml version="1.0" encoding="utf-8"?>
<EncodingOptions xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <HardwareAccelerationType>qsv</HardwareAccelerationType>
  <EnableTonemapping>true</EnableTonemapping>
  <EnableVppTonemapping>true</EnableVppTonemapping>
  <TonemappingPeak>120</TonemappingPeak>
  <EnableIntelLowPowerH264HwEncoder>true</EnableIntelLowPowerH264HwEncoder>
  <HardwareDecodingCodecs>
    <string>h264</string>
    <string>hevc</string>
    <string>vp9</string>
  </HardwareDecodingCodecs>
  <UnknownFutureField>ignored</UnknownFutureField>
</EncodingOptions>`
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Config, "encoding.xml"), []byte(source), 0o600); err != nil {
		t.Fatalf("write encoding.xml: %v", err)
	}
	manager, err := config.Open(t.Context(), paths, newChangeBus(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := manager.Encoding()
	want := config.DefaultEncodingOptions()
	want.HardwareAccelerationType = "qsv"
	want.EnableTonemapping = true
	want.EnableVppTonemapping = true
	want.TonemappingPeak = 120
	want.EnableIntelLowPowerH264HwEncoder = true
	want.HardwareDecodingCodecs = []string{"h264", "hevc", "vp9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Encoding() = %#v\nwant %#v", got, want)
	}
	got.HardwareDecodingCodecs[0] = "mutated"
	if manager.Encoding().HardwareDecodingCodecs[0] != "h264" {
		t.Fatal("Encoding() returned a shared array")
	}
}

func TestManagerMissingEncodingXMLUsesDefaultsWithoutCreatingFile(t *testing.T) {
	paths := mustPaths(t)
	manager, err := config.Open(t.Context(), paths, newChangeBus(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := manager.Encoding(); !reflect.DeepEqual(got, config.DefaultEncodingOptions()) {
		t.Fatalf("Encoding() = %#v, want defaults", got)
	}
	if _, err := os.Stat(filepath.Join(paths.Config, "encoding.xml")); !os.IsNotExist(err) {
		t.Fatalf("encoding.xml stat error = %v, want not exist", err)
	}
}

func TestEncodingOptionsXMLRoundTripUsesUpstreamArrayShape(t *testing.T) {
	encoded, err := xml.Marshal(config.DefaultEncodingOptions())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), "<HardwareDecodingCodecs><string>h264</string><string>vc1</string></HardwareDecodingCodecs>") {
		t.Fatalf("encoded array shape = %s", encoded)
	}
	decoded := config.DefaultEncodingOptions()
	if err := xml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, config.DefaultEncodingOptions()) {
		t.Fatalf("round trip = %#v", decoded)
	}
}

// AllowPlainHttpTranscodeWorkers is Deadfin-only (D-118, #664): it
// round-trips through the encoding configuration endpoint and encoding.xml,
// a stock save that omits it keeps it, and an unset value stays off the wire.
func TestEncodingAllowPlainHttpTranscodeWorkersRoundTrips(t *testing.T) {
	paths := mustPaths(t)
	manager, err := config.Open(t.Context(), paths, newChangeBus(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	encoded, err := json.Marshal(manager.Encoding())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "AllowPlainHttpTranscodeWorkers") {
		t.Fatalf("unset field on the wire: %s", encoded)
	}
	if err := manager.SaveNamedConfiguration(t.Context(), config.KeyEncoding, []byte(`{"AllowPlainHttpTranscodeWorkers":true}`)); err != nil {
		t.Fatalf("save on: %v", err)
	}
	if got := manager.Encoding().AllowPlainHttpTranscodeWorkers; got == nil || !*got {
		t.Fatalf("after save on = %v, want true", got)
	}
	if err := manager.SaveNamedConfiguration(t.Context(), config.KeyEncoding, []byte(`{"EncoderPreset":"fast"}`)); err != nil {
		t.Fatalf("stock save: %v", err)
	}
	if got := manager.Encoding().AllowPlainHttpTranscodeWorkers; got == nil || !*got {
		t.Fatalf("after stock save = %v, want kept true", got)
	}
	reopened, err := config.Open(t.Context(), paths, newChangeBus(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Encoding().AllowPlainHttpTranscodeWorkers; got == nil || !*got {
		t.Fatalf("after reopen = %v, want true from encoding.xml", got)
	}
	if err := manager.SaveNamedConfiguration(t.Context(), config.KeyEncoding, []byte(`{"AllowPlainHttpTranscodeWorkers":false}`)); err != nil {
		t.Fatalf("save off: %v", err)
	}
	if got := manager.Encoding().AllowPlainHttpTranscodeWorkers; got == nil || *got {
		t.Fatalf("after save off = %v, want false", got)
	}
}
