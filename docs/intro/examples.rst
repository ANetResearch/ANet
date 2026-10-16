.. _intro-examples:

========
Examples
========

The examples on this page are self-contained. Each one states its
preconditions, gives the complete commands or configuration, and shows the
expected result. Unless stated otherwise, the examples use the two
identities ``provider`` and ``requester`` that :doc:`tutorial` creates, run
on one machine and registered with the same hub.

.. contents:: On this page
   :local:
   :depth: 1

.. _intro-examples-limits:

A free capability with rate limits
==================================

A public capability is open to every caller on the hub. Its entry in
``inbound.public_capabilities`` bounds the load that one caller and all
callers together can put on the service.

Precondition: the service of the tutorial listens on
``~/.anet-tutorial/digest.sock``.

Replace the entries in the provider's ``config.json`` so that the
capability is free and limited:

.. code-block:: json

   {
     "inbound": {
       "policy": "closed",
       "public_capabilities": [
         {
           "id": "text.digest",
           "per_caller_per_min": 6,
           "per_caller_per_day": 200,
           "global_per_min": 60,
           "max_inflight": 4,
           "per_caller_inflight": 1,
           "max_args_bytes": 65536
         }
       ]
     },
     "modules": {
       "service": {
         "timeout_ms": 5000,
         "capabilities": [
           {
             "id": "text.digest",
             "url": "unix:///home/user/.anet-tutorial/digest.sock:/digest",
             "description": "SHA-256 digest of a text"
           }
         ]
       }
     }
   }

The value of ``url`` must be an absolute path. The configuration shows only
the keys that change; the other keys of the file remain. The limits are:

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Key
     - Limit
   * - ``per_caller_per_min``, ``per_caller_per_day``
     - Calls from one caller in a minute and in a day.
   * - ``global_per_min``
     - Calls from all callers together in a minute.
   * - ``max_inflight``, ``per_caller_inflight``
     - Calls running at the same time, in total and for one caller. The
       caller limit is 1 by default and cannot exceed ``max_inflight``.
   * - ``max_args_bytes``
     - Size of the call arguments.
   * - ``timeout_ms`` (service module)
     - Time limit for one call to the service.

A key that is left out takes its default. A capability without a ``price``
is free, and the requester receives no quote.

Restart the provider and call the capability from the requester:

.. code-block:: console

   $ anet stop provider && anet up provider
   $ anet --id requester delegate "$PROVIDER" --capability text.digest \
       --args '{"text":"hello"}' > /dev/null
   $ anet --id requester results
   {
     "results": [
       {
         "goal": "invoke capability text.digest",
         "state": "completed",
         ...
       }
     ]
   }

The call is free, so no payment line is printed and no limit is spent.

.. _intro-examples-caller:

A service that identifies the caller and a list-only capability
===============================================================

A capability marked ``allow_only`` is served only to peers on the allow or
trust list. To every other caller it is absent: the answer is the same, and
arrives at the same time, as for a capability that the node does not have,
and it is not on any card the hub sees.

The daemon passes the identity of an authenticated caller to the service in
headers, never in the body, so a service that was written for plain JSON
keeps working. A token file lets the service verify that a request comes from
the daemon and not from another process that can open the socket.

The service in this example returns the headers it received. Save it as
``whoami_service.py``:

.. code-block:: python

   import json
   import os
   import socketserver
   from http.server import BaseHTTPRequestHandler

   BASE = os.path.expanduser("~/.anet-example")
   SOCKET = os.path.join(BASE, "whoami.sock")
   TOKEN = open(os.path.join(BASE, "token")).readline().strip()


   class UnixServer(socketserver.UnixStreamServer):
       def get_request(self):
           request, _ = super().get_request()
           return request, ("local", 0)


   class Handler(BaseHTTPRequestHandler):
       def do_POST(self):
           if self.headers.get("Authorization") != "Bearer " + TOKEN:
               self.send_response(401)
               self.send_header("Content-Length", "0")
               self.end_headers()
               return
           self.rfile.read(int(self.headers.get("Content-Length", "0")))
           body = json.dumps({
               "caller": self.headers.get("X-ANet-Caller", ""),
               "call": self.headers.get("X-ANet-Call", ""),
               "capability": self.headers.get("X-ANet-Capability", ""),
           }).encode()
           self.send_response(200)
           self.send_header("Content-Type", "application/json")
           self.send_header("Content-Length", str(len(body)))
           self.end_headers()
           self.wfile.write(body)


   if os.path.exists(SOCKET):
       os.unlink(SOCKET)
   UnixServer(SOCKET, Handler).serve_forever()

Create the directory and a token of at least 16 characters, readable by the
owner only, and start the service:

.. code-block:: console

   $ mkdir -p -m 700 ~/.anet-example
   $ python3 -c 'import secrets; print(secrets.token_urlsafe(32))' > ~/.anet-example/token
   $ chmod 600 ~/.anet-example/token
   $ python3 whoami_service.py

In a second terminal, declare the capability and list it as ``allow_only``.
The command adds the entries to the configuration of the provider:

.. code-block:: console

   $ python3 - <<'EOF'
   import json, os
   home = os.path.expanduser("~")
   path = os.path.join(home, ".anet/ids/provider/config.json")
   config = json.load(open(path))
   config["inbound"]["public_capabilities"].append(
       {"id": "text.whoami", "allow_only": True})
   config["modules"]["service"]["capabilities"].append({
       "id": "text.whoami",
       "url": "unix://" + os.path.join(home, ".anet-example/whoami.sock") + ":/whoami",
       "token_file": os.path.join(home, ".anet-example/token"),
       "description": "Report the caller seen by the service",
   })
   json.dump(config, open(path, "w"), indent=2)
   EOF
   $ anet stop provider && anet up provider

The example assumes that the tutorial configuration, with the capability
``text.digest``, is in place.

While the requester is not on a list, the call is rejected with the reason
``not_accepting``, the same answer as for a capability that the node does
not have, and ``anet find`` does not show the capability:

.. code-block:: console

   $ anet --id requester delegate "$PROVIDER" --capability text.whoami --args '{}' > /dev/null
   $ anet --id requester task list | grep -E 'anet.reason|TASK_STATE'
           "anet.reason": "not_accepting",
           "state": "TASK_STATE_REJECTED",

After the provider has allowed the requester, the call succeeds and the
service sees the caller:

.. code-block:: console

   $ anet --id provider peers allow "$REQUESTER"
   Add unnamed (<requester-aid>) to the allow list: it can send tasks to this node.
   Type yes to confirm: yes
   $ TASK=$(anet --id requester delegate "$PROVIDER" --capability text.whoami \
       --args '{}' | sed -n 's/^  "id": "\(.*\)",$/\1/p')
   $ anet --id requester task get "$TASK" | grep observed_state
                 "observed_state": "{\"caller\": \"<requester-aid>\", \"call\": \"ix_0573e9718990b5e765f96c5354c68785\", \"capability\": \"text.whoami\"}",

The ``X-ANet-Caller`` header is present only when the daemon authenticated
the caller through the relay. A call that originates on the local machine
carries no caller, so the service can tell the two cases apart. The token
proves the daemon to the service; the service must never accept the header
without checking the token first.

The daemon refuses a token file that other users can read, and a token
shorter than 16 characters.

.. _intro-examples-exchange:

A task exchange between listed peers
====================================

A task that is not a capability call is free-form text. A node accepts it
only from a peer on its allow or trust list. The example shows the refusal,
the list entry and a completed exchange in which the requester sends and
the provider answers by hand. The
precondition is that the requester is not yet on the provider's allow list.
If the previous example added it, remove it first:

.. code-block:: console

   $ anet --id provider peers remove "$REQUESTER"

A task from an unlisted peer is rejected without being run:

.. code-block:: console

   $ anet --id requester delegate "$PROVIDER" "Summarise: ANet relays sealed envelopes." > /dev/null
   $ anet --id requester task list --role requester | grep -E 'TASK_STATE|anet.reason'
         "anet.reason": "not_accepting",
           "state": "TASK_STATE_REJECTED",

The sentence in the task names the command that the other operator runs:
"The other side does not take tasks from you and did not run it; ask them
to run: anet peers allow <requester-aid>." The operator confirms on a
terminal:

.. code-block:: console

   $ anet --id provider peers allow "$REQUESTER"
   Add unnamed (<requester-aid>) to the allow list: it can send tasks to this node.
   Type yes to confirm: yes
   {
     "aid": "<requester-aid>",
     "list": "allow",
     "changed": true,
     "pinned": "allow"
   }

``trust`` instead of ``allow`` additionally lets the peer drive the local
agent of the node, and it asks for confirmation in the same way. Now the
requester sends the task again, and the provider reads its inbox, answers
and ends the task:

.. code-block:: console

   $ TASK=$(anet --id requester delegate "$PROVIDER" "Summarise: ANet relays sealed envelopes." \
       | sed -n 's/^  "id": "\(.*\)",$/\1/p')
   $ anet --id provider inbox --pending
   {
     "inbox": [
       {
         "interaction_id": "ix_2fe179cbe113e56d93453424abb0a4ba",
         "requester": "<requester-aid>",
         "goal": "Summarise: ANet relays sealed envelopes.",
         "state": "submitted",
         "trust": "peer",
         ...
       }
     ]
   }
   $ anet --id provider message "$TASK" \
       "A relay passes sealed envelopes without reading them."
   $ anet --id provider end "$TASK"
   {
     "interaction_id": "ix_2fe179cbe113e56d93453424abb0a4ba",
     "status": "completed"
   }

Both sides use the same task identifier, which ``inbox`` also prints. The
requester reads the exchange with the full history:

.. code-block:: console

   $ anet --id requester task get "$TASK" --history 5 | grep -E 'ROLE_|"text"|TASK_STATE'
         "role": "ROLE_USER",
               "text": "Summarise: ANet relays sealed envelopes."
         "role": "ROLE_AGENT",
               "text": "A relay passes sealed envelopes without reading them."
       "state": "TASK_STATE_COMPLETED",

A provider that answers by hand is the simplest case. A local coding agent
can answer instead; see :doc:`/topics/delegation`.

.. _intro-examples-unattended:

A spending policy for an unattended requester
=============================================

A requester that works without an operator at a terminal needs limits that
are set beforehand. All limits and the payee list are changed on a terminal,
because the confirmation cannot be given through MCP or the local A2A
interface.

The policy of this example: pay at most 3 credits per call automatically,
spend at most 20 credits per day in total, pay only the provider of the
tutorial, and let no agent pay on its own.

.. code-block:: console

   $ anet --id requester payees add "$PROVIDER"
   $ anet --id requester payments set auto_max=3 agent_max=0 agent_daily_max=0 daily_max=20
   Change this node's payment limits:
     auto_max  5 → 3
     daily_max  50 → 20
   Type yes to confirm: yes
   {
     "auto_max": 3,
     "agent_max": 0,
     "agent_daily_max": 0,
     "explicit_max": 10,
     "daily_max": 20,
     "spent_today": 0,
     "agent_spent_today": 0,
     "left_today": 20,
     "agent_left_today": 0,
     "payees": 1
   }

The limits take effect as follows:

* A quote of 3 credits or less from the provider is paid when it arrives.
* A quote of more than 3 credits, or a quote from an AID that is not on the
  payee list, leaves the task in ``input-required`` with the reason
  ``needs_operator_approval``. The payment line names the next command,
  for example ``anet pay TASK``, which is limited by ``explicit_max``.
* An agent that calls ``submit_payment`` through MCP is not paid for.
  The task stays in ``input-required`` and states what the operator has to
  do on a terminal.
* After 20 credits have been authorized in a UTC day, no further payment is
  made until the day ends, and a payment confirmed on a terminal is refused
  as well. The daily total counts signed authorizations.

Every change of a limit and of the payee list is written to the evidence
chain with the old and the new value, and ``anet audit`` shows it:

.. code-block:: console

   $ anet --id requester audit --type anet.policy.changed

``anet doctor`` lists a limit that deviates from the defaults.

.. _intro-examples-evidence:

Verifying an evidence chain elsewhere
=====================================

The evidence chain of a node is an append-only, signed sequence of records:
delegations sent, payments authorized and settled, results accepted and
policy changes. It holds no task content. Exporting it and verifying it on
another machine shows to a third party what the node did, without access to
the node.

.. code-block:: console

   $ anet --id requester audit --export ./requester-evidence
   ...
   exported the whole chain (17 records) to ./requester-evidence — check it with: anet verify --chain ./requester-evidence
   $ ls requester-evidence
   audit.json  evidence.ael.jsonl  kel.b64  manifest.json
   $ anet verify --chain ./requester-evidence
   ...
     state  ACTIVE
     keys   the key history in the export itself (kel.b64)

     this shows every record was signed under that key history and none was altered,
     removed or reordered before the head. A chain cut short after some record is still
     a valid chain: pass --head <id> with a head you learned elsewhere to rule that out.
     the key history came with the export: compare the AID above with the one you expect,
     or pass --hub <url> to fetch it from a hub.

The check needs neither a daemon nor a network. The export contains its own
key history, so the AID printed by the command has to be compared with the
AID that the verifier expects, or the key history is fetched from a hub with
``--hub``. A chain that is cut after a record is still valid; ``--head``
takes the identifier of the newest record as learned from another source
and rejects a shorter chain.

.. _intro-examples-retention:

A shorter retention period
==========================

Task content is stored encrypted on the node and deleted after a retention
period: 30 days after a task ends, and 7 days for calls to public
capabilities. ``anet retention`` prints the current period. ``anet
retention set`` changes it, asks for confirmation on a terminal and
asks again before it deletes content that the new period already covers.

.. code-block:: console

   $ anet --id requester retention
   Task content is deleted 30 days after a task ends (public calls: 7 days).
   $ anet --id requester retention set 7d
   Task content will now be kept for 7 days after a task ends.

The period can be set from 1 to 3650 days with ``Nd``, or to ``forever``.
The evidence chain is not affected: it keeps the counterparty, the time, the
content identifier and the amount of every event, and no content.

.. v030-doc-verify: FS-11
