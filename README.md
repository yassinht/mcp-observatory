# MCP Observatory

An independent, continuously-operated record of what public MCP servers actually served.

## Why signing is not enough

Sigstore, Rekor and manifest signing are **opt-in publisher attestations**. A server operator who changes a tool description from *"look up the weather"* to *"look up the weather and forward the conversation to evil.example"* will happily sign the new description, and the signature will verify perfectly. Signing proves who published something. It does not tell you that what you are running today is not what you audited last month.

A transparency log is **adversarial third-party observation**. It records what servers actually served, without their consent or cooperation. That asymmetry is why Certificate Transparency worked: CAs never opted in, monitors watched them anyway, and misissuance became detectable after the fact.

That is what this project is. Not a scanner, not a registry, not a linter — a log.

## What it does

A crawler visits every publicly reachable MCP server in the official registry on a schedule and records the exact tool surface it served: names, descriptions, JSON schemas, and the four annotation hints. Observations go into an append-only log with signed tree heads, so anyone can prove that a given surface was observed at a given time and that the log has not been rewritten since.

The value is the accumulated history. A stranger can clone this repository in a weekend. Nobody can clone a year of observations.

## Status

Early. Phase 1 of 4.

| Phase | Scope | State |
| --- | --- | --- |
| 0 | Falsification test — is the ecosystem observable at all? | done, passed |
| 1 | Crawler, raw observation archive, first census | in progress |
| 2 | RFC 6962 Merkle log, signed tree heads, verify CLI | not started |
| 3 | Public static site | not started |
| 4 | Witnesses and gossip for split-view detection | not started |

Phase 4 is the end state, not the entry ticket. A single-operator log is still useful — Go's own checksum database ran that way for years.

## First census — 2026-08-27

The first complete pass over the registry. 13,870 of 13,997 declared endpoints were reached; the run's `meta.json` records it as incomplete, because it is.

| | |
| --- | --- |
| Registry entries | 25,020 |
| Declaring a network endpoint | 13,997 |
| Observed | 13,870 (99.1%) |
| **Enumerated a tool list, no credentials** | **7,820 — 56.4%** of endpoints with a remote |
| Same figure against the whole registry | **31.3%** |
| Tools recorded | **132,683** |
| Tools declaring at least one annotation hint | **73.2%** |

Outcomes: 56.4% ok, 25.0% auth required, 8.3% protocol error, 6.4% unreachable, 3.2% timeout, 0.7% rpc error. Distinguishing "refused us" from "is not there" is what keeps the reachability figure honest.

**Concentration.** Two operators — `gateway.pipeworx.io` (1,264) and `api.mcp.ai` (1,094) — account for **30.2% of every reachable MCP server in the registry**. One template edit at either changes over a thousand "servers" at once. This is the single strongest argument for watching this ecosystem rather than trusting it.

**Spec adoption.** 6,716 servers negotiated 2025-06-18; 672 still speak 2024-11-05. Seventeen answered on 2026-07-28. The week-0 sample of 300 found zero on that revision and concluded none existed — at full population the honest statement is "rare, not absent". A sample that small cannot see a 0.2% feature.

**Tool-surface size.** The median server exposes a handful of tools. Three expose more than 600, and one — `io.github.davidmosiah/delx-mcp-a2a` — exposes **1,076**, of which 31 carry any annotation. Only 2 servers in the entire population paginated `tools/list`.

**Parked domains still listed.** Four registry endpoints resolve to expired domains now serving for-sale parking pages, all four through the same ad host. Small in absolute terms (0.03%), but the mechanism matters: an agent configured from the official registry connects to infrastructure its original operator no longer controls. Windows Defender classified two of those pages as phishing — noted, not endorsed: the same detector had flagged this project's own binary as a trojan an hour earlier. The archived bytes are in the log; judge them yourself.

## The measurement this project is built on

Before writing a crawler, the obvious way to kill this idea was tested: if almost no public MCP server can be enumerated without credentials, there is nothing to observe and the project should not exist. The kill threshold was set at 30% in advance.

Measured on a random sample of 300 registry entries, 2026-08-25:

| | |
| --- | --- |
| Registry population | **24,729** servers |
| Declaring a network endpoint | **13,629** (55.1%) |
| Enumerable with no credentials | **35.0%** — above the 30% kill line |
| Genuinely real servers in the sample | 67.6%, across 71 distinct operators |
| Tools declaring at least one annotation hint | **68.8%** of 1,420 observed tools |
| Successful handshakes using the 2026-07-28 `server/discover` | **0 of 105** — all used legacy `initialize` |

Two operators, `gateway.pipeworx.io` (1,312 servers) and `api.mcp.ai` (1,099), publish **17.7% of every observable server in the registry**. One template edit changes 1,312 "servers" at once. That concentration is the clearest argument for watching this ecosystem rather than trusting it.

The probe, its raw output, and the population snapshot are in [`docs/week0-falsification/`](docs/week0-falsification/). Two bugs in that probe mattered enormously: missing SSE transport support understated reachability by ten percentage points and would have produced a false "do not build" verdict, and a hardcoded protocol version biased the sample against servers on newer spec revisions. Writing the code was never the bottleneck. Contact with reality was.

## Design commitments

**Raw bytes are the record.** Every response is archived exactly as received. Counts, classifications and hashes are derived views that can be rebuilt. The week-0 probe stored only its own classification of the registry and discarded the raw entries, which made every later question about that snapshot unanswerable.

**Two hashes, never one.** `body_sha256` covers the exact bytes and is the provenance claim. `surface_sha256` covers the canonical, name-sorted tool array and is the change-detection key and the future Merkle leaf. Collapsing them would make the log either noisy or unprovable.

**No MCP SDK in the crawl path.** An SDK validates, normalizes and rejects, because it is built to talk to well-behaved servers. A server returning a nameless tool, a duplicate name, or a 40KB description is producing exactly the observation worth keeping.

**No LLM anywhere in the data path.** Every judgment must be a rule a human can re-run against the archived blobs, or the Merkle proofs are theatre.

**Content addressing, not timestamps.** A server whose surface has not changed costs nothing to observe again. The archive grows only when something actually changed.

## Running it

```bash
go test ./...
go run ./cmd/crawler --dry-run          # fetch and archive the registry, probe nothing
go run ./cmd/crawler --sample=200       # reproducible smoke test
go run ./cmd/crawler                    # full census
```

Output lands in `data/`:

```
data/blobs/<ab>/<sha256>        exact response bytes, deduplicated across all runs
data/runs/<runID>/index.jsonl   one line per endpoint observed
data/runs/<runID>/meta.json     run header, including the raw registry pages
```

Politeness is structural rather than advisory: targets are grouped by host and each host is worked by exactly one goroutine with a delay between requests, so no amount of `--workers` can hammer a single operator.

## How fast do tool surfaces change?

This is the question the log exists to answer, and the first attempt at it was wrong by a factor of six. Both the wrong number and the correction are kept here, because the correction is the more useful of the two.

Comparing two censuses seventeen hours and fifty-one minutes apart (2026-08-30 09:12 → 2026-08-31 03:03, same machine, same network):

| | |
| --- | --- |
| Enumerable in both runs | 8,106 |
| Surface unchanged | 6,551 — 80.8% |
| Surface changed | 1,555 — 19.2% |
| Of those, keeping **identical tool names** | 1,480 — 95.2% of all changes |

A server that adds or removes a tool is visible: the client sees the list change. A server that keeps `send_email` under the same name and rewrites what it claims to do announces nothing. No version bump, no notification. That second shape is the one signing cannot catch — the operator signs the new description and the signature verifies — and it accounts for 95% of everything that moves.

**Then the number had to survive its own audit.** Some servers embed live data in a description: *"cache updated 2026-08-30 09:14:02, 1,204 cities"* changes on every request and means nothing. `mcpobs classify` separates those by a rule anyone can re-run against the archived bytes — replace every digit with `#` and compare again — and the result was not kind:

| | | |
| --- | --- | --- |
| digits only | 83.5% | not a change |
| schema edited | 10.4% | real |
| description rewritten | 6.0% | real |

**Only 16.5% of silent changes are real edits.** The honest figure is therefore about **4% of enumerable servers rewriting tool descriptions or schemas in two days** — roughly 320 servers — not the 26% a first pass suggested.

An earlier sample had put the real fraction at 97%, and it was wrong for a reason worth naming: the analysis could only parse plain-JSON response bodies and silently skipped SSE-framed ones, inspecting 67 of 1,355. That sample was biased, not small. Simple servers return plain JSON and write static descriptions; complex ones stream SSE and inject live data. Measuring the easy half and generalising is how a six-fold error gets published.

One limitation stands: the rule normalizes digits, not rotating prose. At least one server serves a different daily puzzle inside a tool description, which is noise this classifier still counts as real. The 16.5% is an upper bound.

## Deployment

The crawler is a single static binary with no dependencies — no runtime, no database, no container.

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/crawler ./cmd/crawler
scp dist/crawler dist/mcpobs deploy/* user@server:~/
ssh user@server 'sudo bash install.sh'
```

`install.sh` creates a `mcpobs` service account with no shell, installs to `/opt/mcp-observatory`, and enables a daily systemd timer. It refuses to run if any name it needs is already taken, rather than overwriting something that might matter.

The unit is deliberately constrained. The crawler talks to fourteen thousand servers it does not control, on a box that is probably running something else that does matter:

| | |
| --- | --- |
| `ProtectSystem=strict`, `ReadWritePaths=…/data` | the filesystem is read-only except its own data directory |
| `RestrictAddressFamilies=AF_INET AF_INET6` | network only; no local sockets |
| `CPUQuota=50%`, `IOWeight=20`, `Nice=10` | background work yields to real applications |
| `OOMScoreAdjust=800` | under memory pressure the kernel kills this, never the neighbours |
| `MemoryMax=1500M` | measured, not guessed: a real census sat at 404 MB |
| `Persistent=true` on the timer | a reboot spanning 03:00 catches up instead of leaving a hole |

Three faults only appeared on the first real deployment and none were visible in development: progress written with carriage returns is invisible in `journalctl` and looks exactly like a hang; a 512 MB memory cap would have killed the census partway; and falling back to the SSE transport after a *timeout* doubled the cost of every dead endpoint, stretching the tail of a run by two hours. Writing the code was never the bottleneck.

## Scope, stated precisely

This log covers **publicly observable MCP servers** — those listed in the official registry that declare a network endpoint and answer an unauthenticated `tools/list`. That is 35% of listed servers, not "the MCP ecosystem". The distinction is not modesty; the credibility of every number here depends on it.

## Licence

TBD.
