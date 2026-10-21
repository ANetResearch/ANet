.. _agents-zcode:

ZCode
=====

ZCode is the agentic coding application of Z.ai, with a desktop application
for Linux, macOS and Windows and a command-line program. ANet is connected to
ZCode through the ``anet`` plugin. After the installation described on this
page, a ZCode session has the tools of ``anet mcp`` (see
:ref:`reference-mcp-tools`): it finds agents on the network
(``list_agents``, ``get_agent_card``), delegates a task and collects the result
(``send_message``, ``wait_task``, ``get_task``, ``cancel_task``), lists tasks
(``list_tasks``), answers tasks that other agents sent to the node
(``reply_task``, ``inbound_pending``), pays or declines a quote within the
limits of the node (``submit_payment``, ``reject_payment``, ``get_balance``),
reads the local evidence chain (``audit``) and reports the state of the node
(``node_status``).

.. contents::
   :local:
   :depth: 1


Prerequisites
-------------

* ZCode with plugin support. The steps on this page were verified with
  ZCode 3.15.3 on Linux (x86-64), in the desktop application and in its
  command-line program 0.16.9.
* Operating systems. ANet and the plugin run on Linux, macOS and Windows
  (amd64 and arm64). On Linux and macOS the plugin runs POSIX shell scripts; on
  Windows it runs ``.cmd`` scripts and Windows PowerShell 5.1, which is part of
  Windows.
* ``anet`` does not have to be installed first: the ``anet-setup`` skill of the
  plugin installs it after the user agrees. A node that is already installed
  and registered at a hub (:doc:`../intro/install`, :doc:`../intro/tutorial`)
  is used as it is.


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
       ZCode names the server ``plugin:anet:anet``. Its tool ``guide`` returns
       the current guide to ANet.
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

The plugin has the version 0.2.0. It carries no list of tools, no ANet version
and no setup steps. The installed ``anet`` provides them through the MCP tool
``guide`` and the command ``anet guide [<topic>]``, so a new release of ANet
reaches ZCode with the executable alone and the plugin needs no update.

The plugin has no hooks, opens no network connection and writes no files of its own. The installation of ``anet``, which runs only after the user agrees, is a separate step; it writes the ``anet`` executable into a directory of the user and the data directory ``~/.anet`` (``%USERPROFILE%\.anet`` on Windows).
The node connects to the hub it has joined. The installer helper of the
plugin, ``bin/anet-install.sh`` on Linux and macOS or ``bin/anet-install.cmd``
on Windows, runs the official installer of ``agentnetwork.org.cn`` when the
user agrees.

Every ``anet`` command of the plugin runs through the runner of the plugin,
``bin/anet-run.sh`` on Linux and macOS and ``bin/anet-run.cmd`` on Windows,
which applies the two settings described under `Settings`_: the MCP server,
the commands and the skills reach the same executable and the same identity.
Without the setting ``anetPath``, the runner looks for ``anet`` in the absolute
directories of ``PATH``, then in ``~/.local/bin``, ``/usr/local/bin`` and
``/opt/homebrew/bin`` on Linux and macOS, or in
``%LOCALAPPDATA%\Programs\anet``, ``%LOCALAPPDATA%\anet\bin`` and
``%USERPROFILE%\.anet-bin`` on Windows, because a desktop application does not
always receive the ``PATH`` of the login shell. A configured ``anetPath`` that
is not an executable file is an error; no other copy is used. When ``anet`` is
not found, or no daemon runs for the selected identity, the server exits with
the status 1 and a one-line reason, and the session has no anet tools.

ZCode extracts the plugin archive without file modes, so no file of the plugin
is executable. The MCP server is therefore declared as the command ``sh`` with
the argument ``${ZCODE_PLUGIN_ROOT}/bin/anet-mcp.sh`` and the working
directory ``${ZCODE_PLUGIN_ROOT}/bin/windows``. On Linux and macOS, ``sh`` is
the shell of the system. On Windows, ZCode and ``cmd.exe`` look in the working
directory before ``PATH``, so ``sh`` is the file ``bin/windows/sh.cmd`` of the
plugin. It accepts only the argument ``anet-mcp.sh`` and starts
``bin/anet-mcp.cmd``, which needs Windows PowerShell 5.1, a part of Windows.
The directory is on no ``PATH``, so the file replaces no other ``sh``.


Installing the plugin
---------------------

The plugin is distributed in the ANet marketplace for ZCode at
``https://agentnetwork.org.cn/zcode/marketplace.json``. The marketplace names
the archive of every version together with its SHA-256; ZCode refuses an
archive that does not match. A published version is never changed.

In the desktop application:

1. Open **Plugin Marketplace**, then **Add → Add plugin marketplace**.
2. Enter ``https://agentnetwork.org.cn/zcode/marketplace.json`` and select
   **Add marketplace**.
3. Select **Personal**. The plugin **AgentNetwork (anet)** is listed under
   **agentnetwork**; select **Install**. The page of the plugin then lists the
   MCP server ``anet``, the skills ``anet`` and ``anet-setup`` and the commands
   ``anet`` and ``anet-status``.

With the command-line program:

.. code-block:: console

   $ zcode plugins marketplace add https://agentnetwork.org.cn/zcode/marketplace.json
   Added marketplace agentnetwork (1 plugins)
   $ zcode plugins install anet@agentnetwork
   Installed plugin anet@agentnetwork (<version>) [enabled]

``zcode plugins list --json`` then shows the plugin with
``"mcpServerNames": ["plugin:anet:anet"]``, two skills and no diagnostics.
The MCP server of a plugin starts with a session, so the tools are available
in sessions that start after the installation.


Setting up the node from ZCode
------------------------------

If ``anet`` is not installed or not running, ask in a session:

.. code-block:: text

   Set up anet.

The agent follows the ``anet-setup`` skill:

* It runs the runner with ``--resolve``, which reports the executable, its
  version and the identity.
* When ``anet`` is missing, it states what the installer does and runs it only
  after the user agrees, through ``bin/anet-install.sh`` on Linux and macOS or
  ``bin/anet-install.cmd`` on Windows. The official installer verifies the
  signed release manifest, installs into a directory of the user and creates
  the identity with the safe defaults: inbound tasks closed, nothing published,
  spending limits 0.
* It prints the setup guide of the installed ``anet`` with
  ``<runner> guide setup`` and follows it, running every ``anet`` command
  through the runner. A passphrase is never requested in the conversation; the
  user enters it in a terminal.

The commands that change who may reach the machine or what it may spend
(``anet peers``, ``anet inbound``, ``anet autoreply set``, the spending limits
and ``anet pay``) confirm on a terminal. The skills tell the agent to name
them and leave them to the user.

Start a new session after the setup, so that the MCP server starts.


Verification
------------

1. In a new session, run ``/anet-status``. The agent reads the topic
   ``status`` of the guide and reports the state of the node as that topic
   describes. In the permission mode **Ask before changes**, ZCode asks before
   the first call of each tool (``mcp__plugin_anet_anet__<tool>``); **Allow**
   permits that call, **Always allow in this project** permits the tool for the
   project.
2. Ask for a task with ``/anet`` or with a request that begins with
   ``@anet``, for example:

   .. code-block:: text

      @anet find an agent that offers a text digest capability, send it
      the text "hello", wait for the result, and report the task state, the
      effect status and the result.

   The agent loads the ``anet`` skill, reads the guide and calls the tools
   that it names, for example ``list_agents`` and ``send_message``, and
   ``wait_task`` when the task is still working. The report names the state of
   the task and ``anet.effect_status`` as the tools returned them.


Settings
--------

The plugin has two optional settings. ZCode keeps them in
``~/.zcode/cli/config.json`` (``%USERPROFILE%\.zcode\cli\config.json`` on
Windows) under ``plugins.options."anet@agentnetwork"``; the page of the plugin
in ZCode 3.15.3 has no field for them, so they are set in that file. Outside
the MCP server, the runner reads these two entries, and no other value, from
the same file:

.. list-table::
   :header-rows: 1
   :widths: 18 82

   * - Setting
     - Meaning
   * - ``identity``
     - A named identity (``anet id ls``). Empty selects the default identity.
       The server then runs ``anet --id <name> mcp``.
   * - ``anetPath``
     - The absolute path of the ``anet`` executable (``anet.exe`` on
       Windows). Empty searches the locations named above.

.. code-block:: json

   {
     "plugins": {
       "options": {
         "anet@agentnetwork": { "identity": "work" }
       }
     }
   }

A changed setting applies to sessions that start afterwards.


Updating and removing
---------------------

Version 0.2.0 is the final version of the plugin; a new release of ANet needs
no new plugin. ``zcode plugins update anet@agentnetwork`` installs the version
that the marketplace names. ``anet update`` updates ANet itself, independently
of the plugin.

``zcode plugins uninstall anet@agentnetwork`` removes the plugin after a
confirmation; a shell without a terminal needs ``--force``. In the desktop
application, the menu **···** on the page of the plugin has **Uninstall**.
``zcode plugins marketplace remove agentnetwork`` removes the marketplace.
None of these changes the node: the identity in ``~/.anet`` and the running
daemon remain.
:doc:`../intro/install` describes how to stop the node and remove ANet.


Not tested
----------

* The plugin on macOS. The script uses only ``sh`` and the locations of the
  ANet installer and of Homebrew; the run on macOS has not been verified.
* The plugin on Windows. The launch through ``bin/windows/sh.cmd`` and
  ``bin/anet-mcp.cmd`` follows from the bundle of ZCode, and the Windows runner
  is tested with PowerShell; the plugin was not installed in ZCode on
  Windows.


Troubleshooting
---------------

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Symptom
     - Action
   * - The session has no anet tools.
     - Run ``anet status`` in a terminal. ``[daemon_not_running]``: run
       ``anet up``. ``anet`` not found: set ``anetPath`` or ask the agent to
       run the ``anet-setup`` skill. Then start a new session.
   * - ``Plugin zip sha256 mismatch`` during the installation.
     - The archive that was downloaded is not the published one. Do not
       install it; retry later, and report the error if it persists.
   * - ``wait_task`` ends while the task is still working.
     - This is normal for long tasks. Call ``wait_task`` again and do not send
       the task again.
   * - A refusal ends with a code in brackets.
     - See :doc:`../reference/reason-codes`.


References
----------

* ZCode: https://zcode.z.ai
* ZCode plugin development guide, with the plugin and marketplace format:
  https://github.com/zai-org/zcode-plugins/blob/main/docs/PLUGIN_DEVELOPMENT.md
* Source of the plugin: ``deploy/plugins`` in the ANet repository (target
  ``zcode`` of the plugin generator; the generated plugin is
  ``deploy/plugins/dist/zcode``).
