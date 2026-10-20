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
   On Linux and macOS: ``curl``, ``gzip`` (``gunzip``), ``sha256sum`` or
   ``shasum``, and ``ssh-keygen`` from OpenSSH 8.1 or later. The signature
   check uses ``ssh-keygen``. If it is missing, the installer stops and names
   the package to install; no option skips the signature check. On Windows:
   Windows PowerShell 5.1, which Windows includes, and no tool to install.

Network
   Outbound HTTPS to the download host. A node needs no inbound connection
   and no public address.

.. _intro-install-script:

Installing with the script
==========================

Run the installer for the current user:

.. code-block:: console

   $ curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh

The binary is written to ``~/.local/bin/anet``. When that directory is not
on the search path, or another ``anet`` comes before it, the installer adds a
marked block to the shell profiles that places the directory first (see
:ref:`intro-install-path`). An earlier release of ``anet`` on the machine is
replaced in the same run (see :ref:`intro-install-earlier`). The installer
never waits for input.

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
     - Install to ``/usr/local/bin``. ``sudo -n`` is used when required; the
       installer never asks for a password. When ``sudo`` needs one, the
       installer stops and says so; run ``sudo -v`` in the same terminal and
       repeat the command.
   * - ``--prefix DIR``
     - Install to ``DIR``. Overrides ``--user`` and ``--system``.
   * - ``--base URL``
     - Download from ``URL`` instead of the default hosts (see
       :ref:`intro-install-hosts`). Only ``https://`` is accepted. The
       environment variable ``ANET_INSTALL_BASE`` names a host that is tried
       before the default hosts. Every host is checked in the same way.
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
   * - ``--no-modify-path``
     - Do not change the shell profiles. The installer prints the line to add
       instead (see :ref:`intro-install-path`).
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

.. _intro-install-hosts:

Download hosts
--------------

Without ``--base``, the installer tries these hosts in order and uses the
first one that serves a release manifest and its signature:

#. ``https://agentnetwork.org.cn``
#. ``https://hub.agentnetwork.org.cn``
#. The npm package of the build, ``@agentnetwork/anet-<os>-<cpu>``
   (``@agentnetwork/anet-shell-<os>-<cpu>`` with ``--shell``), through the
   content delivery networks npmmirror
   (``https://registry.npmmirror.com/<package>/latest/files``), jsDelivr
   (``https://cdn.jsdelivr.net/npm/<package>@latest``) and unpkg
   (``https://unpkg.com/<package>@latest``). ``<cpu>`` is ``x64`` or
   ``arm64``.

The directory ``dl/`` of each npm package holds ``release.json``, its
signature and the archive of the build exactly as the project hosts serve
them under ``/dl/``. Which host answers does not change what is accepted: the
checks above apply to every host, and a manifest that a host serves and that
fails a check ends the installation instead of moving on to the next host. A
content delivery network can serve an older release for a while after a new
one is published; the installer then installs that release, or refuses it when
a newer ``anet`` is already installed.

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
     - Left in place and named in the report. The installation directory is
       placed before it on the search path. An npm launcher whose command is
       the installation target itself is removed with npm.
   * - A file named ``anet`` that is not a program of AgentNetwork
     - Not run, not changed, and named in the report. At the installation
       target itself (``~/.local/bin/anet`` by default), such a file is
       refused before anything is changed; a link to a file that no longer
       exists is replaced.

An earlier release is recognized by the Go module path in its binary, or by
the ``repository`` field of its npm package. Its version is read with
``anet version`` in an empty home directory and with a time limit. Apart from
that, only its ``stop`` command is run.

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
earlier release and reports ``identity_v02``. The directory is not moved while
a daemon of the earlier release may still use it, that is, while the process
named in its ``daemon.pid`` runs or while its control address answers. The
report then gives the command to run after that daemon has stopped, and
``anet init`` is skipped.

``sudo`` is used only as ``sudo -n``, which fails instead of asking for a
password. A step that needs a password is not carried out; the report gives
the exact command. At the end of the output, the section
``Upgraded from anet <version>`` lists what was stopped, removed, replaced and
moved, and the commands that remain:

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

.. _intro-install-path:

Search path
-----------

A new terminal runs the new ``anet`` without further steps. When the
installation directory is not on the search path, when another ``anet`` comes
before it, or when a new login shell does not find the new binary, the
installer appends this block to the shell profiles, or replaces the block if
it is already present:

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
with a block whose end marker is missing is not changed and is named in the
report.

The installer then starts a login, interactive shell of ``SHELL`` with an
empty environment and input from ``/dev/null``, asks it for
``command -v anet``, and reports the result on one line:

.. code-block:: text

   New terminals: `anet` is /Users/user/.local/bin/anet (zsh -lic 'command -v anet').

The terminal in which the installer runs keeps its search path. The installer
prints the command that updates it:

.. code-block:: console

   $ export PATH="$HOME/.local/bin:$PATH"

With ``--no-modify-path``, the profiles are not changed and the installer
prints the line to add. When the installer runs as the superuser, it does not
change profiles. To undo the change, delete the lines between the two markers.

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
* Outbound HTTPS to the download hosts.

The installer runs in a window that is elevated or not, and in a session
without a visible console, such as the tool call of an agent or a CI job.

Installing with the one-line command
------------------------------------

The installation is started with one line. The line is identical in
``cmd.exe``, Windows PowerShell 5.1, PowerShell 7, a ``bash`` or ``zsh``
shell in WSL 1 or WSL 2, and Git Bash: it contains no character that one of
these shells interprets, and everything after ``-Command`` is one argument in
double quotes. Replace ``CONTROLLER_AID`` with the AID of the node that is to
be given access to the shell module of this computer:

.. code-block:: text

   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1))) -Controller CONTROLLER_AID"

Without ``-Controller``, the line installs the default build: no agent is
given access, and the node is not started. Everything else described in this
section, the choice of the install directory, the takeover of earlier
installations and the ``PATH``, is the same in both forms:

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
  confirmation is asked. At the end, the installer prints what was granted,
  read back from ``config.json`` and ``peers.allow``, and the two steps that
  revoke it.
* The recovery passphrase is not asked during the installation
  (``anet hub-register --passphrase-later``); set it afterwards with
  ``anet identity protect``.
* The installation also completes when standard input and output are not a
  console, as with WSL interoperability, a CI job or the tool call of an
  agent.

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
of the account normally run with, and the installer does not run anything
that is already on the computer from it: an earlier ``anet.exe`` found on the
``PATH`` is classified by its contents without being run, its ``stop``
command and ``npm`` are not run, and the commands are printed instead, to be
run in a normal window of the account.

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
  AgentNetwork 1.x. The installer never ends a process by name.
* The installer then waits, for up to 15 seconds, until the daemon has
  exited: until the process named in ``daemon.pid`` is gone and
  ``127.0.0.1:3998`` no longer answers.
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
data of an earlier release but cannot be moved, because its daemon still runs
after the wait or because the move failed, the installation stops before
``anet init``: nothing in that directory is changed,
the npm package stays, the exit status is 3, and the installer prints the
exact commands that remain, in order (stopping the earlier daemon, renaming
the directory, removing the package, ``anet init``, the grant, ``anet up``
and the hub registration).

The install directory is put first on the user's ``PATH``. The installer then
starts a new ``powershell.exe`` with the ``PATH`` that a new window receives
and reports in one line which ``anet`` it runs. A directory on the machine's
``PATH`` precedes the user's ``PATH``; an earlier ``anet`` there is reported.
What was stopped, moved and removed is listed as it happens, before ``anet
init``; what ``-Controller`` granted is printed at the end.

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
     - Install the build with the ``shell`` module, give exactly this AID
       access to it (see :ref:`intro-install-windows-controller`), and join
       ``https://hub.agentnetwork.org.cn`` unless ``-Hub`` or ``-NoJoin`` is
       given. The parameter is the consent to the grant.
   * - ``-Shell``
     - Install the build with the ``shell`` module. No caller is given access.
   * - ``-Hub <URL>``
     - Start the node and register it with the hub at ``URL`` as the last
       step.
   * - ``-NoJoin``
     - Do not register with a hub. With ``-Controller``, the node is still
       started. Register later with ``anet hub-register``.
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
       local directory, instead of the default hosts. Every check is the same.
   * - ``-NoAutostart``
     - Do not write the value that starts the node at logon.
   * - ``-NoPath``
     - Do not put the installation directory on the user's ``PATH``.
   * - ``-Elevated``
     - Accepted for compatibility with 0.3.4; it has no effect.

The download hosts are those of :ref:`intro-install-hosts`. The npm packages of
the Windows builds are ``@agentnetwork/anet-win32-<cpu>`` and
``@agentnetwork/anet-shell-win32-<cpu>``.

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
* The defaults of ``anet init``: the inbound policy ``closed``, no public
  capabilities and spending limits of 0 (see :ref:`intro-install-init`).
* The node is started, and the ``Run`` value starts it at every logon unless
  ``-NoAutostart`` is given.
* The registration with the hub, unless ``-NoJoin`` is given.

No auto-reply, no other peer, no public capability and no listener beyond the
node's loopback ones are opened. The install directory, the ``PATH`` and the
takeover of earlier installations are the same with and without
``-Controller``.

Up to version 0.3.4, the installer asked one confirmation on the console.
That confirmation is removed: the parameter on the command line is the
consent, and the installer runs ``anet init --shell-controller <AID> --yes``.
Run without ``--yes`` in a terminal, ``anet init --shell-controller`` still
asks the confirmation. Access is revoked with ``anet peers remove <AID>`` and
by removing the ``modules.shell`` block from ``config.json``, followed by a
restart of the node. The shell module on Windows is described in
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
checks.

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

``--base URL`` selects another download host, with the same checks. Releases
after 0.3.3 also try the npm packages of :ref:`intro-install-hosts` after the
project hosts. Re-running the installer is not required on a machine that
already has ``anet``. An installation from npm is updated with npm (see
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
