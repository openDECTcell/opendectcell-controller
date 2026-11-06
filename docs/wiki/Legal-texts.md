# Legal texts: notices every user has to sign off

Who runs a controller for other people (a makerspace, a club, friends) often has to say who they are, what happens to
people's data, and that the service is not a phone company. The controller has four texts for this. Each is **off until an
administrator switches it on** (Administration > Legal texts):

| Text | What it is | Who writes it |
|---|---|---|
| **Legal notice (Impressum)** | who runs this installation and how to reach them | you; there is no standard text |
| **Privacy notice** | what data is kept, why, for how long, who sees it, how to get it erased | you; there is no standard text |
| **Emergency-call warning** | the service must not be relied on for emergency calls (112, 110 ...) | a standard text comes with the controller; yours is added below it |
| **Experimental software** | experimental, no warranty, no guaranteed service | a standard text comes with the controller; yours is added below it |

Texts are plain text; a blank line starts a new paragraph. The standard texts are short and general: have them read by
whoever is responsible for your installation, and add what your situation needs.

## What users see

* **At registration.** The sign-up page shows every switched-on text in a window of its own (they scroll, so the page stays
  short) with a box to tick for each. Without all boxes there is no account.
* **Everybody else, at the next visit.** A user (an administrator too, an account made by an administrator, or one that came in
  through single sign-on) who has not signed off every switched-on text **gets no access** to the web interface or to the API
  until they have: they land on a page that shows the texts and the boxes, and nothing else opens. Handsets that are already
  working keep calling; only the web interface and the API are closed (`403 legal_acceptance_required` for the API, also with a
  token).
* **Again after a change.** Switching a text on, or changing its words, asks everybody again for that text. Saving the form
  without changing a text does not. Switching a text off stops asking.
* **To read again.** A **Legal notice** entry in the menu on the left, and a link in the footer of every page (also the login
  page, so it can be read without an account), show all switched-on texts, each in its own scrolling window.

## What is kept

For each user and text: the version they signed off and when. It is part of the export of their data on the Account page and is
erased with the account. The audit log records that a sign-off happened (`legal.accept`) and that the texts were changed
(`legal.update`), never the texts' words.

The controller does not check what you write for legal correctness, and it is not legal advice.
