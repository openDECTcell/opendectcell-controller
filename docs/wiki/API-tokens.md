# API tokens

A **personal API token** lets a script, a widget (Home Assistant, a dashboard) or another program use your account through the API without your password.
Make one under **Account > API tokens**.

## Making one

1. Give it a **name** (what it is for), so you can tell your tokens apart later.
2. Tick what it **may do**: as little as the program needs. The list is in the table below; ticks marked "changes things" can change or delete your data.
3. Choose how long it **lasts**: 30 days, 90 days (the default), 180 days or a year. There are no tokens that never expire.
4. Confirm with your password (single sign-on: your user name) and press the button.

The token is shown **once**. Copy it then. The controller keeps only a fingerprint of it and cannot show it again; if you lose it, revoke it and make another. You can have 20 tokens at a time.
Treat a token like a password: it acts as you, with what you ticked, until it expires or is revoked.

## Using it

```sh
curl -H "Authorization: Bearer odc_0123456789abcdef_..." https://controller.example.org/api/v1/me
curl -H "Authorization: Bearer $TOKEN" "https://controller.example.org/api/v1/calls?missed=true&since=2026-11-01T00:00:00Z"
curl -X PATCH -H "Authorization: Bearer $TOKEN" -d '{"display_name":"Hall","dual_ring_policy":"busy"}' https://controller.example.org/api/v1/handsets/<id>
```

The API is described in [`docs/api/openapi.yaml`](../api/openapi.yaml). A token works on `/api/v1` only, not on the web pages.

| Scope | What it allows |
|---|---|
| `profile:read` | who you are (`/me`) |
| `handsets:read` | your handsets (name, device, lines, whether they are online), and their call lists and phonebook |
| `handsets:write` | change a handset's name, ring policy, default line and which VoIP line is on which key |
| `lines:read` | your VoIP lines and whether they are registered (**never their passwords**) |
| `lines:write` | make, change and delete your VoIP lines, including their passwords |
| `contacts:read`, `contacts:write` | read, or also make, change and delete, your contacts |
| `calls:read` | your calls, with filters (since, direction, missed, line, handset) and paging |
| `calls:write` | mark your missed calls as read |
| `statistics:read` | your numbers: calls in total, in the last day and week, calls per day |
| `stations:read` | the base stations you may see |
| `registration:write` | open, look at and close a registration window to register a handset |

## What a token can never do

* **Anything of the administrators' side**, organizations and the trust setting, enrolling base stations, or **making tokens**: those need you at the browser. There is no administrator scope.
  Even a token of an administrator, with every scope, is refused there (`403 token_not_allowed`).
* Read a VoIP line's password or a handset's key.
* Be used when its user is disabled.

A request whose token lacks a scope gets `403 insufficient_scope`, and the answer names the scope. A token that is wrong, expired or revoked gets `401 invalid_token`, even if the same request carries a valid session cookie.

## Keeping them safe

The list shows each token's name, what it may do, when it expires and **when and from where it was last used**: look at it now and then, and revoke what you do not use or do not recognise. Revoking works at once.
Changes made with a token are in the audit log under your name, marked with the token's name. Your password is not changed by any of this: changing it does not revoke tokens, revoke them yourself.
