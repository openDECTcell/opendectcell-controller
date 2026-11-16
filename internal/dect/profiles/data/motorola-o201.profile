# Motorola O201: a GAP-only, narrowband-only handset (it reports no CAT-iq level and never offers G.722).
format = 1
id = motorola-o201
kind = device
extends = motorola
name = "Motorola O201"
source = "DECT Command Finder run 2026-11-05, lab 7412; docs/vendor-specific-support.md s.7"
# source: run 2026-11-05 on the lab 7412 under the random identity 10729a3900, and the controller debug log of the same day.
# source: variant 1, basic service 0x88: the handset answered our CC-SETUP with CC-RELEASE-COM (reason 0x0E) after about 380 ms
# source: and never alerted, on the controller and again on the box.
# source: variant 2, basic service 0x80: authenticated, ciphered, alerted, answered, narrowband audio both ways (265 frames up,
# source: 241 down, no underruns).
cc.basic_service = basic
# source: outgoing calls, DECT Command Finder run 2026-11-05: with the plain setup the O201 played nothing, or only after seconds, on 10 or so calls;
# source: with <<PROGRESS-INDICATOR>> cause 8 ("in-band information is now available", EN 300 175-5 section 9.3.1.4) on our SETUP-ACK
# source: alone, audio at once on 2 of 2 calls, with it on SETUP-ACK, CALL-PROC and ALERTING on 3 of 3, on ALERTING alone on 0 of 3.
cc.progress_in_band = yes
# source: bench 2026-11-09, lab 7520 on the controller, incoming call hung up by the caller while the handset rang: our {CC-RELEASE} was
# source: acknowledged and the O201 answered {CC-RELEASE-COM} with reason 0x0E, yet it went on ringing for 10 to 15 s (three runs).
# source: Pressing the hook during that ring makes it place a call of its own ({CC-SETUP} from it), so its CC call was already gone: only
# source: the ringer runs on. <<SIGNAL>> 4FH (alerting off) before the release, cc.alert_off_on_release = yes, did NOT stop it (run 3): left off.
