.. _intro-install:

============
Installation
============

ANet is installed as a single static binary named ``anet`` (``anet.exe`` on
Windows). The installer downloads the binary for the platform, checks it
against a signed release manifest, and writes it to a directory on the search
path. No compiler, language runtime or superuser privilege is required for a
per-user installation. Linux and macOS use the shell script ``install.sh``;
Windows uses the PowerShell script ``install.ps1`` (see
:ref:`intro-install-windows`).

.. contents:: On this page
   :local:
   :depth: 2

.. _intro-install-requirements:

Requirements
============

Supported platforms
   Linux, macOS, and Windows 10 version 1809 or later or Windows Server 2019
   or later, each on amd64 and arm64. Windows runs the native Windows build;
   WSL2 is not required.

Tools used by the installer
   On Linux and macOS: ``curl``, ``gzip`` (``gunzip``), ``tar``,
   ``sha256sum`` or ``shasum``, and ``ssh-keygen`` from OpenSSH 8.1 or later.
   The signature check uses ``ssh-keygen``. If it is missing, the installer
   stops and names the package to install; no option skips the signature
   check. ``tar`` reads the npm packages of :ref:`intro-install-hosts`; without
   it, those sources are passed over. On Windows: Windows PowerShell 5.1,
   which Windows includes, and no tool to install.

Network
   Outbound HTTPS to the download sources (see :ref:`intro-install-hosts`)
   and, unless the node is not to join a hub, to the hub. A node needs no
   inbound connection and no public address.

.. _intro-install-script:

Installing with the script
==========================

On Linux and macOS, the installation is one command:

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh

Without options, the installer carries out these steps:

#. It selects a download source, downloads the release manifest, its
   signature and the archive of the build, and verifies them (see below).
#. It takes over an earlier release of AgentNetwork or ``anet`` on the
   machine (see :ref:`intro-install-earlier`).
#. It installs the binary as ``~/.local/bin/anet`` and makes the command
   available (see :ref:`intro-install-path`).
#. It runs ``anet init``, which writes the safe default configuration (see
   :ref:`intro-install-init`).
#. It starts the node with ``anet up`` and waits at most 30 seconds until the
   node answers. A node that was already running is restarted with the new
   binary.
#. It joins the public hub ``https://hub.agentnetwork.org.cn`` with
   ``anet hub-register <hub> --name <host name> --passphrase-later``. The
   command asks nothing; the recovery passphrase is set afterwards with
   ``anet identity protect`` (see :ref:`topics-identity-protect`). A node
   that has already joined a hub keeps that hub unless ``--hub`` names
   another one.

The installer never reads its input, so it runs unchanged in a terminal, in
the tool call of a coding agent and in a CI job. The only prompt it can cause
is the single password prompt of ``sudo`` described in
:ref:`intro-install-sudo`, and only in a terminal. The options
``--no-join`` and ``--no-start`` leave out the last steps (see
:ref:`intro-install-options`):

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- --no-join

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

.. _intro-install-output:

Output and log
--------------

The installer prints one short line for each phase: the download, the
takeover of an earlier release and the start of the node. A fresh
installation prints:

.. code-block:: text

   Downloaded anet 0.3.6 for darwin-arm64 from npmmirror (8.6 MB in 2 s)
   Starting the node and joining https://hub.agentnetwork.org.cn

   anet 0.3.6 is installed and running.
     AID   bafyrei...
     Hub   https://hub.agentnetwork.org.cn (joined)
   Run 'anet' to get started.  Details: ~/.anet/logs/install-20261020T151644Z.log

When standard error is a terminal, the first line reads ``Downloading anet
0.3.6 for darwin-arm64 from npmmirror`` and is followed by a progress line
with a bar, the megabytes received and expected, the speed and the remaining
time. The progress line is redrawn about five times a second and keeps its
last state, the total size, the average speed and the time taken, when the
download is complete. The bar is red; the variable ``NO_COLOR`` removes the
colour, and outside a UTF-8 locale the bar is drawn with ASCII characters.
Without a terminal, the single ``Downloaded ...`` line takes the place of
both.

The takeover of an earlier release adds a phase line and a line in the
summary:

.. code-block:: text

   Downloaded anet 0.3.6 for darwin-arm64 from GitHub (8.6 MB in 3 s)
   Replacing AgentNetwork 1.1.5
   Starting the node and joining https://hub.agentnetwork.org.cn

   anet 0.3.6 is installed and running.
     AID   bafyrei...
     Hub   https://hub.agentnetwork.org.cn (joined)
   Replaced AgentNetwork 1.1.5; its data is kept in ~/.anet-legacy-1.1.5-20261020T151644Z
   Note: /usr/local/bin/anet (AgentNetwork 1.1.5, owned by root) is no longer used; remove it with: sudo rm /usr/local/bin/anet
   Run 'anet' in a new terminal, or in this one after: export PATH="$HOME/.local/bin:$PATH"  Details: ~/.anet/logs/install-20261020T151644Z.log

The summary takes these other forms:

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Line
     - Meaning
   * - ``Hub   not joined (--no-join)``
     - The node runs and has not joined a hub.
   * - ``Hub   <hub> (awaiting admission; its operator runs: anet-hub admit <AID>)``
     - The hub admits nodes by AID. Send the ``anet-hub admit`` command to
       the operator of the hub; the node completes the join by itself once
       the operator has run it.
   * - ``Hub   <hub>``
     - The node had joined this hub before and keeps it.
   * - ``Hub   <hub> (registering; it completes on its own)``
     - The hub accepted the request but did not confirm it within the wait
       of the installer. The node retries by itself; ``anet status`` shows
       the result.
   * - ``Hub   <hub> (not joined)``
     - The join did not complete; a ``Note`` line names the command to repeat
       it.
   * - ``anet 0.3.6 is installed.`` and ``Run 'anet up' to get started.``
     - The node was not started (``--no-start``).
   * - ``Note: <text>``
     - A step that is left to the user, with the exact command, for example
       an earlier binary that only the superuser can remove.

The exit status of the installer is 0 when the binary is installed and, unless
``--no-start`` or ``--no-join`` leaves them out, the node runs and the join is
complete, pending admission or still registering. It is 3 when the binary is
installed but the node did not start or the join failed; the summary then
names the command to repeat the step. Any other failure ends the installation
with exit status 1 and leaves the installed version unchanged.

``anet`` without arguments prints a short overview: the version, the main
commands, the identity, the hub state with the next command, four common
commands, and ``anet help --all`` for the complete list.

Everything else is written to the log ``<data directory>/logs/install-<UTC
time>.log``: the download sources tried and how fast they answered, each
check, each step of a takeover with the section ``Upgraded from anet
<version>``, the shell profiles that were changed, and the output of
``anet init`` and of ``anet hub-register``. The data directory is
``ANET_DATA_DIR``, else ``ANET_HOME``, else ``~/.anet``. The directory
``logs`` has mode ``0700`` and the log mode ``0600``. The last line of the
output names the log.

When ``ANET_LANG`` begins with ``zh``, the lines are printed in Chinese.

A failure prints the reason and what to do, then whether anything was
changed, then the log:

.. code-block:: text

   Error: <reason and what to do>
   Nothing was installed or changed.
   Details: ~/.anet/logs/install-20261020T151644Z.log

When the failure occurs after an earlier release has been taken over, the
second line is ``The new binary was not installed. Already done:``, followed
by the steps that were carried out.

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
     - Install to ``/usr/local/bin``. ``sudo -n`` is used when required; the
       installer never asks for a password for this option. When ``sudo``
       needs one, the installer stops and says so; run ``sudo -v`` in the same
       terminal and repeat the command.
   * - ``--prefix DIR``
     - Install to ``DIR``. Overrides ``--user`` and ``--system``.
   * - ``--base URL``
     - Download from ``URL/dl/`` and from no other source (see
       :ref:`intro-install-hosts`). Only ``https://`` is accepted. The
       environment variable ``ANET_INSTALL_BASE`` has the same effect.
       The checks are the same for every source.
   * - ``--shell``
     - Install the variant that can run operator-approved commands. This is
       a different binary, not a setting; see :ref:`intro-install-modules`.
   * - ``--controller AID``
     - Install the variant with the ``shell`` module and give exactly this
       AID access to it: the installer runs
       ``anet init --shell-controller AID --yes`` while no node runs (a running
       node is stopped first and started again). The option is the consent to
       the grant; nothing is asked. The summary names the AID and the command
       that revokes the access. It is the counterpart of ``install.ps1
       -Controller`` (see :ref:`intro-install-windows-controller`).
   * - ``--agents[=LIST]``
     - After installation, run ``anet agents wire`` for the coding agents
       found on the machine, or only for the comma-separated ``LIST``
       (``claude``, ``codex``, ``cursor``, ``opencode``, ``hermes``, ``workbuddy``).
   * - ``--hub URL``
     - Join the hub at ``URL`` instead of ``https://hub.agentnetwork.org.cn``.
       A node that has joined another hub before joins this one.
   * - ``--name NAME``
     - The name to join under. The default is the host name.
   * - ``--no-join``
     - Start the node, but do not join a hub.
   * - ``--no-start``
     - Neither start the node nor join a hub.
   * - ``--without MODULES``
     - Switch off the listed modules in the configuration. See
       :ref:`intro-install-modules`.
   * - ``--no-modify-path``
     - Do not change the shell profiles and do not create the command link.
       The installer names the line to add instead (see
       :ref:`intro-install-path`).
   * - ``--help``
     - Print the option summary.

The options may be combined, for example to join another hub under a chosen
name and to connect the local coding agents in one command:

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- \
       --hub https://hub.example.com --name build-host --agents

On a hub that admits by AID, the summary names the ``anet-hub admit``
command that the hub operator runs; the join then completes without further
input. See :doc:`tutorial` for the full sequence.

Hubs no longer use invitation codes. The installer rejects the former
options ``--token`` and ``--token-file`` and says so.

.. _intro-install-hosts:

Download sources
----------------

The installer asks two sources at the same time, each with a limit of three
seconds, and tries those that answer, the fastest first. A source that does
not answer within the limit is not used. Then it tries the remaining sources
in order:

.. list-table::
   :header-rows: 1
   :widths: 22 78

   * - Source
     - Location
   * - ``npmmirror``
     - ``https://registry.npmmirror.com``: the npm package of the build,
       downloaded as the tarball of the registry. Asked first, together with
       GitHub.
   * - ``GitHub``
     - ``https://github.com/ANetResearch/ANet/releases/latest/download/<file>``.
       Asked first, together with npmmirror.
   * - ``npmjs``
     - ``https://registry.npmjs.org``: the same tarball.
   * - ``jsDelivr``
     - ``https://cdn.jsdelivr.net/npm/<package>@latest/dl/<file>``.
   * - ``agentnetwork.org.cn``, ``hub.agentnetwork.org.cn``
     - The project hosts: ``https://agentnetwork.org.cn/dl/<file>`` and
       ``https://hub.agentnetwork.org.cn/dl/<file>``.

The npm package of the build is ``@agentnetwork/anet-<os>-<cpu>``
(``@agentnetwork/anet-shell-<os>-<cpu>`` with ``--shell``), where ``<os>`` is
``linux`` or ``darwin`` and ``<cpu>`` is ``x64`` or ``arm64``. Its directory
``package/dl/`` holds ``release.json``, ``release.json.sig`` and the archive of
the build, exactly as the project hosts serve them under ``/dl/``. The
installer reads the version from ``<registry>/<package>/latest``, builds the
address of the tarball from it, and extracts only these three files, each of
which must be a regular file of bounded size.

The source decides how fast and whether the files arrive, not what is
accepted:

* The signed manifest and the SHA-256 digests decide, by the checks of
  :ref:`intro-install-script`, for every source alike.
* A source that does not serve a release is passed over: an HTTP error, a
  time-out, an HTML page where a manifest is expected, a missing file, a
  redirect to a host that the source does not use, or a malformed tarball.
* A source that serves a release that fails a check ends the installation.
  The installer does not move on to another source.
* A source whose release is older than the installer itself, or than the
  ``anet`` already installed, is a mirror that lags behind. It is passed over
  while another source serves the current release.

The installer follows redirects itself, at most five, over ``https://`` only,
and only to the hosts of that source: ``registry.npmmirror.com`` and
``cdn.npmmirror.com`` for npmmirror, ``github.com`` and
``*.githubusercontent.com`` for GitHub, and the host of the source for the
others. ``--base URL`` and ``ANET_INSTALL_BASE`` replace every source; the
files are then read from ``URL/dl/``, and redirects may lead to any
``https://`` host. ``anet update`` uses the same sources in the same order
(see :ref:`intro-install-update`).

.. _intro-install-earlier:

Earlier installations
---------------------

The installer replaces earlier releases without asking for input. It examines
every ``anet`` on the search path, the ``anet`` in the global ``bin``
directory of npm, and ``/usr/local/bin/anet``, ``/opt/homebrew/bin/anet`` and
``~/.local/bin/anet``, and acts on each one as follows.

.. list-table::
   :header-rows: 1
   :widths: 34 66

   * - Found
     - Action
   * - AgentNetwork 1.x installed with npm (the package
       ``@agentnetwork/anet`` 1.x)
     - The daemon is stopped with the ``anet stop`` command of that release.
       The package is removed with ``npm uninstall -g @agentnetwork/anet``
       under the npm prefix in which it is installed.
   * - AgentNetwork 1.x, anet 0.1 or anet 0.2 as a binary, for example from
       ``https://clawnet.cc/install.sh`` or from an earlier ``install.sh``
     - The daemon is stopped with ``anet stop`` (1.x) or ``anet stop --all``
       (0.1 and 0.2). In a directory that the user can write, the binary is
       replaced by a symbolic link to the new binary, so that scripts and
       configurations that name the old path run the new release. In a
       directory that only the superuser can write, the binary is removed.
   * - ``anet`` 0.3 or later, and the npm launcher of 0.3 or later
     - Left in place and named in the log. The installation directory is
       placed before it on the search path. An npm launcher whose command is
       the installation target itself is removed with npm.
   * - A file named ``anet`` that is not a program of AgentNetwork
     - Not run, not changed, and named in the log. At the installation
       target itself (``~/.local/bin/anet`` by default), such a file is
       refused before anything is changed; a link to a file that no longer
       exists is replaced.

An earlier release is recognized by the Go module path in its binary, or by
the ``repository`` field of its npm package. Its version is read with
``anet version`` in an empty home directory and with a time limit. Apart from
that, only its ``stop`` command is run.

On Linux, a systemd unit named ``anet-daemon`` whose ``ExecStart`` runs a
binary of AgentNetwork 1.x is disabled and stopped before the daemon, since
the unit restarts the daemon (``Restart=always``). This is the unit that
AgentNetwork 1.x prints for its users to install; the npm packages of 1.x
register no service and no launchd agent themselves. A user unit is disabled
with ``systemctl --user disable --now anet-daemon``, a system unit with
``sudo -n systemctl disable --now anet-daemon`` (see
:ref:`intro-install-sudo`). The unit files are not deleted. When a system unit
cannot be disabled, a ``Note`` line gives the command.

The data directory of an earlier release is renamed and never deleted. The
directory concerned is the one that the new release uses: ``ANET_DATA_DIR``,
else ``ANET_HOME``, else ``~/.anet``.

* A directory with the identity file of anet 0.1 or 0.2, or with the files of
  AgentNetwork 1.x and no identity of 0.3, is renamed to
  ``<directory>-legacy-<version>-<time>``, for example
  ``~/.anet-legacy-1.1.15-20261020T120000Z``. The time is UTC. The files that
  ``anet init`` of 0.3 may have written into it hold no identity and move with
  it. ``anet init`` then writes a new configuration, and the first
  ``anet up`` creates a new identity.
* A directory with the files of AgentNetwork 1.x and an identity of 0.3
  (``identity.kel``, or ``ids/<name>/identity.kel``) stays in place. Only the
  1.x files are moved into the new directory.
* A directory of 0.3, or of a newer release, is not changed.

The daemon applies the same rule. It does not start in a data directory of an
earlier release and reports ``identity_v02``.

A daemon of AgentNetwork 1.x can still hold the data directory after the
``stop`` commands above, for example when its binary was deleted by hand or
when an MCP client started it. The installer then stops it itself:

#. through the shutdown interface of 1.x on ``127.0.0.1:3998``, with the API
   token from the data directory. The token is passed to ``curl`` in a
   private configuration file, never on a command line;
#. if the daemon still runs, with ``SIGTERM`` to the process named in
   ``daemon.pid``, and only after the executable of that process has been
   identified as AgentNetwork 1.x.

Each step is followed by a wait of at most 10 seconds. A daemon of this
release that runs on the identity of 0.3 in the same directory is never taken
for an earlier release and never signalled. Only when a daemon of 1.x cannot
be stopped, or a daemon of anet 0.2 still answers on its control address, does
the directory stay in place; ``anet init`` and the start of the node are then
skipped, and a ``Note`` line says what to stop before the installer is run
again.

The output names the takeover in the phase line ``Replacing <release>`` and in
the summary line ``Replaced <release>; its data is kept in <directory>``. The
log lists every step in the section ``Upgraded from anet <version>``:

.. code-block:: text

   Upgraded from anet 1.1.15 (npm)
     Stopped:
       - the daemon of AgentNetwork 1.1.15 (.../anet-darwin-arm64/bin/anet stop)
     Removed:
       - npm package @agentnetwork/anet 1.1.15 (/opt/homebrew/bin/anet)
     Moved aside:
       - /Users/user/.anet → /Users/user/.anet-legacy-1.1.15-20261020T120000Z (identity: anet/identity.json; API tokens: api_token, agent_token, scoped_tokens.json; other data: anet.db). Nothing was deleted; the new anet starts with a new identity.
     PATH:
       - added the anet PATH block to /Users/user/.zprofile
       - added the anet PATH block to /Users/user/.zshrc

The renamed directory keeps the identity and the API tokens of the earlier
release. Release 0.3 does not read them. See :doc:`/upgrading` for the steps
after the installation.

.. _intro-install-sudo:

Administrator rights
--------------------

A per-user installation needs administrator rights only in these cases:

* an earlier release sits where only the superuser can remove it, for example
  ``/usr/local/bin/anet`` owned by ``root``, or a 1.x npm package under a
  prefix owned by ``root``;
* a system unit of AgentNetwork 1.x is to be disabled;
* the command link of :ref:`intro-install-path` is to be created in a
  directory owned by ``root``.

In these cases, and only when a terminal is available, the installer prints
one line with the reason, for example:

.. code-block:: text

   Removing AgentNetwork 1.1.5 from /usr/local/bin requires administrator rights; macOS will ask for your password once.

It then runs ``sudo -v`` once. ``sudo`` reads the password from the terminal,
never from the input of the script. The steps that need the superuser then run
with ``sudo -n`` within that session. When ``sudo`` is declined or fails, the
installation continues, and the summary has one ``Note`` line with the command
for each step that was not carried out. When ``sudo -n`` already succeeds, no
password is asked.

Without a terminal, as in the tool call of a coding agent or a CI job,
nothing is asked: ``sudo`` is used only as ``sudo -n``, and the ``Note`` lines
remain. ``--system`` keeps its behaviour and uses ``sudo -n`` only.

.. _intro-install-path:

Search path
-----------

The binary stays in ``~/.local/bin``, where the user can replace it;
``anet update`` therefore needs no administrator rights.

When ``~/.local/bin`` is not on the search path of the shell that runs the
installer and a terminal is available, the installer creates a command link
``anet`` pointing to ``~/.local/bin/anet``, so that ``anet`` runs in that
terminal at once. The link goes into the first of ``~/bin``,
``/opt/homebrew/bin`` and ``/usr/local/bin`` that is on the search path and
writable by the user; no password is needed. When none of them is, and
``/usr/local/bin`` is on the search path, the link ``/usr/local/bin/anet`` is
created with ``sudo`` within the single session of :ref:`intro-install-sudo`.
An earlier release at the place of the link is replaced by it; another file
there is left unchanged, and no link is created. The last line of the output then reads
``Run 'anet' to get started.``. When ``~/.local/bin`` is already on the search
path, no link is created and nothing is asked.

Without a terminal, no link is created. The last line then says to run
``anet`` in a new terminal, or in the current one after this command:

.. code-block:: console

   $ export PATH="$HOME/.local/bin:$PATH"

For new terminals, when the installation directory is not on the search path,
when another ``anet`` comes before it, or when a new login shell does not find
the new binary, the installer appends this block to the shell profiles, or
replaces the block if it is already present:

.. code-block:: sh

   # >>> anet installer: PATH >>>
   # Puts the directory of anet first on PATH. Written by the anet installer;
   # to manage PATH yourself, delete this block and install with --no-modify-path.
   case "$PATH" in "$HOME/.local/bin"|"$HOME/.local/bin":*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac
   # <<< anet installer: PATH <<<

.. list-table::
   :header-rows: 1
   :widths: 20 80

   * - Shell
     - Profiles
   * - zsh
     - ``.zprofile`` and ``.zshrc`` in ``$ZDOTDIR``, or in the home directory
       when ``ZDOTDIR`` is not set
   * - bash
     - ``~/.bash_profile`` if it exists, else ``~/.bash_login`` if it exists,
       else ``~/.profile``; and ``~/.bashrc``
   * - fish
     - ``~/.config/fish/conf.d/anet.fish`` (below ``XDG_CONFIG_HOME`` when it
       is set), with the same effect in fish syntax
   * - Other shells
     - ``~/.profile``

The profiles of the login shell named by ``SHELL`` are created when they do
not exist. The profiles of other shells are changed only when they exist.
Running the installer again adds no second block; a block that is no longer
last in its file is moved to the end. A profile that is a symbolic link, for
example into a repository of dotfiles, is written through the link. A file
with a block whose end marker is missing is not changed, and a ``Note`` line
names it. The log lists the profiles that were changed.

The installer then starts a login, interactive shell of ``SHELL`` with an
empty environment and input from ``/dev/null``, asks it for
``command -v anet``, and writes the result to the log:

.. code-block:: text

   New terminals: `anet` is /Users/user/.local/bin/anet (zsh -lic 'command -v anet').

When a new terminal would still run another ``anet``, a ``Note`` line of the
summary says so.

With ``--no-modify-path``, neither the profiles nor the command link are
changed, and the last line of the output names the directory to put on the
search path. When the installer runs as the superuser, it does not change
profiles. To undo the change, delete the lines between the two markers, and
remove the command link ``anet`` if it was created; the log names its place.

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
   $ ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh && sh install.sh

The key fingerprint is ``SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4``.
A successful check prints ``Good "anet-release@agentnetwork.org.cn"
signature`` followed by the fingerprint.

.. _intro-install-windows:

Installing on Windows
=====================

The Windows build runs natively on Windows 10 version 1809 or later and on
Windows Server 2019 or later, on amd64 and arm64. WSL2 is not required.

Requirements
------------

* Windows PowerShell 5.1, which every supported version of Windows includes.
  The installer uses no tool that has to be installed; in particular it does
  not use ``ssh-keygen``, because Windows 10 version 1809 ships an OpenSSH
  release older than 8.1.
* Outbound HTTPS to at least one of the download sources (see
  :ref:`intro-install-windows-sources`).

The installer runs in a window that is elevated or not, and in a session
without a visible console, such as the tool call of an agent or a CI job.

Installing with the one-line command
------------------------------------

The installation is started with one line. The line is identical in
``cmd.exe``, Windows PowerShell 5.1, PowerShell 7, a ``bash`` or ``zsh``
shell in WSL 1 or WSL 2, and Git Bash: it contains no character that one of
these shells interprets, and everything after ``-Command`` is one argument in
double quotes. Replace ``CONTROLLER_AID`` with the AID of the node that is to
be given access to the shell module of this computer. The installer installs
the build with the ``shell`` module, gives that AID access to it, starts the
node, registers it with ``https://hub.agentnetwork.org.cn`` and starts it at
every logon:

.. code-block:: text

   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1))) -Controller CONTROLLER_AID"

Without ``-Controller``, the line installs the default build and gives no
agent access. The node is started and registered with the hub in the same way;
``-NoJoin`` leaves out the registration and ``-NoStart`` leaves out both.
Everything else described in this section, the download sources, the choice
of the install directory, the takeover of earlier installations, the ``PATH``
and the output, is the same in both forms:

.. code-block:: text

   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"

The line starts a new ``powershell.exe``, which enables TLS 1.2 (required by
Windows PowerShell 5.1), downloads ``install.ps1`` and runs it as a script
block, to which the execution policy does not apply. The placeholder is a
plain word because ``cmd.exe`` reads ``<`` and ``>`` as redirections; the
installer refuses the unchanged placeholder ``CONTROLLER_AID`` with a message
that names it. The exit status of ``powershell.exe`` is the result of the
installation: 0 when it completed; 3 when ANet is installed but the
installation stopped before its last step and the commands that complete it
were printed (see :ref:`intro-install-windows-elevated` and
:ref:`intro-install-windows-earlier`); 1 for a refusal or a failure before
anything was installed; otherwise the exit status of the ``anet`` command that
failed.

No interaction
--------------

The installer asks no question and reads nothing from the console. Every
decision is made from the parameters on the command line. In particular:

* ``-Controller`` on the command line is the consent to the grant. The
  installer passes it to ``anet init --shell-controller`` as ``--yes``, and no
  confirmation is asked. The summary names the agent that was given access and
  the command that revokes it; the log records what was granted, read back
  from ``config.json`` and ``peers.allow``, and both steps of the revocation.
* The recovery passphrase is not asked during the installation: the
  installer always runs ``anet hub-register`` with ``--passphrase-later``. Set
  the passphrase afterwards with ``anet identity protect``.
* The installation also completes when standard input and output are not a
  console, as with WSL interoperability, a CI job or the tool call of an
  agent.

.. _intro-install-windows-output:

What the installer prints
-------------------------

While it works, the installer prints one line per phase: the download, and the
installed binary with the result of the signature check. On an interactive
console, the download is shown as one line that is updated several times per
second, with a bar, the megabytes received and expected, the current speed and
the estimated time remaining; when the download completes, the line remains
with the size, the average speed and the elapsed time. The bar is drawn in red
and dark gray, or without colours when the variable ``NO_COLOR`` is set, and
with ASCII characters when the code page of the console cannot display the
line-drawing character. When the output is not an interactive console, as in a
CI job, the tool call of an agent or a pipe, the download is reported in one
line once it is complete.

The installation ends with a short summary, for example:

.. code-block:: text

   anet 0.3.6 is installed and running.
     AID   bafyrei...
     Hub   https://hub.agentnetwork.org.cn (joined)
   Remote control: allowed for bafyrei... only; revoke with: anet peers remove bafyrei...
   Run 'anet' in a new window to get started.
   Details: C:\Users\<user>\.anet\logs\install-20261020T151644Z.log

The ``AID`` line gives the identity of the node in full. The ``Hub`` line
states whether the node joined the hub, whether the hub waits for its
operator to admit the node (the line then gives the ``anet-hub admit`` command
for the operator), or whether the hub could not be reached yet, in which case
the node keeps trying. The ``Remote control`` line appears only with
``-Controller``. A line beginning with ``Replaced`` appears only when an
earlier installation was taken over, and names the directory that now holds
its data. A line beginning with ``Note:`` appears only for something that is
left to the user, such as a command to run. With ``ANET_LANG`` set to a value
that begins with ``zh``, the summary is in Chinese.

The last line names the log of the run. The log records everything else: the
download sources that were probed and tried, each check, every step of the
takeover, the changes to the ``PATH`` and to the ``Run`` value, and the output
of ``anet init``, ``anet up``, ``anet doctor`` and ``anet hub-register``. It
is written to ``%USERPROFILE%\.anet\logs\install-<UTC time>.log``, in a
directory with an access control list of its own (full control for the user,
SYSTEM and Administrators, and nothing else). When the data directory is not
ready for it, because the installation stopped before ``anet init`` or the
directory still holds the data of an earlier release, the log is written to
``%TEMP%\anet-install\`` instead. A refusal and a failure are reported in a
short ``Error:`` line followed by the ``Details:`` line.

.. _intro-install-windows-sources:

Download sources
----------------

``install.ps1`` installs exactly the release with which it was published,
because it carries the SHA-256 digests of that release. Every download source
is therefore asked for that version:

#. The npm mirror ``registry.npmmirror.com`` and the GitHub release of the
   project (``github.com/ANetResearch/ANet/releases/download/v<version>/``)
   are probed in parallel, each for at most 3 seconds and for at most 4
   seconds in all. The sources that answered are tried first, the faster one
   first; a source that did not answer is not used.
#. The npm registry ``registry.npmjs.org``.
#. jsDelivr (``cdn.jsdelivr.net``).
#. The project's hosts ``agentnetwork.org.cn`` and ``hub.agentnetwork.org.cn``.

From an npm registry, the installer downloads the tarball of the npm package
of the build (``@agentnetwork/anet-win32-<cpu>`` or
``@agentnetwork/anet-shell-win32-<cpu>``) and reads ``release.json``, its
signature and the compressed binary from its ``dl`` directory, in memory. The
tarball is refused as a whole when it contains an entry whose name is not a
plain relative path, for example one with ``..``; nothing is ever written
under the name of an entry. A download from GitHub may be redirected only to
``github.com`` and to hosts under ``githubusercontent.com``, and only over
HTTPS.

A source that does not serve the release, because it does not answer, returns
an HTTP error, returns a page instead of a file, lacks a file or serves another
version, is passed over for the next source in one line. A source only
determines where the files come from: every file is checked as described in
:ref:`intro-install-windows-checks`, and a file that fails a check ends the
installation, whichever source served it. ``-Base`` replaces the list of
sources, and so does the environment variable ``ANET_INSTALL_BASE`` when
``-Base`` is not given.

Running the command in WSL
--------------------------

Run in a WSL shell, the line installs the Windows build on the Windows host,
for the Windows user who runs WSL: that is the computer that the controller is
given, and the node runs as a Windows program. The command requires that
``powershell.exe`` can be started from WSL, which WSL interoperability
provides and which is enabled by default (``[interop] enabled`` and
``appendWindowsPath`` in ``/etc/wsl.conf``). To install ANet inside the Linux
distribution instead, use ``install.sh`` (see :ref:`intro-install-script`).

The install directory
---------------------

Without ``-Prefix``, the installer chooses the install directory. It uses the
first of the following directories that passes the checks below, and prints
one line for each directory it skips, with the reason; the reason names the
accounts concerned, and an account that Windows cannot name is shown by its
SID followed by ``(unresolved)``:

#. the directory of an ANet 0.3 or later that is already installed for the
   user, found on the user's ``PATH`` or in the ``Run`` value below, when
   that directory holds nothing but the files of that installation
   (``anet.exe``, its release record and signature, binaries moved aside by
   an update); a directory that also holds other files is reported and not
   used;
#. ``%LOCALAPPDATA%\Programs\anet``;
#. ``%LOCALAPPDATA%\anet\bin``, beside the runtime directory
   ``%LOCALAPPDATA%\anet\run`` of the node;
#. ``%USERPROFILE%\.anet-bin``.

A directory passes when no directory above it grants another account the
right to rename it or to change its access control list, and when it is
private to the user: it is owned by the user, SYSTEM or Administrators, and no
access control entry grants another account more than synchronization,
traversal and reading attributes. Such entries, for example the one that
Windows grants an app container capability on the profile directory, give no
access to the contents and are ignored. An entry that grants listing or
reading, writing or appending, creating files or subdirectories, deletion,
reading or changing permissions, taking ownership, or a generic right fails the
check. A directory that does not exist yet is checked through the nearest
directory above it, and each missing level is created with an access control
list of its own: full control for the user, SYSTEM and Administrators, and
nothing else. ``%LOCALAPPDATA%\anet`` is created in this way when the third
directory is used, and the node accepts it as the parent of its runtime
directory.

``-Prefix`` overrides the choice. The given directory is subject to the same
checks, and a directory that fails them is refused.

.. _intro-install-windows-elevated:

An elevated window
------------------

An elevated window is not refused. ANet is installed for the current account,
with the same per-user directories, ``PATH`` and ``Run`` value as in a window
that is not elevated. The parameter ``-Elevated`` is accepted for
compatibility and has no effect.

While Windows has a standard user token for the account, that is, while User
Account Control is on, the elevated window has more rights than the programs
of the account normally run with, and the installer does not run the
programs of the account from it: an earlier ``anet.exe`` found on the
``PATH`` is classified by its contents without being run, its ``stop``
command and ``npm`` are not run, and the commands are printed instead, to be
run in a normal window of the account. PowerShell itself still loads its
modules from the account's ``Documents`` folder before the script runs;
this is one more reason to run the command in a normal window.

The node is never started with the elevated token in that case. The installer
registers a one-time task in the Task Scheduler for the current user, with the
interactive token and the least privileges, runs ``anet up`` through it and
deletes the task; tasks that an earlier run left behind are removed at the
start. When the task cannot run or does not complete within 30 seconds, for
example because the Task Scheduler is unavailable or the user is not logged on
interactively, the node is not started: the installation ends with exit
status 3, the ``Run`` value is written unless ``-NoAutostart`` is given, so
that the node starts at the next logon, and the commands that start it now
(``anet up``, then ``anet hub-register`` unless ``-NoJoin`` is given) are
printed for a normal window. The controller's commands are therefore run with
the user's rights, never with the administrator's.

An elevated window that runs as an account other than the one logged on to
the session, which is the case when another account's credentials are given to
the User Account Control prompt, is refused before anything is changed: ANet
would be installed for that other account, and its node would run as that
account in the user's session. Run the command in a normal window of your own
account.

When Windows has no standard user token for the account, because User Account
Control is off or the account is the built-in Administrator, the window has
the rights the account always has: earlier installations are taken over as in
any other window, the node is started directly with the rights of the window,
and the installer says so in one line.

.. _intro-install-windows-earlier:

Earlier installations
---------------------

The installer takes over from other ANet installations of the user, without
asking, in an order that leaves a consistent state when a step fails:

* An ``anet.exe`` found on the machine's or the user's ``PATH`` is recognized
  by the Go module path in the file: that of AgentNetwork 1.x
  (``ChatChatTech/AgentNetwork``) or that of this project. The npm package
  ``@agentnetwork/anet`` in the global npm prefix (``npm prefix -g``) is
  recognized by its ``package.json``: its name, a version in the form
  ``x.y.z``, and the ``repository`` field or the version, as ``install.sh``
  does. A file named ``anet.exe`` that is not a program of either project is
  reported and never run.
* An ANet of the earlier line is stopped with its own ``stop`` command, with a
  time limit; the binary that is run must carry the module path of
  AgentNetwork 1.x.
* When a daemon of AgentNetwork 1.x still holds the data directory, the
  installer stops it: a daemon is the process named in ``daemon.pid``, or the
  process that listens on ``127.0.0.1:3998``, and only when its program
  carries the module path of AgentNetwork 1.x. A daemon of ANet 0.3 or later,
  or another program that reuses the process ID of a stale ``daemon.pid``, is
  never taken for one. The installer first asks the daemon to shut down
  through its local API, with the API token from the data directory, as the
  ``stop`` command of 1.x does, and waits up to 10 seconds; a daemon that is
  still running is then ended by its process ID, after its program has been
  checked again. The installer never ends a process by name.
* Its data in ``%USERPROFILE%\.anet`` (or ``ANET_DATA_DIR``) is moved aside,
  never deleted, by the same rule as ``install.sh`` and the daemon (see
  :ref:`intro-install-earlier`). A directory with the identity file of 0.2,
  or with the files of 1.x and no identity of 0.3, is renamed to
  ``%USERPROFILE%\.anet-legacy-<version>-<UTC time>``. A directory with the
  files of 1.x and an identity of 0.3 (``identity.kel``, or
  ``ids\<name>\identity.kel`` of this version) stays in place, and only the
  1.x entries (``anet``, ``anet.db`` and its journals, ``api_token``,
  ``agent_token``, ``scoped_tokens.json``, ``peer-journal.json``, ``cas``,
  ``orgs``, ``state`` and ``daemon.pid``) are moved into a directory of that
  name. A directory of 0.3, of a newer release, or whose identity file does
  not read is not changed.
* Only after the data has been moved is the npm package removed with
  ``npm uninstall -g @agentnetwork/anet``. When that is not possible, the
  installer prints the command to run.
* Another installation of ANet 0.3 or later outside npm is left in place. Its
  directory is taken off the user's ``PATH`` only when it holds nothing but
  the files of that installation; a directory that also holds other programs
  stays on the ``PATH`` and is reported. The npm launcher of 0.3 or later is
  left alone.

The data directory is moved in this way whether or not an earlier installation
was found, as ``install.sh`` does, once no daemon holds it. When it holds the
data of an earlier release but cannot be moved, because its daemon could not be
stopped or because the move failed, the installation stops before
``anet init``: nothing in that directory is changed,
the npm package stays, the exit status is 3, and the installer prints the
exact commands that remain, in order (stopping the earlier daemon, renaming
the directory, removing the package, ``anet init``, the grant, ``anet up``
and the hub registration).

The install directory is put first on the user's ``PATH``. The installer then
starts a new ``powershell.exe`` with the ``PATH`` that a new window receives
and reports in one line which ``anet`` it runs. A directory on the machine's
``PATH`` precedes the user's ``PATH``; an earlier ``anet`` there is reported
in a ``Note:`` line. The summary reports a takeover in one line; what was
stopped, moved and removed is recorded in the log.

.. _intro-install-windows-verify:

Verifying the installer before running it
-----------------------------------------

The one-line command trusts the host that serves ``install.ps1`` and its TLS
certificate. To avoid that trust, download the script and its signature,
verify the signature with the release key, and run the verified copy.

The signature is checked with ``ssh-keygen -Y verify`` from OpenSSH 8.1 or
later (``ssh -V`` prints the version). The following block, for a PowerShell
window, writes the file ``allowed_signers`` with the release key line given
in :ref:`intro-install-verify` as UTF-8 without a byte order mark, which
``ssh-keygen`` requires; the ``>`` redirection of Windows PowerShell 5.1
writes UTF-16. It runs the check in ``cmd.exe``, which provides the input
redirection that PowerShell lacks, and stops before running the script when
the signature does not verify:

.. code-block:: powershell

   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1 -OutFile install.ps1
   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1.sig -OutFile install.ps1.sig
   [IO.File]::WriteAllText((Join-Path $PWD.ProviderPath 'allowed_signers'), 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL')
   cmd /c "ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.ps1.sig < install.ps1"; if ($LASTEXITCODE -ne 0) { throw 'install.ps1 did not verify' }; & ([scriptblock]::Create((Get-Content -Raw .\install.ps1))) -Controller CONTROLLER_AID

When this computer has an older OpenSSH, copy ``install.ps1``,
``install.ps1.sig`` and ``allowed_signers`` to a computer with OpenSSH 8.1 or
later and run the check there. The command is the same in ``cmd.exe`` and in
a POSIX shell:

.. code-block:: console

   $ ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.ps1.sig < install.ps1

A successful check prints ``Good "anet-release@agentnetwork.org.cn"
signature`` followed by the key fingerprint. Only then run the verified copy
in PowerShell, in the directory that holds it:

.. code-block:: powershell

   & ([scriptblock]::Create((Get-Content -Raw .\install.ps1))) -Controller CONTROLLER_AID

Run in an open PowerShell window in this way, the script leaves the window
open and sets ``$LASTEXITCODE`` to the result.

Installer parameters
--------------------

.. list-table::
   :header-rows: 1
   :widths: 26 74

   * - Parameter
     - Effect
   * - ``-Controller <AID>``
     - Install the build with the ``shell`` module and give exactly this AID
       access to it (see :ref:`intro-install-windows-controller`). The
       parameter is the consent to the grant.
   * - ``-Shell``
     - Install the build with the ``shell`` module. No caller is given access.
   * - ``-Agents``
     - After installation, run ``anet agents wire --all``: the coding agents
       found on the computer receive the tools and the guide of anet. Every
       file it changes is backed up first. A failure is reported as a note;
       the installation continues.
   * - ``-AgentList <list>``
     - As ``-Agents``, for the comma-separated agents named (for example
       ``claude,codex``).
   * - ``-Hub <URL>``
     - Register with the hub at ``URL`` instead of
       ``https://hub.agentnetwork.org.cn``.
   * - ``-NoJoin``
     - Start the node, but do not register with a hub. Register later with
       ``anet hub-register``.
   * - ``-NoStart``
     - Neither start the node nor register with a hub. Start the node later
       with ``anet up``.
   * - ``-Name <name>``
     - The name to register under. The default is the computer name.
   * - ``-Prefix <directory>``
     - Install to this absolute directory instead of the directory the
       installer chooses. The directory must pass the checks of the chosen
       directories. A directory name that contains ``;`` or ``%`` is refused.
   * - ``-Without <modules>``
     - Switch off the listed modules in the configuration, as
       ``install.sh --without`` does (see :ref:`intro-install-modules`). The
       configuration is changed after the binary is installed.
   * - ``-Base <URL or directory>``
     - Download from this ``https://`` address, or read a release from this
       local directory, instead of the download sources. Every check is the
       same.
   * - ``-NoAutostart``
     - Do not write the value that starts the node at logon.
   * - ``-NoPath``
     - Do not put the installation directory on the user's ``PATH``.
   * - ``-Elevated``
     - Accepted for compatibility with 0.3.4; it has no effect.

The download sources are described in :ref:`intro-install-windows-sources`.
The npm packages of the Windows builds are ``@agentnetwork/anet-win32-<cpu>``
and ``@agentnetwork/anet-shell-win32-<cpu>``.

Paths
-----

.. list-table::
   :header-rows: 1
   :widths: 32 68

   * - Item
     - Location
   * - Binary
     - ``anet.exe`` in the install directory (see above), with the release
       manifest ``anet.exe.release.json`` and its signature
       ``anet.exe.release.json.sig`` beside it.
   * - Data directory
     - ``%USERPROFILE%\.anet``
   * - Runtime directory
     - ``%LOCALAPPDATA%\anet\run``
   * - Start at logon
     - The value ``AgentNetwork`` under
       ``HKCU\Software\Microsoft\Windows\CurrentVersion\Run``. Removing the
       value stops the start at logon.

.. _intro-install-windows-checks:

What the installer checks
-------------------------

* ``install.ps1`` is signed (``install.ps1.sig``) and carries the SHA-256
  digests of the Windows release binaries.
* Nothing that is downloaded is run or installed before it matches those
  digests.
* The downloaded binary then verifies, with the release key built into it, the
  signature of ``release.json``, its expiry, its version, the SHA-256 digest of
  the binary itself and its module set, and it refuses a downgrade.
* No option skips a check.

.. _intro-install-windows-controller:

What ``-Controller`` opens
--------------------------

``-Controller`` opens exactly the following, and nothing else:

* The build with the ``shell`` module is installed instead of the default
  build.
* The configuration block
  ``modules.shell = {"allow":["<AID>"],"allow_arbitrary":true,"shell":"powershell"}``.
* The AID on ``peers.allow``.

Everything else is what every installation does: the defaults of
``anet init`` (the inbound policy ``closed``, no public capabilities and
spending limits of 0, see :ref:`intro-install-init`), the start of the node,
the ``Run`` value that starts it at every logon unless ``-NoAutostart`` is
given, and the registration with the hub unless ``-NoJoin`` or ``-NoStart`` is
given. No auto-reply, no other peer, no public capability and no listener
beyond the node's loopback ones are opened. The install directory, the
``PATH`` and the takeover of earlier installations are the same with and
without ``-Controller``.

Up to version 0.3.4, the installer asked one confirmation on the console.
That confirmation is removed: the parameter on the command line is the
consent, and the installer runs ``anet init --shell-controller <AID> --yes``.
Run without ``--yes`` in a terminal, ``anet init --shell-controller`` still
asks the confirmation. Access is revoked with ``anet peers remove <AID>`` and
by removing the ``modules.shell`` block from ``config.json``, followed by a
restart of the node. The summary gives the first command, which takes effect at
once; the log gives both steps. The shell module on Windows is described in
:ref:`topics-shell-windows`.

Background operation
--------------------

``anet up`` starts the daemon detached from the console. The daemon keeps
running when the window is closed, stops at logoff, and starts again at logon
through the ``Run`` value above. It runs as the logged-on user and never as a
Windows service.

Updating on Windows
-------------------

``anet update`` applies the checks described in :ref:`intro-install-update`.
A running ``anet.exe`` cannot be overwritten, so the update renames it aside
and puts the new binary in its place. The daemon keeps the old binary until it
is restarted. Windows PowerShell 5.1 has no ``&&`` operator; run the two
commands one after the other:

.. code-block:: powershell

   anet stop --all
   anet up --all

Running the one-line command again also updates an installation; it reuses the
install directory of the existing installation when that directory passes the
checks. A node that runs an earlier version is stopped and started again with
the new binary; with ``-NoStart``, it keeps running, and the summary gives the
commands that restart it.

Uninstalling on Windows
-----------------------

#. Stop the daemons with ``anet stop --all``.
#. Remove the value ``AgentNetwork`` under
   ``HKCU\Software\Microsoft\Windows\CurrentVersion\Run``.
#. Delete the install directory; ``(Get-Command anet).Source`` names the
   binary in it.
#. Remove that directory from the user's ``PATH``.

The data directory ``%USERPROFILE%\.anet`` holds the identity. Delete it only
deliberately, as described in :ref:`intro-install-remove`.

.. _intro-install-npm:

Installing with npm
===================

The npm package ``@agentnetwork/anet`` runs the same release binary. It needs
Node.js 18 or later:

.. code-block:: console

   $ npm install --global @agentnetwork/anet
   $ anet version

The package contains a launcher and no binary. The build of the platform is
in the optional dependency ``@agentnetwork/anet-<os>-<cpu>``, as the archive
and the signed release manifest. Before the first run of a version, the
launcher checks that ``release.json`` carries a valid signature by the ANet
release key (the file ``allowed_signers`` of the package) and names the
version of the package, and that the SHA-256 digests of the archive and of the
binary equal the digests in the manifest. A failure of any check stops the
launcher before anything is run. The checked binary is written, with the
manifest and its signature, to ``~/.cache/anet/npm/<version>/``
(``~/Library/Caches/anet/npm/<version>/`` on macOS,
``%LOCALAPPDATA%\anet\npm\<version>`` on Windows, or below
``XDG_CACHE_HOME`` when it is set) and is run from there.

Update the package with ``npm update --global @agentnetwork/anet``. Do not run
``anet update`` on an installation from npm: the launcher runs the version of
the package and replaces a changed binary with that version on its next run.
Installations with ``--omit=optional`` lack the build and are refused with a
message. The variant with the ``shell`` module is not available through the
launcher; install it with ``install.sh --shell``, or on Windows with
``install.ps1 -Shell``.

The package also contains ``install.sh`` and ``install.sh.sig``, so that the
installer can be obtained from npm when the project hosts cannot be reached,
for example from ``https://cdn.jsdelivr.net/npm/@agentnetwork/anet@latest/install.sh``.
Verify it as described in :ref:`intro-install-verify` before running it.

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

On Linux and macOS, the installer has already started the node and joined the
public hub, unless ``--no-start`` or ``--no-join`` was given (see
:ref:`intro-install-script`). The commands above start and inspect a node by
hand.

``anet up`` starts the daemon in the background. It continues to run after
the terminal closes. ``anet status`` prints the AID, the data directory,
the address of the local console and the hub state. Until the node is
registered with a hub, the hub state is ``none`` and the status carries
the command to run next:

.. code-block:: console

   $ anet hub-register https://hub.example.com --name my-node

The registration and the first exchange are described in :doc:`tutorial`.
For the lifecycle commands, see :ref:`reference-cli`.

The data directory is ``~/.anet`` (``%USERPROFILE%\.anet`` on Windows). Set
``ANET_DATA_DIR`` to use another location. Further identities, created with ``anet id new NAME``, are kept
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
   ``--shell`` (``-Shell`` or ``-Controller`` on Windows) at installation and
   is retained by ``anet update``.

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

On Windows, the running ``anet.exe`` is renamed aside instead of replaced; see
:ref:`intro-install-windows`.

``anet update`` downloads from the sources of :ref:`intro-install-hosts`, in
the same order as ``install.sh``: npmmirror and GitHub are asked at the same
time with a limit of three seconds and tried fastest first, followed by
npmjs, jsDelivr and the project hosts. A source whose release is older than
the running binary is passed over while another source serves a release at
least as new. The output names the source that was used:

.. code-block:: text

   source    npmmirror (https://registry.npmmirror.com/@agentnetwork/anet-darwin-arm64)

The tarball of an npm package is read before anything in it is verified, and
is therefore read with bounds: its size, its unpacked size and the size of
each file are limited, only regular files under ``package/dl/`` are used, an
entry whose name is absolute or contains ``..`` ends the read, and the sha512
``integrity`` of the package metadata is checked when it is present. A source
that does not serve a release is passed over; a release that is served and
fails a check ends the update.

``--base URL``, or else the environment variable ``ANET_INSTALL_BASE``,
replaces every source: the files are read from ``URL/dl/`` only, with the same
checks. Re-running the installer is not required on a machine that already
has ``anet``. An installation from npm is updated with npm (see
:ref:`intro-install-npm`).

.. _intro-install-remove:

Removing ANet
=============

Stop the daemons and delete the binary:

.. code-block:: console

   $ anet stop --all
   $ rm "$(command -v anet)"

On Windows, follow the steps in :ref:`intro-install-windows` instead.

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

When ``install.sh`` stops, it prints ``Error:`` with the reason, then
``Nothing was installed or changed.``, or the steps of a takeover that were
already carried out, and last the log (see :ref:`intro-install-output`).

``no download source served a release``
   None of the download sources answered with a release. Check the
   connection of the machine, or name a source with ``--base URL``.

``ssh-keygen is required``
   Install the OpenSSH client tools: ``openssh-client`` on Debian and
   Ubuntu, ``openssh`` on Fedora and RHEL, ``openssh-keygen`` on Alpine.

``is not signed by the anet release key``
   Either the download source serves a file that the release key did not
   sign, or the installed ``ssh-keygen`` is older than OpenSSH 8.1. Try
   another source with ``--base URL``; every source is checked in the same
   way.

``Refusing to downgrade``
   The manifest names a version older than the installed one, which
   usually indicates a stale mirror. Use ``--base URL`` to select another
   source.

For failures after installation, see :doc:`/howto/troubleshooting`.
