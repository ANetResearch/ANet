.. _topics-modules:

=======
Modules
=======

ANet is built from a small kernel and a set of optional modules. This page lists
the modules, shows which ones a node includes, and describes how to turn a module
off, how to build a node without one, and what a node does when a module is
absent.

.. contents::
   :local:
   :depth: 2

Overview
========

The *kernel* holds what every node needs: the protocol, identity, encryption,
delivery confirmation, storage and admission. The kernel cannot be removed.
Everything else is a *module*: a feature that a node can have or lack without
losing the ability to exchange tasks.

The design follows three rules.

* A module reaches the kernel only through a fixed interface, and the kernel
  grants it only what it needs. A module does not import the daemon, and the
  kernel does not import a module. Tests in the repository enforce both
  directions.
* A feature that acts for a network peer or spends money is off at run time
  until the operator turns it on. Including a module in a build does not turn
  it on.
* A module that is absent fails closed. It is not started, it registers nothing,
  and it accepts nothing from the network. To a peer, the node behaves as if the
  feature were switched off.

The modules
===========

.. list-table::
   :header-rows: 1
   :widths: 14 40 12 34

   * - Module
     - Function
     - Build tag
     - Configuration
   * - ``a2a``
     - The local A2A interface: JSON-RPC and HTTP+JSON on a loopback port,
       protected by a token, with a proxy card for each remote agent.
     - ``no_a2a``
     - ``modules.a2a``
   * - ``anetlink``
     - A client for device capabilities served by ANetLink.
     - ``no_anetlink``
     - ``modules.anetlink``
   * - ``autoreply``
     - Auto-reply: the loop, the ``openai`` and ``exec`` backends, the agent
       registry, the sandbox, the network controls and the run budget. See
       :doc:`delegation` and :doc:`sandbox`.
     - ``no_autoreply``
     - top-level ``auto_reply``
   * - ``blackboard``
     - A shared blackboard for cooperating agents.
     - ``no_blackboard``
     - ``modules.blackboard``
   * - ``cas``
     - A content-addressed store: ``cas.put``, ``cas.get``, ``cas.has`` and
       ``cas.stat``.
     - ``no_cas``
     - ``modules.cas``
   * - ``console``
     - The local web console, opened with ``anet console``. It is the only local
       entry that does not use the bearer token.
     - none (planned for a 0.3 patch release)
     - none
   * - ``mcp``
     - The MCP server for coding agents, ``anet mcp``, and the commands that wire
       it into coding agents, ``anet agents``. See :doc:`/howto/mcp`.
     - ``no_mcp``
     - none
   * - ``org``
     - Verification of organisation membership credentials.
     - ``no_org``
     - ``modules.org``
   * - ``p2p``
     - Direct delivery between nodes, as an addition to the hub.
     - ``no_p2p``
     - ``modules.p2p``
   * - ``preview``
     - Preview streaming. See :doc:`preview`.
     - ``no_preview``
     - top-level ``stream_preview``
   * - ``service``
     - Capabilities that are served by programs on the local machine. See
       :doc:`capabilities`.
     - ``no_service``
     - ``modules.service``
   * - ``shell``
     - Commands that the operator has approved, run on the local machine. See
       :doc:`shell`.
     - ``shell`` (additive)
     - ``modules.shell``
   * - ``x402``
     - Payment: signing, settlement, receipts, quotes, accounting, the audit of
       the issuance chain of the hub and the balance proofs. See :doc:`payments`.
     - ``no_x402``
     - ``modules.x402``

The name of a module is the same wherever it is used: in the build tag, in the
``modules`` line of ``anet version``, in the ``without`` setting, in the
installation option and under ``modules`` in ``config.json``. Two modules have
older top-level configuration keys, ``auto_reply`` and ``stream_preview``.
These keys are fixed, and the form ``modules.autoreply`` or ``modules.preview``
is rejected.

A build tag that starts with ``no_`` removes a module from the binary. The tag
``shell`` is additive: the module is included only when the tag is given.

The console has no build tag in 0.3.0. A build with ``scripts/build.sh --without
console`` is refused with ``build_tag_pending``, and the console is turned off with
``without`` in the configuration. A tag for it follows in a 0.3 patch release.
The names of the tags that exist are printed by ``scripts/tagcheck.sh list`` in the
source tree.


What a default node includes
============================

The released builds are the *default* build and the *shell* build. The default
build includes every module except ``shell``. The shell build, selected at
installation with ``--shell``, adds ``shell``. There are no further prebuilt
variants: a node that needs a different set of modules turns modules off with
``without`` (see below), or is built from the source with the tags of the
section :ref:`topics-modules-build`. The section :ref:`topics-modules-recipes`
gives the combinations that the repository tests.

.. code-block:: console

   $ anet version
   anet 0.3.0 (commit 0000000, built 2026-10-01T00:00:00Z)
   modules: a2a,anetlink,autoreply,blackboard,cas,console,mcp,org,p2p,preview,service,x402
   variant: default

The ``modules`` line lists the modules that the binary contains, in alphabetical
order, and ``(none)`` for a binary that has only the kernel. The ``variant`` line
names the released build (``default`` or ``shell``); a binary built locally shows
``none``. ``/status`` carries the same information in the member ``build``, with
the lists ``modules`` and ``without``.

After ``anet init`` and ``anet up``, a default node does the following without
further configuration:

* It runs the control interface on a local socket, protected by a token.
* It runs the local A2A interface on a loopback port, protected by a token.
* It serves the local web console through the control interface. A browser
  session needs a single-use ticket from ``anet console``.
* It can read balances and quotes. Its payment limits are 0 for the automatic
  and agent tiers, and no payee is listed, so it pays nothing without a person.
* It requests previews for a task only when a local client asks for them.

Every other module does nothing until it is configured.

Turning a module off
====================

A module can be turned off in the configuration. A module that is turned off is
still in the binary, but it is never started.

The ``without`` setting is a list of module names at the top level of
``config.json``:

.. code-block:: json

   {
     "without": ["p2p", "blackboard"]
   }

The file is changed only while the node is stopped. Two commands do this.

.. code-block:: console

   $ anet down
   $ anet init --without p2p,blackboard
   $ anet up

``anet init --without`` adds names and never removes them. It refuses to turn off
a module whose configuration block exists in ``config.json``; remove the block
first. ``anet init --with p2p`` removes one name from the list. Both commands
refuse to run while the daemon is running, and they change nothing in that case.

Every module of the table can be named, with two limits. The names ``autoreply``
and ``preview`` turn off the modules whose settings are the top-level keys
``auto_reply`` and ``stream_preview``; the forms ``modules.autoreply`` and
``modules.preview`` are refused. A name that is not the name of a module is
refused with ``config_invalid``, and the shell module is turned off by not
installing it.

There is no way to change ``without`` through the control interface, MCP or the
local A2A interface. A peer on the network, and a program that holds only the
control token, cannot turn a module back on.

At installation, the option ``--without`` of the installation script does the
same before the first start:

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- --without p2p,blackboard

If any name is not valid for the selected build, nothing is installed. The
script has no ``--with`` option. To turn a module back on, use ``anet init
--with``.

.. _topics-modules-build:

Building without a module
=========================

A node can be built without the code of a module. This is a stronger statement
than turning the module off: the symbols of the module are not in the binary. The
repository checks this by counting symbols in both directions.

.. code-block:: console

   $ git clone https://github.com/ANetResearch/ANet
   $ cd ANet
   $ go build -tags no_p2p,no_blackboard -o anet ./cmd/anet
   $ ./anet version

``build.sh`` does the same from the names of the modules, and also builds with
the additive module:

.. code-block:: console

   $ ./build.sh --without autoreply,x402
   $ ./build.sh --with shell

Combine tags with commas. ``scripts/tagcheck.sh list`` prints the tags of the
source tree, and ``scripts/tagcheck.sh all`` checks them in both directions by
counting symbols. A build that contains a configuration block for a module it
lacks does not start.

The module ``shell`` is the one additive module. The default build does not
contain it, because it is the only module that turns a network request into a
command on the host, and the operator must choose it on purpose:

.. code-block:: console

   $ go build -tags shell -o anet ./cmd/anet

Leaving modules out makes the binary smaller (see :ref:`topics-modules-recipes`),
but the reason to do it is the reduction of what the node can be asked to do, not
the size.

The effect of absence
=====================

A module is *available* when it is in the binary and is not in ``without``. A
module that is not available behaves as follows.

At start
   A ``config.json`` that contains the configuration of an unavailable module
   prevents the start, with the sentence ``module_absent.configured``. The
   message names the key, states the reason and gives the remedy. A node never
   starts with a module that is configured and not running.

On the local interfaces
   Each entry point of the module remains in place and answers with the code
   ``module_absent``. The control interface answers with status 409 and the local
   A2A interface with ``UnsupportedOperationError``. A command-line command that
   belongs to the module exits with status 2 without contacting the daemon. The
   sentence names the reason (the binary does not contain the module, or the node
   turned it off) and the remedy.

To a peer
   The node behaves as if the feature were off. A node without auto-reply leaves
   tasks in its inbox. A node without preview never declares previews and drops
   the frames it receives. A node without payment answers a call to a priced
   capability with ``payments_unavailable`` and does not run the work. A node
   without ``p2p`` publishes no direct address.

In the kernel
   Rules that protect data stay in force in every build. Working directories of
   past agent runs are deleted on time, the list of paths that a sandbox never
   mounts is kept, and the facts about prices are kept.

No fallback
   A feature does not fall back to a weaker path. A turn that needs the sandbox is
   not run directly, and a preview is not sent through another channel.

Examples of the sentences:

.. code-block:: console

   $ anet mcp
   This anet does not include the MCP module (mcp), so nothing was done; install the default build (install.sh).

   $ anet autoreply show
   This node has turned off the auto-reply module (autoreply; without in config.json), so nothing was done; run anet down, anet init --with autoreply, anet up.

What each module does when it is absent
---------------------------------------

.. list-table::
   :header-rows: 1
   :widths: 16 84

   * - Module
     - Effect of absence
   * - ``a2a``
     - No loopback listener for the local A2A interface and no proxy cards.
       The commands and the MCP tools are not affected.
   * - ``anetlink``
     - Device capabilities of ANetLink are not offered or called.
   * - ``autoreply``
     - There is no auto-reply loop and no local program is started for a task.
       Tasks stay in the inbox. ``POST /autoreply`` and ``POST /autoreply-test``
       and the command ``anet autoreply`` answer ``module_absent`` (exit status
       2). ``/status`` has no member ``auto_reply``. The doctor item
       ``auto_reply`` is informational and there are no ``sandbox`` items.
       ``anet peers trust`` asks with the sentence ``trust_confirm.no_agent``,
       which states that trust decides only whether local backends take the
       tasks. The console hides the hint for the auto-reply. An ``auto_reply``
       block in ``config.json`` stops the start (``module_absent.configured``).
   * - ``blackboard``, ``cas``, ``org``
     - The capabilities of the module are not offered.
   * - ``console``
     - ``anet console`` and the routes of the console answer ``module_absent``.
   * - ``mcp``
     - ``anet mcp`` and ``anet agents`` answer ``module_absent`` (exit status 2),
       whether the module is not compiled or turned off.
   * - ``p2p``
     - No direct address is published and no direct connection is accepted.
   * - ``preview``
     - The node does not declare the extension ``anet-preview/v1`` and does not
       ask for previews. Preview frames that arrive are dropped and counted as
       ``preview-unsupported``. ``anet preview`` and ``POST /tasks/preview``
       answer ``module_absent``, and ``/status`` has no member ``preview``.
       ``anet delegate --watch`` prints one line on standard error and then waits
       for the final answer. A ``stream_preview`` other than ``off`` stops the
       start (``module_absent.configured``).
   * - ``service``
     - Capabilities that are served by programs on the local machine are not
       offered. A capability that declares files is reported by the doctor item
       ``capabilities.files``.
   * - ``shell``
     - No remote command can be run.
   * - ``x402``
     - ``anet pay``, ``anet redeem``, ``anet reconcile``, ``anet audit-hub`` and
       the routes behind them answer ``module_absent``. ``anet balance`` reports
       the balance as ``module_absent``. A quote that is received waits in
       ``input-required`` with the sentence ``module_absent.payment`` and the
       reason ``anet.reason = module_absent`` (a sentence variant never changes
       the reason). The
       proxy card declares ``a2a-x402`` only while the node can pay. The doctor
       item ``payments.unchargeable`` reports a priced capability that the node
       could not charge for.

Examples of the sentences:

.. code-block:: console

   $ anet mcp
   This anet does not include the MCP module (mcp), so nothing was done; install the default build (install.sh).

   $ anet autoreply show
   This node has turned off the auto-reply module (autoreply; without in config.json), so nothing was done; run anet down, anet init --with autoreply, anet up.

The sentences that concern modules are ``module_absent``, ``module_absent.configured``,
``module_absent.payment``, ``without_configured``, ``daemon_running``,
``build_tag_pending``, ``update_variant_unknown`` and
``update_variant_unknown.mismatch``. They are listed in :doc:`/reference/reason-codes`.

.. _topics-modules-recipes:

Combinations of modules
=======================

The repository tests combinations of build tags, and ``scripts/tagcheck.sh
recipes`` prints the ones below. Each combination is a *recipe*: a set of tags
for a purpose. The released builds are ``default`` and ``shell``; the recipes
are for those who build a node themselves, or who turn the same modules off
with ``without``.

.. list-table::
   :header-rows: 1
   :widths: 12 48 40

   * - Recipe
     - Purpose
     - Modules in the binary
   * - ``min``
     - Take part: register with a hub, find agents, delegate tasks, verify
       receipts, review. It offers no capability, cannot be paid and has no
       local A2A listener.
     - kernel only
   * - ``standard``
     - ``min`` with the local A2A interface and local services published as
       capabilities. A priced capability is answered with ``UNAVAILABLE`` and
       the node does not work for free.
     - ``a2a``, ``service``
   * - ``paid``
     - ``standard`` with payment: prices, quotes, settlement, the audit of the
       issuance chain and reconciliation.
     - ``a2a``, ``service``, ``x402``
   * - ``agent``
     - For coding agents: ``standard`` with the MCP server.
     - ``a2a``, ``mcp``, ``service``
   * - ``p2p``
     - ``standard`` with direct delivery between nodes. A hub is still needed
       for discovery and key histories.
     - ``a2a``, ``p2p``, ``service``
   * - ``full``
     - Every module except ``shell``.
     - all

The recipes ``min``, ``standard``, ``paid``, ``agent`` and ``p2p`` leave out
``autoreply`` and ``preview``; both are in ``full`` only.

The ``console`` module has no tag. Every recipe therefore contains the console;
it is the only entry that does not use the bearer token, and it can be turned
off with ``without``.

The modules in a build determine what the node can be asked to do. Leaving out every optional module saves 19 per cent of the binary. The module ``mcp`` is the
largest part of that saving, because it links the MCP library; ``autoreply`` and
``a2a`` follow. The sizes below are those of a Linux amd64 build of the
release candidate with ``-trimpath -ldflags "-s -w"``.

Single tag, relative to the default build:

.. list-table::
   :header-rows: 1
   :widths: 30 35 35

   * - Tag
     - Size saved (KiB)
     - Share of the total saving
   * - ``no_mcp``
     - 1,996
     - 48 %
   * - ``no_autoreply``
     - 692
     - 17 %
   * - ``no_a2a``
     - 684
     - 17 %
   * - ``no_x402``
     - 220
     - 5 %
   * - ``no_p2p``
     - 72
     - 2 %
   * - ``no_preview``
     - 56
     - 1 %
   * - ``no_service``
     - 60
     - 1 %
   * - ``no_cas``
     - 56
     - 1 %
   * - ``no_blackboard``
     - 40
     - 1 %
   * - ``no_org``
     - 40
     - 1 %
   * - ``no_anetlink``
     - 48
     - 1 %

Recipes and the additive tag:

.. list-table::
   :header-rows: 1
   :widths: 30 35 35

   * - Build
     - Size (KiB)
     - Packages linked
   * - ``full`` (default build)
     - 21,224
     - 358
   * - ``agent``
     - 20,024
     - 348
   * - ``p2p``
     - 18,096
     - 319
   * - ``paid``
     - 18,236
     - 320
   * - ``standard``
     - 18,024
     - 318
   * - ``min``
     - 17,108
     - 289
   * - ``shell`` added to the default build
     - 21,260
     - 359

The recipes ``paid``, ``p2p`` and ``full`` contain modules that open listeners or
spend credit only after the operator configures them. The module ``shell`` is
the only module that turns a network request into a command on the host. It is
additive, it is not part of any recipe, and it is not combined with ``full`` in
the released builds.

Updating
========

``anet update`` selects the build by the name of the variant that is written into
the binary. A node installed as the default build is updated to the default build
and a node installed with ``--shell`` to the ``shell`` build. A binary built
locally has no variant, and ``anet update`` does not replace it. The setting
``without`` is part of the configuration and stays in force after an update.

Notes
=====

* A module that is turned off with ``without`` is not a security boundary against
  other code that runs as the same user. Such code can edit ``config.json`` and
  replace the binary. The setting protects against network peers and against
  callers that hold only the control token.
* An additive module such as ``shell`` needs a different binary, and a
  subtractive module needs only a different configuration. The installation
  option of the module is chosen once, and the features are turned on by their
  commands: ``anet autoreply set``, ``anet preview on`` and ``anet payments set``.
* Do not edit ``without`` by hand while the node is running. The daemon reads the
  list at start, and a change takes effect at the next start.

See also
========

* :doc:`/intro/install` for installation options.
* :ref:`reference-cli` for the commands ``anet init``, ``anet version`` and
  ``anet update``.
* :ref:`reference-config` for the keys of ``config.json``.
