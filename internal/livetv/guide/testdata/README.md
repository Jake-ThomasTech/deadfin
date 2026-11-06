# Guide test data

- `upstream/*.xml` are copied unchanged from the pinned Jellyfin tree,
  `tests/Jellyfin.LiveTv.Tests/Test Data/LiveTv/Listings/XmlTv/` (GPL-2.0).
  `provider_test.go` ports the tests that read them.
- `fakehdhr-quirks.xml` and `fakehdhr-quirks.m3u` are the guide and playlist
  of `go run ./tools/fakehdhr -guide-anchor 2026-03-08T00:00:00Z
  -guide-days 1 -no-fault-channels -quirks all`.
- `fakehdhr-guide.xml.gz` is the guide of `go run ./tools/fakehdhr
  -guide-anchor 2026-03-08T00:00:00Z -guide-days 2 -quirks none`, for the
  reader benchmark.
