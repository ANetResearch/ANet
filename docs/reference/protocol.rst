.. _reference-protocol:

Wire protocol
=============

ANetCore is the Go module that holds the parts of ANet which must be identical in
every program: how objects are encoded, hashed, signed and sealed, and how an
identity is derived and verified. The ``anet`` daemon, the hub and the ANetLink
bridge import it and never each other, so a wire type is defined once. This page
describes the protocol of ANetCore v0.16 (ANet 0.3). The complete register of key
numbers, labels, routes and codes is the file ``docs/wire-keys.md`` of the ANetCore
repository, generated from ``wire/registry.json``; the tables on this page are
generated from the same file.

.. contents::
   :local:
   :depth: 2


Layers
------

.. list-table::
   :header-rows: 1
   :widths: 20 22 58

   * - Layer
     - Package
     - Content
   * - Encoding
     - ``coredet``, ``anetcid``
     - Deterministic CBOR and content identifiers.
   * - Identity
     - ``identity``
     - Self-certifying identifiers (AID) and the key event log (KEL).
   * - Sealing
     - ``seal``
     - Signed encryption key sets, the two-layer sealed envelope, delivery
       credentials and delivery confirmations.
   * - Hub access
     - ``relayauth``, ``regpow``, ``dirbox``
     - Signed requests and first-contact sessions, proof of work for
       registration, and the sealed directory.
   * - Cards
     - ``a2acard``
     - Signing and verification of A2A agent cards.
   * - Objects
     - ``delegation``, ``evidence``, ``ael``, ``tsir``, ``effect``, ``payment``
     - Task messages, receipts and reviews, the event ledger, task documents and
       effects, and payment objects.

The versions that 0.3 introduced are listed at the end of this page.


Encoding: CoreDet-CBOR
----------------------

Every object that is hashed or signed is encoded as CoreDet-CBOR, the profile of
RFC 8949 section 4.2 (Core Deterministic Encoding) with two additional rules.

* Integers and lengths have the shortest form, items have a definite length and map
  keys are sorted by the bytewise order of their encodings. The length-first sort
  of the older RFC 7049 canonical form is not used.
* A value must not be NaN or infinite, and no CBOR tag may occur.

A decoder rejects duplicate map keys. The strict decoder, used for every object of
the protocol, also rejects a map key that the destination type does not name.
Objects are maps with integer keys. Key numbers, labels and codes are never reused:
a removed key is *retired* and refused when present, and a key that was never
assigned is *reserved* and also refused. Objects that carry the note *keys 0 to 63
must be understood* ignore unknown keys from 64 upward but still cover them in the
digest. Times are Unix milliseconds. A signed body travels as a byte string that
holds its CoreDet encoding, so that the signature covers the exact bytes.

A content identifier (CID) is CIDv1 with the codec dag-cbor and the hash
sha2-256. The binary prefix is ``0x01 0x71 0x12 0x20`` and the text form uses the
multibase prefix ``b`` (base32, lower case), which gives identifiers that begin
with ``bafyrei``.

Signed objects use a detached Ed25519 signature (COSE algorithm -8) over a
preimage that starts with a domain-separation label, so that a signature made for
one purpose cannot be accepted for another. The labels are listed in
:ref:`protocol-labels`.


Identity: AID and key event log
-------------------------------

An AID is the CID of the preimage of the inception event of a key event log. The
log is signed and append-only and has three event types: ``icp`` (inception),
``rot`` (rotation) and ``dip`` (deactivation). The AID stays the same across
rotations.

Pre-rotation. Every inception and rotation commits to the SHA-256 of the next
public key. The following ``rot`` or ``dip`` must reveal exactly that key and is
signed by it. The key that is replaced takes no part, so a party that stole it can
neither rotate nor deactivate the identity. The next key is derived from a recovery
seed (HKDF-SHA256), which is not kept in the data directory in normal operation; it is
stored in a recovery file that is encrypted with a passphrase (Argon2id with
``t = 3``, ``m = 64 MiB``, ``p = 4``), or, for an identity that was set up without a
terminal, with a key held in the data directory.

Every event carries the type tag ``anet.kel/2`` in key 0 and a timestamp that
strictly increases along the log and lies within 5 minutes of the clock of the
verifier. A log without the tag does not replay, and there is no decoding path for
the 0.2 format.

Witness. The inception fixes the witness mode: a hub identity has no witness and an
agent identity has exactly one, the hub with which it registered, named by its first
rotation. The witness signs *key state notices* (``anet.ksn/1``): a short statement
that the current key of an AID is the key at a given sequence number and head. A
message carries one notice with its key history. The receiving side requires the
notice to be at most 6 hours old, which bounds how long a party that holds a
rotated-out key can impersonate the identity toward a node that has not heard of the
rotation.

Rotation. After a scheduled rotation, messages signed with the old key are accepted
for 1 hour, counted from the time at which the hub of the recipient accepted them.
An emergency rotation (``emergency`` set in the ``rot`` event) and a deactivation
leave no grace: a recipient that knows of the event rejects the old key at once.

.. v030-doc-verify: KEL-3, KEL-4 (notice age and grace rules), KEL-10 (CLI)


Encryption keys
---------------

An identity publishes a signed *encryption key set* (``anet.enckeys/2``) that holds
at most 32 keys and a *mailbox signing key* (``mbox``). Every encryption key is an
*epoch key* that is valid for one UTC day. A node publishes its keys 14 days ahead,
so a sender with a cached key set can seal for that period without asking the hub.
The key set is signed with the current identity key.

An epoch key is deleted by its holder after its day has passed, once the mailbox has
been read in full and the clocks agree; the delivery deadline is one hour after the
end of the day, and the key is deleted at the latest 15 days after the end of the
day. After the deletion, an envelope sealed to that key can no longer be opened. This
is the forward secrecy of ANet (see :doc:`security`). A message in an interaction
also has a second layer under an *interaction key*, which is deleted when the
interaction ends on the local side.


Sealed envelopes
----------------

Every message between nodes is a sealed envelope. The outer layer is the only part
that a hub sees.

.. list-table::
   :header-rows: 1
   :widths: 20 80

   * - Part
     - Content
   * - Outer envelope
     - Version ``v = 2``, recipient AID ``to``, the suite, the key identifier ``kid``
       of an epoch key of the recipient, the HPKE encapsulation ``enc``, the
       ciphertext ``ct``, the delivery credential ``access`` and, for a one-shot
       envelope, the expiry ``exp``.
   * - Layer-1 plaintext (``Wrap``)
     - Either the signed inner message, or, when the interaction has a second layer,
       the interaction key identifier, the second encapsulation and the second
       ciphertext, followed by zero padding to a Padmé length.
   * - Inner message
     - Sender, key state, recipient, type, interaction, message identifier, time,
       expiry, body, the sender's key history and witness notice, its key set, an
       optional grant, its interaction key and the signature.

The cryptography of suite 1 is HPKE (RFC 9180) in base mode with DHKEM(X25519,
HKDF-SHA256), HKDF-SHA256 and ChaCha20-Poly1305. The HPKE info binds the recipient
AID, the suite and the key identifier under the label ``anet-seal/v2`` (layer 1) or
``anet-ix/v2`` (layer 2); the associated data is empty.

*Sign, then encrypt.* The inner message is signed with the identity key of the
sender before it is encrypted. The signature covers the inner fields as sent, the
body replaced by its SHA-256, and the outer encapsulation, key identifier and suite
together with the second layer's identifier and encapsulation. A recipient therefore
cannot re-encrypt a message and present it to a third party as addressed to that
party, and nobody can remove, add or exchange a layer without invalidating the
signature.

Only the layer-1 plaintext is padded, so the size of the ciphertext discloses a size
class and not the exact length.

A receiver checks, among other things: the outer version, the key identifier, the
decryption of layer 1, the structure of the wrap, the validity of the delivery
credential, the time (not later than ``exp``; ``ts`` at most 5 minutes in the
future; ``exp - ts`` at most 15 days), the witness notice against the key history,
the inner signature under the key state that was current at the arrival time, and
the attached key set. Every failure carries a stable reason string; the reasons
describe the input and are permanent: the same bytes fail the same way on every
retry. A message is accepted at most once for each pair of sender and message
identifier.

.. _protocol-credentials:

Delivery credentials
~~~~~~~~~~~~~~~~~~~~

The envelope carries a credential in the outer key ``access``, outside the HPKE info
and the inner signature. Nothing in it names the sender.

* ``kind = 0`` is a *first contact*. The sender opened a session at the hub with one
  signed request (``POST /relay/session``). The hub returned a session key, sealed
  to a one-time key of the sender. Each send carries a MAC over the request under
  that key (label ``anet-sess-mac/v1``). The hub knows the sender for this session
  and does not keep it, and holds no signed evidence of it.
* ``kind = 1`` is a *capability* that the recipient issued and signed with its
  mailbox key (``anet.cap/1``). It states the owner, the mailbox epoch, a random
  identifier, a class (``contact`` or ``stranger``), an expiry and the public key of
  a holder. The sender hands the holder's private seed over with the capability and
  proves possession by a signature over the digest of this envelope. A capability
  is valid for at most 31 days. A new mailbox epoch voids every capability signed
  under the previous one, and the recipient can revoke single capabilities at the
  hub.

A recipient hands a capability to a peer inside a sealed message (the *grant*). A
class ``contact`` capability may carry the direct address of the recipient. A
one-shot capability is bound to one interaction.

Delivery confirmation
~~~~~~~~~~~~~~~~~~~~~

When a recipient has made a final decision about an envelope, it answers with a
*delivery confirmation* (``dack``) of 80 bytes: a 16-byte token that states whether
the message was processed or refused, and a 64-byte encrypted note with an optional
reason code and wait time. Both are derived from a key that only the two ends can
compute (an HPKE export of the layer-1 context), so nothing in between can create or
change one. The confirmation is deniable: the sender can compute it too, so it proves
nothing to a third party. A sender asks the hub about a queued envelope by a *handle*
derived from the same key and by the random ``rid``. The confirmation proves that the
node of the recipient decided; it does not prove that an agent or person read the
message.

Direct connections
~~~~~~~~~~~~~~~~~~

Nodes can also exchange envelopes directly, over lines of JSON on a TCP connection
between two ``anetpeer`` processes. Each frame carries ``v: 3``. The operations are
``hello``, ``send`` (with the answer), ``recv``, ``ack``, ``nack`` and ``error``.
The envelope inside is the same sealed envelope as on the hub path. The connection is
plain TCP; the content is encrypted, but an observer sees the size of the envelope, its
recipient and the credential identifier. A direct address is given only to contacts.


Signed requests to a hub
------------------------

A hub request for a registered agent is signed with the label ``anet-relay/v2/``. The
text is described in :doc:`hub-api`. The action in the text is one of the actions
listed there, the hub AID is part of it, and the time is checked against the clock of
the hub within a skew limit. The hub keeps, for each signer, the latest accepted time
and a window of recent signatures to refuse replays; the size of the window depends on
the class of the signer (small for accounts in probation, larger for agents, the
largest for peer hubs).

A registration is protected by a proof of work. The hub gives a seed derived from a
secret and the hour; the client finds a nonce of at most 32 bytes such that the
SHA-256 of ``anet-regpow/v1``, a zero byte, the hub AID, a zero byte, its AID, a zero
byte, the seed and the nonce has at least 22 leading zero bits. The difficulty is the
same on every hub.


Directory entries
-----------------

An entry in the directory is sealed so that only a party that already knows an AID can
find and open it. The key of the lookup is the hash ``SHA-256("anet-dir-hash/v1" || 0x00
|| aid)``. The entry is encrypted with ChaCha20-Poly1305 under a key that HKDF-SHA256
derives from the AID, with the hash as associated data. A client asks for a *bucket*,
the set of entries whose hash begins with a prefix of at most 10 bits, so the hub learns
that one of the entries of a bucket is wanted and not which one.


Agent cards
-----------

An agent card is an A2A ``AgentCard`` signed with the Ed25519 key that the key event log
of the agent designates as current. The signature follows A2A section 8.4: the payload is
the card in canonical form (RFC 8785, JSON Canonicalization Scheme) and the signature is
a detached JWS (EdDSA) whose protected header holds ``alg``, ``jku``, ``kid`` and ``typ``.
The ``jku`` names the key set that the hub serves at ``/agents/{aid}/jwks.json``. Verifying
a card checks the key state, the ``anet-card`` extension bound to the AID, the relay
interfaces addressed to that AID, the validity period, the required members and the size
limits.

The extensions and bindings that ANet registers in A2A cards are:

.. list-table::
   :header-rows: 1
   :widths: 46 54

   * - URI
     - Use
   * - ``https://agentnetwork.org.cn/a2a/bindings/anet-relay/v2``
     - Protocol binding of the public card: messages are relayed to the AID.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-card/v1``
     - Binds the card to the AID, with sequence number and validity. Public and
       extended card.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-evidence/v1``
     - Completed tasks carry a receipt signed by the provider. Public card.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-price/v1``
     - Prices per skill. Only in the extended card that the provider signs for a
       caller; a hub refuses a public card that carries it.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-origin/v1``
     - Marks the card that the local A2A interface serves for a remote agent, with
       ``originExtendedCard`` and ``keyState``.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-preview/v1``
     - The provider sends previews. Declared when ``stream_preview`` is not ``off``;
       never required, without parameters.
   * - ``https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2``
     - Payment with a2a-x402.

The relay binding of 0.2 (``anet-relay/v1``), the direct binding (``anet-p2p/v1``) and the
public price extension (``anet-pricing/v1``) are removed.

.. v030-doc-verify: PAY-10 (anet-price/v1 extended card), UX-10 (anet-preview/v1)


Task messages and metadata
--------------------------

The body of an inner message is one of the task messages of the ``delegation`` package: a
signed request (``DelegateReq``), a chat message, a result, a reply, a status message and a
card request and response. The request carries the task document (TaskDoc) whose CID the
receipt binds. Metadata keys with the prefix ``anet.`` carry the information that
travels end to end (for example ``anet.reason`` and ``anet.retry_after_ms``) or that the
local interfaces add (for example ``anet.delivery``, ``anet.payment_note`` and
``anet.receipt_verified``). The register lists every key with its scope.


.. _protocol-labels:

Labels and type tags
--------------------

Class A is a typed CoreDet map whose key 0 is the tag, class B is a prefix string of the
form ``label || 0x00 || fields``, class C is a KDF, HPKE or MAC context, and *text* is a
relayauth text preimage. Labels are ASCII bytes without length prefix and without trailing
zero byte.

.. include:: _generated/protocol-labels.inc


Objects
-------

Key tables of the main objects. *Presence* is *required*, *optional* or *conditional*; the
note of a conditional member states the condition. Rows marked *reserved* have never been
assigned; *retired* rows were used by an earlier release and are refused.

.. include:: _generated/protocol-objects.inc


Versions
--------

.. list-table::
   :header-rows: 1
   :widths: 22 30 48

   * - Layer
     - 0.2
     - 0.3
   * - Release
     - ANetCore 0.15, anet 0.2.1
     - ANetCore 0.16, anet 0.3.0, ANetHub 0.3.0, ANetLink 0.2.0
   * - Hub wire
     - ``X-ANet-Wire: 2``
     - ``X-ANet-Wire: 3``
   * - Outer envelope
     - ``v = 1``
     - ``v = 2``; the credential ``access`` is required and ``exp`` optional
   * - HPKE label
     - ``anet-relay/v1``
     - ``anet-seal/v2``, with ``anet-ix/v2`` for the second layer
   * - Inner signature
     - ``anet-relay-sig/v1``
     - ``anet-relay-sig/v2``
   * - Encryption key set
     - ``anet.enckeys/1``, at most 4 keys of 30 days
     - ``anet.enckeys/2``, at most 32 keys of 2 days, with the mailbox key
   * - Key event log
     - icp, rot, dip, ixn, drt
     - ``anet.kel/2``; icp, rot, dip only
   * - Identity file
     - ``identity.kel``
     - ``anet.identity/2``
   * - Direct frames
     - ``v: 2``
     - ``v: 3``; adds ``dack`` and ``addr``
   * - C1 (ANet to ANetLink)
     - JSON, ``/v0``
     - CBOR only, ``/v1``
   * - Evidence and payment
     - Receipt and review v1, vouchers
     - ``anet.receipt/2``, ``anet.review/2``, supply epochs; vouchers removed
   * - Local data
     - no schema version
     - ``user_version = 3``; ``enc_keys.cbor`` ``v = 2``

A 0.3 program refuses data and messages of the earlier versions with a stable reason; there
is no migration path (see :doc:`../upgrading`).
