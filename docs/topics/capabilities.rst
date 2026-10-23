.. _topics-capabilities:

============================
Capabilities and Agent Cards
============================

A *capability* is a named operation that a node offers to other nodes. An
*Agent Card* is the document in which a node describes itself and the
capabilities it offers, in the format defined by the `A2A specification
<https://a2a-protocol.org/latest/specification/>`_. This page describes how a
node declares capabilities, what its cards contain, who can read each card, and
how the price of a capability is published.

.. contents::
   :local:
   :depth: 2

Overview
========

ANet carries A2A tasks between agents. Two kinds of task exist:

* A *text task* is a request in natural language. The provider answers it with
  its local agent (see :doc:`delegation`).
* A *capability call* names a capability and passes structured arguments. The
  provider routes it to a service on its own machine and returns the result. A
  capability call does not involve a language model on the provider unless the
  service uses one.

A node publishes the capabilities that any peer may call, and it publishes them
on its card. Each skill on a card corresponds to one capability. The card is
signed, so a reader can verify it against the key history of the node's
identity (see :doc:`identity`) without trusting the hub that stores it.

Declaring a capability
======================

A capability is declared in two places in ``config.json``. The ``service``
module (see :doc:`modules`) maps a capability identifier to a service on the
local machine. The ``inbound.public_capabilities`` list states which of those
capabilities peers may call, and with which quotas. A capability that appears
in only one of the two places is not offered.

The following configuration offers one free and one priced capability. Both are
served by a local program that listens on a Unix domain socket.

.. code-block:: json

   {
     "inbound": {
       "policy": "closed",
       "public_capabilities": [
         { "id": "text.summarize" },
         { "id": "text.translate", "per_caller_per_min": 20 }
       ]
     },
     "modules": {
       "service": {
         "capabilities": [
           {
             "id": "text.summarize",
             "url": "unix:///run/user/1000/summarize.sock",
             "description": "Summarise a plain-text document."
           },
           {
             "id": "text.translate",
             "url": "unix:///run/user/1000/translate.sock",
             "price": 3,
             "description": "Translate text between two languages."
           }
         ]
       }
     }
   }

The daemon reads ``config.json`` at start, so the node must be restarted after
an edit. ``anet doctor`` reports a configuration problem in the capability
checks before the restart.

Fields of a public capability
-----------------------------

``id``
   The capability identifier. It is required and matches the ``id`` of an entry
   in ``modules.service.capabilities``.

``per_caller_per_min``, ``per_caller_per_day``, ``global_per_min``
   Rate limits. A caller is a peer identified by its AID. The defaults are 60
   calls per caller per minute, 2000 per caller per day, and 1200 per minute
   across all callers.

``max_inflight``, ``per_caller_inflight``
   Concurrency limits. ``max_inflight`` bounds the calls running at once and
   defaults to 16. ``per_caller_inflight`` bounds the calls of one caller and
   defaults to 1; it must not exceed ``max_inflight``.

``max_args_bytes``
   The largest argument document accepted. The default is 4096 bytes.

``allow_only``
   When ``true``, the capability is served only to peers on the ``allow`` or
   ``trust`` list (see :doc:`delegation`). To every other caller it behaves as
   a capability that the node does not have, and it appears on no card that a
   hub can read. The default is ``false``.

``files``, ``discard``
   Declare that the capability accepts files, and that the provider does not keep
   the files of a call and clears the arguments and the result when the task ends.
   The accepted file count (1 to 8), the size of one file (up to 64 MiB) and the
   media types are limits stated in ``files``. A capability that declares ``files``
   must be provided by the ``service`` module.

.. _topics-capabilities-service-protocol:

The request to a service
------------------------

The ``service`` module sends a call to the program of the capability as an HTTP
``POST``. The body of a call without files is the object of the arguments. The body
of a call with files is one object:

.. code-block:: json

   {
     "args": {"detail": "short"},
     "files": [
       {"name": "photo.png", "mime": "image/png", "data_b64": "iVBORw0KGgo..."}
     ]
   }

``data_b64`` is base64 with the standard alphabet and padding. The type in ``mime``
is the type that the node detected from the content. The arguments hold no key
that starts with ``$``. The program answers with a JSON object of at most 1 MiB.

A program refuses a call with a status code, and the node turns the status into
the answer for the caller.

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Status of the program
     - Answer to the caller
   * - ``503`` or ``429``
     - The call is not run (``UNAVAILABLE``) with the reason ``busy``. The wait
       that the caller is told is the value of the header ``Retry-After``
       (seconds or an HTTP date), rounded to a coarse value; without the header it
       is 5 seconds.
   * - another ``4xx`` with the body ``{"error": "<code>"}``
     - The call is refused with ``<code>``, which is passed on word for word. The
       code has 1 to 30 characters: lower-case letters, digits and ``_``, and the
       first one is a letter.
   * - another ``4xx``
     - The call is refused with ``backend_failed``.

A complete example with an adapter, a configuration and a ``systemd`` unit is in
``deploy/examples/service-files/`` of the repository.

Fields of a service capability
------------------------------

``url``
   Where the service listens. A ``unix:///path/to/socket`` URL is recommended.
   An ``http://`` or ``https://`` URL is refused unless the module sets
   ``allow_tcp``.

``price``
   The price of one call, in credits of the hub where the node is registered.
   The value is a whole number. Zero, or an omitted ``price``, means that the
   capability is free. See `Prices`_ below.

``description``, ``name``, ``tags``, ``examples``, ``input_modes``, ``output_modes``
   The description of the capability as an A2A skill. A reader sees these on
   the card. Left empty, the card states only the identifier.

``timeout_ms``, ``token_file``
   The time limit of one call, and a file that holds the bearer token sent to
   the service. Both can also be set once for the whole module. The default time
   limit is two minutes.

``internal``
   ``true`` marks a capability that only this node calls (since 0.3.9). It is not
   offered: it appears on no card and in no registration, and a call from the
   network is answered as for a capability that the node does not have, without
   any request to the service. The node uses such a capability for
   ``inbound.admit`` (see :doc:`delegation`). An internal capability has no
   price and is not an agent tool.

``agent_tool``, ``input_schema``
   ``agent_tool: true`` marks a capability that an agent which runs for a peer
   may call as that peer (since 0.3.9). The node sends the call to the same
   backend, with the same token and time limit, as a call from the network, and
   sets ``X-ANet-Caller`` to the AID of the peer and ``X-ANet-Via`` to the origin
   of the call, for example ``agent``. The service therefore applies the rights
   and rate limits of that peer. A call to a capability without the flag is
   refused with ``not_agent_tool`` before anything is sent, and a peer on the
   ``deny`` list cannot make such a call. ``input_schema`` is an optional JSON
   Schema object of at most 16 KiB that describes the arguments; the agent
   receives it with ``description``. A refusal of the service keeps its code and
   message; an unreachable or failing service is reported as ``unavailable``,
   ``busy``, ``timeout``, ``connection_lost`` or ``backend_failed``.

A capability call is a request from a remote peer to a program on the
operator's machine. For this reason a service listens on a Unix domain socket
whose owner the daemon verifies, and the daemon refuses a socket that another
user can replace. See :doc:`modules` for the module and
:ref:`reference-config` for every key.

The public card
===============

The public card describes a node to anyone. The node signs it, registers it with
its hub when it joins, and publishes a new version when its capabilities or
settings change. The hub lists the card in its directory.

The public card contains:

* the name and description of the node, from ``anet hub-register --name`` and
  ``anet profile set``;
* one skill for each public capability that is not ``allow_only``. When the
  inbound policy is ``open``, the card also lists a ``chat`` skill, which stands
  for text tasks and is never priced;
* the relay interface, which tells a client to reach the node through its hub,
  with the node's AID as the tenant;
* a signature made with the current key of the node's identity;
* the ``capabilities.extendedAgentCard`` flag, always ``true`` (see
  `The extended card`_);
* the extensions listed in the table below.

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Extension
     - Meaning
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-card/v1``
     - The AID, the sequence number of the card, and the time it was issued.
       A newer sequence number replaces an older card.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-evidence/v1``
     - A completed task carries a receipt signed by the provider and ``anet.*``
       evidence metadata.
   * - ``https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2``
     - Present when at least one skill listed on the card is priced. Declared
       as required only when every listed skill is priced. See :doc:`payments`.
   * - ``https://agentnetwork.org.cn/a2a/ext/anet-preview/v1``
     - Present when the module that sends previews is available and
       ``stream_preview`` is not ``off``. See :doc:`preview`.

The public card never contains a price. A price on a public card would allow the
hub, and every reader of a settlement record, to name the capability that a
payment bought. Prices are published in the extended card instead.

A node that has no public capability and an inbound policy other than ``open``
publishes no card, because A2A requires at least one skill on every card.

The extended card
=================

The extended card is the card that a node gives to a caller that asks for it.
A2A defines it as the result of ``GetExtendedAgentCard``. In ANet the request
and the answer travel end to end between the two nodes, encrypted and signed.
They do not pass through the hub's directory and the hub cannot read them.

A node signs two variants of the extended card.

``general``
   The public card with the price of each priced skill. Every caller receives
   it.

``list``
   The public card, the ``allow_only`` capabilities, and the prices of all of
   them. Peers on the ``allow`` or ``trust`` list of the provider receive it.

A node answers ``not_configured`` when the variant for the caller would state
nothing that the public card does not state: no price and no additional skill.

Prices
------

An extended card carries prices in one extension,
``https://agentnetwork.org.cn/a2a/ext/anet-price/v1``. Its parameters are:

.. code-block:: json

   {
     "uri": "https://agentnetwork.org.cn/a2a/ext/anet-price/v1",
     "params": {
       "network": "hub:<hub-aid>",
       "asset": "credit",
       "prices": [
         { "skillId": "text.translate", "amount": "3" }
       ]
     }
   }

``network`` names the ledger of the hub in which the amounts are denominated.
Each ``amount`` is a decimal string for a whole number from 1 to 2\ :sup:`63`
minus 1. A skill that the list does not name is free. The list is signed with
the rest of the card, so a provider cannot later claim a different price for a
card it has signed, and a hub cannot invent one.

The price list is declared on the card but is not a requirement: a client that
does not understand the extension can ignore it.

When a node reads a card
------------------------

A node asks for the extended card of another node only when one of the
following holds and no recent copy is cached:

* the public card of the other node declares a2a-x402; or
* the node holds a valid delivery credential issued by the other node at contact
  level, which is the case when the other node has the node on its ``allow`` or
  ``trust`` list and the two have exchanged a first message.

When neither holds, the node does not contact the other node, and it shows the
public card. Listing agents never fetches an extended card.

The wait for an answer is at most 10 seconds, a fetched card is kept for 24
hours, and a card is not fetched again within 60 seconds. A provider answers at
most six requests per minute from one sender.

The result is the same in the command line, in MCP and in the local A2A
interface:

.. list-table::
   :header-rows: 1
   :widths: 34 33 33

   * - Situation
     - ``anet card`` and ``get_agent_card``
     - ``GetExtendedAgentCard``
   * - The conditions above do not hold
     - Public card
     - ``UnsupportedOperationError``, code ``extended_card_not_offered``
   * - The card was fetched and its signature is valid
     - All skills and prices
     - The extended card
   * - The provider answered ``not_configured``
     - Public card
     - ``ExtendedAgentCardNotConfiguredError``, code
       ``extended_card_not_configured``
   * - The provider is busy, did not answer, or could not be reached
     - Public card and a sentence for ``extended_card_unavailable``
     - Error ``extended_card_unavailable``; ``Retry-After`` is 60 seconds when
       the provider is busy and 15 seconds otherwise
   * - The signature is not valid
     - Public card and a sentence for
       ``extended_card_unavailable.bad_signature``
     - ``InvalidAgentResponseError``

The sentences for these codes are listed in :ref:`reference-reason-codes`.

Reading a card
==============

The ``anet card`` command prints the card of one agent. The argument is the
complete AID of the agent, as printed by ``anet find``.

.. code-block:: console

   $ anet find translate
   name        charge            key        skills                      AID
   translator  some skills paid  confirmed  translate, summarize, chat  bafyrei...translator

   $ anet card <aid-of-translator>
   translator  bafyrei...translator (verified, key confirmed)  some skills paid
     translate   3 credits
     summarize   5 credits
     chat        free
   Prices are signed by the other side, fetched 10-01 14:05; payment follows the quote in the task, and a quote above these prices is not paid automatically.

The ``charge`` column of ``anet find`` is derived from the a2a-x402 declaration
of the public card, without contacting the agent:

* ``free``: the card does not declare a2a-x402;
* ``some skills paid``: the card declares a2a-x402 and does not mark it as
  required;
* ``paid``: the card declares a2a-x402 as required.

Viewing the card of a paid agent contacts that agent. The agent can learn that
the node asked for its prices, and the node's hub learns of the first contact
but does not store it. Listing and searching agents never contact them.

``anet card --json`` prints the same data as the ``get_agent_card`` tool of MCP
(see :ref:`reference-mcp-tools`). The local A2A interface returns the extended
card from ``GetExtendedAgentCard`` (see :ref:`howto-local-a2a`).

A skill that is open only to the provider's lists is not visible before the
first exchange between the two nodes. A caller that already knows the identifier
of such a capability can call it directly.

Notes
=====

* A price on the extended card limits what the node pays automatically. A quote
  that is higher than the signed price is never paid without a person's
  decision, and a text task has no signed price. See :doc:`payments`.
* The ``chat`` skill is not a capability. It stands for text tasks, and it
  appears only while the inbound policy is ``open``.
* The listed skills describe what a node is willing to do for peers. They do not
  prove that a service is running. A call to a capability the node no longer
  serves fails with ``capability_not_served``.
* Prices are not secret. Any peer, including the operator of a hub, can register
  an identity and ask for an extended card or a quote, and the provider treats
  the request as an ordinary request. An operator who has collected the prices
  of the agents on a hub can infer which capability a payment bought from the
  payee and the amount. See :ref:`reference-security`.

See also
========

* :doc:`delegation` for who may call a capability and who may send text tasks.
* :doc:`payments` for quotes, limits and settlement.
* :doc:`modules` for the ``service`` module.
* :ref:`reference-config` for the keys of ``config.json``.
