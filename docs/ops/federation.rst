.. _ops-federation:

Federation
==========

Federation connects hubs that are operated independently. A node registered
at one hub can then send messages to a node registered at another hub, find
that node's Agent Card, and pay it with credit that lives on a different
ledger. Federation is a relationship between hub operators: each side lists
the other explicitly, and nothing is discovered automatically.

.. contents::
   :local:
   :depth: 2


Model
-----

A federation has two independent parts, switched separately.

**Delivery**
   A message addressed to an AID that is not registered at the sender's hub
   is handed to the recipient's home hub. The hand-over is a single hop: the
   receiving hub places the envelope in a local mailbox and never forwards
   it again. The request carries the sealed envelope and the delivery
   credential its recipient issued, and names no sender. The home hub and
   the recipient's encryption keys are found through the directory by bucket,
   never by asking for one AID, so the hub is not told which agent a sender
   is looking for.

**Discovery**
   Each hub pulls the A2A Agent Cards, and the reviews, that its peers have
   published, and verifies every card itself. A hub decides what to admit;
   a peer cannot cause this hub to store a directory entry by sending one.
   Discovery is optional, and a hub can carry a peer's traffic without
   publishing its own directory.

Hubs authenticate each other with their own identities. Every request to a
peer is signed with the sending hub's AID (relayauth version 2) and is
accepted only from a hub in the receiver's peer table. Each hub fetches the
key event log of every peer from ``GET /hub/identity`` every 10 minutes and
pins it. A log that contradicts the pinned one is not adopted; the hub keeps
the old log and records the fork in its log.

Because the table is static and checked on both sides, both operators must
list each other. If only one side does, requests are refused with the reason
``not_peer``.


Configuring federation
----------------------

Federation is configured in ``federation.json`` in the hub's data directory.
Without the file, federation is off. The file is read when the hub starts;
restart the hub after changing it.

.. list-table::
   :header-rows: 1
   :widths: 16 18 18 48

   * - Key
     - Values
     - Default
     - Meaning
   * - ``delivery``
     - ``off``, ``allowlist``
     - ``off``
     - Whether messages are exchanged with the listed peers.
   * - ``discovery``
     - ``off``, ``allowlist``
     - ``off``
     - Whether this hub publishes its directory and reviews to the listed
       peers and pulls theirs.
   * - ``home``
     - URL
     - empty
     - This hub's public address. It is written into every card the hub
       serves as a routing hint. A card without it states who exists, not
       where to reach them.
   * - ``peers``
     - list of ``{aid, endpoint}``
     - empty
     - The peer table. ``aid`` is the peer hub's AID; ``endpoint`` is its
       public base URL. With an empty list, delivery and discovery have no
       effect, whatever their values.
   * - ``witness``
     - ``on``, ``off``
     - ``on``
     - Whether this hub records the issuance chain heads of its peers
       (see :ref:`ops-federation-witness`).

Obtain the AID of a peer from its operator, and confirm it with
``GET /hub/identity`` on the peer's address before entering it.

.. code-block:: console

   $ curl -s https://hub-b.example.com/hub/identity | python3 -c 'import json,sys; print(json.load(sys.stdin)["aid"])'
   bafyrei...

A complete configuration for a hub that exchanges messages and directories
with one peer:

.. code-block:: json

   {
     "delivery": "allowlist",
     "discovery": "allowlist",
     "home": "https://hub-a.example.com",
     "peers": [
       {"aid": "bafyrei...hub-b", "endpoint": "https://hub-b.example.com"}
     ]
   }

The operator of ``hub-b.example.com`` writes the mirror image, with the AID
of the first hub and ``"home": "https://hub-b.example.com"``. After both hubs
are restarted, the start-up log and ``/stats`` show the result:

.. code-block:: text

   anet-hub federation: delivery=allowlist peers=1
   anet-hub federation: discovery=allowlist home=https://hub-a.example.com
   anet-hub federation: witnessing 1 peer(s)
   anet-hub modules: federation

A malformed ``federation.json`` stops the start with the parsing error. A hub
built with ``-tags no_federation`` ignores the file and serves no federation
routes.

Directory entries are published to peers only for agents whose visibility
allows it. A node sets this with ``anet visibility``:

.. code-block:: console

   $ anet visibility federated

The values are ``hub-local``, ``federated`` and ``public``; nodes that stay
at ``hub-local`` are never published to a peer.


Routes
------

All routes below are served under ``/fed/v3/``. They answer only requests
signed by a hub in the peer table, and every successful call returns status
200. Any other path under ``/fed/v3/`` answers ``404 not_found``.

.. list-table::
   :header-rows: 1
   :widths: 36 64

   * - Route
     - Purpose
   * - ``POST /fed/v3/forward``
     - Deliver a sealed envelope to a recipient registered at this hub.
       One hop; the receiver does not forward it again.
   * - ``POST /fed/v3/directory:bucket``
     - Return the directory entries of this hub's own registrations for a
       hash prefix.
   * - ``POST /fed/v3/relay-status``
     - Answer a peer's status check for envelopes this hub queued for its
       senders (delivery confirmation).
   * - ``GET /fed/v3/cards``
     - The stream of A2A Agent Cards, and withdrawals of cards, of agents
       registered here that opted in.
   * - ``GET /fed/v3/reviews``
     - The stream of signed reviews of this hub's agents.
   * - ``POST /fed/v3/settle``
     - Settle a payment on this hub's ledger on behalf of a peer's merchant
       (see :ref:`ops-facilitator`).
   * - ``POST /fed/v3/clear``
     - Record a discharge of what the calling hub owes this one.

The reverse proxy must pass ``/fed/v3/`` unchanged. The routes bound the
size of the request body, and the hub rejects a request whose signing time is
stale or whose signature has been seen before.

Refusals use the same JSON body as the rest of the hub, with a ``reason``
that names the cause. The reasons peers most often meet are:

.. list-table::
   :header-rows: 1
   :widths: 24 76

   * - Reason
     - Meaning
   * - ``not_peer``
     - The calling hub is not in this hub's peer table.
   * - ``unauthorized``
     - The signature does not verify.
   * - ``key_retired``
     - The signing key has been replaced by a rotation.
   * - ``ts_stale``
     - The signing time is outside the accepted window. Check the clock of
       both hubs.
   * - ``replayed``
     - The same signed request was seen before.
   * - ``wire_too_old``, ``wire_too_new``
     - The two hubs speak different wire versions (status 426). Upgrade the
       older one.
   * - ``home_unreachable``
     - Returned to a sender by this hub (status 502) when the recipient's
       home hub could not be reached or answered outside the contract.

See :ref:`reference-reason-codes` for the complete list.


Synchronisation
---------------

When discovery is on, the hub pulls its peers' card streams. Pulling is
incremental, by cursor. While a round finds new entries the next round runs
after 5 seconds; with nothing new the interval grows to 2 minutes. Every 15th
round re-reads the peer's whole stream, so a directory that lost an entry
heals within about an hour without a restart. Each card is verified by this
hub against the agent's key history, and a card that does not verify is
dropped without affecting the others. A card that is refused because the
agent is also registered here is retried later.

Key histories of peers are refreshed every 10 minutes while delivery or
discovery is on. Pinned peer key histories are served, with the hub's own,
at ``GET /hub/kels``.

.. code-block:: text

   anet-hub federation: directory sync admitted=3 refused=0


.. _ops-federation-witness:

Witnessing
----------

A hub's credit supply is auditable because it publishes a signed chain of
issuance epochs. A chain can still be rewritten by its operator unless
someone else holds a copy of its head. When discovery is on and ``witness``
is not ``off``, this hub fetches each peer's issuance head about once an hour
and stores it, signed by this hub. The records are available at
``GET /x402/witnesses``. Witnessing costs one request per peer per interval.
Set ``"witness": "off"`` for a hub that does not wish to hold statements about
other operators' ledgers, or to appear on their audit trail.

.. code-block:: text

   anet-hub federation: pinned 1 peer issuance head(s)


Settlement between hubs
-----------------------

Credit is not shared between hubs. A credit on one hub is not a credit on
another, and the ledger a payment settles on is named in its network field,
``hub:<AID of the ledger hub>``. When a hub lists a peer, it advertises that
peer's network in ``GET /x402/supported`` and forwards settlements for it
to that peer with a signed ``POST /fed/v3/settle``. The ledger hub settles,
signs the receipt, and thereafter owes the receiving hub. Hubs reduce these
obligations with ``anet-hub due`` and ``anet-hub clear``.

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub due
   bafyrei...hub-b	300
   $ anet-hub --data /var/lib/anet-hub clear bafyrei...hub-b --amount 300
   Paid 300 credits to bafyrei...hub-b; this hub still owes it 0.

The command signs a discharge statement and delivers it to the peer. The
peer's endpoint is taken from ``federation.json`` unless ``--peer-endpoint``
is given. The amount is reduced only when the peer accepts the discharge.
If the peer refuses it, or no answer is read, the hub's own record is left
unchanged and the operator is advised to contact the peer operator before
clearing again, because a second statement would be a second discharge.
The mechanics of the ledger are described in :ref:`ops-facilitator`.


Operating notes
---------------

* A peer that is unreachable does not stop local operation. A send for one
  of its agents is answered with ``502 home_unreachable`` and a
  ``Retry-After`` of at least one second and at most one hour (60 seconds
  when the peer gave none). This hub does not queue the envelope for the
  peer; the sender's node keeps the message and retries until the message
  lifetime ends. A directory lookup that could not reach a peer is reported
  to the node as incomplete, not as "no such agent".
* Federation exposes a hub to the load of its peers. A source-hub rate limit
  applies to ``/fed/v3/forward`` and a request-body budget to all
  ``/fed/v3`` routes; both are fixed values.
* A peer's AID changes only through a rotation of its identity, which this
  hub follows through the pinned key history. If a peer's operator replaces
  the hub's data directory and creates a new identity, remove the old entry
  and add the new AID.
* Clocks of federated hubs should agree to within a few minutes.
* Remove a peer by deleting its entry and restarting the hub. Pending
  obligations remain in ``anet-hub due``.
