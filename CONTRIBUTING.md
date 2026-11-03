# Contributing to Deadfin

## Report a bug

Open an issue with the bug report form. Read
[docs/alpha/reporting-bugs.md](docs/alpha/reporting-bugs.md) first. Report
security problems privately; see [SECURITY.md](SECURITY.md).

## Build and test

You need Go 1.26.10, libvips development files with `pkg-config`, and ffmpeg.
`shell.nix` provides all of them (`nix-shell`). On Linux, `make skia` installs
the pinned SkiaSharp native library that the image tests load.

```sh
make build
make test
go vet ./...
go run ./tools/convention-gates -root .
```

## License

Deadfin is `GPL-2.0-only`. By contributing, you agree that your contribution
is licensed under the same terms. Contribute only code that is compatible
with GPL-2.0-only.

## Pull requests

Pull requests are welcome. Keep each one small and focused on one change,
and say which commands you ran. Deadfin follows Jellyfin's behavior; record
an intentional difference in [docs/divergences.md](docs/divergences.md).

This repository is a mirror. The maintainer publishes `master` here from the
upstream repository, so pull requests do not merge on GitHub. Open your pull
request here as usual. The process is:

1. The maintainer reviews the pull request here. One maintainer reviews, so
   a review can take some time.
2. The maintainer applies an accepted change upstream, with you kept as the
   commit author.
3. The change arrives on `master` here with the next publish.
4. The maintainer closes the pull request with a link to the resulting
   commit.

## Questions

Ask in [GitHub Discussions](https://github.com/deadfin-org/deadfin/discussions).
See [SUPPORT.md](SUPPORT.md).

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
