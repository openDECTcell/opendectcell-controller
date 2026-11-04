# Glossary

| Word | Meaning |
|---|---|
| **Base, base station** | The part of a DECT system that talks to the handsets over the air. Here: a FRITZ!Box with openDECTcell. |
| **Cell** | A base that is part of a controller. It runs the radio only; the controller runs everything else. |
| **Controller** | The server that runs the DECT stack for all cells, the SIP accounts, the web interface and the database. |
| **DECT** | The cordless-phone radio standard. **GAP** is the basic profile every handset speaks; **CAT-iq** is the newer one with HD voice and lines. |
| **EIC** | The Equipment Installer's Code: the 16-bit operator code in a base's identity, given out by ETSI. A system may use its own. |
| **FPN, FPS, RPN** | Parts of a base's identity: the fixed-part number and sub-number, and the radio-part number. Each cell has its own, derived from the system identity. |
| **RFPI** | The identity a base broadcasts: EIC, FPN, FPS and RPN. |
| **System identity** | What all your cells share so that a handset sees them as one network. |
| **IPEI** | A handset's own serial number. |
| **IPUI** | The identity a handset has inside the network, derived from its IPEI. Shown on the handset page. |
| **UAK** | The key a handset and the controller share after registering. Stored encrypted. |
| **PIN** | The digits you type into a handset to register it. |
| **TPUI** | The short temporary number a cell calls a handset by. Given by the controller when a handset registers on a cell. |
| **Slot class** | Handsets whose TPUI ends in the same hexadecimal digit. On a 7412 cell only one of them can hold a link at a time. |
| **Location registration** | A handset telling a cell "I am here". It repeats it every few minutes while idle. |
| **Working set** | The handsets a cell currently holds in memory. |
| **VoIP line** | A SIP account you set up in the web interface (user name, password, registrar). |
| **Line 1, Line 2** | The two places on a handset that each hold one VoIP line. A call to a VoIP line rings the handsets that have it as a line; a call from a handset goes out on its default line. |
| **Device profile** | A handset model's small set of habits, chosen when registering. |
| **SIP, RTP, SRTP** | The call signalling, the audio stream, and the encrypted audio stream. |
| **G.722, G.711** | The HD (wideband) and standard (narrowband) voice codecs. |
| **mTLS** | TLS where both sides prove who they are with certificates: the link between cell and controller. |
| **OIDC** | OpenID Connect, single sign-on, for example with Authentik. |
| **Enrollment string** | The one-time secret that makes a box a cell. |
| **libopendectcell** | The C library with the DECT protocol stack that the controller (and the standalone daemon) uses. |
| **PCM** | The voice bus between the DECT chip and the router's processor. |
