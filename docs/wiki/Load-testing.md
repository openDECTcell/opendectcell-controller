# Load testing: how many calls, cells and handsets does your machine carry?

The [limits page](Limits.md) gives the numbers we measured on a Raspberry Pi 4. Yours will differ with the machine, the
codecs your lines use and the room's temperature. This page shows how to **measure your own**, in about 30 minutes, with
the same tools. What we did and found is written up in [load-test-2026-11-05.md](../load-test-2026-11-05.md).

## What the tests do and do not tell you

| Test | What it loads | Answers |
|---|---|---|
| **Calls** | The controller's media engine: N calls at once, each with a SIP leg (SRTP) and a cell leg (SRTP) and the real jitter buffers, converters and concealment between them. The far ends (the PBX and the cells) are simulated, by a second process, so the engine's CPU time is its own. | How many calls the engine carries before the audio breaks up |
| **Cells** | The cell link: 100 and 300 simulated cells enrol, connect with TLS and send heartbeats and statistics. | How many cells connect, and how much memory each takes |
| **Handsets** | The database side: 500 handsets on 100 cells, moving between them. | Whether the tables and the move logic keep up |

**What they do not test:** the DECT stacks that answer every frame of a real call (their cost per cell and per call is
not measured yet), real radios, the web interface and the API under load, and a real network between the parts (the
traffic runs on loopback, so nothing is lost or reordered on the way). Read the call numbers as a **ceiling for the
media engine**, not as what a whole installation carries. Take about half of them for a first plan.

## Before you start

- **Use a machine that carries no real calls.** The test fills the machine on purpose. A test controller next to a
  production one on the same board is fine for a short run (the soak of 2026-11-05 ran next to it unharmed), but
  expect the production one to feel it. Nothing is installed, deployed or restarted: the tests are plain programs
  that open their own ports and touch no controller state.
- **Ports.** The calls test uses UDP ports 20000 to 20000 + 2N (the engine) and 24000 to 24000 + 2N (the far end)
  on 127.0.0.1. Nothing else may use them.
- **Heat.** A Raspberry Pi without a fan climbs 2 to 4 degrees per step and may reach 80 degrees. The script waits for
  the board to cool before a step (below 74 degrees) and stops above 80 (`LOADTEST_COOL_TEMP`, `LOADTEST_MAX_TEMP`).
  Above 80 the board slows itself down and the result is the heat's, not the engine's.
- **Close the other work.** Anything else busy on the machine goes into the result.

## 1. Build the test programs

On your development machine (Linux or macOS with docker; no Go installation needed), in a clone of the controller:

```sh
tools/load-test.sh build ./loadtest-bin arm64      # for a Raspberry Pi 4 or 5 (64-bit system)
tools/load-test.sh build ./loadtest-bin amd64      # for an ordinary PC or server
```

This compiles the three test programs (`media.test`, `cell.test`, `core.test`) with the Go version of the
controller's Dockerfile and copies the script next to them. Copy the directory to the machine under test:

```sh
scp -r loadtest-bin pi@your-pi:/tmp/
```

(Without docker: install the Go version named in `go.mod` and run `GOOS=linux GOARCH=arm64 go test -c -o media.test
./internal/media`, and the same for `./internal/cell` and `./internal/dect/core`.)

## 2. Calls

On the machine under test:

```sh
cd /tmp/loadtest-bin
./load-test.sh calls g722 20 60 90 110 130
./load-test.sh calls pcma-wide 20 40 60 80
./load-test.sh calls pcma-narrow 40 70 100
```

The first word is the **kind of call**; the numbers are the **steps**: one run of 5 s warm-up and 20 s measured per
number, calls started together. Choose the kind that matches your lines and handsets:

| Kind | SIP line | Handset side | The engine does |
|---|---|---|---|
| `g722` | G.722 | wideband (G.722) | passes the audio through (decrypts and encrypts SRTP only) |
| `pcma-narrow` | G.711 (PCMA) | narrowband | decodes and encodes G.711, no resampling |
| `pcma-wide` | G.711 (PCMA) | wideband (G.722) | decodes and encodes both and resamples between 8 and 16 kHz: the most expensive case |

A comma list (`g722,pcma-wide`) makes the calls take the kinds in turn. Choose steps that start at a few calls and
climb until the audio breaks; a run of one kind takes about 35 seconds per step. `LOADTEST_SECONDS` changes the
measured time.

### Reading a line

```
calls=90 g722 engine_cores=1.258 per_call=0.014 machine_busy=62.7% rss=17.7MiB loss(cell/sip)=0/0%
  worst_p99_variation(cell/sip)=12.9/12.9ms max_gap=42.7ms generator_late=1/5196 concealed(d/u)=4/2
```

| Field | Meaning |
|---|---|
| `engine_cores` | CPU the engine's process used, in cores (the Pi has 4) |
| `per_call` | the same per call |
| `machine_busy` | the whole machine's CPU (includes the traffic generator, about a third of it) |
| `rss` | memory of the engine |
| `loss` | packets that never arrived, to the cell and to the SIP side |
| `worst_p99_variation` | of the worst call, how much later than the RTP clock the slowest 1 % of the packets arrived: **the number that tells you when the audio breaks** |
| `max_gap` | the longest silence between two packets of any call (20 ms is normal) |
| `generator_late` | how often the traffic generator itself was late by more than 5 ms (if this grows, the generator is the limit, not the engine; use fewer calls or a faster machine) |
| `concealed` | frames the engine had to make up (downlink/uplink) because the real ones were not there |

### Deciding when it is too much

| | Clean | Borderline | Degraded |
|---|---|---|---|
| `worst_p99_variation` | under 15 ms | 15 to 40 ms | over 40 ms |
| `concealed` | about 0 | a few | dozens and growing |
| `machine_busy` | under 50 % | 50 to 70 % | over 70 % |

On a Pi 4 the line between "clean" and "degraded" was at 90 (G.722 through), 70 (narrowband) and 50 to 60 (converting)
calls. The audio did not fail suddenly: the variation grew from 6 to 13 to 90 to 437 ms over 60, 90, 110 and 130 calls. **Plan with
the "clean" column, and a margin.** If the variation is already high at a small number of calls, something else is
running on the machine, or it is throttled (`vcgencmd get_throttled` on a Pi; `0x0` is good).

## 3. Cells and handsets

These need a PostgreSQL to run against. Start a throwaway one (not your controller's):

```sh
docker run -d --name loadtest-pg -e POSTGRES_USER=pg -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55432:5432 \
  postgres:18 -c fsync=off -c max_connections=200
export OPENDECTCELL_TEST_DATABASE_URL='postgres://pg:pw@127.0.0.1:55432/postgres?sslmode=disable'
./load-test.sh cells
docker rm -fv loadtest-pg          # removes the container and its volume, and only those
```

(The image is the one in the controller's `docker-compose.yml`.) Do not run
`docker volume prune` for the cleanup: it removes every volume no running container uses, and that can be your
controller's data.

You get, for 100 and for 300 simulated cells, how long enrolling took, how long until every cell was connected, the
memory and goroutines per cell, and for the handsets the time of one move:

```
300 cells: enrolled in 30.359s (101 ms each), all connected in 8.147s (11812 connections turned away and retried), goroutines +1800 (6.0 per cell), heap +23.5 MiB (80 KiB per cell), ...
enrolled 500 handsets on 100 cells in 4.166s
1982 moves in 13.693s (6.909ms each)
```

"Connections turned away and retried" is expected: the controller admits cells from one address 16 at a time, which is
what a site coming back from a power cut looks like.

## 4. The cost of what is already running

To see what your controller costs while idle, with real cells connected, read the CPU time of its processes over a
minute (nothing here loads the machine):

```sh
snap() { for p in $(pgrep -f 'opendectcell-controller|opendectcell-worker|postgres: '); do
  echo "$p $(cat /proc/$p/comm) $(awk '{print $14+$15}' /proc/$p/stat) $(awk '/VmRSS/{print $2}' /proc/$p/status)"; done | sort; }
snap > /tmp/s1; sleep 60; snap > /tmp/s2
join /tmp/s1 /tmp/s2 | awk '{d=$6-$3; c[$2]+=d; r[$2]+=$7} END{for(k in c) printf "%-20s %.1f%% of one core, %d MiB\n", k, c[k]/60, r[k]/1024}'
```

(The CPU time is in 1/100 s, so the sum over 60 seconds divided by 60 is the percentage of one core.)

## Sending us your numbers

Open an issue with the output of the three commands, the machine (`uname -m`, `nproc`, memory, the Pi model),
the controller version, and what else ran on it. More machines make the limits page less of a guess. The result that
we would most like to see: **a call test on the server you intend to use for many cells**, and the **cost of the DECT
stack per cell** from a controller with real cells and a real call (the missing piece of the estimate).

## Troubleshooting

| What you see | Why |
|---|---|
| `media.test not found` | set `LOADTEST_BIN` to the directory `build` wrote |
| `rtp: no free port pair in the range` | something uses the ports 20000 to 20000 + 2N or 24000 to 24000 + 2N; free them or use fewer calls |
| `too many open files` above about 300 calls | raise the limit with `ulimit -n 4096` before running (each call opens 3 sockets in each of the two processes) |
| `peers: exit status 1` | the far-end process failed; its message is printed above the line |
| everything is slow from the first step | the machine is busy or throttled (`vcgencmd get_throttled`, `uptime`, `top`) |
