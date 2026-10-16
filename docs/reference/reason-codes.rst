.. _reference-reason-codes:

Reason codes
============

Every refusal, failure, wait and status line that ANet reports to a person
carries a *reason code* and one sentence. The command line, the MCP tools, the
local A2A interface, the control plane and the local console take their
sentences from one table, so the same event produces the same code and the same
words on every surface. This page lists that table.

.. contents::
   :local:
   :depth: 1


How a code is reported
----------------------

A code is a lower-case identifier of at most 30 characters that matches
``[a-z][a-z0-9_]*``. Some rows have a *variant*; the row key is then the code, a
dot and the variant (for example ``delivery_unconfirmed.queued``). A variant
changes the words of the sentence and never the code.

The same code appears in these places:

``anet.reason``
   The metadata key of a task or of a single message in the local A2A
   interface, the MCP task projection and the control plane.

``reason``
   The field of a control-plane error body. The body has the form
   ``{"error": "...", "reason": "...", "next": "..."}``. The command line prints
   the ``error`` sentence on standard error and, when ``next`` is present, a
   second line that begins with an arrow (``→``). It does not print the JSON body.

``ErrorInfo``
   The details of an A2A error returned by the local interface. The ``details``
   array holds two ``google.rpc.ErrorInfo`` entries. The first has the domain
   ``a2a-protocol.org`` and the reason of the A2A error. The second has the
   domain ``agentnetwork.org.cn``, an upper-case form of the code as the reason,
   and the lower-case code in ``metadata.reason``.

The sentence is rendered by the node, in the language of the node's
environment. The language is taken from the first non-empty variable of
``LC_ALL``, ``LC_MESSAGES`` and ``LANG``: a value that begins with ``zh``
selects Chinese, any other value selects English, and an unset environment
selects English. Clients pass the sentence on unchanged.

A sentence is built from a template. Placeholders in braces, such as ``{aid}``,
``{peer}``, ``{task}``, ``{retry}`` and ``{limit}``, are filled only from data
that the node itself holds. Text written by a remote peer never becomes part of
a sentence. A placeholder without a value is replaced by a neutral form; it is
never printed as a brace expression or left empty.


Classes
-------

The *Class* column of the tables names where a sentence appears and how A2A
carries it.

.. list-table::
   :header-rows: 1
   :widths: 8 40 52

   * - Class
     - Meaning
     - A2A and a2a-x402 mapping
   * - R
     - The other side certainly did not run the request.
     - Task state ``TASK_STATE_REJECTED``. On a capability task,
       ``anet.effect_status`` is ``UNAVAILABLE``. When a wait applies,
       ``anet.retry_after_ms`` is present.
   * - F
     - A failed end.
     - Task state ``TASK_STATE_FAILED``. The effect status of a capability task
       is given with the row (``UNVERIFIED``, ``UNAVAILABLE`` or ``FAILED``).
   * - FP
     - A payment did not settle.
     - ``x402.payment.status`` is ``payment-failed`` and ``x402.payment.error``
       is the standard a2a-x402 code given with the row. The task state is not
       changed.
   * - IP
     - Waiting for a person to decide whether to pay.
     - Task state ``TASK_STATE_INPUT_REQUIRED`` and
       ``x402.payment.status`` ``payment-required``.
   * - W
     - A wait that resolves on its own.
     - The task state is unchanged. ``anet.delivery`` is ``pending`` and
       ``anet.delivery_wait`` holds the code. No ``status.message`` is written.
   * - M
     - A single message was refused. The task is unchanged.
     - The code is the ``anet.reason`` metadata of that message.
   * - E
     - This node refused the request and sent nothing.
     - An A2A error: JSON-RPC ``-32602`` (REST 400) when the request itself or a
       lasting state of the node is the cause, ``-32603`` (REST 503 with
       ``Retry-After``) when the condition is temporary.
   * - C
     - Appears only in the command line or in a control-plane error body.
     - Control-plane error body ``{error, reason, next}``.
   * - S
     - A status sentence.
     - Fields ``message`` and ``summary`` of ``/status``; the MCP tool
       ``node_status`` returns them unchanged.
   * - I
     - Information that does not change any state.
     - The same projection as the task.
   * - T
     - A terminal prompt or confirmation line.
     - Printed by the command line only.

The *Next step* column names what the sentence asks of the reader. ``command``
rows give the command, with its placeholders. The other values are
``none needed``, ``auto`` (the node is handling the matter), ``retry later``,
``change input`` (change the request, an argument, a file or the condition of the
node, then repeat), ``contact`` (ask the other side or an operator),
``start over`` (create a new task) and ``futile`` (repeating the request does not
help).


Codes that are not shown
------------------------

Some conditions are handled by the node and have no sentence: an expired hub
session that the daemon reopens, a stale encryption key identifier that the
outbound engine re-wraps, replayed or stale request timestamps that the daemon
signs again, and the many lower-case, hyphenated rejection counters of the
sealed-envelope layer. These are counted and written to the log. They do not
appear in ``anet.reason``, in a control-plane ``reason`` or in an ``ErrorInfo``.

Rejections of the ANetLink C1 interface are mapped to user codes and are not
exposed: ``not_permitted`` is reported as ``provider_policy``,
``unknown_capability`` and ``not_found`` as ``capability_not_served``,
``too_large`` as ``args_too_large``, and ``bad_request``, ``unsupported_media``,
``internal`` and ``c1_version`` as ``backend_failed``.


The table
---------

The tables are generated from the reason table in the ANet source by
``docs/_tools/gen_reason_codes.py``. Each sentence is given in English and in
Chinese. Both are the exact text that the node prints.

.. include:: _generated/reason-codes.inc


Hub operator sentences
----------------------

The sentences printed by ``anet-hub`` are kept in a separate table in the
ANetHub source. The ``Class`` column is ``C`` for a refusal and ``I`` for a
result. Refusals use the exit codes given in the table.

.. include:: _generated/hub-sentences.inc


See also
--------

* :doc:`cli` for the exit codes of the command line.
* :ref:`howto-troubleshooting` for a list of the codes that an operator meets most
  often, with the usual causes.
* :doc:`hub-api` for the codes that a hub returns on the wire.
