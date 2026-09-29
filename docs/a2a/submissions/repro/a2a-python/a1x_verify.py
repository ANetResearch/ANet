"""A1 (cross-SDK part): verify the cards written by
../a2a-go/a1x-cross-sdk-vectors with a2a-python's own verifier.

Usage: python a1x_verify.py <dir written by the Go program>

Prints one line per card: VERIFIED or FAILED, and what A2A §8.4.1 rule 1
expects for a card signed over its bytes as given (a2a-go v2.6.0), then the
rows where this SDK differs from rule 1. Informational: exit 0 after
printing, 2 on a setup error.
"""

import json
import pathlib
import sys

from google.protobuf.json_format import Parse
from jwt.api_jwk import PyJWK

from a2a.types import AgentCard
from a2a.utils.signing import SignatureVerificationError, create_signature_verifier

# Under rule 1 a conformant verifier drops these defaults, so a signature made
# over the bytes as given cannot verify; `optional` fields keep an explicit
# false, REQUIRED fields keep their default and Struct contents are left alone,
# so those cards should verify.
EXPECTED = {
    'base': 'VERIFIED',
    'ext-required-false': 'FAILED',
    'ext-description-empty': 'FAILED',
    'skill-examples-empty': 'FAILED',
    'caps-streaming-false': 'VERIFIED',
    'caps-push-false': 'VERIFIED',
    'streaming-and-required-false': 'FAILED',
    'base-reserved-by-a2a-go': 'FAILED',
    'card-description-empty': 'VERIFIED',
    'ext-params-empty-string': 'VERIFIED',
}


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    d = pathlib.Path(sys.argv[1])
    keys = {k['kid']: PyJWK(k) for k in json.loads((d / 'jwks.json').read_text())['keys']}
    verify = create_signature_verifier(lambda kid, jku: keys[kid], ['EdDSA'])
    differs = []
    for entry in json.loads((d / 'index.json').read_text()):
        name = entry['file'].removesuffix('.json')
        card = Parse((d / entry['file']).read_text(), AgentCard())
        try:
            verify(card)
            got = 'VERIFIED'
        except SignatureVerificationError:
            got = 'FAILED'
        want = EXPECTED[name]
        if got != want:
            differs.append(name)
        print(f'{name:30s} a2a-python: {got:8s} rule 1 expects: {want:8s} ({entry["note"]})')
    print('a2a-python differs from rule 1 on: %s' % (', '.join(differs) or 'none'))
    return 0


if __name__ == '__main__':
    sys.exit(main())
