# ADR 0007: Organizations, and whose base stations a handset may use

Status: **decided** by the maintainer 2026-11-03 (answer to the external audit's CTRL-SEC-04 / CTRL-PRIV-02); built the same day.

## The problem

In the shared network ([ADR 0004](0004-shared-network.md)) a handset of user A may attach to a base station that user B
runs. DECT lets a base station do that only if it holds the handset's authentication key. Whoever has root on that box can
read the key, listen to the calls that pass through it and see the numbers dialled. That is a property of DECT, not of this
software: TLS between the cell and the controller and SRTP towards the provider do not help, because the cell itself is the
endpoint. A key that was read once stays useful until the handset is registered again, so removing a cell from the controller
does not take it back.

So a user must be able to decide whom they trust, and must be told what the choice means.

## Decision

1. **Disclosure.** Where the user chooses (the trust setting), when a handset is registered, and on the base stations
   page, the controller says in plain words what the operator of a base station can read, that this is how DECT works and
   cannot be changed here, and that re-registering the handset is the only way to void a key that has been read.
2. **A trust setting per user** (`app_user.trust_scope`), for the user's own handsets:

   | Value | Handsets of this user may be placed on |
   |---|---|
   | `all` | every base station of the controller (the default, as before) |
   | `own` | the user's own base stations only |
   | `orgs` | the user's own plus those of the users who are members of the organizations the user chose to trust |

   Whether a handset may be on a cell is one SQL function, `handset_may_use_cell(owner, cell)`, used everywhere a record is
   put on a cell, a call chooses cells to ring on, and the clean-up that removes records after a change. A cell's
   *operator* is the user who enrolled it (`cell.enrolled_by`). A cell without one is trusted only under `all`.
3. **Organizations.** Any user or admin can create one; the creator is its owner and an admin of it. Members join with an
   invitation token. An organization is only a group to trust: it gives its members no rights over each other's handsets,
   cells or accounts.

   | Who | May |
   |---|---|
   | owner | everything an admin may; appoint and demote admins; rename or delete the organization |
   | admin | create invitation tokens; remove members (not the owner, and only the owner removes admins) |
   | member | see the organization's overview; give it a display name of their own; trust it or not; leave |
   | controller admin | the same as an owner, for any organization (needed for one whose owner is gone) |

   A token is single use and valid for 7 days; only its hash is stored. A user who joins is offered the organization's own
   name as their display name and can change it.
4. **What a member sees:** the display names of the other members and the number of cells each runs; the number of cells of
   the organization with the online and offline counts under it; the same for the handsets of its members. Nothing else
   about another member's cells or handsets (no names, locations, identities).
5. **A change takes effect at once.** When the setting, a membership, an organization or a cell's operator changes, the
   controller removes the records that are no longer allowed from the cells' tables. A key in a chip slot stays in the chip
   until the slot is overwritten or the chip reloaded ([ADR 0004](0004-shared-network.md), "Keys only when needed"); that is
   stated on the page and is another reason to register the handset again after a suspected leak.

6. **A setting that strands the user's handsets asks first.** If, under the new setting, the user has handsets and no active base station
   they may be put on, the setting is not applied until the user confirms (`SetTrustChecked`; in the API `409 no_usable_cell` unless
   `confirm: true`). `all` is never refused, as it is the widest. Whenever the user is in that state (also after the fact, when a
   cell is revoked or a member leaves) a warning shows on the handsets and trust pages and as `no_usable_cell` in `GET /me/trust`.
7. **The API** has the same operations (`docs/api/openapi.yaml`: `/me/trust`, `/organizations...`) with three new authorisation rules,
   `org-member`, `org-admin`, `org-owner`; a test fails when a registered API route is missing from the document.

## Alternatives considered

- **A per-cell opt-out** (a cell refuses foreign handsets). It protects the operator's capacity, not the handset owner's key,
  so it answers a different question; it stays open in ADR 0004.
- **Organizations as a unit of ownership** (shared handsets and accounts). Rejected for now: it needs a model for who may
  change what, and nobody asked for it. Trust is the only thing an organization does here.
- **Trusting by single named users** instead of organizations. Rejected by the maintainer: an organization is the unit people
  think in (a makerspace, a family), and it scales to the hosted case.
- **Default `own`.** Safer, but it would break the shared network for everyone without a cell. The default stays `all` and the
  choice is shown prominently; the maintainer may change it.

## Not solved

A cell operator can still read the keys of the handsets that are on their cell, and the controller cannot prevent it. The
policy only limits which cells ever receive a key.
