.. _intro-install:

============
Installation
============

ANet is installed as a single static binary named ``anet``. The installer
downloads the binary for the platform, checks it against a signed release
manifest, and writes it to a directory on the search path. No compiler,
language runtime or superuser privilege is required for a per-user
installation.

.. contents:: On this page
   :local:
   :depth: 2

.. _intro-install-requirements:

Requirements
============

Supported platforms
   Linux and macOS, on amd64 and arm64. Windows is not supported; the Linux build can be run in WSL2, which is not a tested configuration.

Tools used by the installer
   ``curl``, ``gzip`` (``gunzip``), ``sha256sum`` or ``shasum``, and
   ``ssh-keygen`` from OpenSSH 8.1 or later. The signature check uses
   ``ssh-keygen``. If it is missing, the installer stops and names the
   package to install; no option skips the signature check.

Network
   Outbound HTTPS to the download host. A node needs no inbound connection
   and no public address.

.. _intro-install-script:

Installing with the script
==========================

Run the installer for the current user:

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh

The binary is written to ``~/.local/bin/anet``. If that directory is not on
the search path, add it in the shell profile:

.. code-block:: console

   $ export PATH="$HOME/.local/bin:$PATH"

Before anything is written outside a temporary directory, the installer
performs these checks. A failure of any check ends the installation and
leaves the installed version unchanged.

1. ``release.json`` carries a valid signature by the ANet release key.
2. The manifest has not expired.
3. The manifest does not name a version older than the ``anet`` already
   installed at the destination.
4. The SHA-256 digest of the downloaded archive, and of the binary inside
   it, equals the digest in the manifest.
5. The new binary reports the version and the module set that the manifest
   names for its variant. The default variant must not contain the
   ``shell`` module.

The manifest and its signature are stored next to the binary, so that
``anet doctor`` can repeat the check later.

When the script finishes it runs ``anet init``, which writes the safe
default configuration (see :ref:`intro-install-init`), and prints the
result of ``anet doctor``.

.. _intro-install-options:

Installer options
-----------------

Options follow ``sh -s --`` when the script is read from a pipe.

.. list-table::
   :header-rows: 1
   :widths: 26 74

   * - Option
     - Effect
   * - ``--user``
     - Install to ``~/.local/bin``. This is the default.
   * - ``--system``
     - Install to ``/usr/local/bin``. ``sudo`` is used when required.
   * - ``--prefix DIR``
     - Install to ``DIR``. Overrides ``--user`` and ``--system``.
   * - ``--base URL``
     - Download from ``URL`` instead of the default hosts. Only ``https://``
       is accepted. The environment variable ``ANET_INSTALL_BASE`` has the
       same effect. Every host is checked in the same way.
   * - ``--shell``
     - Install the variant that can run operator-approved commands. This is
       a different binary, not a setting; see :ref:`intro-install-modules`.
   * - ``--agents[=LIST]``
     - After installation, run ``anet agents wire`` for the coding agents
       found on the machine, or only for the comma-separated ``LIST``
       (``claude``, ``codex``, ``cursor``, ``opencode``, ``hermes``, ``workbuddy``).
   * - ``--hub URL``
     - Start the node and register it with the hub at ``URL`` as the last
       step.
   * - ``--name NAME``
     - The name to register under. The default is the host name.
   * - ``--without MODULES``
     - Switch off the listed modules in the configuration. See
       :ref:`intro-install-modules`.
   * - ``--help``
     - Print the option summary.

The options ``--hub`` and ``--name`` may be combined with ``--agents`` to
install, register and connect the local coding agents in one command:

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- \
       --hub https://hub.example.com --name build-host --agents

The registration output is printed as the last lines. On a hub that admits
by AID, the last line is the ``anet-hub admit`` command that the hub
operator runs; registration then completes without further input. See
:doc:`tutorial` for the full sequence.

Hubs no longer use invitation codes. The installer rejects the former
options ``--token`` and ``--token-file`` and says so.

.. _intro-install-verify:

Verifying the installer before running it
-----------------------------------------

``curl ... | sh`` trusts the host that serves the script and that host's
TLS certificate. The signature checks above protect the binary, not the
script itself. To avoid trusting the host, download the script and its
signature, and verify both with the published release key before running
the script.

The release key belongs to the project maintainers. It signs binaries,
the installer and the manifest of release artifacts. It is not an identity
key of any node. The key and its fingerprint are also published in
``SECURITY.md`` and ``README.md`` of the ANet repository; compare them with
the values below, and use the repository copy if they differ.

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh
   $ curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh.sig
   $ echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL' > allowed_signers
   $ ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn \
       -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh && sh install.sh

The key fingerprint is ``SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4``.
A successful check prints ``Good "anet-release@agentnetwork.org.cn"
signature`` followed by the fingerprint.

.. _intro-install-check:

Checking the installation
=========================

``anet version`` prints the version and the modules that the binary
contains. The list is read from the module registry of the running binary,
not from a string written at build time.

.. code-block:: console

   $ anet version
   anet 0.3.0 (commit 1a2b3c4, built 2026-10-17T00:00:00Z)
   modules: a2a,anetlink,autoreply,blackboard,cas,console,mcp,org,p2p,preview,service,x402
   variant: default

.. _intro-install-init:

Initial configuration
=====================

``anet init`` writes the safe defaults explicitly into ``config.json`` in
the data directory, so that each value is visible and changeable instead
of existing only as a built-in default. The command is idempotent. It adds
keys that are missing and never changes a value that exists. Settings that
differ from a fresh installation are listed in its report and left for the
operator to decide.

.. code-block:: console

   $ anet init
   anet init — /home/user/.anet
     created /home/user/.anet/config.json with the safe defaults written out
     + config.json                        control_addr 127.0.0.1:39811
     + peers.allow                        empty file /home/user/.anet/peers.allow
     + peers.deny                         empty file /home/user/.anet/peers.deny
     + peers.trust                        empty file /home/user/.anet/peers.trust
     + payees.allow                       empty file /home/user/.anet/payees.allow

   Next: anet doctor

The written values are:

.. list-table::
   :header-rows: 1
   :widths: 36 18 46

   * - Key
     - Value
     - Meaning
   * - ``inbound.policy``
     - ``closed``
     - Only peers on the allow or trust list can send tasks.
   * - ``inbound.public_capabilities``
     - ``[]``
     - No capability is open to callers that are not on a list.
   * - ``payments.auto_max``
     - ``0``
     - No payment is made without confirmation on a terminal.
   * - ``payments.agent_max``, ``payments.agent_daily_max``
     - ``0``
     - Payments requested through MCP or the local A2A interface are not
       made.
   * - ``payments.explicit_max``, ``payments.daily_max``
     - ``10``, ``50``
     - Limits for a payment confirmed on a terminal: per payment and per
       UTC day.
   * - ``no_response_after``
     - ``"15m"``
     - A task that hears nothing from its peer for this period after
       delivery fails.
   * - ``content_retention``
     - ``"30d"``
     - Task content is deleted this long after the task ends.

``peers.allow``, ``peers.trust`` and ``peers.deny`` are the peer lists, one
AID per line. ``payees.allow`` is the payee list. Automatic payment goes
only to AIDs on it, and the file is empty at first.

``anet doctor`` reads the data directory, without needing the daemon, and
reports which of these values hold. Each deviation from the defaults is
listed separately.

.. _intro-install-run:

Starting the node
=================

.. code-block:: console

   $ anet up
   $ anet status
   $ anet doctor

``anet up`` starts the daemon in the background. It continues to run after
the terminal closes. ``anet status`` prints the AID, the data directory,
the address of the local console and the hub state. Until the node is
registered with a hub, the hub state is ``none`` and the status carries
the command to run next:

.. code-block:: console

   $ anet hub-register https://hub.example.com --name my-node

The registration and the first exchange are described in :doc:`tutorial`.
For the lifecycle commands, see :ref:`reference-cli`.

The data directory is ``~/.anet``. Set ``ANET_DATA_DIR`` to use another
location. Further identities, created with ``anet id new NAME``, are kept
under ``~/.anet/ids/NAME``. The data directory contains the identity
(``identity.kel``), the configuration, the peer lists, the evidence chain
and the control token. The directory must not be shared or copied to
another machine while the original keeps running. The sections on
:ref:`unprotected identities <topics-identity-unprotected>` and on
:ref:`recovery <topics-identity-protect>` describe how to protect it.

.. _intro-install-agents:

Connecting coding agents
========================

``anet agents wire`` registers the MCP server of the node with the coding
agents installed on the machine and adds usage instructions to them. Every
file the command edits is backed up first.

.. code-block:: console

   $ anet agents wire --all

A single tool can be named instead of ``--all``: ``claude``, ``codex``,
``cursor``, ``opencode``, ``hermes`` or ``workbuddy``. ``anet agents`` lists which of them
are installed and wired, and ``anet agents unwire`` reverses the change. The
agent must be restarted after wiring. See :doc:`/howto/coding-agents`; the
guide for each tool, with verification steps, starts at :doc:`/agents/index`.

.. _intro-install-modules:

Selecting modules
=================

The node is built from modules. The prebuilt release contains two
variants.

``default``
   Every module except ``shell``. Modules that act for other nodes or spend
   credit are disabled at run time until the operator configures them.

``shell``
   The default variant plus the ``shell`` module, which can run commands
   approved by the operator. Even this variant runs nothing until the
   configuration names the commands and lists the AIDs that may call them;
   an empty list refuses every caller. The variant is selected with
   ``--shell`` at installation and is retained by ``anet update``.

The module names are used for the installer option, for ``anet init`` and
in the configuration.

.. list-table::
   :header-rows: 1
   :widths: 18 82

   * - Module
     - Purpose
   * - ``a2a``
     - The loopback A2A interface for A2A clients.
   * - ``anetlink``
     - Client for device capabilities provided by ANetLink.
   * - ``autoreply``
     - Answering tasks with a local agent, including the sandbox, the
       credential injection route, the egress allow list and the budgets.
   * - ``blackboard``
     - Shared blackboard capabilities.
   * - ``cas``
     - Content-addressed storage capabilities.
   * - ``console``
     - The local web console.
   * - ``mcp``
     - The MCP server and the coding agent wiring.
   * - ``org``
     - Verification of organisation membership credentials.
   * - ``p2p``
     - Direct delivery between nodes.
   * - ``preview``
     - Streaming previews of a result in progress.
   * - ``service``
     - Local services published as capabilities.
   * - ``shell``
     - Operator-approved commands (variant ``shell`` only).
   * - ``x402``
     - Payments: quotes, authorizations, settlement and receipts.

A module that is switched off behaves like a module that is not present:
the peer sees the same response as for a disabled feature, and every local
entry point answers with the reason code ``module_absent``. A switched-off
module is never replaced by a weaker code path.

``--without`` turns modules off during installation. The installer runs
``anet init --without`` with the freshly verified binary before it
replaces the installed one, so a failure leaves the system unchanged.

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- \
       --without autoreply,preview

The same switch is available on an installed node, with the daemon stopped.
``--without`` adds names to the top-level ``without`` list in
``config.json`` and never removes one. It is refused with the reason code
``without_configured`` when the configuration already contains the key of
the module; the key has to be removed first. ``--with`` removes one name.

.. code-block:: console

   $ anet stop
   $ anet init --without preview
   $ anet up
   $ anet stop
   $ anet init --with preview
   $ anet up

The list can be changed only while the daemon is stopped and only on the
local machine. No control-plane, MCP or A2A call can change it.

``--without`` can name every module of the table above, including ``autoreply``,
``preview`` and ``console``. A module that is switched off is still part of the
binary. To leave the code out of the binary, build from the source (see below);
:ref:`topics-modules-recipes` lists the combinations that the repository tests.

The reference for the module behaviour is :doc:`/topics/modules`.

.. _intro-install-source:

Building from source
====================

The source tree builds with a current Go toolchain and needs neither CGO
nor a C compiler.

.. code-block:: console

   $ git clone https://github.com/ANetResearch/ANet.git
   $ cd ANet
   $ ./build.sh

``./build.sh`` writes the binary to ``./anet``. ``./build.sh --check``
first runs formatting, ``go vet``, the tests and the build-tag checks. A
module that is included by default can be left out of the binary with
``--without``, which sets the build tag ``no_<module>``, and the ``shell``
module is added with ``--with shell``:

.. code-block:: console

   $ ./build.sh --without x402,p2p
   $ ./build.sh --with shell

The names are those of the ``modules`` line of ``anet version``. The module
``console`` has no build tag in 0.3.0; ``./build.sh --without console`` stops
with ``build_tag_pending`` and builds nothing, and the console is turned off
with ``anet init --without console`` instead. ``bash scripts/build.sh`` stamps
the commit into the binary and takes the tags in the environment variable
``TAGS``.

A binary built from source carries no release signature. ``anet doctor``
reports the signature state as unknown, and ``anet update`` replaces it
with a signed release build.

.. _intro-install-update:

Updating
========

``anet update`` replaces the installed binary in place. It applies the same
checks as the installer, using the release key that is compiled into the
running binary, so it does not depend on the host that served the installer.
The new binary is written to a temporary file in the same directory and
renamed over the old one; if any step fails, the old binary is untouched.

.. code-block:: console

   $ anet update --check
   $ anet update

``--check`` reports whether a newer release exists and changes nothing.
An update never moves to an older version, and it keeps the variant: a
binary that cannot run commands is never replaced by one that can. A daemon
that is already running keeps the old binary until it is restarted:

.. code-block:: console

   $ anet stop --all && anet up --all

``--base URL`` selects another download host, with the same checks. Re-running
the installer is not required on a machine that already has ``anet``.

.. _intro-install-remove:

Removing ANet
=============

Stop the daemons and delete the binary:

.. code-block:: console

   $ anet stop --all
   $ rm "$(command -v anet)"

A hub that is the witness of an identity cannot be left with
``anet hub-leave``, because without it nobody can confirm the key of the
identity. To move an identity to another hub, run ``anet hub-register`` with
the address of the new hub.

The data directory holds the only copy of the identity keys. Deleting it
ends the identity permanently, and the AID cannot be recovered without the
recovery file and its passphrase. Delete ``~/.anet`` only when the identity
is no longer needed:

.. code-block:: console

   $ rm -r ~/.anet

.. _intro-install-problems:

Installation problems
=====================

Every message of the installer ends with the statement ``Nothing was
installed or changed.`` when the installation was not completed.

``ssh-keygen is required``
   Install the OpenSSH client tools: ``openssh-client`` on Debian and
   Ubuntu, ``openssh`` on Fedora and RHEL, ``openssh-keygen`` on Alpine.

``is not signed by the anet release key``
   Either the download host serves a file that the release key did not
   sign, or the installed ``ssh-keygen`` is older than OpenSSH 8.1. Try
   another host with ``--base URL``; every host is checked in the same way.

``Refusing to downgrade``
   The manifest names a version older than the installed one, which
   usually indicates a stale mirror. Use ``--base URL`` to select another
   host.

For failures after installation, see :doc:`/howto/troubleshooting`.
