.. _topics-preview:

=================
Preview streaming
=================

Preview streaming lets a requester see the text of a reply while the provider is
still writing it. This page describes how a provider enables previews, how a
requester asks for them, which limits apply to a preview, and why a preview is
not a result.

.. contents::
   :local:
   :depth: 2

Overview
========

A provider that answers a text task with a language model can take a long time
to finish. Without previews the requester sees nothing until the result arrives.
With previews the provider sends a snapshot of the reply at intervals. The
requester sees the end of the text so far and can tell whether the task is
progressing.

A preview is not part of the task. It is not stored, it does not appear in the
history of the task, and it is not signed as a result. The result that ends the
task replaces every preview, and the result is the only text that the receipt
covers.

Previews are off by default. Both sides take part:

* the provider enables previews on its node, and a text task is the only kind of
  task that can be previewed;
* the requester asks for previews for each task. A requester that does not ask
  receives none.

Enabling previews on a provider
===============================

The setting ``stream_preview`` takes three values.

``off``
   The default. The node sends no preview.

``on``
   The node sends previews to requesters that ask for them.

``with_reasoning``
   As ``on``, and the node also sends the reasoning text of the model to
   requesters that ask for it.

The ``anet preview`` command shows and changes the setting.

.. code-block:: console

   $ anet preview
   $ anet preview on
   $ anet preview off

Setting ``with_reasoning`` asks for confirmation on a terminal, because the
reasoning of a model can contain the system prompt and internal information. The
other values do not ask for confirmation. Changing between ``off`` and the other
values causes the node to publish its card again, so that requesters can see
whether the provider sends previews (see :doc:`capabilities`).

.. v030-doc-verify: UX-9
   The anet preview command and its confirmation for with_reasoning are
   specified in the design and are not yet in the main checkout. The
   stream_preview configuration key and the card declaration are specified in the
   same package. Confirm the command output and the doctor messages (preview.on,
   preview.with_reasoning, preview.no_source) after the merge.

.. v030-doc-verify: MOD-6
   Preview becomes a separate module with the build tag ``no_preview``. The
   behaviour described here is the same before and after the move. Confirm that
   ``--without preview`` and the module-absent behaviour match the text in
   :doc:`modules`.

Sources of preview text
-----------------------

A node sends a preview only when something supplies the text so far. There are
two sources.

The ``openai`` auto-reply backend
   When the task asks for previews, the daemon calls the model service in
   streaming mode, collects the text as it arrives, and hands it to the preview
   sender. The final reply is identical, byte for byte, to the reply of a
   non-streaming call. If a streaming call fails, the node does not repeat it as
   a non-streaming call, because that would be billed twice.

The control interface
   A program that the operator runs can push the text written so far to
   ``POST /tasks/preview`` on the local control interface. The request body is
   ``{"task_id": "...", "text": "...", "reasoning": "..."}``, where ``text`` is at
   most 1 MiB and ``reasoning`` is optional. The call needs the control token. A
   successful call returns ``{"sent": true}``, which means that the text was
   handed to the sender, not that it arrived.

``POST /tasks/preview`` answers with these refusals:

``preview_off`` (409)
   Previews are off on this node.

``task_not_open`` (409)
   The task has ended or is not a text task sent to this node.

``not_requested`` (200 with ``"sent": false``)
   The requester did not ask for a preview in this turn.

The following command pushes a text for the default identity. It reads the
control address from ``config.json`` and the token from ``control_token.txt`` in
the data directory.

.. code-block:: console

   $ DATA=~/.anet
   $ curl -sS -X POST "http://$(jq -r .control_addr $DATA/config.json)/tasks/preview" \
       -H "Authorization: Bearer $(cat $DATA/control_token.txt)" \
       -H "Content-Type: application/json" \
       -d '{"task_id": "<task-id>", "text": "The first part of the reply"}'
   {"sent":true}

The ``exec`` backend of auto-reply does not supply preview text.

.. v030-doc-verify: UX-9
   Confirm the response bodies of POST /tasks/preview after the merge of the
   route; the route is specified in the design and is not yet in the main
   checkout.

Asking for previews as a requester
==================================

A requester asks for previews for a single task.

Command line
   ``anet delegate <aid> "text" --watch`` sends the task, asks for previews and
   continues with ``anet task watch``. ``anet task watch <task-id>`` follows a
   task that already exists and prints previews, delivery state changes and the
   final result. The option ``--reasoning`` prints the reasoning text when the
   provider sends it.

Local A2A interface
   A client that streams a message with ``SendStreamingMessage`` or follows a
   task with ``SubscribeToTask``, and that has activated the extension
   ``https://agentnetwork.org.cn/a2a/ext/anet-preview/v1``, receives each
   preview as an update of the artifact ``anet.reply``. The update replaces the
   previous text of the artifact and is marked with ``anet.preview: true`` in its
   metadata. When the preview omits the beginning of the text, it starts with an
   ellipsis. A client that has not activated the extension receives no preview
   event.

MCP
   MCP does not expose previews. ``wait_task`` returns the task when its state
   changes.

.. code-block:: console

   $ anet delegate <provider-aid> "Explain the difference between a hub and a node." --watch
   delivery: sent, waiting for the other node to confirm.
   [delivered] Delivered: the other node confirmed it.
   [working]
   preview (14:05:20, 0 bytes omitted):
     A hub carries encrypted messages between nodes. ...
   [completed]
   A hub carries encrypted messages between nodes. ...

``anet card <aid>`` shows a line when the provider declares previews.

.. v030-doc-verify: UX-10
   The commands ``anet delegate --watch`` and ``anet task watch`` (with
   ``--reasoning``), the ``delivery`` lines they print, and the preview updates of
   the local A2A interface depend on the events route of the control interface.
   They are specified in the design and are not yet in the main checkout.
   Confirm the output format after the merge.

Limits
======

The values are fixed in the code and have no setting.

.. list-table::
   :header-rows: 1
   :widths: 60 40

   * - Limit
     - Value
   * - Interval between two previews of one task
     - at least 5 seconds
   * - Delay of the last change
     - sent within 5 seconds
   * - Previews sent by one node
     - at most 20 per second; the rest are dropped and counted
   * - Text of a preview
     - the last 8 KiB, cut at a character boundary
   * - Lifetime of a preview in transit
     - 60 seconds
   * - Previews accepted for one task
     - one per 2 seconds
   * - Previews accepted from one peer
     - 10 per second
   * - Tasks whose latest preview is kept by a requester
     - 1024

A preview is sent only while the task is open and while the provider has not
yet sent an answer for the current turn. When the real answer or result is
queued, the node cancels the previews that are still waiting, and no preview of
that turn is sent afterwards. A requester accepts a preview only if it is for a
task that it sent, is newer than the previews it has seen, and answers the
message that asked for it. A preview that fails these checks is discarded
without a reply.

Properties
==========

* A preview is sent as an ordinary encrypted message through the hub, signed by
  the provider. The hub cannot read it. The message carries a short lifetime of
  60 seconds, and a hub discards it when the lifetime ends.
* A preview is held in memory only. It is not written to the task database, the
  outbound queue or the evidence chain, and its text is not written to a log.
* A requester keeps only the latest preview of each task. A new message or the
  result of the task, and the end of the task, delete it.
* A restart of the provider forgets the turn that was being previewed. That turn
  sends no further previews and its result is delivered as usual. The next turn
  can be previewed again.
* ``GetTask``, the task history and every MCP tool never contain a preview. The
  reasoning text appears only on the local control interface and is never in the
  local A2A projection.

Notes
=====

* A preview can differ from the final reply. A model can write a text and then
  replace it. Any program that displays previews must label them as previews,
  and must not act on them.
* Reasoning previews can reveal the instructions and internal data of the
  provider. They are off by default and need a separate confirmation.
* A peer that holds an earlier key of the provider, and that has not yet learned
  of a key rotation, can forge a preview for a task in progress. Previews are
  verified against the identity record stored for the peer and do not require a
  fresh key confirmation. A forged preview is never stored and is replaced by the
  result.
* A provider whose node has previews on and no ``openai`` backend sends previews
  only through ``POST /tasks/preview``. ``anet doctor`` reports this as
  information.

See also
========

* :doc:`messaging` for the delivery confirmation lines that ``anet task watch``
  prints.
* :doc:`delegation` for auto-reply.
* :doc:`modules` for the ``preview`` module.
