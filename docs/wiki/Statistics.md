# Statistics

**Statistics** (menu: Phones or Monitoring, depending on your role) shows your own calls; administrators also have **Administration > Statistics** for the whole
controller. Choose the range at the top: 7, 14, 30 or 90 days. Every table has a **CSV** link: the file holds what the table shows, for the range you chose, and
nothing of anybody else's.

| Part | What it says |
|---|---|
| **The last N days against the N before** | calls, how many were answered, missed incoming calls, outgoing calls that never reached a conversation, talk time, each with its change |
| **Calls per day** | as bars: the answered part solid |
| **Most calls at once, per day** | the highest number of calls going on at the same moment on each day. A call counts from its start to its end, also across midnight |
| **When the calls are** | a grid of weekday by hour (the controller's local time); the darker, the more calls |
| **Most calls at once, per base station** | the same, per base station, against what the base station says it can carry. **full** means it reached that number at some moment: a further call would not have been carried |
| **Call quality, by day** | the average MOS of the calls whose media was measured, and how many were poor (MOS below 3.5; 4 and above is good) |
| **VoIP lines** | per line: how much of the time it was registered, calls, answered, missed, talk time, quality. Administrators see the least available lines first |
| **Handsets** | per handset: calls, answered, missed, talk time, quality |
| **The worst calls** | your five calls with the lowest MOS, each with a link to its details |
| **How calls ended**, **Base stations used** | as before |

*Registered* is the share of the time a line was switched on and watched in which it was registered, from its registration history, which is kept 90 days (so the 90-day range
can show less than 90 days). A line without a record shows "no record". Time before the first record and time a line was switched off are not counted.

Not there: how long handsets were online (nothing samples that), call set-up time, and the SIP status of a failed call (the log keeps a reason in words, shown under How calls ended).

## The monthly usage report (administrators)

**Administration > Statistics > Monthly usage report** lists, for a month, the calls of each **user** and of each **organization** (calls, answered, talk time, and the handsets and VoIP
lines they have now), with CSV for both. An organization's figures are those of its members; a user in two organizations is counted in both. It is meant for whoever hosts others and
wants to say "your group used the system this much in October". The default month is the one before this one.
