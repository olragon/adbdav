# Contributing

Thanks for looking. This is a small, deliberately boring codebase: four packages, one
dependency that matters, and a test suite that runs without a phone attached.

## Getting set up

```sh
git clone <this repo> && cd adbdav
make check          # gofmt + go vet + go test — no device needed
make build          # ./adbdav
```

The tests drive the WebDAV layer against an in-memory fake device (`internal/adbfs`), so you
can develop most of this on a machine with no Android hardware. Only `internal/adb`,
`internal/mnt` and the benchmark need a real device and a real Mac.

## What a good change looks like

- **A bug fix comes with the test that fails without it.** Every entry under *What this cost
  to get right* in the README is pinned by a test, because each one was found the hard way and
  none of them are obvious enough to survive a refactor unassisted.
- **Measure before and after when the change is about speed.** `./bench/bench.sh` prints the
  table the README quotes; include both runs in the pull request. Loopback WebDAV is fast and
  `adb` is not, so a plausible-sounding optimisation is usually measuring the wrong layer.
- **Keep the layers apart.** `internal/adb` knows about shells and devices and nothing about
  HTTP; `internal/adbfs` knows about WebDAV and talks to a `Device` interface. If a change
  needs those two to know about each other, it probably belongs in `internal/session`.
- **Default to safe.** The zero value of a `session.Config` is a read-only mount, and the zero
  value of `CacheTTL` is no caching. A change that makes an omitted setting more dangerous than
  an explicit one will be sent back.

## Things that are known-missing

- `mtime` is not preserved on upload.
- There is no login item / LaunchAgent for the menu bar app.
- Linux and Windows can run `adbdav serve`; only macOS can `adbdav mount`.

## Reporting a bug

Please include the output of `adbdav status`, your macOS version, and `adb --version`. If it is
about speed, include `./bench/bench.sh`. If it is about a specific file, the useful detail is
almost always the exact name — spaces, quotes and non-ASCII in filenames are the interesting
cases, since every device command goes through a shell on the phone.
