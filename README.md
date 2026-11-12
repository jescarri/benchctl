# benchctl

[![ci](https://github.com/jescarri/benchctl/actions/workflows/ci.yml/badge.svg)](https://github.com/jescarri/benchctl/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/jescarri/benchctl)](https://github.com/jescarri/benchctl/releases)

A SCPI command-line tool for running bench measurements from an agent. It drives a Siglent
oscilloscope (SDS1000X-E), function generator (SDG1000X) and multimeter (SDM3000X) over
VXI-11. A raw socket on TCP 5025 is a last resort, used only when you ask for it with
`--transport raw` (the SDS1000X-E leaks one raw session per connection). Design and hardware findings are in
[docs/adr/0001](docs/adr/0001-benchctl-scpi-instrument-cli.md).

Output is JSON on stdout (`--human` gives tables) and diagnostics on stderr. Exit codes:
`0` ok, `1` instrument/SCPI error, `2` usage, `3` unreachable, `4` verification failed.

## Install

Prebuilt static binaries (linux/darwin × amd64/arm64) are on the
[releases page](https://github.com/jescarri/benchctl/releases):

```sh
v=v0.1.0; os=linux; arch=amd64
curl -fsSL "https://github.com/jescarri/benchctl/releases/download/$v/benchctl-$os-$arch.tar.gz" | tar -xz benchctl
sudo install benchctl /usr/local/bin/
```

Each release has a `checksums.txt` (SHA-256). Or build from source with Go ≥ 1.27:

```sh
go install github.com/jescarri/benchctl/cmd/benchctl@latest
```

## Build

```sh
make            # fmt-check, vet, unit tests, build → bin/benchctl (static)
make test       # unit tests only (no hardware)
make race       # unit tests with -race
make cover      # coverage → bin/coverage.html
make cross      # linux/darwin × amd64/arm64 into bin/
make integration  # hardware tests: drives FG outputs at 1 Vpp into the scope
make help
```

`go` switches to the toolchain pinned in `go.mod` (go1.27.4) on its own. `make lint` needs a
golangci-lint built with Go ≥ 1.27; a plain `go install` builds it with Go 1.26, so force the toolchain:
`GOTOOLCHAIN=go1.27.4 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.

CI (`.github/workflows/ci.yml`) runs `make all`, `race`, `cross` and `lint` on every push to
`master` and every pull request. Pushing a `v*` tag runs `.github/workflows/release.yml`, which
tests, cross-compiles with the tag as the version, and publishes the tarballs and checksums as a
GitHub release:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## Quick start

```sh
benchctl discover --subnet 192.168.2.0/24 --save   # pins instruments by serial number
benchctl --human ping                                # connection test: reach, serial, *IDN? round trip
benchctl --human status
benchctl caps scope                                  # datasheet capability sheet + verified quirks
benchctl wiring show [--plan plan.yaml]              # instructions for the human
benchctl wiring verify [--plan plan.yaml] [--confirm] # electrical check, outputs left OFF;
                                                     # names swaps, 50 Ω/HiZ, 10X probe switch, bad contact

benchctl fg set 1 --wave sine --freq 1k --amp 1Vpp --offset 0 --load hiz
benchctl fg output 1 on                              # outputs are never enabled implicitly
benchctl scope setup 1 --vdiv 200m --offset 0 --coupling dc --probe 1x --on
benchctl scope timebase --tdiv 2m
benchctl scope trigger --source C1 --mode auto --level 0
benchctl scope measure 1 ampl rms pkpk freq         # **** → null, with settings + warnings
benchctl scope capture 1 2 --points 20000 -o cap.csv
benchctl scope autoset                               # waits until applied; before/after settings
benchctl screenshot scope fg dmm -o shots/           # PNG (or .bmp) from every instrument
benchctl dmm config vdc && benchctl dmm read --samples 5
benchctl safe                                        # every output off

benchctl apply examples/fg-scope-smoke.yaml          # verifies the plan's wiring, then runs it
benchctl raw scope "SARA?"                           # any SCPI, error-checked
benchctl raw fg "C1:BSWV FRQ,1000" --wait auto       # wait for completion (srq|poll|opc)
benchctl raw dmm SCDP --read -o dmm.bmp              # read a reply from a command without '?' 
benchctl raw scope "C1:WF? DAT2" --block -o c1.bin
benchctl script fg examples/sdg-burst.scpi
```

The inventory is `~/.config/benchctl/bench.json` (`--config` / `BENCHCTL_CONFIG`). It holds
instruments (serial, model, last IP, optional `transport`), `wiring` (links can include an
`expect` reading for external sources) and `limits` (`fg.max_amp_vpp`, `fg.max_abs_v`,
`fg.max_freq_hz`). They are checked before anything is sent by `fg set`, `apply` `fg.set` steps
and the `wiring verify` test signals. **`raw`, `script` and `raw` plan steps send SCPI as given and
bypass these limits.** An invalid limit value (e.g. `"5volts"`) is a config error, not "no limit".

Global flags: `--timeout` (per SCPI operation, default 5s), `--transport auto|vxi11|raw` (`auto` = VXI-11 only; a VXI-11 failure tells you to retry with `raw`),
`--ip`, `--verbose` (SCPI trace on stderr), `--lock-wait`.

## Layout

```
cmd/benchctl/            CLI adapter (urfave/cli/v3), JSON/human output
internal/bench/domain/   pure types: units, measurements, inventory, error kinds
internal/bench/port/     interfaces: Transport, SCPI, Scope, Generator, DMM, ...
internal/bench/app/      use cases: discover, status, measure, capture, wiring, apply, safe
internal/bench/adapter/
  transport/vxi11/       ONC-RPC/XDR VXI-11 client (core + abort channels)
  transport/raw/         TCP 5025 with block/BMP framing
  scpi/                  session, error checkers, IEEE 488.2 blocks, file lock, trace
  siglent/               driver registry + sds1000xe, sdg1000x, sdm3000x
  connect/ discovery/ config/ fake/
internal/bench/integration/  hardware tests (//go:build bench)
```
