# ADR 0001: `benchctl`, a SCPI bench-instrument CLI for agentic workflows

- Status: Accepted (implemented for SDS1104X-E, SDG1032X, SDM3045X; see "Implementation notes")
- Date: 2026-11-10 (implementation notes 2026-11-11)

## Context

We want an agent (Claude/pi) to run bench measurements end to end:

1. The agent tells the human how to wire the hardware.
2. The agent configures instruments and takes measurements through a CLI.
3. The agent corroborates results across instruments, e.g. generator setpoint vs. scope
   measurement vs. DMM reading.

### Inventory

Discovered on 2026-11-10 on `192.168.2.0/24` (all instruments use DHCP):

| Role | Model | IP (DHCP) | MAC | Serial | Firmware |
|---|---|---|---|---|---|
| Function generator | SDG1032X | 192.168.2.72 | 48:70:1e:d9:ab:45 | SDG1XCBD5R6238 | 1.01.01.33R1B6 |
| Oscilloscope | SDS1104X-E | 192.168.2.75 | 00:0a:35:00:01:22 | SDSMMFCX5R4853 | 8.1.6.1.35R2 |
| Multimeter | SDM3045X | 192.168.2.76 | 24:76:25:37:d2:6b | SDM34FBQ5R2887 | 5.01.01.06R1 |
| Power supply | SPD3303X-E | offline | | | |
| Spectrum analyzer/VNA | SVA1032X | offline | | | |

Current wiring: FG CH1 → scope C1, FG CH2 → scope C2, an external 5 V bench PSU → DMM V input.

### Discovery findings

- All three expose **raw SCPI on TCP 5025**, **telnet on 5024**, and **VXI-11 (portmapper
  on 111)**. Only the scope serves a web UI (port 80). None advertises itself over mDNS/LXI.
- **The MACs are not Siglent OUIs** (`00:27:00` / `74:5B:C5`), so they come from network-module
  vendors. MAC filtering cannot be used for discovery. The reliable method is to scan for
  VXI-11 (portmapper on 111) and send `*IDN?`; port 5025 is scanned only with
  `--transport raw` (see the SDS raw-socket leak below).
- DHCP addresses change, so instruments must be **pinned by serial number**, not IP.

### Capability probe (read-only, verified live)

**SDS1104X-E (scope)** uses Siglent's *legacy* command set (`C1:VDIV`, `TDIV`, `PAVA`, `WF?`),
not the newer `:WAVeform:DATA?` tree.
- `C1:VDIV?` → `C1:VDIV 2.00E+00V`. `C1:OFST?`, `C1:ATTN?`, `C1:CPL?` (`D1M`) and `Cn:TRA?`
  (trace on/off) all work.
- `TDIV?`, `SARA?` (`2.00E+06Sa/s`), `MSIZ?` (`14M`), `TRMD?`, `TRSE?`, `ACQW?` all work.
- `C1:PAVA? PKPK` → `C1:PAVA PKPK,2.40E-01V`. **Returns `****` when the measurement is
  invalid** (e.g. `FREQ` with no signal). Parse that as "no value", not as an error.
- `C1:WF? DESC` → IEEE 488.2 definite-length block `#9000000346` + 346-byte `WAVEDESC` struct.
  `C1:WF? DAT2` returns raw int8 samples in the same block format.
- `SCDP` → BMP screenshot (begins with `BM`).
- `WFSU SP,…,NP,…,FP,…` controls decimation, point count, and first point for transfers.
- `CHDR?` → `SHORT`: responses echo the header (`TDIV 5.00E-01S`). Set `CHDR OFF` per session
  so responses can be parsed.

**SDG1032X (function generator)**
- `Cn:BSWV?` → `WVTP,SINE,FRQ,1000HZ,PERI,0.001S,AMP,4V,AMPVRMS,1.414Vrms,OFST,0V,HLEV,2V,LLEV,-2V,PHSE,0`.
  These are key/value pairs; `AMP` is Vpp.
- `Cn:OUTP?` → `OFF,LOAD,HZ,PLRT,NOR`. Outputs are currently **OFF**. Load is **HiZ**, which
  matches the scope's 1 MΩ input, so the set amplitude equals the displayed amplitude.
- `Cn:MDWV?` (modulation), `Cn:SWWV?` (sweep), `Cn:BTWV?` (burst), `Cn:ARWV?` (arbitrary
  waveform), `ROSC?` (10 MHz reference) all work.

**SDM3045X (DMM)** uses a standard SCPI-1999 tree.
- `CONF?` → `"VOLT +6.00000000E-01"` (quoted). `SENS:FUNC?`, `TRIG:SOUR?` (`IMM`) and
  `SAMP:COUN?` work.
- `SYST:ERR?` works and returns `-113,"Undefined header;*OPT?"`.

**Common to all three:** `*IDN?` works. **`*OPT?` is unsupported** (it raises `-113`), so
capability detection must come from model and firmware, not `*OPT?`. `SYST:ERR?` works on
the SDG and SDM; the SDS uses `CMR?`/`EXR?` for errors instead.

## Decision

### 1. Standalone repo `siglent-ctl`, binary `cmd/benchctl`, built on `urfave/cli/v3`

- Module: `github.com/jescarri/benchctl`, **Go 1.27** (`go 1.27.4` toolchain).
- It lives in its own repo, separate from agentctl. The agent image installs the released
  binary like any other tool.

- CLI framework: **`github.com/urfave/cli/v3`** (v3.14.0). It provides nested commands
  (`benchctl scope measure`), typed flags with `Sources: cli.EnvVars(...)` for env overrides,
  shell completion, and `context.Context` passed into every `Action`. The context carries the
  SCPI I/O deadlines and is cancelled on SIGINT so outputs can be turned off safely.
- v3 requires Go ≥ 1.22, which the 1.27 module satisfies.
- The CLI layer only parses flags and maps them to `app` use cases. Business logic never
  imports `urfave/cli`.
- Global flags: `--config` (env `BENCHCTL_CONFIG`), `--timeout` (default 5s), `--human`,
  `--verbose` (logs SCPI traffic to stderr).

### 2. Transport: a `Transport` port with VXI-11 and raw-socket adapters

Goal: use the **full SCPI capability** of every instrument, including the slower, more
stateful ones (SVA1032X VNA sweeps, large trace/file transfers), not just a subset of
quick queries.

The transport does not limit *which* SCPI commands can be sent; any transport carries any
command string. What it limits is **session control**: aborting a stuck operation, locking
the instrument, end-of-message detection, and service requests. Long VNA sweeps and
multi-megabyte transfers need that control, so both standard LXI transports are
implemented behind one port:

```go
type Transport interface {
    Write(ctx context.Context, cmd []byte) error
    Read(ctx context.Context) ([]byte, error) // one complete response message
    Clear(ctx context.Context) error           // device clear / recover a desynced stream
    Lock(ctx context.Context, wait time.Duration) error
    Unlock(ctx context.Context) error
    ReadSTB(ctx context.Context) (byte, error) // status byte without a SCPI round trip
    Close() error
}
```

- **VXI-11 (preferred when the instrument supports it).** Pure Go over stdlib `net`: the
  portmapper `GETPORT` call, ONC-RPC record marking, XDR, and the core channel
  (`create_link`, `device_write`, `device_read`, `device_readstb`, `device_clear`,
  `device_lock`/`device_unlock`, `destroy_link`), plus `device_abort` on the abort channel.
  It gives:
  - an END flag on every message, so line and block responses are delimited by the protocol
    instead of by `\n` heuristics;
  - `device_clear` and `device_abort` to recover from a hung sweep or truncated transfer
    without dropping the link;
  - an **instrument-side lock**, which would also block other hosts and EasyScope/EasyVNA,
    not just other `benchctl` processes. (Verified 2026-11-11: Siglent firmware accepts
    `device_lock` but does **not** enforce it between links, so it is requested but the
    local file lock is the effective guard.);
  - `device_readstb` for polling the status byte during long operations. (Siglent always
    answers 0 here, so benchctl polls with `*STB?` instead.)
  - SRQ via the interrupt channel (`create_intr_chan`, `device_enable_srq`, and an RPC
    server for `device_intr_srq`). It is used only once a probe proves the instrument
    delivers service requests; Siglent firmware accepts the channel but never connects
    back.
- **Completion** (`raw --wait auto|srq|poll|opc`): `auto` uses SRQ if proven, else
  `*ESE 1; <cmd>; *OPC` with `*STB?` polling of ESB when the instrument sets ESR OPC on
  `*OPC` (SDG, SDM), else `*OPC?` (SDS). The SRQ probe result is cached in the inventory
  (`"srq"`) so later runs skip it.
- **Raw socket, TCP 5025 (last resort, explicit only).** One `net.Conn`, newline-terminated
  writes, reads with deadlines. Used **only** with `--transport raw` or `"transport": "raw"`
  on an instrument in `bench.json`, never automatically. Easy to
  reproduce by hand with `nc`. `Clear` is emulated by reconnecting, `Lock` falls back to the
  local file lock only, and `ReadSTB` sends `*STB?`.
- Selection: per instrument in `bench.json` (`"transport": "vxi11" | "raw" | "auto"`),
  default `auto`, which means VXI-11 only. Global override `--transport`. When VXI-11 fails,
  the error says so and tells the user to retry with `--transport raw` (or set it in the
  inventory); benchctl never switches to raw on its own. `discover` likewise probes only
  VXI-11 unless `--transport raw` is given, and says so when it finds nothing.
- No VISA and no cgo in either case: one static binary.

Shared SCPI layer on top of the port (`adapter/scpi`), used by every driver:

- Responses are a line (`\r\n` trimmed) or an **IEEE 488.2 block**: definite
  `#<n><len><bytes>` and indefinite `#0…<END>`. Blocks are buffered in memory (a full
  SDS1104X-E channel is 3.5–14 MB); streaming to an `io.Writer` is deferred until a VNA
  or larger instrument needs it.
- **IEEE 488.2 status model**: `*CLS`, `*ESE`, `*SRE`, `*ESR?`, `*STB?`/`ReadSTB`. Long
  operations (VNA sweeps, calibration steps, averaging) use `*OPC` + polling of the ESB/OPC
  bit with a **per-operation timeout**, not the global `--timeout`.
- Every set command is followed by an error check (`SYST:ERR?` drained until `0,…` on
  SCPI-1999 instruments; `*ESR?`, then `CMR?`/`EXR?` for detail, on the SDS). The CLI fails
  loudly if the instrument reports an error; a silently ignored command would invalidate a
  measurement.
- Every numeric set is **read back and compared**. Both the SDS and the SDG silently clamp
  out-of-range values (`C1:VDIV 999` → 10 V, `FRQ 1E12` → 30 MHz) without any error.
- Ordering: `*OPC?` where supported, plus model-specific settle delays (e.g. after scope
  timebase changes, before a `PAVA?` read).
- One client at a time: the instrument-side lock when the transport has one, **plus** a
  per-instrument file lock (`$XDG_RUNTIME_DIR/benchctl/<serial>.lock`) so local processes
  fail fast instead of waiting on the instrument.

### 3. Hexagonal layout (ports and adapters)

```
cmd/benchctl/                 # CLI adapter: urfave/cli/v3 command tree, JSON output
  main.go                     #   root cli.Command, global flags, signal → safe()
  cmd_discover.go, cmd_scope.go, cmd_fg.go, cmd_dmm.go, cmd_apply.go, cmd_wiring.go
internal/bench/
  domain/                     # pure types: Channel, Waveform, Measurement, Units, Limits
  port/                       # interfaces the app uses
    scope.go                  #   Scope{Setup, Measure, Capture, Screenshot}
    generator.go              #   Generator{SetWave, Output, State}
    dmm.go                    #   DMM{Configure, Read}
    psu.go                    #   PSU{Set, Output, Read}   (when SPD3303X-E is online)
    analyzer.go               #   SpectrumAnalyzer{Setup, Sweep, Trace, Markers}
    vna.go                    #   VNA{Setup, Calibrate, Sweep, SParams, Export}
    transport.go              #   Transport (see §2)
  app/                        # use cases: discover, apply-setup, measure, verify-wiring, run-plan
  adapter/
    transport/vxi11/          # ONC-RPC/XDR, core + abort channels, optional intr channel
    transport/raw/            # TCP 5025 socket
    scpi/                     # line/block readers, status model, error polling, file lock
    siglent/sds1000xe/        # Scope implementation (legacy cmd set, WAVEDESC decoder)
    siglent/sdg1000x/         # Generator implementation (BSWV kv parser)
    siglent/sdm3000x/         # DMM implementation (SCPI-1999)
    siglent/sva1000x/         # SA + VNA implementation (command tree TBD, see Open items)
    fake/                     # in-memory instruments for tests
```

Drivers are selected by the `*IDN?` model string, matched against a prefix registry
(`SDS1*X-E` → `sds1000xe`, etc.). Supporting a new model means adding one adapter.

Typed ports cover the common, safety-relevant workflows; they are **not** the ceiling.
Anything a driver doesn't wrap is still reachable through `raw`/`script` (§5) over the same
transport, status model and error checking.

### 4. Inventory file, pinned by serial number

`~/.config/benchctl/bench.json`:

```json
{
  "subnet": "192.168.2.0/24",
  "instruments": {
    "scope": {"serial": "SDSMMFCX5R4853", "model": "SDS1104X-E", "last_ip": "192.168.2.75"},
    "fg":    {"serial": "SDG1XCBD5R6238", "model": "SDG1032X",   "last_ip": "192.168.2.72"},
    "dmm":   {"serial": "SDM34FBQ5R2887", "model": "SDM3045X",   "last_ip": "192.168.2.76"}
  },
  "wiring": [
    {"from": "fg:CH1", "to": "scope:C1"},
    {"from": "fg:CH2", "to": "scope:C2"}
  ]
}
```

On connect, the CLI tries `last_ip`, checks the serial with `*IDN?`, and re-runs discovery if
it doesn't match. Overrides: `BENCHCTL_CONFIG` and `--ip`.

### 5. Command surface, designed for agents

All commands print **JSON to stdout** (`--human` gives a table), put diagnostics on stderr,
and use these exit codes: `0` ok, `1` instrument/SCPI error, `2` usage error,
`3` instrument unreachable, `4` verification failed.

```
benchctl discover [--subnet CIDR] [--save]
benchctl ping [<alias>...] [--rounds 3]      # connection test: reach, serial check, *IDN? round trip
benchctl status [<alias>...]                 # IDN + current settings snapshot
benchctl caps <alias>                        # static capability sheet for the model (ranges, channels)

benchctl fg set <ch> --wave sine --freq 1k --amp 1Vpp --offset 0 [--load hiz|50]
benchctl fg output <ch> on|off

benchctl scope setup <ch> --vdiv 200m --offset 0 --coupling dc --probe 1x [--on|--off]
benchctl scope timebase --tdiv 200u [--delay 0]
benchctl scope trigger --source C1 --mode auto|normal|single --level 0 --slope rise
benchctl scope autoset                       # waits until the scope has applied it; before/after
benchctl scope measure <ch> pkpk freq rms mean ...   # PAVA; "****" → null
benchctl scope capture <ch>... [--points 10000] -o out.csv|out.json   # WF? DAT2 → volts/seconds
benchctl scope screenshot [-o shot.png|dir]
benchctl screenshot <alias>... [-o file|dir] [--format png|bmp]   # any instrument with SCDP

benchctl dmm config vdc|vac|idc|iac|res|fres|freq|cap|diode|cont [--range auto|<v>]
benchctl dmm read [--samples N] [--interval 100ms]

benchctl sa  setup --center 100M --span 10M --rbw 10k --ref 0dBm
benchctl sa  trace [--trace 1] -o trace.csv
benchctl vna setup --start 1M --stop 1G --points 401 --param S11|S21 --ifbw 1k
benchctl vna cal open|short|load|thru|save             # guided; prompts the human per step
benchctl vna sweep [--avg N] -o dut.s1p|dut.s2p        # Touchstone export

# Full SCPI access, with the same locking, timeouts and error checks as typed commands
benchctl raw <alias> "<SCPI>" [--block -o out.bin] [--read] [--opc] [--wait auto|srq|poll|opc] [--timeout 120s]
benchctl script <alias> cmds.scpi            # one locked session; stops on first SCPI error
benchctl transfer <alias> get|put <instr-path> <local-path>   # MMEM file transfer
benchctl apply plan.yaml                     # verifies the plan's wiring, then setup + measurement + assertions
benchctl wiring show|verify [--plan plan.yaml] [--confirm]
benchctl wiring show|verify                  # see §7
```

Engineering suffixes (`1k`, `200m`, `4Vpp`, `1.414Vrms`) are parsed in `domain/units.go`.
Every measurement result includes the instrument settings in effect when it was taken
(vdiv, tdiv, coupling, probe), so the agent can tell whether a reading is trustworthy (e.g.
whether the signal was clipped).

### 6. Waveform capture

1. `CHDR OFF`, `WFSU SP,<decim>,NP,<n>,FP,0`.
2. `Cn:WF? DESC` → decode `WAVEDESC` (little-endian): `vertical_gain`, `vertical_offset`,
   `horiz_interval`, `horiz_offset`, `wave_array_count`.
3. `Cn:WF? DAT2` → int8 samples.
4. Convert to volts with the descriptor fields (SDS1000X-E formula:
   `V = code * vdiv/25 - offset`). **Validate this formula in the first integration test
   against a known FG signal** before trusting it.
5. Write CSV (`t,v`) or JSON with metadata. Keep the raw block alongside with `--raw` for audit.

### 7. Human-in-the-loop wiring protocol

The agent can't see the bench, so every wiring instruction is checked electrically:

1. The agent generates step-by-step connection instructions from a plan file
   (`wiring show --plan plan.yaml`, from the plan's `wiring:` section; without `--plan`, the
   inventory wiring), e.g. "Connect FG CH1 BNC → scope C1, BNC-BNC cable".
2. The human confirms. Usually that happens in the agent conversation; `wiring verify
   --confirm` also prints the instructions and waits for "y" on stdin, and anything else
   aborts (exit 2) before any signal is driven.
3. `benchctl wiring verify` drives a distinctive test signal on each FG channel in turn
   (e.g. CH1 = 1 kHz sine, CH2 = 3.3 kHz square) and checks with `PAVA? FREQ` which scope
   channel sees which signal. It detects swapped or missing cables and exits `4` with a diff.
   The amplitude ratio (measured/set, 1 Vpp into HiZ) names the likely cause:
   - ×2 or ×½: 50 Ω / HiZ load mismatch;
   - ×0.1: a probe switched to 10X on a channel set to 1X (×10 for the reverse). A 10X
     probe leaves the signal at half a division, where the SDS cannot read its frequency,
     so a channel that misses the test frequency is measured again at 10× sensitivity
     (and a clipped one at 10× coarser) before the link is called missing;
   - square test wave with pkpk but almost no top-to-base amplitude: only the edges get
     through, i.e. the path is capacitively coupled (probe tip or connector not making
     contact).
   `wiring show` tells the human to set any probe's switch to 1X, or to match the scope
   channel's probe factor.
4. For the DMM, it applies a known DC level from the FG with output on and the DMM in VDC,
   and checks the reading is within tolerance. A link from a non-instrument source (e.g.
   `ext:5V`, a bench PSU) is checked against its `expect` reading.
5. Links that cannot be checked electrically (to `ext:` or `dut:` endpoints, or a DMM link
   without `expect`) are listed under `unchecked` and never count as verified; a report with
   no checked link fails (exit 4). Any other unknown destination alias is a usage error (a
   typo must not pass). Test signals and DC test levels are checked against the bench limits
   before any output changes, and a generator → DMM link can only use `vdc`.
6. `apply` runs step 3–5 on the plan's `wiring:` before step 1 (`verify_wiring: false` to
   skip) and runs no step if it fails.

`benchctl ping` is the plain connection test: it opens each instrument the normal way
(last IP, serial check, rediscovery), times the connection and `*IDN?` round trips, and
reports the transport and SRQ status.

### 8. Safety rails

- FG outputs, and later PSU outputs, are **never enabled implicitly**. `output on` is an
  explicit command, and `apply` plans must state it.
- Per-instrument limits live in `bench.json` (e.g. `fg.max_amp_vpp`, `psu.max_v`,
  `psu.max_i`). The CLI refuses values over a limit before sending anything. An invalid limit
  value is a config error for every command that can energize an output, never "no limit";
  `safe` does not validate limits, so a bad value can never keep an output on. `raw`, `script` and
  `raw` plan steps send SCPI unchecked and bypass the limits.
- `benchctl safe` turns all outputs off on every instrument, and exits non-zero (3 if
  unreachable, else 1) when any output is not confirmed off. Plans run it in a
  deferred step (on failure and on SIGINT).
- Load mismatch warning: if FG load is `50` but the scope coupling is `D1M`, warn that the
  amplitude will read 2× the setting (and the reverse).

### 9. Testing

- Unit tests: line/block readers, `WAVEDESC` decoder (fixture captured live into
  `testdata/`), BSWV parser, unit parser, `****` handling.
- Adapter tests against `adapter/fake` plus a tiny TCP SCPI echo server in `testdata`.
- Integration tests behind the `//go:build bench` tag. They run only with real hardware and
  `BENCHCTL_CONFIG` set. First test: FG CH1 1 kHz 1 Vpp sine → scope C1 `AMPL ≈ 1.0 V ± 3%`,
  `RMS ≈ 0.354 V ± 3%`, `PKPK ≈ 1.0 V ± 8%`, `FREQ ≈ 1 kHz ± 0.1%`, plus a capture whose
  FFT peak sits at 1 kHz ± 0.5% and whose span matches PKPK (run over both transports).

## Consequences

- No VISA dependency: one static binary, which can be cross-compiled for the agent sandbox.
- Two direct dependencies: `urfave/cli/v3` (no transitive deps) and `gopkg.in/yaml.v3` for
  `apply` plans. VXI-11 is implemented in-house, which is more code to own and test than a
  raw socket. It is tested against a fake RPC server and cross-checked live against
  `--transport raw`.
- A separate repo means separate CI and releases, and the agent image has to pull the binary
  in explicitly.
- On the raw socket (when explicitly chosen) there is no device clear, no instrument-side lock and no SRQ:
  recovery means reconnecting, and locking is local only. Completion relies on `*OPC` and
  status-byte polling on both transports unless SRQ is verified on an instrument.
- Driver code is model-specific because Siglent's scope command set differs between series.
  Supporting a newer SDS model means writing another adapter.
- Serial-number pinning makes DHCP changes harmless, at the cost of a rescan (~5 s for a /24)
  when an IP changes.

## Alternatives considered

- **Raw socket on 5025 only.** Simplest code, but no device clear, no instrument-side lock
  and no message END flag. Fine for quick queries, weak for long VNA sweeps and large
  transfers. Kept as an explicit last-resort adapter for instruments without VXI-11.
- **HiSLIP.** The modern LXI transport, but TCP 4880 is closed on the SDG, SDS and SDM
  (checked 2026-11-11). Revisit if a future instrument offers it; it fits the same port.
- **Telnet port 5024.** Has an echo/prompt that has to be stripped. Rejected in favour of 5025.
- **PyVISA wrapper.** Mature, but means a Python runtime in the agent sandbox and conflicts
  with the Go requirement. Rejected.
- **stdlib `flag` + `switch`.** No dependency, but nested subcommands, env-var flag sources
  and help text would all be hand-written. Rejected.
- **urfave/cli v2.** In maintenance mode, with no native `context.Context` per action.
  Rejected in favour of v3.
- **Inside the agentctl repo.** It could share agentctl's CI, but it would couple an
  instrument tool to the agent bus. Rejected; it lives in its own repo.

## Implementation notes (verified on hardware, 2026-11-11)

**Transport**
- VXI-11 works on all three instruments (core 395183, abort 395184, intr 395185 registered
  with the portmapper). `maxRecvSize` is 8 MiB. `device_clear` works. `device_readstb`
  **always returns 0** (in the same state `*STB?` returns 96), so the status byte is read
  with `*STB?`.
- `create_link` reports `abortPort=0`, but the abort program is registered with the
  portmapper; the client looks it up there, and `device_abort` then works.
- `device_lock` is accepted but **not enforced**: a second link gets the lock while the
  first holds it. Locking is effectively local (file lock) only.
- TCP 4880 (HiSLIP) is closed on SDG, SDS and SDM.
- **SRQ:** the interrupt channel is implemented. All three instruments accept
  `create_intr_chan` and `device_enable_srq` but never open the connection back (0
  connections, while `*STB?` shows MSS set; the host firewall is off). `--wait auto` detects
  this with a 400 ms probe, caches it, and falls back to `*STB?` polling (SDG/SDM) or
  `*OPC?` (SDS).
- **SDS raw-socket leak:** the scope never closes its side of a TCP 5025 session (our
  side stays in FIN-WAIT-2; SDG and SDM close properly). Each raw connection leaks a
  session, and after enough of them the scope's socket server accepts connections but never
  answers, until it is power-cycled. VXI-11 is unaffected. benchctl therefore never uses
  raw unless asked: `auto` is VXI-11 only, discovery probes only VXI-11 by default, and the
  scope's raw integration subtest is opt-in (`BENCH_SCOPE_RAW=1`).
  This happened during testing on 2026-11-11: **the scope's 5025 port is currently wedged**.
- **Status model:** SDG and SDM set ESR OPC on `*OPC` (`*STB?` = 96 with `*ESE 1; *SRE 32`);
  the SDS does not (ESR stays 0), so it can only use `*OPC?`.
- **Screenshots:** all three answer `SCDP` (no `?`) with a BMP: SDS 800×480 16-bit bitfields
  top-down, SDG/SDM 480×272 24-bit bottom-up. benchctl saves BMP or converts to PNG.
- Raw socket framing handles `DESC,#9…` headers, the SDS's `\n\n` after blocks, and
  `SCDP` BMP screenshots (sized from the BMP header). Both transports give identical
  results; a full 3.5 M-point channel transfers in about 1.6 s on either.

**SDS1104X-E (fw 8.1.6.1.35R2)**
- Unknown commands set `*ESR?` CME (32) but leave `CMR?` at 0, so `*ESR?` is the primary
  error check. Unknown *queries* return nothing (a timeout).
- These commands are applied but **always flag CME**: every `WFSU` form, and `TRSE` with a
  hold-off argument. The driver verifies them by exact read-back instead of ESR, and uses
  the short `TRSE EDGE,SR,<src>` form (it resets hold-off to OFF).
- `SANU? C1` answers; `SANU? C2` and `C2:SANU?` time out. The point count is taken from
  WAVEDESC `wave_array_count` instead.
- WAVEDESC: `vertical_gain` holds V/div, `vertical_offset` the channel offset,
  `horiz_interval` = 1/SARA, `horiz_offset` = **−TRDL**, so the first sample is at
  `horiz_offset − 7 × TDIV` and t = 0 is the trigger point. (Verified with delays of 0, +300
  and −250 µs: a 1 kHz sine's rising edge lands at 0 ± 0.5 µs. The first implementation
  had the sign reversed and was off by 2 × TRDL.) Volts are
  `code × vdiv/25 − offset`, and the time base spans **14 divisions**. Both are confirmed:
  the captured span matches `PAVA PKPK` to within 0.5 %, and the capture duration is
  14 × TDIV.
- **Autoset:** `*OPC?` returns immediately after `ASET`, and the scope applies the new
  settings only after acquiring (≈6 s from 500 ms/div, ≈9 s total). benchctl polls the
  settings until they change and stay stable (≈1.2 s), with a deadline of one screen
  (14 × TDIV) + 8 s. Verified: from 500 ms/div, 2 V/div it settles on 500 µs/div, 200 mV/div
  for a 1 kHz 1 Vpp sine.
- `PAVA PKPK` reads about 6 % high on a 1 Vpp sine at 200 mV/div (noise peaks); `AMPL`
  (1 %) and `RMS` (1.8 %) are better amplitude checks. The integration test asserts
  AMPL/RMS ±3 % and PKPK ±8 %.

**SDG1032X / SDM3045X**
- SDG `SYST:ERR?` format is `+0, No error` / `-113,Undefined header,C1:FOO`; out-of-range
  values are clamped silently (read-back catches it).
- SDM `READ?` on auto range works. It reads the external 5 V supply as 4.997 V.

**Bench finding**
- `wiring verify` confirms FG CH1 → scope C1 (1.06 Vpp, 1 kHz) and the 5 V on the DMM.
  FG CH2 → scope C2 arrived at the right frequency but at **×0.05–×0.15 of the set
  amplitude, varying between runs**. Cause 1: the C2 probe's switch was at 10X with the
  channel at 1X; with the switch at 1X the path read 4.16 Vpp for 4 Vpp and verify passed.
  Shortly after, C2 showed decaying spikes on each edge of a square wave and an amplitude
  that rose with frequency (sine at 10X setting: 100 Hz noise, 1 kHz 0.23 V, 3.3 kHz
  0.37 V): cause 2, an intermittent contact at the probe tip or a connector, coupling only
  capacitively. Both are now diagnosed by name.

## Open items

- Probe SPD3303X-E and SVA1032X when they're on the network. The SPD3303X-E's 5025 support
  and the SVA's SA/VNA command tree (sweep, calibration, trace and Touchstone export) are
  **unverified**. The `psu`, `analyzer` and `vna` ports and the `sa`/`vna` commands are
  therefore not implemented yet; `raw`/`script` already reach those instruments.
- SRQ is implemented but unusable on the current Siglent firmware (see notes). Re-check on
  the SVA1032X and after firmware updates.
- `transfer` (MMEM:DATA) is generic and instrument-dependent; it is untested on hardware.
