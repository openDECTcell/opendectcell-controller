# DECT features beyond basic calls, and the per-model feature framework

**Status:** design reference. **None of these features is planned for the first release.** The controller must be built so that they can be added later as **feature modules**, enabled **per handset** based on what the handset declares and on a per-model quirks table.

Sources: ETSI EN 300 444 V2.5.1 (GAP), TS 102 527-1 V1.5.1 (NG-DECT Part 1, wideband), TS 102 527-3 V1.7.1 (Part 3, extended wideband speech services), TS 102 527-4 V1.3.1 (Part 4, light data / SUOTA), TS 102 527-5 V1.3.1 (Part 5, additional feature set nr. 1), EN 300 175-5 V2.9.1 (NWK layer), TS 102 939-1/-2 (ULE). CAT-iq naming as used by the DECT Forum; the mapping to the ETSI parts is from secondary sources and not verified against the DECT Forum test specs.

## 1. Profile map

| Marketing name | ETSI spec | Content (short) |
|---|---|---|
| GAP | EN 300 444 | Basic calls, registration, authentication, ciphering. CLIP, CNIP and internal calls are optional |
| CAT-iq 1.0 "HD voice" | TS 102 527-1 | Wideband (G.722) codec negotiation and switching; CLIP mandatory |
| CAT-iq 2.0 "multi-line" | TS 102 527-3 | Missed calls, MWI, date/time, parallel and multiple calls, transfer, multi-line, list access (call lists, phonebook, settings), CLIR, call forwarding, DTMF rules, handset locator, … |
| CAT-iq 2.1 "green" | TS 102 527-5 | Line/diagnostic status, SMS, answering machine, call screening, TX power control |
| CAT-iq Data (formerly 3.0) | TS 102 527-4 | SUOTA, content download, HTTP apps. Needs a DPRS data U-plane |
| CAT-iq IoT (formerly 4.0) / ULE | TS 102 939 | Home-automation devices. Out of scope |

## 2. Candidate features (all later; none for 1.0)

The "Data source" column shows where the controller would get the information. Because the controller is **not a PBX**, most data comes from SIP, from the controller's own call log, or from external services.

| Feature | Spec | NWK mechanism (summary) | Data source | Notes |
|---|---|---|---|---|
| **CLIP / CNIP** (caller number and name) | GAP 8.41/8.42; Part 1/3 | Calling-party number and name IEs in CC-SETUP/CC-INFO; UTF-8 CNIP in Part 3 | SIP From/PAI display name | Basic CLIP is part of the core call path; CNIP is a module |
| **Date and time** | Part 3 N.5 | FACILITY (CISS) with the TIME-DATE IE | Controller system time (NTP) | Push on registration, locate and periodically. Simple, so a good first module |
| **Missed-call notification** | Part 3 N.3 | FACILITY with EVENTS-NOTIFICATION (type "missed call", count) plus line id | Controller call log per handset/line | Reset when the user reads the list or picks up |
| **Message waiting (voicemail)** | Part 3 N.4 | EVENTS-NOTIFICATION type "message waiting", subtype voice | SIP MWI (`message-summary` NOTIFY) of the line's account | Pass-through only; the voicemail itself stays in the PBX |
| **Call lists** (missed, outgoing, accepted, all) | Part 3 N.16 list access | IWU-INFO with IWU-TO-IWU (list-access protocol), from idle via a "LiA service call" | Controller call log | |
| **Phonebook (contacts)** | Part 3 N.16 list access (contacts list) | as above | External source later (e.g. CardDAV or LDAP); never a PBX function | |
| **Line settings, CLIR, call forwarding settings** | Part 3 N.12–N.18 | List access (line settings) | Per-line settings. Forwarding is relayed to the PBX/provider (SIP feature codes or headers) only if configured | Must stay pass-through (not a PBX) |
| **Multi-line, line selection** | Part 3 N.12/N.14 | CALL-INFORMATION IE (line id) in CC messages | The handset's line 1 and line 2 | Used by the core for **outgoing line selection** on CAT-iq 2.0 handsets |
| **Call waiting, toggle, hold, resume, transfer, 3-party conference** | Part 3 N.6–N.9 | Control codes as keypad info in CC-INFO plus a call id; call-waiting indication with the SIGNAL IE | SIP re-INVITE (hold), REFER (transfer). Conference only if the PBX provides it | Unsupported codes are answered with the standard negative acknowledgement ("control code not supported") |
| **Call deflection, intrusion** | Part 3 N.10/N.11 | Control codes | SIP 302/REFER (deflection); intrusion between own handsets only | Low priority |
| **DTMF** | Part 3 N.19 | Keypad info after CONNECT; downlink in-band | SIP DTMF modes | Uplink is part of the core call path |
| **Handset locator (paging)** | Part 3 A.3 | CC-SETUP to all idle handsets with special presentation | Admin/user UI button | Nice for the web UI |
| **Internal names list** | Part 3 list access | List access | Handset names in the controller | |
| **SMS** | Part 5 N.24 | List access lists plus events | SIP MESSAGE (pass-through) | Low priority |
| **Answering machine (DTAM), call screening** | Part 5 N.25/N.26 | List access plus a DTAM consulting call | — | Not planned (PBX function) |
| **Line and diagnostic status** | Part 5 N.23 | List access (read-only) plus events | Registration state of the SIP accounts | Useful: shows "line not registered" on the handset |
| **SUOTA (handset firmware updates)** | Part 4 | FACILITY/IWU-TO-IWU version indication; HTTP over DPRS | Vendor update servers | Needs DPRS data support in the STUB; very low priority |
| **Vendor features** (AVM FRITZ!Fon, Gigaset, …) | Proprietary | ESCAPE-TO-PROPRIETARY IE (EMC-based), list-access commands/list ids ≥ 80h, reserved IWU-TO-IWU protocol discriminators | — | Only via the per-model quirks mechanism, and only for documented behaviour |

## 3. Capability discovery

| What | Where it comes from | Use |
|---|---|---|
| **Profile support** (Part 1 / Part 3 / Part 5), headset, display capabilities, DSAA2/DSC2 | TERMINAL-CAPABILITY IE in ACCESS-RIGHTS-REQUEST and LOCATE-REQUEST | Primary switch for enabling feature modules |
| **Manufacturer and model** | MODEL-IDENTIFIER IE (MANIC = EMC, MODIC = model/version, or IMEISV) in ACCESS-RIGHTS-REQUEST, LOCATE-REQUEST, IDENTITY-REPLY (optional IE) | Key into the per-model quirks table |
| **Codec list** | Sent at registration/locate (Part 1) | Wideband yes/no |
| **Supported lists** | List access, list 0 "supported lists" | Runtime discovery for list-based features |
| **Unsupported control codes** | Negative acknowledgement | Graceful degradation in both directions |

**FP side (advertised to handsets).** The base broadcasts "extended higher layer capabilities" in the MAC Q-channel: profile bits (Part 1/3/5) and per-feature bits (CLIR, conference, intrusion, deflection, multi-line, no-emission, DSAA2/DSC2, …). This broadcast is generated by the STUB on the cell. **Open question for openDECTcell:** how these bits are configured through STUB mails. The controller must only advertise what it actually implements.

## 4. Framework design

```
             ┌─────────────── handset record ─────────────────┐
             │ IPUI · terminal capabilities · model id (EMC,  │
             │ MODIC) · codec list · supported lists · quirks │
             └───────────────────────┬────────────────────────┘
                                     │ evaluated on registration / locate
              ┌──────────────────────▼───────────────────────┐
              │ feature registry: module.requires(handset)   │
              │   → enabled set per handset (+ user/admin    │
              │     overrides in `feature_setting`)          │
              └──────────────────────┬───────────────────────┘
          core call path (always) ───┼─── optional modules via hooks
```

- **Feature module interface** (language-neutral):
  - `id`, `version`, `requires(capabilities, model) -> bool`, `default_enabled`
  - **hooks:**
    - `on_register` / `on_locate`
    - `on_idle_tick`
    - `on_call_event` (setup, connect, release, missed)
    - `on_sip_event` (MWI NOTIFY, registration state)
    - `on_nwk_message` (FACILITY, IWU-INFO, CC-INFO control codes, list-access sessions)
  - `settings_schema` for the UI
- **Dispatch:** incoming NWK messages and IEs that the core does not handle are routed to the enabled module that claims them, for example a list-access session to the "lists" module. If no module claims them, the standard negative acknowledgement or reject is sent.
- **Quirks table:** `(EMC, MODIC range) → {forced on/off features, workarounds}`. Kept in data (YAML or the DB), not in code, so model support can be added without a release.
- **Admin and user control:** an admin enables modules globally; users toggle per handset where it makes sense, e.g. date/time sync or the missed-call indicator.
- **Data-model hooks:** `handset.capabilities` (raw IE plus parsed fields), `handset.model` (EMC, MODIC), `feature_setting`, a per-handset per-line `call_log` (for missed calls and call lists), and the per-line SIP MWI state.
- **Core vs module boundary (rule):**
  - **core:** CLIP, DTMF uplink, codec negotiation (narrowband and wideband), outgoing line selection
  - **modules:** everything else in §2

  The core never depends on a module.

## 5. Suggested order (when the time comes)

1. Date/time (FACILITY + TIME-DATE): small, visible, and exercises the module plumbing.
2. Missed-call notification (needs the call log).
3. MWI (from SIP).
4. CNIP and multi-line display.
5. Call lists via list access (introduces the list-access protocol).
6. Hold/toggle/call waiting, then transfer.
7. Line/diagnostic status (shows the SIP registration state on the handset).
8. Phonebook (with an external source).
