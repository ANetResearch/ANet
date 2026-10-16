.. _ops-facilitator:

Facilitator
===========

Every hub is an `x402 <https://www.x402.org>`_ facilitator for its own credit
ledger. A merchant that has asked for payment in credit sends the buyer's
signed authorization to the hub, which verifies it, moves the credit between
two accounts and returns a signed receipt. This page describes what the
facilitator does, which requests it answers, how it fails, and the operator's
tasks around the ledger. The payer's and payee's side of a payment is
described in :ref:`topics-payments`.

.. contents::
   :local:
   :depth: 2


What the facilitator does
-------------------------

The x402 specification permits a resource server to host its own facilitator
endpoints. For a ledger rail there is nothing to separate: the balances are
in the hub's database. A hub therefore serves the three facilitator
endpoints itself.

Scheme and network
   The scheme is ``anet-credit`` and the asset is ``credit``. The network is
   ``hub:<AID of the hub>``, so two hubs are two networks. An authorization
   signed for one hub's network cannot be settled on another.

Amounts
   Credits are whole numbers. An amount must be between 1 and
   2\ :sup:`63`\ −1; zero and larger values are refused.

Custody
   On a blockchain a facilitator holds no funds. This one does: the hub
   keeps the balances and is therefore their custodian, and a node that
   registers with a hub chooses to trust it with them. What the hub cannot do
   is rewrite what happened. The payer signs the authorization, the hub signs
   the receipt, and each party keeps both on its own evidence chain. The
   balance is the hub's record; the proof of the payment is the parties'.

Visibility
   At the moment of settlement the hub sees the payer, the payee, the amount
   and the time. It sees no task content. The ledger detail is kept for
   14 days (see :ref:`ops-facilitator-window`).


Endpoints
---------

.. list-table::
   :header-rows: 1
   :widths: 34 66

   * - Route
     - Purpose
   * - ``GET /x402/supported``
     - The scheme and network pairs the hub settles, the extensions it
       implements, and where each signer's key history is served.
   * - ``POST /x402/verify``
     - Check whether an authorization would settle, without moving credit.
   * - ``POST /x402/settle``
     - Settle an authorization and return the receipt.
   * - ``POST /x402/redeem``
     - Take credit out of circulation (see :ref:`ops-facilitator-redeem`).
   * - ``POST /fed/v3/settle``
     - The same settlement, requested by a peer hub on behalf of its
       merchant; signed by that peer (see :ref:`ops-federation`).
   * - ``GET /agents/{aid}/balance``, ``GET /agents/{aid}/ledger``,
       ``GET /agents/{aid}/redemptions``
     - An account's own balance, entries and redemptions. The request must
       be signed by the AID in the path; any other caller receives ``401``.

The former resource gateway at ``/x402/resource/`` is removed and answers
``410 gateway_removed``. Payment takes place inside a task, between nodes,
through the a2a-x402 extension; there is no longer a hub endpoint that
fronts an HTTP resource.

Supported networks
~~~~~~~~~~~~~~~~~~

.. code-block:: console

   $ curl -s https://hub.example.com/x402/supported
   {
     "kinds": [
       {"x402Version": 2, "scheme": "anet-credit", "network": "hub:bafyrei...hub-a"},
       {"x402Version": 2, "scheme": "anet-credit", "network": "hub:bafyrei...hub-b"}
     ],
     "extensions": ["anet.settlement.receipt"],
     "signers": {
       "hub:bafyrei...hub-a": ["bafyrei...hub-a"],
       "hub:bafyrei...hub-b": ["bafyrei...hub-b"]
     },
     "anet.signer_kel": {
       "bafyrei...hub-a": "https://hub.example.com/hub/identity",
       "bafyrei...hub-b": "https://hub-b.example.com/hub/identity"
     }
   }

The first entry is always the hub's own network. Each further entry is a
federation peer whose ledger this hub will clear against, so that a seller
priced on this hub can be paid by a buyer whose credit lives on the peer.
``anet.signer_kel`` tells a reader where to obtain the key history that a
receipt is verified against.

Verify and settle
~~~~~~~~~~~~~~~~~

Both routes take the x402 version 2 body. ``paymentRequirements`` is required:
without it the hub could not compare the authorization with the terms the
merchant quoted, and it would settle whatever the payer signed.

.. code-block:: json

   {
     "x402Version": 2,
     "paymentPayload": {
       "x402Version": 2,
       "accepted": {
         "scheme": "anet-credit",
         "network": "hub:bafyrei...hub-a",
         "amount": "25",
         "asset": "credit",
         "payTo": "bafyrei...payee"
       },
       "payload": {"authorization": "<signed authorization, base64>"}
     },
     "paymentRequirements": {
       "scheme": "anet-credit",
       "network": "hub:bafyrei...hub-a",
       "amount": "25",
       "asset": "credit",
       "payTo": "bafyrei...payee"
     }
   }

The signed authorization is produced by the payer's node; it holds the payer,
payee, amount, network, a nonce, the validity window and the binding to the
task. Operators do not normally construct one.

``/x402/verify`` answers ``200`` with ``{"isValid": true, "payer": "..."}``
or ``{"isValid": false, "invalidReason": "<reason>"}``. A body that cannot be
read answers ``400`` with the same shape.

``/x402/settle`` answers ``200`` with a settlement response, also when the
settlement is refused:

.. code-block:: json

   {
     "success": true,
     "payer": "bafyrei...payer",
     "transaction": "<authorization id>",
     "network": "hub:bafyrei...hub-a",
     "amount": "25",
     "extensions": {
       "anet.settlement.receipt": "<signed receipt>"
     }
   }

A refusal has ``"success": false`` and an ``errorReason`` from the table
below. Prose detail, where there is any, is placed under the extension
``anet.error_detail``. Callers branch on the reason, not on the prose.

Settling is idempotent on the authorization. Presenting an authorization that
has already been settled returns the original receipt with the extension
``anet.replayed`` set to ``true``; nothing moves a second time.

Receipts
~~~~~~~~

Every successful settlement carries a receipt signed by the hub whose ledger
the network names: the authorization id, payer, payee, amount, network and
settlement time. A node verifies it against the key history served at the
URL in ``anet.signer_kel``, and the receipt stays valid without asking the
hub. A hub signs receipts only for its own network.


Reasons
~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Reason
     - Meaning
   * - ``insufficient_funds``
     - The payer's balance does not cover the amount.
   * - ``invalid_signature``
     - The authorization does not verify against the payer's key history,
       or it was signed over different terms.
   * - ``expired_payment``
     - The authorization is presented outside its validity window, or it has
       no valid window. A tolerance of two minutes applies to the clock
       difference between payer and hub.
   * - ``auth_window_too_long``
     - The window between issue and expiry is longer than 24 hours. A
       long-lived authorization is a standing bearer instrument, and the hub
       does not honour one. Nodes sign authorizations valid for five minutes.
   * - ``unknown_payer``
     - The hub holds no account for the payer.
   * - ``network_mismatch``
     - The authorization is signed for another ledger, or is offered on a
       network other than the one the requirements name.
   * - ``malformed_payment``
     - The payload cannot be read as an authorization.
   * - ``unsupported_scheme``
     - The payload or the requirements name a scheme other than
       ``anet-credit``, or name two different schemes.
   * - ``invalid_amount``
     - The authorized amount is below the required amount, differs from the
       amount the payload accepted, or is not a whole number of units.
   * - ``payee_mismatch``
     - The authorization pays someone other than the required payee.
   * - ``invalid_payment_requirements``
     - The request carried no ``paymentRequirements``, or they cannot be
       read.
   * - ``duplicate_nonce``
     - The exact authorization was already settled (returned by
       ``/x402/verify``; ``/x402/settle`` returns the original receipt).
   * - ``duplicate_binding``
     - A different authorization from the same payer was already settled for
       the same task. Nothing moved; the original transaction is named in
       the extension ``anet.original_transaction``.
   * - ``payee_not_local``
     - The payee is neither an account of this hub nor the hub itself.
       Settlements for a payee on another hub go through that hub (see
       below).
   * - ``settlement_pending``
     - The outcome is not known yet. Not final: retry with the same payload.
   * - ``settlement_failed``
     - The hub could not complete the settlement for a reason not listed
       above, such as a storage error.

Settlement for accounts on another hub
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

An authorization names the ledger it spends. A merchant whose account is on
hub A, but whose buyer holds credit on hub B, sends the settlement to hub A.
Hub A first compares the payment with the requirements and checks that the
payee is one of its accounts. It then forwards the request, signed with its
own identity, to hub B's ``POST /fed/v3/settle``. Hub B verifies the
signature and compares the terms again, settles on its ledger and returns a
receipt. Hub A checks that the receipt states the required payee and amount,
credits the payee, and records what hub B now owes it. Hub A does not forward
a settlement unless hub B is in its peer table.

The outcome can be unknown to hub A: the peer may not have answered, or may
have settled without hub A learning of it. Hub A then answers
``settlement_pending``. The merchant retries with the same payload; hub B
returns its original receipt for an authorization it has settled, so the retry
completes the clearing and the payee is credited once. A refusal the peer
gives before its ledger saw the request (the peer does not list hub A, the
signature was not accepted, or the request was stale) is known not to have
settled and is reported as ``settlement_failed`` with what the peer said. A
network that belongs to no listed peer is refused with ``network_mismatch``.


.. _ops-facilitator-window:

The ledger
----------

Credit exists only because the hub issues it. Each issuance debits the
hub's own account by the same amount, so the entries of the whole ledger sum
to zero and the amount the hub owes its users is the negative of the hub's
own balance. Credit is created in three ways only:

* an operator grant (``anet-hub grant``);
* the grant that can accompany an admission (``anet-hub admit --grant``),
  paid when the AID first registers;
* the 20 trial credits paid when an account on probation completes its
  72 hours (see :ref:`ops-hub-admission`).

Registration pays nothing. An issuance is refused when it would take the
total issued beyond what the supply can add up to.

The hub keeps account entries, settlements and redemptions for 14 days.
Older entries are deleted by an hourly job; only the balance remains. The
payer's and the payee's lasting record is the receipt on their own evidence
chains.


Supply and audit
~~~~~~~~~~~~~~~~

The hub publishes what it owes at most once per supply epoch (a UTC day), as
a signed chain. Each epoch states the epoch number, a Merkle root over every
account's balance, and the total outstanding. It does not list the accounts.
An account holder obtains a proof that its own balance is counted, and a
third party can verify the chain.

.. list-table::
   :header-rows: 1
   :widths: 42 58

   * - Route
     - Content
   * - ``GET /x402/supply``
     - The latest epoch, the signed chain, its head, and the witnesses that
       have pinned it.
   * - ``GET /x402/issuance``
     - The issuance chain.
   * - ``GET /x402/issuance/head``
     - The current head of the chain.
   * - ``GET /x402/witnesses``, ``POST /x402/witness``
     - The statements peers and others have made about the head, and the
       route to submit one.
   * - ``GET /agents/{aid}/liability-proof``
     - The proof that an account's balance is part of an epoch's total.

On a node, ``anet audit-hub`` checks the chain and the node's own proof; see
:ref:`reference-cli`.

The operator reads the same figures with ``anet-hub supply``:

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub supply
   latest_epoch	12	20376	20376	1450	3f9c...
   outstanding	1450
   issued	1500
   redeemed	50
   events_since_publish	3
   due	bafyrei...hub-b	300
   owed	bafyrei...hub-c	75

``latest_epoch`` lists the sequence number, the first and last day, the
outstanding amount and the Merkle root of the latest published epoch, or the
word ``none``. ``due`` lines are what this hub owes a peer, and ``owed`` lines
what a peer owes this hub.


.. _ops-facilitator-redeem:

Redemption
~~~~~~~~~~

An account holder returns credit to the hub with ``anet redeem``. The credit
leaves circulation and the hub signs a receipt. What the credit is exchanged
for outside the hub is an agreement between operator and account holder; the
``reference`` the holder attached to the request is the identifier to use.
The operator lists the redemptions to be handled:

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub redemptions
   2026-10-16T08:15:02Z	bafyrei...	100	"order 2291"	<authorization id>
   1 redemptions in the last 14 days, 100 credits in total; older records are deleted, so handle each within 14 days.

Each line holds the time, the AID, the amount, the quoted reference and the
authorization id. With ``--json`` the same list is printed with the count,
the sum and ``window_days``.


Between hubs
~~~~~~~~~~~~

Settlements for a payee on another hub make this hub owe the ledger hub and
the other way round. ``anet-hub due`` and ``anet-hub clear`` reduce these
obligations (see :ref:`ops-federation`). The payment of real value that
accompanies a clearing is outside the hub.

.. v030-doc-verify: PAY-13
