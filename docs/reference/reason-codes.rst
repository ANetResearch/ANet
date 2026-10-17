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
   the ``error`` sentence followed by the code in brackets on standard error and,
   when ``next`` is present, a second line that begins with an arrow (``→``). It
   does not print the JSON body. See :ref:`reason-codes-shown`.

``ErrorInfo``
   The details of an A2A error returned by the local interface. The ``details``
   array holds two ``google.rpc.ErrorInfo`` entries. The first has the domain
   ``a2a-protocol.org`` and the reason of the A2A error. The second has the
   domain ``agentnetwork.org.cn``, an upper-case form of the code as the reason,
   and the lower-case code in ``metadata.reason``.

The sentence is rendered by the node. The language is English unless the
environment variable ``ANET_LANG`` of the node begins with ``zh`` (in any letter
case), which selects Chinese. Any other value, and an unset variable, select
English; ``LC_ALL``, ``LC_MESSAGES`` and ``LANG`` are not consulted. Rows of the
class that is read by the sandboxed agent (the egress refusals) are English in
both languages. Clients pass the sentence on unchanged.

A sentence is built from a template. Placeholders in braces, such as ``{aid}``,
``{peer}``, ``{task}``, ``{retry}`` and ``{limit}``, are filled only from data
that the node itself holds. Text written by a remote peer never becomes part of
a sentence. A placeholder without a value is replaced by a neutral form; it is
never printed as a brace expression or left empty.


.. _reason-codes-shown:

Where the code is shown
-----------------------

A person who sees a refusal can name it by its code, and a script can match on
it.

Command line
   A refusal is printed on standard error as the sentence, a space and the code
   in brackets on the same line, then the ``next`` line. For example::

      The other side is too busy right now and did not run it; send it again in 2 minutes. [busy]
        → anet task get t_9f3c

   This applies to every refusal that the control plane answers with a reason,
   to the refusals of ``anet agents wire`` and ``anet agents unwire`` (the
   ``wire_`` codes), and to the refusals of ``anet hub-register`` and
   ``anet hub-leave``. When the hub refused the request with a code of its own,
   the line shows both: ``[hub_refused:<hub code>]``. The hub codes are listed in
   :ref:`hub-api-codes`.

MCP tools
   The text of a tool error is the sentence of the control plane followed by the
   code in brackets, in the same form.

JSON
   Unchanged. The control-plane body, ``anet.reason`` and ``ErrorInfo`` carry the
   code in their own fields, and the output of ``--json`` has no bracketed
   suffix.

Diagnostic report
   ``anet doctor`` keeps its own format, which already shows the reason of each
   check.


.. _reason-codes-lifecycle:

Code lifecycle
--------------

A reason code is a stable interface. Scripts, agents and operators rely on it, so
the following rules hold for every code.

* The meaning of a code never changes. A sentence may be reworded and a variant
  may be added, but the event that the code names stays the same.
* A code is never renamed. A different event has a different code.
* A code is never reused. When a code is retired, its name stays reserved.
* A retired code stays on this page, in :ref:`reason-codes-retired`, with the
  version that retired it and, when there is one, the code that replaces it.
* New and retired codes are announced in the release notes (:doc:`../news`).
* A script matches on the code, never on the sentence. Sentences are rewritten
  between releases and differ between English and Chinese.

The *Since* column of the tables gives the first version of ``anet`` that emitted
the code. The register of every code ever published is the file
``internal/a2ashape/codes.txt`` in the source; a test holds it against the reason
table, so a code cannot be added, removed or reused without a deliberate change
to the register. The procedure is in :doc:`../contributing`.

Every code has an anchor of the form ``reason-code-<code>``, with each
underscore of the code replaced by a hyphen: the code ``busy`` is
``reason-code-busy`` and ``hub_refused`` is ``reason-code-hub-refused``. A page
can link to a code with the role ``ref`` and the label ``reason-code-busy``, and a
browser can open ``reason-codes.html#reason-code-busy``.


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

The tables are generated from the reason table and the code register in the ANet
source by ``docs/_tools/gen_reason_codes.py``. Each sentence is given in English
and in Chinese. Both are the exact text that the node prints. The *Since* column
is empty for a row that is a sentence key and not a code (a dotted key).

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
* :ref:`hub-api-codes` for the codes that a hub returns, with their first version.
* :ref:`howto-troubleshooting` for a list of the codes that an operator meets most
  often, with the usual causes.
* :doc:`hub-api` for the codes that a hub returns on the wire.
