"""Verify the cards of G-PAY-EXT-1 (ext-card-1.json) with the a2a-python reference SDK.

Each card is parsed the way the SDK's card resolver parses a fetched card, its JWS payload is
recomputed with the SDK's own canonicalisation and compared with the vector's, and its signature
is verified with the SDK's verifier under the conformance agent identity's key: Ed25519 seed
SHA-256("anet-suite-identity-v1/cur"), the key the vector names as kid did:anet:<AID>#0.

  python check_ext_card_1.py ext-card-1.json

Exits 0 and prints "verified N cards" when every card verifies; 1 otherwise. Needs a2a-sdk with
the signing extra (pip install 'a2a-sdk[signing]').
"""

import hashlib
import json
import sys

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from a2a.client.card_resolver import parse_agent_card
from a2a.utils import signing

SEED = hashlib.sha256(b"anet-suite-identity-v1/cur").digest()


def main(path):
    with open(path, encoding="utf-8") as f:
        vec = json.load(f)
    pub = Ed25519PrivateKey.from_private_bytes(SEED).public_key()
    kid = vec["kid"]

    def key_for(got_kid, _jku):
        if got_kid != kid:
            raise signing.SignatureVerificationError(f"unexpected kid {got_kid}")
        return pub

    verify = signing.create_signature_verifier(key_for, ["EdDSA"])
    n, bad = 0, []
    for node in vec["nodes"]:
        for c in node["cards"]:
            name = f'{node["name"]}/{c["variant"]}'
            card = parse_agent_card(json.loads(c["card"]))
            payload = signing._canonicalize_agent_card(card)
            if payload != c["payload"]:
                bad.append(f"{name}: payload differs\n  sdk    {payload}\n  vector {c['payload']}")
                continue
            try:
                verify(card)
            except signing.SignatureVerificationError as e:
                bad.append(f"{name}: {e}")
                continue
            n += 1
            print(f"ok {name}")
    for b in bad:
        print("FAIL " + b)
    print(f"verified {n} cards")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
