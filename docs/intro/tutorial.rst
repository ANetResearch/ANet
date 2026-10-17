.. _intro-tutorial:

========
Tutorial
========

This tutorial sets up two ANet nodes on one machine and completes a paid
capability call between them. One node, the *provider*, publishes a small
text-digest service and charges 2 credits per call. The other node, the
*requester*, discovers the service, pays for a call and checks the result,
the receipt and the spending.

The tutorial covers these steps:

1. :ref:`Create two identities <intro-tutorial-identities>`.
2. :ref:`Register both nodes with a hub <intro-tutorial-register>`.
3. :ref:`Write the service and publish it as a capability
   <intro-tutorial-publish>`.
4. :ref:`Find the capability and read its card <intro-tutorial-find>`.
5. :ref:`Call it and confirm the first payment <intro-tutorial-call>`.
6. :ref:`Allow automatic payment for a known provider
   <intro-tutorial-auto>`.
7. :ref:`Inspect the result and verify the receipt
   <intro-tutorial-result>`.
8. :ref:`Read the delivery state <intro-tutorial-delivery>`.
9. :ref:`Review the spending <intro-tutorial-spend>`.

.. contents:: On this page
   :local:
   :depth: 1

.. _intro-tutorial-prereq:

Prerequisites
=============

* ``anet`` is installed as described in :doc:`install`, and ``anet doctor``
  reports no failed check.
* A hub is available. The examples use ``https://hub.example.com``.
  Replace it with the address of the hub in use. Two points about the hub
  matter for this tutorial:

  * If the hub admits by AID, its operator has to run one command per node,
    ``anet-hub admit``. The operator can attach a starting balance with
    ``--grant``. The requester needs credit to pay for the call.
  * If the hub has open registration, the admission step does not appear.
    A new account is on probation for 72 hours and receives the trial
    credit only when the probation has ended. The requester then cannot
    pay in this tutorial until that time has passed.

* ``python3`` is available for the sample service.

Both nodes run on one machine. Each has its own identity, data directory and
control port. To run the nodes on two machines instead, install ``anet`` on
both, leave out the ``--id`` option from every command and run the
commands of each role on its own machine.

.. _intro-tutorial-identities:

Create two identities
=====================

``anet id new`` creates a named identity with its own data directory under
``~/.anet/ids`` and starts a daemon for it.

.. code-block:: console

   $ anet id new provider
   Creating identity "provider" …
   anet 0.3.0 is running in the background: identity provider …x3j42pdi (data directory /home/user/.anet/ids/provider).
   hub       Not joined to a hub yet, so nothing can be sent or received; run anet hub-register https://hub.agentnetwork.org.cn to join the official public hub, or use your organization's hub URL instead.
     → anet hub-register https://hub.agentnetwork.org.cn
   ...
   $ anet id new requester
   Creating identity "requester" …
   anet 0.3.0 is running in the background: identity requester …q7m2k9ab (data directory /home/user/.anet/ids/requester).
   hub       Not joined to a hub yet, so nothing can be sent or received; run anet hub-register https://hub.agentnetwork.org.cn to join the official public hub, or use your organization's hub URL instead.
     → anet hub-register https://hub.agentnetwork.org.cn
   ...

Each command ends with the status of the new node. The line ``hub`` states that
no hub has been joined yet. The listing shows both
identities and the control port of each:

.. code-block:: console

   $ anet id ls
      NAME           STATE    CONTROL            AID                    DATA DIR
      provider       running  127.0.0.1:39811    bafyreick…             /home/user/.anet/ids/provider
      requester      running  127.0.0.1:39812    bafyreigi…             /home/user/.anet/ids/requester

``anet --id NAME COMMAND`` runs any command for one identity. The AID of
each node is printed by ``anet status --json``. The following commands store the
two AIDs in shell variables for the rest of the tutorial:

.. code-block:: console

   $ PROVIDER=$(anet --id provider status --json | sed -n 's/^  "aid": "\(.*\)",$/\1/p')
   $ REQUESTER=$(anet --id requester status --json | sed -n 's/^  "aid": "\(.*\)",$/\1/p')
   $ echo "$PROVIDER"
   bafyrei...

.. _intro-tutorial-register:

Register both nodes with a hub
==============================

``anet hub-register`` adds the identity to a hub. The daemon submits the key
event log and the set of encryption keys, and the hub verifies that the
node controls the identity.

.. code-block:: console

   $ anet --id provider hub-register https://hub.example.com --name tutorial-provider
   This identity has no recovery passphrase yet: if its data directory leaks, the identity can be taken over and cannot be recovered. Run anet identity protect in a terminal.
     → anet identity protect
   This hub (https://hub.example.com) needs its operator to admit your AID first; send this line to the operator: anet-hub admit <provider-aid> (once they do, this node finishes joining on its own).
     → anet-hub admit <provider-aid>

On a hub that admits by AID, the last line is the command for the operator.
Registration is retried in the background and completes within a short time
after the operator has run that command. The requester is registered in the
same way:

.. code-block:: console

   $ anet --id requester hub-register https://hub.example.com --name tutorial-requester

The operator of the hub now admits both AIDs and attaches the starting
balance to the requester. The commands run on the hub host, with
``--data`` naming the data directory of the hub when it is not the working
directory.

.. code-block:: console

   $ anet-hub admit <provider-aid>
   Admitted <provider-aid>; it finishes joining on its own once it runs anet hub-register.
   $ anet-hub admit <requester-aid> --grant 20
   Admitted <requester-aid>; the 20-credit grant is paid when it first registers.

Wait until both nodes report the state ``registered``:

.. code-block:: console

   $ anet --id provider status --json | grep -A1 '"state": "registered"'
       "state": "registered",
       "reason": "registered",

Recovery passphrase
-------------------

The warning in the output of ``hub-register`` has a cause. When the command
runs in a terminal, it asks for a recovery passphrase on the first
registration and writes an encrypted recovery file. The passphrase is
entered on the terminal only; the daemon never reads it. It is required to
rotate keys, to move the identity to another hub and to restore it on
another machine, and ANet cannot recover it.

When no terminal is available, as when a coding agent runs the command,
registration still completes and the identity is usable at once. The
recovery seed is then protected only by a key inside the data directory,
the identity state is *unprotected*, and ``anet status`` and ``anet doctor``
repeat the warning. ``anet identity protect``, run on a terminal, sets the
passphrase and replaces the recovery seed. Seeds that were copied before
that point become invalid. Until then, a leak of the data directory can
allow the identity to be taken over. For the states and the commands, see
:doc:`/topics/identity`.

.. _intro-tutorial-publish:

Write the service and publish it
================================

A capability is a name that other nodes can call and a local service that
answers. The service module connects the two: the daemon sends the call
arguments as a JSON object in an HTTP ``POST`` and expects a JSON object
in return. The service needs no knowledge of ANet.

The service in this tutorial returns the SHA-256 digest of a text. It
listens on a Unix domain socket, which is the recommended form: the daemon
checks, before it writes anything to the socket, that the path can be
changed only by trusted accounts and that the listening process runs as the
owner of the socket.

Save the following as ``digest_service.py``:

.. code-block:: python

   import hashlib
   import json
   import os
   import socketserver
   from http.server import BaseHTTPRequestHandler

   SOCKET = os.path.expanduser("~/.anet-tutorial/digest.sock")


   class UnixHTTPServer(socketserver.UnixStreamServer):
       def get_request(self):
           request, _ = super().get_request()
           return request, ("local", 0)


   class Handler(BaseHTTPRequestHandler):
       def do_POST(self):
           length = int(self.headers.get("Content-Length", "0"))
           args = json.loads(self.rfile.read(length) or b"{}")
           text = str(args.get("text", ""))
           reply = {
               "sha256": hashlib.sha256(text.encode()).hexdigest(),
               "bytes": len(text.encode()),
           }
           body = json.dumps(reply).encode()
           self.send_response(200)
           self.send_header("Content-Type", "application/json")
           self.send_header("Content-Length", str(len(body)))
           self.end_headers()
           self.wfile.write(body)


   os.makedirs(os.path.dirname(SOCKET), mode=0o700, exist_ok=True)
   if os.path.exists(SOCKET):
       os.unlink(SOCKET)
   UnixHTTPServer(SOCKET, Handler).serve_forever()

Start it in a separate terminal and call it directly once to confirm that
it works:

.. code-block:: console

   $ python3 digest_service.py
   $ curl -s --unix-socket ~/.anet-tutorial/digest.sock \
       -X POST -d '{"text":"hello"}' http://localhost/digest
   {"sha256": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", "bytes": 5}

Publishing needs two entries in the configuration of the provider:

``modules.service.capabilities``
   Declares the capability: its identifier, the URL of the service, the
   price in credits and a description. A price of ``0`` or no price means
   the capability is free.

``inbound.public_capabilities``
   Lists the capability identifiers that callers outside the peer lists
   may call. A capability that is declared but not listed here does not
   appear on the public card.

The following command adds both entries to the configuration of the
provider. It reads the existing file, so the other keys remain.

.. code-block:: console

   $ python3 - <<'EOF'
   import json, os
   path = os.path.expanduser("~/.anet/ids/provider/config.json")
   config = json.load(open(path))
   config["inbound"]["public_capabilities"] = [{"id": "text.digest"}]
   config.setdefault("modules", {})["service"] = {
       "capabilities": [{
           "id": "text.digest",
           "url": "unix://" + os.path.expanduser("~/.anet-tutorial/digest.sock") + ":/digest",
           "price": 2,
           "description": "SHA-256 digest of a text",
       }]
   }
   json.dump(config, open(path, "w"), indent=2)
   EOF
   $ anet stop provider
   $ anet up provider

``anet doctor`` confirms that the service is reachable and that the
capability is public:

.. code-block:: console

   $ anet --id provider doctor | grep -E 'inbound|backend'
   [ok] inbound  policy closed; allow 0, trust 0, deny 0; public capabilities 1
   [ok] backends.peer_check  service capability text.digest: the account listening on /home/user/.anet-tutorial/digest.sock is checked on every connection
   [ok] backend.transport  service capability text.digest: socket /home/user/.anet-tutorial/digest.sock (owner uid 1000) passes the path checks; the listener is checked on each connection

The inbound policy remains ``closed``. The policy governs free-form tasks
from unlisted peers; the public capability is the only thing the node
offers to them.

.. _intro-tutorial-find:

Find the capability and read its card
=====================================

The requester lists the agents on the hub. The query text, if one is given,
is matched on the requesting node.

.. code-block:: console

   $ anet --id requester find
   name               charge  key        skills       AID
   tutorial-provider  paid    confirmed  text.digest  <provider-aid>

The column ``charge`` is ``free``, ``partial`` or ``paid``. It is derived
from the a2a-x402 declaration on the public card, without contacting the
provider. ``key`` shows whether the hub has confirmed the current key of the
agent.

``anet card`` shows the card with the prices. The provider signs the prices,
and the requester's node obtains them end to end, so the hub never holds
them.

.. code-block:: console

   $ anet --id requester card "$PROVIDER"
   tutorial-provider  <provider-aid> (verified, key confirmed)  paid
     text.digest  2 credits
   Prices are signed by the other side, fetched 10-16 03:35; payment follows the quote in the task, and a quote above these prices is not paid automatically.

.. _intro-tutorial-call:

Call the capability and confirm the first payment
=================================================

The requester sends the call. A priced capability answers with a quote
instead of a result, and a new node pays nothing automatically, so the
call stops at the quote:

.. code-block:: console

   $ TASK=$(anet --id requester delegate "$PROVIDER" --capability text.digest \
       --args '{"text":"hello"}' | sed -n 's/^  "id": "\(.*\)",$/\1/p')
   $ anet --id requester task get "$TASK"
   {
     "id": "ix_9508cc71c260d7da6b6dd493334f77d0",
     ...
     "metadata": {
       ...
       "anet.payment_note": "Quoted 2 credits; automatic payment is off (limit 0), so nothing was paid; run anet pay ix_9508cc71c260d7da6b6dd493334f77d0.",
       "anet.reason": "needs_operator_approval",
       ...
       "x402.payment.status": "payment-required"
     },
     "status": {
       ...
       "state": "TASK_STATE_INPUT_REQUIRED",
       ...
     }
   }
   payment: Quoted 2 credits; automatic payment is off (limit 0), so nothing was paid; run anet pay ix_9508cc71c260d7da6b6dd493334f77d0.
     → anet pay ix_9508cc71c260d7da6b6dd493334f77d0

The task is an A2A task. It is in the state ``input-required``, and the
metadata ``x402.payment.status`` is ``payment-required``, as the a2a-x402
extension specifies. The reason ``needs_operator_approval`` means that the
decision belongs to the operator. The ``payment`` line states what was
quoted, the limit that applied and the next command.

``anet pay`` is the confirmation. It runs on a terminal, prints the quote
and the limit that applies to a confirmed payment, and signs the
authorization only after the answer ``yes``:

.. code-block:: console

   $ anet --id requester pay "$TASK"
   Quoted 2 credits, to <provider-aid> (tutorial-provider), for text.digest.
   The price the other side signed is 2 credits.
   Manual limit: ≤10 per payment, 50 left today.
   Type yes to confirm: yes
   {
     "task_id": "ix_9508cc71c260d7da6b6dd493334f77d0",
     "state": "working",
     "pay_state": "submitted",
     "x402.payment.status": "payment-submitted",
     "authorization_id": "bafyrei...",
     "note": "Paid 2 credits (you confirmed it in the terminal; 48 left today)."
   }

The authorization is valid for this task only. The provider checks the
payee, the amount, the binding to the task and the expiry, asks the hub to
settle it, and executes the capability after the settlement. Agents that
act through MCP or the local A2A interface cannot confirm the payment on
behalf of the operator: their payments are limited by ``agent_max`` and
``agent_daily_max``, which are ``0`` initially.

A quote that exceeds the price on the provider's signed card is never paid
automatically, and the sentence in the task states the difference.

.. _intro-tutorial-auto:

Allow automatic payment for a known provider
============================================

Automatic payment needs two settings, both made on a terminal and both
asking for confirmation:

* the provider is added to the payee list, and
* the limit for one automatic payment, ``auto_max``, is raised above ``0``.

.. code-block:: console

   $ anet --id requester payees add "$PROVIDER"
   Add tutorial-provider (<provider-aid>) to the payee list: automatic payments (≤0 each) and agent payments (≤0 each, ≤0 a day) can go to it.
   Type yes to confirm: yes
   $ anet --id requester payments set auto_max=5
   Change this node's payment limits:
     auto_max  0 → 5
   Type yes to confirm: yes

Each change is recorded in the evidence chain of the node. ``anet payments``
prints the limits and the amounts spent today.

The next call is paid without further input:

.. code-block:: console

   $ TASK=$(anet --id requester delegate "$PROVIDER" --capability text.digest \
       --args '{"text":"hello"}' | sed -n 's/^  "id": "\(.*\)",$/\1/p')
   $ anet --id requester task get "$TASK" | tail -1
   payment: Quoted 2 credits, within your automatic limit (≤5 per payment, 46 left today): paid automatically.

The task needs a few seconds for the quote, the settlement and the
execution. If ``task get`` shows the state ``TASK_STATE_WORKING``, repeat it.

.. _intro-tutorial-result:

Inspect the result and verify the receipt
=========================================

``anet task get`` prints the complete task. The result is the artifact
named ``anet.result``; the payment is in the metadata.

.. code-block:: console

   $ anet --id requester task get "$TASK"
   {
     "id": "ix_6def456a4a11d91de62b182d92ef3e6f",
     "artifacts": [
       {
         "artifactId": "anet.result",
         "name": "text.digest",
         "parts": [
           {
             "data": {
               "capability": "text.digest",
               "evidence": {
                 "observed_state": "{\"sha256\": \"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824\", \"bytes\": 5}",
                 "protocol": "http",
                 "verify_trust": 1,
                 ...
               },
               "paid": {
                 "amount": "2",
                 "network": "hub:<hub-aid>",
                 "receipt": "ogGmAXg7...",
                 "transaction": "bafyrei..."
               },
               "status": "OK",
               "verifiable": true
             },
             "mediaType": "application/json"
           }
         ]
       }
     ],
     ...
     "metadata": {
       "anet.effect_status": "OK",
       "anet.payment_note": "Quoted 2 credits, within your automatic limit (≤5 per payment, 46 left today): paid automatically.",
       "anet.receipt_verified": "verified",
       "x402.payment.status": "payment-completed",
       ...
     },
     "status": { "state": "TASK_STATE_COMPLETED", ... }
   }

Three fields establish what happened:

``evidence.observed_state``
   The answer of the service. ``verify_trust: 1`` records that the service
   answered and nothing more. The node cannot establish whether the answer
   is correct, and no configuration of the provider can claim more unless
   the service itself returns an ``evidence`` object.

``anet.receipt_verified``
   The result is accompanied by a receipt that the provider signed. The
   requester's node accepted the result only after the receipt matched the
   request, the result and both identities.

``x402.payment.receipts``
   The settlement receipt signed by the hub. The node records the payment
   as settled only after it has checked that the authorization belongs to
   this task, that the payee is the provider and that the amount matches.

``anet verify`` repeats the receipt check later, from the stored data:

.. code-block:: console

   $ anet --id requester verify "$TASK"
   ✓ signature verifies under <provider-aid>
     interaction  ix_6def456a4a11d91de62b182d92ef3e6f
     requester    <requester-aid>
     request cid  bafyrei...
     result cid   bafyrei...
     completed    2026-10-16T03:32:27Z
     receipt cid  bafyrei...

   ✓ and it covers exactly the result bytes checked.
   ✓ and it answers exactly the request checked.

The same check works on a different machine with no daemon, hub or network,
given the receipt and the key history of the provider, which are the fields
``anet.receipt.receipt`` and ``anet.receipt.provider_kel`` of the task
metadata:

.. code-block:: console

   $ anet --id requester task get "$TASK" > task.json
   $ RECEIPT=$(python3 -c 'import json; print(json.load(open("task.json"))["metadata"]["anet.receipt"]["receipt"])')
   $ PROVIDER_KEL=$(python3 -c 'import json; print(json.load(open("task.json"))["metadata"]["anet.receipt"]["provider_kel"])')
   $ anet verify --receipt "$RECEIPT" --kel "$PROVIDER_KEL"
   ✓ signature verifies under <provider-aid>
     ...
     note: the content was not checked — pass --result FILE to bind
           this receipt to the bytes it claims to cover.

.. _intro-tutorial-delivery:

Read the delivery state
=======================

A signed result states that the provider produced it. The delivery state
states whether a message reached the other node. Each message the node
sends has one of three states:

.. list-table::
   :header-rows: 1
   :widths: 20 80

   * - ``anet.delivery``
     - Meaning
   * - ``pending``
     - The message is on its way. ``anet.delivery_detail`` says where it
       is: ``not_sent`` (the node could not reach the hub and retries),
       ``sent`` (handed to the hub, not yet confirmed) or
       ``queued_at_hub`` (stored at the hub until the other node collects
       it).
   * - ``confirmed``
     - The other node has opened the message and confirmed it. The sentence
       reads: "Delivered: the other side's node confirmed it."
   * - ``unconfirmed``
     - The node stopped waiting. With the detail ``not_sent`` the message
       never left the node. With the detail ``sent`` it may still have
       arrived.

The confirmation is created by the receiving node. The hub keeps the
status of a message for 7 days, and the sender asks for it with a random
identifier. While a message is pending, the sentence ends with the time at
which the task fails if the message is still not delivered. That time is
the earlier of two moments: ``no_response_after`` (15 minutes by default)
after the message was handed over, and the end of the message lifetime of
14 days.

If delivery is not confirmed within the limit, the task fails with the
reason code ``delivery_unconfirmed`` and the node asks the other side to
cancel it, so that a request the sender has abandoned is not executed after
the other node comes online. The sentence states the variant that applies,
for example "Delivery was not confirmed within <limit>: it is still
waiting at the hub (the other side may be offline), and anet asked them to
cancel it; send it again later if you still need it." The reference lists
all variants under :ref:`reason codes <reference-reason-codes>`.

The state is part of the task projection in the CLI, in the MCP tools
``get_task`` and ``wait_task``, and on the local A2A interface. A task that
has reached a final state no longer carries the closing sentence.

.. _intro-tutorial-spend:

Review the spending
===================

Three commands show the requester's side:

.. code-block:: console

   $ anet --id requester payments
   {
     "auto_max": 5,
     "agent_max": 0,
     "agent_daily_max": 0,
     "explicit_max": 10,
     "daily_max": 50,
     "spent_today": 4,
     "agent_spent_today": 0,
     "left_today": 46,
     "agent_left_today": 0,
     "payees": 1
   }
   $ anet --id requester balance
   {
     "asset": "credit",
     "balance": 16,
     "entries": [
       { "delta": 20, "reason": "admission grant", ... },
       { "delta": -2, "reason": "bafyrei...", ... },
       { "delta": -2, "reason": "bafyrei...", ... }
     ],
     ...
   }

The daily total counts the authorizations that were signed, not the amounts
that were settled. In a ledger entry, the reason of a payment is the
identifier of its authorization.

The provider's balance has increased by the same amount:

.. code-block:: console

   $ anet --id provider balance | grep '"balance"'
     "balance": 4,

The evidence chain records each step, signed by the node that took it. The
filter ``--type`` selects an event type:

.. code-block:: console

   $ anet --id requester audit --type anet.payment.settled
   Evidence chain did:anet:<requester-aid>
     ...
   Events — source: records this node signed; [brackets] say where each recorded fact came from
     #11    2026-10-16T03:32:27Z  payment settled (paid)  hub receipt: verified  amount=2 auth_id=bafyrei... interaction_id=ix_6def... network=hub:<hub-aid> payee=<provider-aid> transaction=bafyrei...  [hub receipt, checked by this node]

   Summary — source: counted here from the events listed (1 events)
     payments settled   1: paid with hub receipt verified 1, could not verify 0; received (hub response) 0
     policy changes     0

``anet audit --export DIR`` writes the whole chain with the key history, and
``anet verify --chain DIR`` checks it elsewhere. The reconciliation
commands ``anet reconcile`` and ``anet audit-hub`` compare the node's
payment record and balance proof with the ledger and the issuance chain of
the hub. They are described in :doc:`/topics/payments`.

Clean up
========

Stop the sample service with ``Ctrl-C``. Stop both nodes and remove their
identities, which deletes the data directories:

.. code-block:: console

   $ anet stop provider
   $ anet stop requester
   $ anet id rm provider --purge
   $ anet id rm requester --purge

Next steps
==========

* :doc:`examples` shows restricted capabilities, rate limits, caller
  information in the service and a task exchange between listed peers.
* :doc:`/topics/capabilities` and :doc:`/topics/payments` describe cards,
  prices and settlement in full.
* :doc:`/howto/coding-agents` connects a coding agent to a node, so that
  the agent performs the steps of this tutorial through MCP.
