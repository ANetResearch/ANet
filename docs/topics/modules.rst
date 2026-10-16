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
     - none yet
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
     - The local web console, opened with ``anet console``.
     - none yet
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
     - none yet
     - top-level ``stream_preview``
   * - ``service``
     - Capabilities that are served by programs on the local machine. See
       :doc:`capabilities`.
     - ``no_service``
     - ``modules.service``
   * - ``shell``
     - Commands that the operator has approved, run on the local machine.
     - ``shell`` (additive)
     - ``modules.shell``
   * - ``x402``
     - Payment: signing, settlement, receipts, quotes, accounting. See
       :doc:`payments`.
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

.. v030-doc-verify: MOD-4
   The tag no_autoreply and the move of auto-reply into its own package are part
   of the module work. The names of the tags that exist are given by
   ``scripts/tagcheck.sh list``. The table lists "none yet" for autoreply,
   preview and console. Change a row to its tag only when the list contains the
   tag.

.. v030-doc-verify: MOD-6
   The tag no_preview and the move of the preview code into its own package.
   See the note above.

.. v030-doc-verify: MOD-8
   The console becomes a module that can be turned off with ``without``. Its
   build tag follows in a later 0.3 release.

What a default node includes
============================

The default build of ANet includes every module except ``shell``. A second build,
selected at installation with ``--shell``, adds ``shell``. There are no further
prebuilt variants.

.. code-block:: console

   $ anet version
   anet 0.3.0 (commit 0000000, built 2026-10-01T00:00:00Z)
   variant: default
   modules: a2a,anetlink,autoreply,blackboard,cas,console,mcp,org,p2p,preview,service,x402

The ``modules`` line lists the modules that the binary contains, in alphabetical
order. The ``variant`` line is empty for a binary built locally.

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

.. v030-doc-verify: MOD-7a
   The ``variant:`` line of ``anet version`` and the modules line with
   ``autoreply``, ``console`` and ``preview`` are specified in the design. In the
   main checkout, ``anet version`` prints the modules line without those three
   names. Confirm the output of the release build.

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

.. v030-doc-verify: MOD-7a
   ``anet init --without`` and ``--with``, and ``install.sh --without``, are
   specified in the design. ``anet init`` in the main checkout accepts neither
   option, and ``install.sh`` accepts ``--system``, ``--user``, ``--prefix``,
   ``--base``, ``--shell``, ``--agents``, ``--hub`` and ``--name``.

.. v030-doc-verify: MOD-1
   The ``without`` key, the validation of its names, the ``module_absent`` code
   and the refusal of a name whose absent-path is not ready are specified in the
   framework package. The setting and the code are not yet in the main checkout.

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

Combine tags with commas. ``scripts/tagcheck.sh list`` prints the tags of the
source tree, and ``scripts/tagcheck.sh all`` checks them. A build that contains
a configuration block for a module it lacks does not start.

The module ``shell`` is the one additive module. The default build does not
contain it, because it is the only module that turns a network request into a
command on the host, and the operator must choose it on purpose:

.. code-block:: console

   $ go build -tags shell -o anet ./cmd/anet

The binary size changes little. In an earlier measurement of the default build,
removing the MCP server saved most of the space that the modules use, and the
other modules together saved a few percent. The reason to omit a module is the
reduction of what the node can be asked to do, not the size.

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

.. v030-doc-verify: MOD-1
   The exact sentences of ``module_absent`` follow the table of the design. Replace
   the examples with the output of the release build.

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
