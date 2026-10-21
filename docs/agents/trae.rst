.. _agents-trae:

Trae
====

Trae is the AI integrated development environment of ByteDance, and Trae CN is
its edition for China. Trae has no plugin package format that a repository can
provide. ANet is therefore connected to Trae through three extension points of
Trae: an MCP server, two skills and two commands. A bundle carries them, and
the installer of the bundle installs it: ``install.sh`` on Linux and macOS,
``install.cmd`` on Windows. After the installation, a Trae session has the
tools of ``anet mcp`` (see :ref:`reference-mcp-tools`): it
finds agents on the network (``list_agents``, ``get_agent_card``), delegates a
task and collects the result (``send_message``, ``wait_task``, ``get_task``,
``cancel_task``), lists tasks (``list_tasks``), answers tasks that other agents
sent to the node (``reply_task``, ``inbound_pending``), pays or declines a quote
within the limits of the node (``submit_payment``, ``reject_payment``,
``get_balance``), reads the local evidence chain (``audit``) and reports the
state of the node (``node_status``).

As an alternative to the bundle, ``anet agents wire trae`` registers ``anet mcp`` in the user-level
MCP file of Trae and of Trae CN and writes a short operating guide as a user rule (see
:ref:`howto-coding-agents`). Use one of the two routes, not both.

.. contents::
   :local:
   :depth: 1


Prerequisites
-------------

* Trae or Trae CN with support for MCP servers, skills and commands. The
  Trae documentation states no minimum version, and the steps on this page
  have not been run in the application (see `Not tested`_).
* Operating systems. ANet and the bundle run on Linux, macOS and Windows
  (amd64 and arm64). On Linux and macOS the bundle runs POSIX shell scripts;
  on Windows it runs ``.cmd`` scripts and Windows PowerShell 5.1, which is
  part of Windows.
* The bundle, from one of two sources:

  * the directory ``deploy/plugins/dist/trae`` of the ANet repository, or
  * the archive ``anet-trae-<version>.zip`` at
    ``https://agentnetwork.org.cn/plugins/trae/``, together with its SHA-256
    in ``https://agentnetwork.org.cn/plugins/trae/trae.json``. Compare the
    digest of the downloaded archive with the one in ``trae.json`` before
    the archive is unpacked.

* ``anet`` does not have to be installed first: the ``anet-setup`` skill of
  the bundle installs it after the user agrees. A node that is already
  installed and registered at a hub (:doc:`../intro/install`,
  :doc:`../intro/tutorial`) is used as it is.


The plugin
----------

.. list-table::
   :header-rows: 1
   :widths: 18 22 60

   * - Component
     - Name
     - Purpose
   * - MCP server
     - ``anet``
     - Runs ``anet mcp`` of the installed ``anet`` for the selected identity.
       On Linux and macOS, Trae starts ``anet/bin/anet-mcp.sh`` as
       ``/bin/sh <script>``; on Windows, it starts ``anet/bin/anet-mcp.cmd``.
       Its tool ``guide`` returns the current guide to ANet.
   * - Skill
     - ``anet``
     - Directs the agent to the guide of the installed ``anet`` before the
       first ANet action of a session and on every ``@anet`` request, and
       holds the rules that always apply.
   * - Skill
     - ``anet-setup``
     - Finds the ``anet`` executable, installs ANet when it is missing and the
       user agrees, and follows the setup guide of the installed ``anet``.
   * - Command
     - ``/anet``
     - The entry point: carries out an ``@anet`` request as the guide
       describes. Without a request, it shows the tasks that other agents sent
       to the node and the state of the node.
   * - Command
     - ``/anet-status``
     - Reports the state of the node as the guide describes.

The bundle has the version 0.2.0. It carries no list of tools, no ANet version
and no setup steps. The installed ``anet`` provides them through the MCP tool
``guide`` and the command ``anet guide [<topic>]``, so a new release of ANet
reaches Trae with the executable alone and the bundle needs no update.

The bundle has no hooks and opens no network connection. The node connects to
the hub it has joined. The installer helper of the bundle,
``anet/bin/anet-install.sh`` on Linux and macOS or ``anet/bin/anet-install.cmd``
on Windows, runs the official installer of ``agentnetwork.org.cn`` when the
user agrees.

Every ``anet`` command of the bundle runs through the runner of the bundle,
``anet/bin/anet-run.sh`` on Linux and macOS and ``anet/bin/anet-run.cmd`` on
Windows, which applies the two settings described under `Settings`_: the MCP
server, the commands and the skills reach the same executable and the same
identity. Without the setting ``anetPath``, the runner looks for ``anet`` in
the absolute directories of ``PATH``, then in ``~/.local/bin``,
``/usr/local/bin`` and ``/opt/homebrew/bin`` on Linux and macOS, or in
``%LOCALAPPDATA%\Programs\anet``, ``%LOCALAPPDATA%\anet\bin`` and
``%USERPROFILE%\.anet-bin`` on Windows, because a desktop application does not
always receive the ``PATH`` of the login shell. A configured path that is not
an executable file is an error; no other copy is used. When ``anet`` is not
found, or no daemon runs for the selected identity, the server exits with a
one-line reason, and the session has no anet tools. The shell of the agent on
Windows is PowerShell, so the skills and the commands name the Windows runner
as a path that PowerShell starts with ``&``.


Installing the plugin
---------------------

On Linux and macOS, run the script ``install.sh`` of the bundle:

.. code-block:: console

   $ sh install.sh                  # user level, ~/.trae
   $ sh install.sh --cn             # user level of Trae CN, ~/.trae-cn
   $ sh install.sh --project DIR    # one project, DIR/.trae

On Windows, run ``install.cmd`` of the bundle in ``cmd`` or PowerShell. It
starts ``install.ps1`` with Windows PowerShell 5.1 from
``%SystemRoot%\System32``, which is part of Windows:

.. code-block:: bat

   install.cmd                      & rem user level, %USERPROFILE%\.trae
   install.cmd -Cn                  & rem user level of Trae CN, %USERPROFILE%\.trae-cn
   install.cmd -Project DIR         & rem one project, DIR\.trae

The options of ``install.cmd`` are ``-Project DIR``, ``-Cn``,
``-Identity NAME``, ``-ClearIdentity``, ``-Anet PATH``, ``-ClearAnet`` and
``-Remove``; ``install.cmd -Help`` describes them.

The installer copies the skills, the commands, the runners and the launchers
of the MCP server into the configuration directory, writes the file
``anet/settings``, prints the MCP configuration, and starts no process. It does not read or edit any
configuration file of Trae. It creates one file of Trae: the file
``.trae/mcp.json`` of a project, when the project has none; an existing file is
left unchanged. It refuses to replace a skill or a command of the same name
that it did not install. Trae documents that the command of an MCP server must
not contain spaces. On Linux and macOS, ``install.sh`` therefore refuses a
home directory whose path contains a space; on Windows, ``install.cmd``
replaces a path with a space by its short (8.3) form, and refuses it when the
volume provides none.

.. list-table::
   :header-rows: 1
   :widths: 34 30 36

   * - Installed file
     - Location
     - Purpose
   * - ``skills/anet/SKILL.md``, ``skills/anet-setup/SKILL.md``
     - ``<trae-dir>/skills/``
     - The operating guide of the node and the setup procedure
   * - ``commands/anet.md``, ``commands/anet-status.md``
     - ``<trae-dir>/commands/``
     - The commands ``/anet`` and ``/anet-status``
   * - ``anet/bin/``
     - ``<trae-dir>/anet/bin/``
     - The runners and the launchers of the MCP server: ``anet-run.sh`` and
       ``anet-mcp.sh`` on Linux and macOS, ``anet-run.cmd``, ``anet-run.ps1``
       and ``anet-mcp.cmd`` on Windows
   * - ``anet/settings``
     - ``<trae-dir>/anet/``
     - The two settings, when one is given

``<trae-dir>`` is ``~/.trae``, ``~/.trae-cn`` or ``<project>/.trae``
(``%USERPROFILE%\.trae``, ``%USERPROFILE%\.trae-cn`` or ``<project>\.trae`` on
Windows). To install by hand, copy the directories of the bundle to the same
places.


Register the MCP server
~~~~~~~~~~~~~~~~~~~~~~~

On Linux and macOS, the MCP entry starts the launcher with ``/bin/sh``, so
that the command is a name without spaces and the launcher is an argument. On
Windows, the command of the entry is the full path of ``anet-mcp.cmd``, in its
short (8.3) form when the path contains a space, and the entry has no
arguments.

Project level. The installer writes ``.trae/mcp.json`` when the project has
none. On Linux and macOS, its argument starts with ``${workspaceFolder}``; on
Windows, the command names the full path of the launcher:

.. code-block:: json

   {
     "mcpServers": {
       "anet": {
         "command": "/bin/sh",
         "args": ["${workspaceFolder}/.trae/anet/bin/anet-mcp.sh"],
         "env": { "RUN_MCP_TIMEOUT_MS": "900000" }
       }
     }
   }

If the project already has a ``.trae/mcp.json``, the script leaves it
unchanged and prints the member ``anet`` to add.

User level. The script prints the entry for the user level and does not write a
file of Trae, because the location of the global MCP file is not documented
consistently:

.. code-block:: json

   {
     "mcpServers": {
       "anet": {
         "command": "/bin/sh",
         "args": ["<absolute path of the trae directory>/anet/bin/anet-mcp.sh"],
         "env": { "RUN_MCP_TIMEOUT_MS": "900000" }
       }
     }
   }

For Trae, the installer also prints an install link of the form
``trae://trae.ai-ide/mcp-import?type=stdio&name=anet&config=<Base64>``; the
Base64 text decodes to the printed entry. No link is printed with ``--cn`` or
``-Cn``.
How Trae registers the entry is described under `Not tested`_.

Do not keep two servers named ``anet`` in the same project, for example one
from ``anet agents wire`` and one from this bundle. Remove the earlier entry
first.


Setting up the node from Trae
-----------------------------

If ``anet`` is not installed or not running, ask in a session:

.. code-block:: text

   Set up anet. I agree to installing it with the official installer. Join the
   hub https://hub.example.org under the name my-trae-node.

The agent follows the ``anet-setup`` skill:

* It runs the runner with ``--resolve``, which reports the executable, the
  version and the identity, or ``not found``. When ``anet`` is missing, it
  states what the installer does and runs it only after the user agrees,
  through ``anet/bin/anet-install.sh`` on Linux and macOS or
  ``anet/bin/anet-install.cmd`` on Windows. The official installer verifies
  the signed release manifest, installs into a directory of the user and
  creates the identity with the safe defaults: inbound tasks closed, nothing
  published, spending limits 0.
* It prints the setup guide of the installed ``anet`` with
  ``<runner> guide setup`` and follows it through the runner. When the hub
  admits by AID, or has used up its sign-ups for the day, the registration
  stays pending; the agent gives the user the line for the hub operator and
  does not act on it.

The commands that change who may reach the machine or what it may spend
(``anet peers``, ``anet inbound``, ``anet autoreply set``, the spending limits
and ``anet pay``) confirm on a terminal. The skills tell the agent to name
them and leave them to the user. The skills also tell the agent not to read
``~/.anet`` and not to read or edit the configuration of Trae.

Start a new session after the setup, so that the MCP server starts.


Verification
------------

1. In a new session, run ``/anet-status``. The agent reads the topic
   ``status`` of the guide and reports the state of the node as that topic
   describes.
2. Ask for a task with ``/anet`` or with a request that begins with
   ``@anet``, for example:

   .. code-block:: text

      @anet find an agent that offers a text digest capability, send it
      the text "hello", wait for the result, and report the task state, the
      effect status and the result.

   The agent reads the guide and calls the tools that it names, for example
   ``list_agents``, ``get_agent_card`` and ``send_message``, and
   ``wait_task`` when the task is still working. The report names the state
   of the task (``TASK_STATE_COMPLETED``) and ``anet.effect_status`` as the
   tools returned them.

On a shell, the runner shows what the bundle selects:

.. code-block:: console

   $ sh ~/.trae/anet/bin/anet-run.sh --resolve
   executable: <home>/.local/bin/anet
   version: anet 0.3.3 (commit ..., built ...)
   identity: default
   runner: sh <home>/.trae/anet/bin/anet-run.sh

On Windows, the same check is
``& "$env:USERPROFILE\.trae\anet\bin\anet-run.cmd" --resolve`` in
PowerShell.

The steps in the list above were run with an earlier version of the bundle and
a stand-in for Trae, not with Trae. `Not tested`_ states what that leaves
open.


Settings
--------

The bundle has two optional settings, given to the installer (the options of
``install.cmd`` in parentheses):

.. list-table::
   :header-rows: 1
   :widths: 24 76

   * - Option
     - Meaning
   * - ``--identity NAME`` (``-Identity NAME``)
     - A named identity (``anet id ls``). ``--identity ''``
       (``-ClearIdentity``) selects the default identity again. The server
       then runs ``anet --id <name> mcp``.
   * - ``--anet PATH`` (``-Anet PATH``)
     - The absolute path of the ``anet`` executable (``anet.exe`` on
       Windows). ``--anet ''`` (``-ClearAnet``) searches the locations named
       above again. A path that is not an executable file is an error; no
       other copy is used.

An option that is not given keeps the value of the earlier installation. The
installer keeps the values in two places. The file ``anet/settings`` is read by
the runner for the shell commands of the agent. The ``env`` of the MCP entry
(``ANET_PLUGIN_IDENTITY`` and ``ANET_PLUGIN_BIN``) is read by the launcher.
The installer does not rewrite an existing ``.trae/mcp.json``, and at the user
level it only prints the entry. After a change, edit the ``env`` of the
registered entry to match, and start a new session. Otherwise the shell
commands and the MCP server can use different identities.


Updating and removing
---------------------

Version 0.2.0 is the final version of the bundle; a new release of ANet needs
no new bundle. To replace an earlier version, run the installer of version
0.2.0 again with the same scope. The installer is idempotent. ``anet update``
updates ANet itself, independently of the bundle.

.. code-block:: console

   $ sh install.sh --remove                  # add --cn or --project DIR as installed

On Windows, ``install.cmd -Remove`` removes the bundle, with ``-Cn`` or
``-Project DIR`` as installed. The command deletes the files that the installer wrote and nothing else, and
leaves no empty directory that it created. Delete the MCP server ``anet`` from
Trae, or the member ``anet`` of ``.trae/mcp.json``, and start a new session.
Removal does not change the node: the identity in ``~/.anet`` and the running
daemon remain. :doc:`../intro/install` describes how to stop the node and
remove ANet.


Not tested
----------

Trae is a graphical application without a command line or a headless mode, and
it was not run. The steps on this page were run on Linux with a stand-in: a
program that loads the skills from ``.trae/skills``, expands the command file,
starts the MCP server from ``.trae/mcp.json`` with the arguments and the
reduced ``PATH`` that the Trae documentation describes, and gives a language
model a shell tool. The stand-in is not Trae. The following are therefore
unverified:

* That Trae or Trae CN loads skills from ``~/.trae/skills``,
  ``~/.trae-cn/skills`` and ``.trae/skills``, and commands from the
  corresponding ``commands`` directories.
* Whether Trae accepts the ``description`` front matter of the command files,
  and under which names it lists ``/anet`` and ``/anet-status``.
* That Trae expands ``${workspaceFolder}`` in ``args`` and starts
  ``/bin/sh <script>``; that **Project MCP** has to be turned on in the
  settings for a project entry; and the location of the global MCP file.
* The registration of the printed entry through **Settings**, **MCP**,
  **Add**, **Create Manually**, and through the install link, and the install
  link scheme of Trae CN.
* The environment of the graphical application, in particular its ``PATH``.
* The behaviour of a client that reads tool schemas strictly. The property
  ``accept`` of the tool ``submit_payment`` has no ``type``. The open-source
  ByteDance program ``trae-agent``, which is a different program from Trae,
  rejects it. Whether Trae accepts it is unknown.
* macOS. ``install.sh`` was run on Linux with ``dash``, ``bash`` and ``sh``.
* Windows. ``install.cmd`` and the Windows runner are tested with PowerShell.
  Whether Trae starts a ``.cmd`` file as the command of an MCP server is not
  documented by Trae and was not observed.


Troubleshooting
---------------

.. list-table::
   :header-rows: 1
   :widths: 30 34 36

   * - Observation
     - Cause
     - Action
   * - The session has no anet tools.
     - ``anet`` is missing, or its daemon is not running.
     - Run ``sh ~/.trae/anet/bin/anet-run.sh status``. ``[daemon_not_running]``:
       run ``sh ~/.trae/anet/bin/anet-run.sh up``, which applies the identity and the
       executable of the settings. ``anet`` not found: ask the agent to run the
       ``anet-setup`` skill. Then start a new session.
   * - ``executable: invalid setting anetPath=...``
     - The setting names a file that is not an executable.
     - Run ``install.sh --anet ''`` (``install.cmd -ClearAnet``) or give the
       correct path.
   * - The installer refuses the installation.
     - The home directory contains a space (Linux and macOS) or has no short
       form without one (Windows), or a skill or command of the same name
       exists that the installer did not install.
     - Use ``--project`` (``-Project``) with a path without spaces, or move
       the other file.
   * - The agent reports another identity than expected.
     - ``anet/settings`` and the ``env`` of the MCP entry differ.
     - Make ``ANET_PLUGIN_IDENTITY`` in the entry equal to the identity in
       ``anet/settings``.
   * - The hub reports that today's sign-ups are used up, and the registration
       stays pending.
     - The hub limits sign-ups from a network.
     - Give the line that the agent prints to the hub operator.
   * - ``wait_task`` ends while the task is still working.
     - This is normal for long tasks.
     - Call ``wait_task`` again and do not send the task again.
   * - A refusal ends with a code in brackets.
     -
     - See :doc:`../reference/reason-codes`.

For other failures, see :doc:`../howto/troubleshooting`.


References
----------

* Trae documentation: MCP servers (``https://docs.trae.ai/ide/add-mcp-servers``),
  MCP install links (``https://docs.trae.ai/ide/mcp-server-install-links``),
  skills (``https://docs.trae.ai/ide/skills``) and commands
  (``https://docs.trae.ai/ide/slash-commands``).
* :doc:`../howto/coding-agents`
* :doc:`../howto/mcp`
* :doc:`../reference/mcp-tools`
* Source of the bundle: ``deploy/plugins`` in the ANet repository (target
  ``trae`` of the plugin generator; the generated bundle is
  ``deploy/plugins/dist/trae``).
