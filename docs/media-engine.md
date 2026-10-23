# Media engine (C5)

The media engine carries one call's audio between the **cell leg** (RTP or
SRTP to the cell, `cell-protocol.md` §8) and the **SIP leg** (RTP or SRTP to
the PBX or provider, `sip-ua.md`). It does not touch the cell link's
C-plane, so it is independent of the `libopendectcell` integration freeze
(opendectcell-controller#46). Code: `internal/media` and its subpackages.

Status 2026-10-23: built and tested without hardware. Nothing creates a
pipeline in production yet; the DECT core (C3) will, once it binds audio
(§9).

## 1. Shape

```
             downlink (what the handset user hears)
 SIP leg ──► jitter ► decode ► conceal ► rate ► encode ──► cell leg
 (rtp.Endpoint)  buffer                    convert          (CellLeg)
 SIP leg ◄── encode ◄ DTMF ◄ rate ◄ conceal ◄ decode ◄ jitter ◄── cell leg
                     tones   convert                 buffer
             uplink (what the far end hears)
```

Each direction runs on the pipeline's 10 ms clock and emits exactly one
frame per ptime, whatever its input did, so the output is always paced.
Internally audio is signed 16-bit linear PCM at the plan's rate.

| Package | Role |
|---|---|
| `media` | `Choose` (path selection), `Pipeline`, `CellLeg`, `Summarise` (quality) |
| `media/codec` | G.711 A/µ-law, G.722, L16 |
| `media/dsp` | 16 ↔ 8 kHz conversion, loss concealment, DTMF tones |
| `media/jitter` | Adaptive jitter buffer |
| `media/rtpstats` | RFC 3550 statistics, SR/RR, RTT |
| `media/quality` | Signal metrics and test signals |
| `calllog` | `call_log` rows, including the quality record |

## 2. Path selection (C5.4)

The cell link's formats are **per direction and per cell**
(`cell-protocol.md` §8.2): HELLO tag 12 lists what the cell accepts, tag 16
what it sends. `Choose(caps, wideband, sipCodec)` returns a `Plan`:

| Rule | Why |
|---|---|
| A cell is wideband only if a wideband format is in **both** lists and `cap_audio_wb > 0` (`CellCaps.Wideband`) | Wideband may never work on this chip (openDECTcell notes, 2026-10-23); it must be reported, not assumed |
| The call is wideband only if the cell is and the air codec is | A narrowband-only cell still carries a G.722 SIP call, by transcoding to L16 8 kHz |
| Per direction, the format equal to the SIP codec wins if listed, else the cell's first | Passthrough where it is free, the cell's preference otherwise |
| Internal rate 16 kHz only if both legs are wideband | Nothing is resampled up only to come back down |
| Passthrough per direction, only with equal ptimes; uplink passthrough also needs RFC 4733 | In-band DTMF has to be mixed into decoded audio |

The DECT core must offer G.722 in `<<CODEC-LIST>>` only to a handset on a
cell for which `CellCaps.Wideband()` is true.

## 3. Codecs

| Codec | Implementation | Verified by |
|---|---|---|
| G.711 A-law, µ-law | Own, from the G.711 segment definition | Every code round-trips; every 16-bit sample decodes within half a step; G.711 table values |
| G.722 64 kbit/s | `github.com/gotranspile/g722` (BSD and public domain), a Go translation of the CMU/Underwood code that Asterisk and FreeSWITCH use | 40 dB SNR on speech at its 22-sample QMF delay; tone frequency kept; golden hash |
| L16 | Own; RFC 3551, network byte order | Byte-order vector |

**G.722 headroom:** the encoder overloads within about 1 dB of full scale.
A talker who clips at ±32767 came out at 3.5 dB SNR. The encoder wrapper
limits samples to ±30000 (−0.8 dBFS), which gives 26 dB and leaves every
sample below the limit untouched. The regression corpus found this (§8).

## 4. Rate conversion

A 47-tap half-band FIR (Kaiser window, β 5.65): pass band flat to 3.4 kHz,
about 60 dB rejection above 4.6 kHz, 23 samples (1.4 ms) delay at 16 kHz.
It sits at the codec boundary: G.722 decodes to 16 kHz, then `Down2` to the
8 kHz internal rate, and the reverse on the way out. A wideband pipeline
(16 kHz internal) needs no conversion at all, so switching wideband on later
changes the plan, not the code.

## 5. Jitter buffer and delay

The buffer controls **playout slack**: how far the playout point lags the
fastest arrivals. A loss lowers the buffer level but not the slack, so it
never inflates the delay. A first, level-based version did, and a test
caught it.

| Parameter | Value | Reason |
|---|---|---|
| Target slack | 95th percentile of delay variation over ~4 s | Late packets about 5 % of the worst case, at least delay |
| `MinDelay` | 20 ms | The least jitter always absorbed. Enough for the cell leg (LAN) |
| `MaxDelay` | 120 ms | Above it, the call is over the delay budget anyway; a longer gap is treated as silence, not loss |
| Grow | Conceal a frame without advancing, when short of target; ≥ 100 ms apart | One frame of concealment per step |
| Shrink | Skip a frame after 1 s at > target + 1.5 frames; ≤ once a second | Slow, so a burst does not cause oscillation |

**Delay budget** (one way, mouth to ear; ITU-T G.114 recommends ≤ 150 ms):

| Segment | Estimate |
|---|---:|
| Handset, DECT air, chip | 15–25 ms |
| Cell lane and the cell's own buffer | 20–40 ms (cell side) |
| Cell ↔ controller network | < 5 ms on a LAN (`cell-protocol.md` §2 allows 50 ms p99 for the C-plane) |
| Controller jitter buffer | 20 ms minimum, up to 120 ms |
| Packetisation | 20 ms |
| SIP network, PBX, far end | 40–80 ms, not ours |

The sum is already about 150 ms at the minimum, which is why `MinDelay` is
one frame and not two. The cell link's latency budget (`cell-protocol.md`
§2) is about C-plane round trips, not media, and is unaffected.

## 6. Loss concealment and DTMF

**Concealment** repeats the last pitch period (found by autocorrelation,
2.5–15 ms), at full level for 10 ms and fading to silence by 60 ms, with a
4 ms cross-fade back into real audio: a simplified G.711 Appendix I. It runs
on decoded audio for every codec. A G.722 decoder's state is not repaired
after a loss (G.722 Appendix III would do that). In G.722 passthrough a lost
packet is not filled, because a bitstream spliced from two encoders would
desynchronise the far decoder; the far end conceals it. G.711 passthrough
does conceal.

**DTMF** from the handset (`Pipeline.SendDTMF`):

| SIP peer | Sent as |
|---|---|
| Negotiated telephone-event | RFC 4733 events by the SIP endpoint; uplink audio pauses for the event so the timestamps stay the event's |
| No telephone-event | Q.23 tones generated into the uplink, replacing the audio while they play (−10/−8 dBm0, 60 ms gap) |
| Account set to SIP INFO | Signalling, sent by the SIP call (`sip-ua.md`), not media |

Events received from the SIP peer are reported (`rtp.Endpoint.OnDTMF`);
DECT has no use for them in v1.

## 7. Statistics (C5.3)

| Source | What |
|---|---|
| SIP leg, received | RFC 3550 A.1/A.3/A.8: loss, interarrival jitter |
| SIP leg, RTCP | We send an SR or RR with CNAME every 5 s on average (0.5–1.5 ×). The peer's reports give its view of our stream and, from LSR/DLSR, the RTT |
| Cell leg | Loss and jitter from sequence numbers and timestamps; there is no RTCP on the cell link in v1 |
| Pipeline | Per direction: frames concealed, silence, jitter-buffer target, late packets, passthrough |

`media.Summarise` turns these into `call_log.quality` (JSON):

```json
{"sip": {"packets": 990, "loss_pct": 1, "jitter_ms": 12, "peer_loss_pct": 2, "peer_jitter_ms": 8, "rtt_ms": 60},
 "cell": {"packets": 1000, "loss_pct": 0, "jitter_ms": 2},
 "down": {"passthrough": false, "concealed_pct": 1.1, "buffer_ms": 20, "late": 0},
 "up":   {"passthrough": false, "concealed_pct": 0, "buffer_ms": 20, "late": 0},
 "mos": 4.0, "worse_leg": "sip", "plan": "SIP PCMA, cell down L16 (transcode), up L16 (transcode), 8000 Hz"}
```

- **MOS** is an E-model estimate (ITU-T G.107: R = 93.2 − Id − Ie,eff, with
  Ie = 0 and Bpl = 25.1 for G.711 with concealment, G.113 Appendix I) for
  the worse direction, on the narrowband scale even for G.722. The delay is
  RTT/2 plus the larger jitter-buffer target plus fixed 40 ms (cell path) and
  20 ms (packetisation).
- **worse_leg** blames the leg with more loss if either loses ≥ 1 %, else
  the one with more jitter if either has ≥ 20 ms, else nobody.

`/admin/calls` lists calls with both legs side by side.

## 8. Testing

| Layer | How |
|---|---|
| Codecs, DSP, jitter buffer, RTCP | Unit and property tests per package |
| Pipeline | Driven on virtual time (`Receive`, `Tick`) with fake legs: deterministic loss, jitter, reorder |
| End to end | Real UDP sockets: a PBX endpoint with SDES-SRTP, the controller, and a cell with MEDIA_OPEN-keyed SRTP; speech both ways, judged by envelope correlation |
| Regression corpus | 168 cases: 6 signals × 7 paths × 4 network conditions, `internal/media/corpus_test.go` |

The corpus checks each case **absolutely** and against
`testdata/corpus.golden.json`:

- The output level must equal the input's within 1 dB. A dead path and a
  level deficit both fail, however healthy the logs look. This is the cell
  side's lesson from 2026-10-23: a silent result cannot be told from a dead
  path.
- Silence must stay below −60 dBFS.
- The loudness envelope must follow the source. This is the check the cell
  side found decisive: a tone cannot tell a wrong format from a dead path.
- A tone must keep its frequency.
- The metrics must match the golden file within tolerance, and on amd64 the
  output must match exactly, by hash. On other architectures the compiler
  may fuse floating-point multiply-adds, so only the metrics are compared.

The corpus is mutation-checked: L16 in the wrong byte order (a misread lane)
and a 12 dB level deficit both fail the absolute checks. Its inputs are
generated (synthetic speech, tones, noise, silence), so it rebuilds bit for
bit and holds no audio files. Regenerate after an intended change with
`go test ./internal/media -run Corpus -update-corpus`, and review the diff.
It skips itself under `-race`; CI runs it in its own step.

## 9. What C3 has to do

1. From the cell's `cell.Status` (`AudioDown`, `AudioUp`, `CapAudioWB`) make
   a `CellCaps`. Offer wideband in `<<CODEC-LIST>>` only if
   `caps.Wideband()`.
2. At connect: `Choose(caps, airWideband, sipCodec)`.
3. After the audio bind (`cell-protocol.md` §8.1): open a UDP socket,
   `NewMediaOpen(…)`, send it, then `NewCellLeg(conn, msg, 8000)`.
4. `New(Config{…})` with `SIPLeg(call.Media())` and `Events` set only if
   the peer negotiated telephone-event, then `Run(ctx)`.
5. At the end: `calllog.Store.Ended(…, Quality: Summarise(…))`.

## 10. Decisions

| Decision | Alternative rejected | Reason |
|---|---|---|
| Format per direction on the cell link (HELLO tag 16, MEDIA_OPEN tag 13; openDECTcell#82) | One format for both | The downlink is measured as L16 and the uplink is not measured; the two may differ |
| G.722 from `gotranspile/g722` | Write our own; cgo to spandsp | Same lineage as Asterisk and FreeSWITCH, so bit-compatible with the peers; pure Go; permissive licence. It is a one-commit translation, so it sits behind our `codec` interface and can be swapped |
| G.722 input limited to ±30000 | Full scale; a fixed −1 dB gain | Cures the encoder's overload without changing any normal signal |
| Jitter buffer controls slack, not level | Level-based control | A loss lowered the level and permanently added a frame of delay |
| `MinDelay` 20 ms, `MaxDelay` 120 ms | 40/200 ms | The delay budget (§5) |
| In-band DTMF replaces audio | Mix into audio | Speech can mask the far end's detector |
| MOS on the narrowband scale | G.107.1 wideband scale | One scale for every call, so calls compare; a G.722 call is under-rated, and the record says so |
| Corpus signals generated | Recorded speech | Reproducible and licence-free. A real recording would be a good addition if one with a clear licence is found |
