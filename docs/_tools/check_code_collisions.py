#!/usr/bin/env python3
"""Check that no hub reason code is also an anet reason code by accident.

    python3 docs/_tools/check_code_collisions.py --hub-codes /path/to/ANetHub/internal/wire3/codes.txt

Both registers list the codes of one repository each. A name that is in both
must be there on purpose: the hub returns the code on the wire and the node
passes it on under the same name and meaning. Such names are listed in
MIRRORED below; any other shared name is an error, and so is a name in
MIRRORED that is no longer shared. Retired codes count, because a retired
name is never used again.

build-site.sh runs this check when the hub register can be found (the
ANET_HUB_CODES variable, or ../ANetHub beside the ANet checkout).
"""
import argparse
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
ANET_CODES = HERE.parent.parent / "internal" / "a2ashape" / "codes.txt"

# Codes that the hub returns and the node reports under the same name.
MIRRORED = {
    "bad_envelope", "bad_request", "home_unreachable", "invite_removed",
    "mailbox_full", "probation", "proof_unavailable", "rate_limited",
    "receipt_not_reviewable", "recipient_unknown", "redelivery_limited",
    "requester_commit_mismatch", "review_exists", "review_window_closed",
    "strangers_busy", "too_large", "too_large_for_stranger", "wire_too_new",
    "wire_too_old",
}


def read(path):
    out = set()
    for line in pathlib.Path(path).read_text(encoding="utf-8").splitlines():
        if line.strip() and not line.startswith("#"):
            out.add(line.split("\t")[0])
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hub-codes", required=True)
    args = ap.parse_args()
    shared = read(ANET_CODES) & read(args.hub_codes)
    bad = False
    for c in sorted(shared - MIRRORED):
        print("%s is a code of anet and of the hub, and is not listed as mirrored in %s" % (c, pathlib.Path(__file__).name),
              file=sys.stderr)
        bad = True
    for c in sorted(MIRRORED - shared):
        print("%s is listed as mirrored and is no longer in both registers; remove it from MIRRORED" % c,
              file=sys.stderr)
        bad = True
    if bad:
        sys.exit(1)
    print("code collisions: none (%d mirrored on purpose)" % len(shared))


if __name__ == "__main__":
    main()
