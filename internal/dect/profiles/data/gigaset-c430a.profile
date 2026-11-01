# Gigaset C430A (NG-DECT Part 1).
format = 1
id = gigaset-c430a
kind = device
extends = gigaset
name = "Gigaset C430A"
source = "bench 2026-10-28: vRINGC430A-20261028-095744-1; [CR A-0125]"
# source: test 1 -- the name shows only as base-drawn display text
cc.name.method = both
# source: [CR A-0125] section 5, stock's framing, as bench test S3b sent it;
# test 1 put the number line, not the name, on screen: unconfirmed
cc.name.display = hex:02 0a 1a 91 02 02 {len} {text} 0d
