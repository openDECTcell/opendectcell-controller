# Move or back up your setup

**Account > Move or back up my setup** writes your handsets, VoIP lines and contacts into one file, and reads such a file back. Use it to
move to another controller, to have everything again after a loss, or to bring many VoIP lines in from a spreadsheet.

## What the file holds

| Part | Without keys | With keys and passwords |
|---|---|---|
| **Handsets** | name, device profile, ring policy, internal number, which VoIP line is on Line 1 and 2, default line | also the **key** the handset and the base station share (so the handset need not be registered again) |
| **VoIP lines** | every setting (server, transport, codecs, encryption, number rules, ...) | also the **password** and the TURN password |
| **Contacts** | your own contacts (as vCards) | the same |

Not in the file: address books (they come from their server), call history, shared VoIP lines, base stations.

## Keys and passwords: the file is a secret

With **Include the handsets' keys and the VoIP passwords** ticked, the file is **encrypted** with a passphrase you choose (at least 12
characters; Argon2id for the key, AES-256-GCM for the data). Treat it as you treat a password:

* Whoever has the file **and** the passphrase can pose as your handsets and use your VoIP accounts.
* The controller never stores the passphrase and cannot recover it. A lost passphrase is a lost file.
* To export with keys you confirm with your password (single sign-on: your user name), so a session left open cannot be used to take them.
* Keys and passwords are never written to a log or the audit log; the audit log only notes that an export or import happened, and how many things were in it.
* A file without keys is plain JSON and holds no secret. A plain file that does carry a key or a password is refused on import.

## Importing

**Check the file** shows what an import would do and changes nothing. **Import** does it. For every item the result says one of:

| Result | Meaning |
|---|---|
| added | made new, for **you** (an import only ever creates things in the account that imports) |
| updated | a handset you have already got the file's name, ring policy and lines; its key stays as it is |
| left as it is | a VoIP line with the same user name and domain, or a contact with the same name and number, you have already |
| not possible | a setting out of range, a damaged key, a limit (20 lines for a user who is not an administrator); the reason is shown, the rest goes on |

A handset whose identity is registered on this controller, **by anyone**, is never taken over by a file: a key cannot be replaced by importing.
Delete the handset first if it is yours and you want the file's version.

A handset that comes in with its key gets a temporary identity of this controller and no place on a base station yet; the first base station
that hears it takes it in. **Whether it then works on air has not been tried on hardware**: try one handset before you delete the old controller.

A VoIP line without a password (a file without keys) comes in without one: enter it on the line's page.

## Many VoIP lines from a spreadsheet

On the same page, **VoIP lines from a spreadsheet** reads a CSV file (comma or semicolon). Download the template, one row per line. Needed: `username`
and `domain`; optional: `label`, `password`, `auth_username`, `display_name`, `registrar`, `outbound_proxy`, `transport`. Everything else takes
the defaults of a new line. Lines are on and registering. Check first; the passwords in your file are not kept or shown.
