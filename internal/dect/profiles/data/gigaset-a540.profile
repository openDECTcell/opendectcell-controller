# Gigaset A540 (NG-DECT Part 3). Everything it needed on the bench turned
# out to be the standard; this is its one habit.
format = 1
id = gigaset-a540
kind = device
extends = gigaset
name = "Gigaset A540"
source = "bench 2026-10-28 test 20: RES1 matched PIN 0000"
reg.tries_0000_first = yes
# The list change notices after its location registration (the Line Settings
# List, the List of Supported Lists) make it read the List of Supported Lists
# and give up the list session: it never reads the Internal Names List, so it
# keeps no name (bench 2026-11-01: none of the contacts with the notices got
# a name, all three without them did). Without a notice it reads lists 06 and
# 08 by itself when it reconnects.
notify = off
