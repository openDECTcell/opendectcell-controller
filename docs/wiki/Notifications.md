# Notifications

The controller tells you when something needs you, once, and tells you again when it is over. You find the notifications under the **bell** in the
menu (and at **Notifications**), with a red number when some are unread. You can also have them by e-mail.

## What it tells

| Kind | Who | When |
|---|---|---|
| **A VoIP line cannot register** | the line's owner; the administrators for a shared line | the line has been refused or unreachable for 10 minutes; again when it registers |
| **A handset has not been heard for a long time** | the handset's owner, **only if they ask** (Account > Notifications > hours; 0 is off) | the handset was not heard for as long as you set; again when it is heard |
| **A base station is offline** | the one who enrolled it, and the administrators | offline for 10 minutes; again when it is back |
| **Base station certificates, or the certificate authority, end soon** | administrators | certificates end within 14 days and could not renew; the authority ends within 90 days |
| **A backup failed or is old** | administrators | the last run of the [backup service](Operating.md) failed (also: it did not restore, or the SFTP copy failed), or the last good backup is more than 36 hours old; again when it is in order. Nothing is said if there is no backup service |

A short outage (less than the time above) is not told. A line you delete or switch off while it is down is forgotten, not announced as "back".

Not there yet: a database or disk nearly full (the controller cannot see the disk its database is on), a DECT worker that restarted, a new
release, (a failed or old backup is told now).

## Choosing

**Account > Notifications** has, for every kind, **Show** (in the interface) and **Mail**; the hours for a handset that is not heard; and **quiet hours**.
Mail that comes during the quiet hours waits for their end and then comes as **one** mail for all of it. The hours are in the controller's local
time. Mail needs your account to have an e-mail address and the controller a mail server. The mail for a base station that is offline is the older
option "When a cell has been offline for 10 minutes" under E-mail notifications.

Notifications are kept for 90 days. Mail that could not be sent for a day is given up, not sent late.
