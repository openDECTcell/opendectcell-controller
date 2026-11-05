# Webhooks

A **webhook** has the controller post an event to an address of your own the moment it happens: a call comes in, is missed or ends; a handset is heard or not heard;
the bell has something to say. Home Assistant, n8n, ntfy and your own scripts can receive them. Set them up under **Account > Webhooks**.

Only **your own** events are sent (your calls, your handsets, your notifications), to webhooks that are yours.

## Setting one up

1. **Name** and **address**. The address must start with `https://`. Where the operator has allowed private networks (`OPENDECTCELL_WEBHOOK_ALLOW_PRIVATE`) it may also be `http://` and an address on your own network, for a Home Assistant at home.
   The controller never posts to its own machine or the network around it (and never follows a redirect), whatever you write; a name that resolves there is refused when the post is made.
   Put no user name or password in the address. If the receiver wants a token, use the **Authorization** field (sent with every post, kept sealed).
2. The **events** it wants, and **what it posts**: *JSON* (the event as data, below) or *a short message* (plain text with `Title`, `Tags` and, for missed calls and danger notifications, `Priority: high` headers: what ntfy takes as it is).
3. Confirm with your password. The **signing secret** is shown **once**: put it into your receiver. You can make a new one on the webhook's page (the old one stops working at once).

**Send a test event** on the webhook's page posts a `ping` to it, whether it is on or not, and the page shows how it went.

## The events

| Event | When | `data` |
|---|---|---|
| `call.incoming` | a call comes in on one of your handsets | the call, as below |
| `call.ended` | any call of yours ends | the call |
| `call.missed` | an incoming call nobody answered ends (sent with `call.ended`) | the call |
| `handset.online` | a handset that was not heard is heard again | `id`, `display_name`, `presence`, `summary` |
| `handset.offline` | a handset is not heard any more (it said it was switched off, or did not answer for a while) | the same |
| `notification` | the notification centre tells you something | `kind`, `level`, `subject`, `body`, `href`, `summary` |
| `ping` | the test | `message` |

The call: `id`, `direction` (`in` or `out`), `remote_party` (the number), `remote_name` (the contact's name, if the number is one of your contacts), `started_at`, `answered_at`, `ended_at`,
`duration_seconds`, `result` (`answered`, `missed`, `not answered`, `ringing`, ...), `handset_id`, `handset`, `sip_account_id`, `line`, and `summary` (a sentence).
A handset's presence is looked at every 30 seconds: a handset that comes and goes inside that time is not told.

## What a post looks like

```
POST /hooks/opendectcell HTTP/1.1
Content-Type: application/json
User-Agent: opendectcell-controller
X-OpenDECTcell-Event: call.missed
X-OpenDECTcell-Delivery: 3f2d9c1e-...            (the id of the event; the same on every retry)
X-OpenDECTcell-Signature: t=1762344000,v1=5257a869...

{"id":"3f2d9c1e-...","type":"call.missed","created_at":"2026-11-05T12:00:00Z","data":{"id":4711,"direction":"in","remote_party":"+4930123456","remote_name":"Eva Meier", ... ,"summary":"Missed call from Eva Meier (+4930123456)"}}
```

**Check the signature** before you trust a post. `v1` is the hexadecimal HMAC-SHA256, keyed with your signing secret, of the text `<t>.<body>` (the number after `t=`, a full stop, and the body exactly as received).
Refuse a post whose `t` is more than a few minutes old (a recorded post played again), and use the delivery id to ignore a post you have already handled (a retry after your answer got lost).

```python
import hmac, hashlib, time

def valid(secret: bytes, header: str, body: bytes, tolerance=300) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(","))
    if abs(time.time() - int(parts["t"])) > tolerance:
        return False
    expected = hmac.new(secret, parts["t"].encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, parts["v1"])
```

## When the receiver is not there

The controller does not wait for the receiver. A post that gets no answer, a timeout, `408`, `429` or a `5xx` is tried again after 1 minute, 5 minutes, 30 minutes, 2 hours and 12 hours, and then given up.
Any other answer (`2xx` is success; `3xx` and `4xx` are not retried: a receiver that says `404` will say it again) ends the delivery. A webhook whose deliveries fail **ten times in a row** is switched off, with the reason on its page;
switch it on again there once the receiver is fixed. At most 500 posts wait for one webhook; past that the oldest are dropped. The log of the last posts is on the webhook's page and is kept 7 days.
You can have 10 webhooks.

## Receivers

* **Home Assistant:** create an automation with the *Webhook* trigger, use `https://<your home assistant>/api/webhook/<id>` as the address (on your own network this needs `OPENDECTCELL_WEBHOOK_ALLOW_PRIVATE`), and read `trigger.json.data.summary` and the rest.
* **ntfy:** address `https://ntfy.sh/<your topic>` (or your own server), format *a short message*; for a server with access control put `Bearer <token>` in the Authorization field.
* **n8n and others:** a webhook node; verify the signature in a code node if the address is reachable by others.
