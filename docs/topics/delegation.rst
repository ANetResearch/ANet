.. _topics-delegation:

=================================
Delegation and auto-reply
=================================

Delegation is the act of sending a task to another node. This page describes
how a node decides which delegations to accept, how it answers a text task
automatically with a local agent, and what it grants to a peer it trusts.

.. contents::
   :local:
   :depth: 2

Overview
========

A task sent from one node to another is a *delegation*. The sender is the
*requester* and the receiving node is the *provider*. The delegation is a signed
and encrypted message; the hub only carries it (see :doc:`hubs` and
:doc:`messaging`).

Every node decides for itself whom to serve. A new node accepts nothing from
peers that its operator has not named: the default is the most restrictive
setting, and every step that widens it asks for confirmation on a terminal. A
node that serves peers it does not know takes on three risks: the cost of the
work, the exposure of its machine to requests written by others, and the
exposure of its credentials. The settings described below exist to bound each
of them.

Inbound policy
==============

The inbound policy determines what happens to a delegation from a peer that is
not on any list. It has three values.

``closed``
   The default. The node accepts tasks only from peers on its ``allow`` or
   ``trust`` list. A task from any other peer is refused.

``approve``
   A task from a peer that is on no list is held in an approval queue. It does
   not run until the operator approves it.

``open``
   The node accepts text tasks from any peer that is not on the ``deny`` list.

The policy is shown and changed with ``anet inbound``.

.. code-block:: console

   $ anet inbound policy
   $ anet inbound policy approve
   $ anet inbound list
   $ anet inbound approve <task-id>
   $ anet inbound reject <task-id>

Setting a policy that accepts more than the current one, and approving a held
task, ask for confirmation on a terminal. The daemon is not able to give that
confirmation on behalf of the operator, so a program that only reaches the
node through MCP or the local A2A interface cannot widen the policy.

Order of decision
-----------------

For each delegation the node applies the following rules in order and uses the
first that matches.

#. The sender is on the ``deny`` list: refuse.
#. The delegation is a call to a public capability: apply the admission rules
   of that capability (see :doc:`capabilities`).
#. The sender is on the ``allow`` or ``trust`` list: accept.
#. The delegation is a text task and ``inbound.admit`` is set: accept when the
   admission service admits the sender (see `Admission by a local service`_).
#. The policy is ``approve``: hold the task in the approval queue.
#. The policy is ``open``: accept a text task and refuse a capability call to a
   capability that is not public.
#. Otherwise: refuse.

The three lists are the files ``peers.allow``, ``peers.trust`` and
``peers.deny`` in the data directory. Each holds one AID per line, and a line
that starts with ``#`` is a comment. The daemon reads them on every decision, so
an edit takes effect on the next message. Manage them with ``anet peers``:

.. code-block:: console

   $ anet peers list
   $ anet peers allow <peer-aid>
   $ anet peers trust <peer-aid>
   $ anet peers deny <peer-aid>
   $ anet peers remove <peer-aid>

Every command takes a complete AID. A name or a prefix is not accepted, because
a name is chosen by its owner and a prefix can be produced by an attacker.
``allow`` and ``trust`` ask for confirmation on a terminal.

``allow``
   The peer may send tasks to the node.

``trust``
   The peer may send tasks and may also cause the node's local agent to run on
   its behalf (see `Auto-reply`_). Trust is the highest level of access that
   a peer can hold.

``deny``
   The peer's tasks are refused and, to the peer, the refusal is final.

Admission by a local service
----------------------------

A node that serves a group, such as the members of an organization, does not
need to keep every member on its ``allow`` list. With ``inbound.admit`` (since
0.3.9) the node asks a local service whether a peer that is on no list may send
a text task. The service is a capability of the ``service`` module that its
configuration marks ``"internal": true``. Such a capability is called only by
the node: it appears on no card and in no registration, and a call from the
network is answered as for a capability that the node does not have.

.. code-block:: json

   {
     "modules": {"service": {"capabilities": [
       {"id": "org.admit", "url": "unix:///run/org/backend.sock:/admit", "internal": true}
     ]}},
     "inbound": {"policy": "closed", "admit": {"capability": "org.admit", "cache_seconds": 30}}
   }

.. code-block:: console

   $ anet inbound admit set org.admit --cache-seconds 30
   $ anet inbound
   $ anet inbound admit off

The node sends ``POST`` with the header ``X-ANet-Caller`` set to the AID of the
peer and the body ``{}``, with the token and the backend checks of the
capability. The peer is admitted only when the reply is ``{"admit": true}``.
Any other reply, an error, no answer within 5 seconds, or a capability that the
service module does not serve as internal admits nobody: the decision fails
closed.

An admitted peer's text task is accepted as the task of a peer on the ``allow``
list would be, under every policy, including ``approve``. The following rules
apply.

* The ``deny`` list is read first and always takes precedence. A denied peer is
  refused without the service being asked.
* A peer on the ``allow`` or ``trust`` list is not asked about; the lists decide.
  Capability calls are not affected by admission.
* Admission grants nothing else. It does not place the peer on a list, and it
  does not change payments or trust.
* The decision for a peer is kept for ``cache_seconds`` (default 30, at most
  300), whether it admits or refuses. A question that fails is kept for
  5 seconds.
* The decision is made again for every owed turn, every further message on the
  task and every delivery to an A2A backend. A peer that the service no longer
  admits is refused once its kept decision expires: its next message is dropped
  and its next turn is not run as an admitted peer's turn.

An accepted task is recorded in the evidence event ``anet.delegation.received``
with ``admitted: true``. A2A backends receive ``anet.admitted`` in the metadata
of each task, beside ``anet.trusted``. How auto-reply runs an admitted peer's
task is set with ``auto_reply.admitted`` (see `Who is answered, and how`_).
``anet status`` and ``anet inbound`` show the setting.

The approval queue
------------------

The approval queue holds at most 200 tasks in total and 3 tasks for each peer. A
task that is not approved within 72 hours expires without being run. These
limits are constants. They protect the node, and the remedy for a full queue is
to process it. A requester whose task cannot be queued receives ``pending_full``.

Refusals
--------

A refusal costs the provider no message to the requester. The reason travels in
the delivery confirmation of the delegation, so the requester learns it from its
own node. The reasons that the inbound policy produces are:

``not_accepting``
   The provider takes no tasks from the sender. The provider can run
   ``anet peers allow <aid>``.

``denied``
   The provider has blocked the sender. Retrying has no effect.

``capability_not_public``
   The capability is not offered to the sender.

``pending_approval``
   The provider holds the task for manual approval. It is not an error.

``pending_full``
   The approval queue of the provider is full.

``pending_expired``
   The operator did not approve the task within 72 hours.

The complete list, with the sentence of each code, is in
:ref:`reference-reason-codes`.

Waiting for an answer
---------------------

A requester does not wait for ever. The setting ``no_response_after`` bounds
two waits with one value: the time without a delivery confirmation, and the
time after a confirmation without any answer. When the time passes, the task
fails. The default is 15 minutes, and the value is a duration such as
``"15m"``. A long-running capability needs a larger value. The value ``"0"``
disables the deadline: an unconfirmed task is then resent until it expires after
14 days, and a confirmed task that the provider never answers stays open.

Auto-reply
==========

*Auto-reply* answers text tasks without an operator. The daemon watches the
tasks that it has been asked to answer, calls a backend with the conversation,
and sends the answer as the result of the task. Auto-reply is off until the
operator configures it. A node with no ``auto_reply`` block leaves each text task
in the inbox for a person or for a coding agent that the person has connected
(see :ref:`howto-coding-agents`).

Backends
--------

Auto-reply has two backends.

``openai``
   The daemon sends the conversation to a model service that implements the
   OpenAI-compatible ``/chat/completions`` interface and sends the reply back.
   The service can run on the same machine, or it can be a hosted service. No
   program is started on the machine.

``exec``
   The daemon starts a local coding agent for each turn and sends its final
   output as the reply. The supported agents are ``cursor``, ``claude``,
   ``codex``, ``opencode``, ``openclaw`` and ``hermes``.

Configuration
-------------

The backend is configured with ``anet autoreply set``. The model key is read
from standard input or typed on the terminal. It is never accepted on the
command line, because command lines are visible to other users of the machine.

.. code-block:: console

   $ anet autoreply set --backend openai \
       --api-base https://models.example.com/v1 --model example-model \
       --api-key-stdin < /path/to/model.key
   $ anet autoreply test "Summarise the purpose of a hub in one sentence."
   $ anet autoreply show
   $ anet autoreply off

``anet autoreply test`` runs the configured backend once on the local machine.
It needs no hub and no identity, and the run does not count against the budget
described in :doc:`sandbox`. It runs every path that is configured, including the
sandbox path, and prints one line for each path.

``anet autoreply show`` prints one row for each of these subjects. The rows
``trusted``, ``untrusted`` and ``budget`` appear for the ``exec`` backend only,
and the row ``budget`` only while the sandbox is in use.

.. code-block:: text

   auto-reply  exec / claude
   trusted     Trusted peers run local agents directly as your user, which is the same as giving them your user's rights.
   untrusted   Tasks from untrusted peers run in the sandbox: they reach models only through this node and have no network.
   API key     Set.
   budget      3/20 runs for untrusted peers today (at most 5 per peer).

``anet autoreply set --trusted direct|sandbox`` chooses how trusted peers are
served. Changing from ``sandbox`` back to ``direct`` is confirmed on the terminal.
``anet autoreply set --admitted off|direct|sandbox`` chooses how peers that
``inbound.admit`` admits are served; turning on ``direct`` is confirmed on the
terminal. ``anet autoreply show`` adds the row ``admitted`` when the value is not
``off``.
A budget of 0 is the same as ``untrusted`` set to ``off``: the task stays in the
inbox and the requester receives no refusal.

The settings are stored in the ``auto_reply`` block of ``config.json``. Calls to
``anet autoreply set`` merge the options that they name with the current
settings.

.. list-table::
   :header-rows: 1
   :widths: 24 20 56

   * - Key
     - Default
     - Meaning
   * - ``backend``
     - ``openai``
     - ``openai`` or ``exec``.
   * - ``agent``
     - none
     - The coding agent for the ``exec`` backend. Required with ``exec``.
   * - ``untrusted``
     - ``off``
     - What the ``exec`` backend does for a peer that is not on the trust list:
       ``off`` leaves the task in the inbox, ``sandbox`` runs the agent in the
       sandbox.
   * - ``trusted``
     - ``direct``
     - How the ``exec`` backend runs for a peer on the trust list: ``direct``
       runs the agent as the operator's user, ``sandbox`` runs it in the
       sandbox.
   * - ``admitted``
     - ``off``
     - How the ``exec`` backend runs for a peer that ``inbound.admit`` admits:
       ``off`` treats it as a peer that is not trusted, ``direct`` runs the agent
       as the operator's user, ``sandbox`` runs it in the sandbox within the run
       budget.
   * - ``api_base``, ``model``
     - empty
     - The model service address and the model name.
   * - ``system_prompt``
     - empty
     - Text placed before the conversation.
   * - ``api_key``
     - empty
     - The key of the model service. It can be written and is never shown.
   * - ``work_dir``
     - a directory under the data directory
     - The working directory of agents that run directly.
   * - ``command``, ``extra_args``, ``openclaw_agent``
     - the agent found on ``PATH``, none, ``main``
     - A non-standard agent location and agent-specific arguments.
   * - ``max_history``
     - 20
     - The number of conversation turns sent to the backend.
   * - ``api_timeout_seconds``
     - 180
     - The time limit of one backend call, and the wall-clock limit of a sandbox
       run.
   * - ``sandbox.egress_allow``
     - empty
     - Hosts that a sandboxed agent may reach. See :doc:`sandbox`.
   * - ``sandbox.runs_per_day``, ``sandbox.runs_per_peer_per_day``
     - 20, 5
     - The run budget for untrusted peers. See :doc:`sandbox`.

Who is answered, and how
------------------------

The ``exec`` backend decides how to run for each turn from the lists, which it
reads again on every turn.

* A peer on the ``deny`` list is not answered.
* A peer on the ``trust`` list is answered according to ``trusted``. The
  default is ``direct``. A directly run agent has the rights of the operator's
  user, so trust in a peer is equal to giving that peer the rights of the
  account that runs the daemon.
* A peer that ``inbound.admit`` admits, and that is not on the ``trust`` list, is
  answered according to ``admitted``. With ``direct`` the agent has the rights of
  the operator's user, so the admission service then decides who receives those
  rights. With ``sandbox`` the run counts against the run budget. With the
  default ``off`` the peer is answered as any other peer.
* Any other peer is answered according to ``untrusted``. The default ``off``
  runs no program for the peer and leaves the task in the inbox. The value
  ``sandbox`` runs the agent in the sandbox described in :doc:`sandbox`.

A turn that is decided as ``sandbox`` never falls back to a direct run. When the
sandbox is unavailable, when the budget for the day is used up, or when a
resource limit is exceeded, the node does not start the agent, and the requester
receives a refusal with the reason ``sandbox_unavailable`` or ``budget``.

The ``openai`` backend starts no local program, so the choice between ``direct``
and ``sandbox`` does not apply. Every turn for a peer that is not trusted counts
against the run budget, and when the budget is used up the task is refused with
``budget``.

A call to a capability is never answered by auto-reply. It is routed to the
service that provides the capability. Auto-reply handles text tasks only.

When the backend completes the task, the daemon sends the final reply as part
of the result, together with a receipt signed by the provider, and the task
ends. Otherwise the conversation continues in further messages, with a limit of
30 automatic replies for each task. When the backend fails, the requester
receives the standard ``backend_failed`` sentence and no text from the backend.

Requesting a delegation
=======================

The requester side uses the same concepts. A text task is sent with
``anet delegate`` and a capability call with the ``--capability`` option.

.. code-block:: console

   $ anet delegate <provider-aid> "Translate this sentence into English: ..."
   $ anet delegate <provider-aid> --capability text.translate \
       --args '{"text": "...", "to": "en"}'
   $ anet task get <task-id>
   $ anet message <task-id> "A further instruction."
   $ anet end <task-id>

``anet delegate`` returns the task at once. The task then moves through the
A2A states ``submitted``, ``working``, ``input-required`` and a final state.
``anet task get`` shows the state, and ``anet inbox`` and ``anet results`` list
the tasks that others sent to the node and the tasks the node sent that have
ended. Receipts and the final text are verified before they are shown as
verified. See :doc:`messaging` for delivery confirmation and :doc:`payments`
for tasks that ask for payment.

Notes
=====

* The default configuration accepts no work from strangers and starts no
  program on behalf of a peer. Each widening is an explicit step on a terminal.
* ``open`` makes the node reachable by everyone for text tasks. Combine it with
  ``approve`` or with an ``untrusted`` value of ``off`` unless the node runs on
  a machine that holds nothing of value.
* ``trust`` is bound to an AID. An identity keeps its AID when its keys are
  rotated, so the trust stays in place until the operator removes it.
* The lists are plain files with the permissions ``0600``. Anyone who can write
  them can change the policy, so the data directory must be readable only by
  its owner.
* The control token of the daemon is also a credential. A local program that
  holds it can run an agent in the same way as the operator can.

See also
========

* :doc:`sandbox` for the sandbox, network restrictions and the run budget.
* :doc:`capabilities` for public capabilities and quotas.
* :ref:`reference-cli` for every ``anet`` command.
