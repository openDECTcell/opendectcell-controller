# AGENTS.md: rules for AI agents (and humans) working on opendectcell-controller

Read this completely before changing anything. For the current project state, see the maintainers' notes. If a rule is unclear or wrong, propose a change to this file in its own commit; don't silently deviate. The sister repository **openDECTcell** (cell side: firmware loader, kernel module, `libopendectcell`) has its own `AGENTS.md`. Its clean-room rules apply here too.

## 1. What this project is, and what it is not

- **It is** a DECT↔SIP translator. Each handset is represented by up to **two SIP user agents** (lines). The controller also provides the web UI, the admin UI and cell management.
- **It is not a PBX:**
  - no dial plan, call routing or hunt groups
  - no voicemail storage, IVR or conferencing server
  - no SIP registrar or proxy for third parties

  Feature requests that belong in a PBX are declined, or solved by passing data through: for example, showing the PBX's message-waiting state on the handset.
- Handset-to-handset calls go through the SIP server like any other call.

## 2. Clean-room and blob rules (inherited from openDECTcell)

1. Never commit AVM material (binaries, firmware, decompiled code, calibration data).
2. Never paste decompiled or disassembled code anywhere. Use facts in your own words, with the source named.
3. The DECT protocol logic comes from `libopendectcell` (C) in the openDECTcell repo. Don't fork it into this repo; use it as a dependency.

## 3. Licence

- **AGPL-3.0-or-later.** Every source file carries `SPDX-License-Identifier: AGPL-3.0-or-later`.
- Dependencies must be AGPL-3.0 compatible (GPL-2.0-**or-later**, GPL-3.0, LGPL, MIT, BSD, Apache-2.0). GPL-2.0-**only** code cannot be linked in. This also applies to `libopendectcell` from the openDECTcell repo, which therefore must be GPL-2.0-or-later or more permissive.

## 4. Commit and push discipline

- **Commit early and often:** one logical change per commit (a doc section, a model plus its migration, a handler plus its tests).
- **Push after every commit** (or every few small ones), so changes stay traceable and easy to revert.
- **Never force-push** or rewrite published history; use `git revert` or follow-up commits.
- **Message format:** an imperative summary of 72 characters or less, a blank line, then the *why*. Reference issues (`Refs #n`, `Fixes #n`), plus the attribution line your tool requires.
- **Check `git diff --staged` before committing.** Never commit secrets, `.env` files, keys, certificates or databases.
- **Author:** `ctandi <ctandi@users.noreply.github.com>` (repo-local).

## 5. Security rules

- **Secrets encrypted at rest:**
  - DECT authentication keys (UAK/DCK) and SIP credentials are encrypted in the database. The key comes from config or a secret store, never from the repo.
  - Logs never contain secrets, PINs or keys.
- **Cell enrollment:** cells enroll with a one-time token and then authenticate with **mutual TLS** (per-cell certificate). There are no shared static passwords.
- **Access control:** every web or API action is checked server-side against the user's role (admin / user) and ownership (a user only sees their own handsets and accounts).

## 6. Feature framework rule

DECT features beyond basic calls (see `docs/dect-features.md`) are implemented as **feature modules**:
- Each module declares the handset capabilities or models it needs.
- It is enabled per handset, based on the capabilities the handset signalled and a model-quirks table.
- The core call path must never depend on an optional feature module.

## 7. Testing

- Unit tests for every module. SIP interop is tested against at least Asterisk and FreeSWITCH, ideally also a FRITZ!Box as registrar.
- DECT-side tests use openDECTcell's simulator and golden traces until real cells are available.
- **Gate G7 is passed** (openDECTcell, 2026-10-18 to 2026-10-25: a handset rings and audio passes), so DECT and SIP
  call logic is built and runs on real hardware. The rule that stays: do not build protocol logic on an
  assumption about the chip or the air interface that has not been measured or sourced. Say in the worklog
  what was seen on air and what was only tested against simulated cells.

## 8. Documentation

- Keep `docs/` current. Every design decision gets a short note: the decision, the alternatives and the reason.
- Write in English, concisely, preferring tables.

## 9. Asking openDECTcell questions

The DECT protocol logic lives in the **openDECTcell** repo and is developed by a
different agent on different hardware. That agent has the chip, the handsets
and the clean-room answers; you do not. When the controller needs a fact about
chip behaviour, mail primitives, `libopendectcell` APIs or air-side protocol, **ask
rather than guess** -- the cost of a wrong assumption here is a design built on
it.

**How to ask.** Open an issue **in `openDECTcell/opendectcell`** (not in this repo -- the
answer lives where the knowledge is):

- title prefixed **`Controller Q: `**, label **`question`**
- **Question.** one sentence, answerable
- **Why.** what depends on it, linked to the controller doc or task
- **Fallback.** what you will assume if nobody answers, so you are not blocked
- **Blocking?** say plainly whether work stops without it
- the openDECTcell commit you checked against

Issue #66 in that repo is a good model; it was written to this shape before
this section existed.

**The state model is the issue state.** **Open means waiting on openDECTcell.
Closed means answered** -- the answer is in the comments. Nothing else to
track.

- **Work on your own and do not block.** Ask, state the fallback, and carry
  on with something else. If the answer later contradicts the fallback, that
  is a normal revision, not a crisis. Switching task is always better than
  waiting.
- **Chase your own answers.** Nobody will hand them to you. Check with
  `gh issue list -R openDECTcell/opendectcell --label question --state closed --limit 20`,
  and read the answer with `gh issue view <n> -R openDECTcell/opendectcell --comments`.
- **When to check, without burning tokens.** Do it at natural boundaries --
  the start of a work session, the end of each task, and before finalising
  anything that rests on a fallback. **Never poll in a loop**, and never wake
  repeatedly just to look: a question typically takes 10-20 minutes to answer
  and may take hours, so a frequent poll is pure waste.
  If you are genuinely blocked with nothing else to do, wait in **one** shell
  call rather than a sequence of turns:

  ```
  sleep 600; gh issue view <n> -R openDECTcell/opendectcell --json state,comments
  ```

  That costs one tool call per ten minutes instead of a wake-up cycle. Do it
  at most two or three times, then proceed on the fallback and move on --
  being blocked is not a reason to idle, it is a reason to change task.
- If an answer is wrong or incomplete, **reopen the issue** and say why. The
  openDECTcell side would rather be corrected than have a bad answer propagate.
- Answers may be marked `[observed]`, `[assumed]`, `[derived]` or
  `[not established]`. **Treat `[assumed]` and `[not established]` as what they
  say** -- do not harden them into design assumptions without saying so.

**What you must not ask for.** The clean-room rules in §2 apply to questions
too: never request AVM firmware, source, disassembly or binaries, and never
request or publish DECT identities (RFPI, IPUI, TPUI), authentication keys or
captured speech. Ask about *behaviour and interfaces*, which is what you need
anyway.

## 10. Work log and review loop

The maintainers review this work and decide what gets built next. They are not
watching the commits as they happen, so the log is how they see what you did.

- **Keep the maintainers' notes current.** Add an entry for every working session,
  newest first. An entry states what changed (with commit hashes), the
  decisions taken and why, and anything that is uncertain or incomplete.
- **Surface decisions, don't bury them.** Anything a reviewer might reasonably
  overrule — a library choice, a schema shape, an interpretation of a design
  doc — goes in the entry's decisions list explicitly, with the alternative you
  rejected. A decision only mentioned in a commit message has not been
  surfaced.
- **Ask rather than assume.** Open questions go in the entry under their own
  heading. If a question blocks you, say so and work on something else
  meanwhile rather than guessing.
- **Report honestly.** If something does not work, is half-finished, or was
  skipped, the entry says so plainly. An entry that only lists successes is not
  useful for review.
- **Commit and push frequently** (§4). Small, reviewable commits that are
  already pushed; nothing of value sits only on your machine.
- **Wait for review before starting a new milestone.** Finishing the tasks you
  were given is the point at which you report and stop, not the point at which
  you pick the next ones yourself.
