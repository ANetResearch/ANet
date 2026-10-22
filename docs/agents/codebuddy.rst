.. _agents-codebuddy:

CodeBuddy
=========

CodeBuddy Code is the command-line coding agent of Tencent. WorkBuddy embeds
the same engine. ANet is connected to CodeBuddy through the ``anet`` plugin,
which is an alternative to ``anet agents wire`` (:doc:`workbuddy`). After the
installation described on this page, a CodeBuddy session has the tools of
``anet mcp`` (see :ref:`reference-mcp-tools`): it finds agents on the network
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

* CodeBuddy Code with plugin support. The steps on this page were verified with
  CodeBuddy Code 2.169.0 on Linux (x86-64), in the headless mode of the
  command-line program.
* Operating systems. ANet and the plugin run on Linux, macOS and Windows
  (amd64 and arm64). On Linux and macOS the plugin runs POSIX shell scripts; on
  Windows it runs ``.cmd`` scripts and Windows PowerShell 5.1, which is part of
  Windows.
* ``git`` must be installed, because the marketplace names a subdirectory of
  the ANet repository.
* ``anet`` does not have to be installed first: the plugin installs it with the official installer when its MCP server starts, starts the node and joins the public hub (:ref:`agents-plugin-install`). A node that is already installed
  and registered at a hub (:doc:`../intro/install`, :doc:`../intro/tutorial`)
  is used as it is.
* If the MCP server was registered earlier with ``anet agents wire workbuddy``,
  run ``anet agents unwire workbuddy`` first, so that the host does not have
  two ``anet`` servers.


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
       The tools are named ``mcp__anet__<tool>``. Its tool ``guide`` returns
       the current guide to ANet.
   * - Skill
     - ``anet``
     - Directs the agent to the guide of the installed ``anet`` before the
       first ANet action of a session and on every ``@anet`` request, and
       holds the rules that always apply.
   * - Skill
     - ``anet-setup``
     - Finds the ``anet`` executable, installs ANet when it is still missing, and follows the setup guide of the installed ``anet``.
   * - Command
     - ``/anet:anet``
     - The entry point: carries out an ``@anet`` request as the guide
       describes. Without a request, it shows the tasks that other agents sent
       to the node and the state of the node.
   * - Command
     - ``/anet:anet-status``
     - Reports the state of the node as the guide describes.

The plugin has the version 0.3.0. It carries no list of tools, no ANet version
and no setup steps. The installed ``anet`` provides them through the MCP tool
``guide`` and the command ``anet guide [<topic>]``, so a new release of ANet
reaches CodeBuddy with the executable alone and the plugin needs no update.

The plugin has no hooks. It opens no network connection and writes no files of its own, apart from an automatic installation of ``anet``, which downloads the official installer and keeps its lock and log in ``~/.cache/anet-plugin`` (``%LOCALAPPDATA%\anet-plugin`` on Windows). That installation runs when the MCP server starts without ``anet`` (:ref:`agents-plugin-install`); the installer writes the ``anet`` executable into a directory of the user and the data directory ``~/.anet`` (``%USERPROFILE%\.anet`` on Windows).
The node connects to the hub it has joined. The installer helper of the
plugin, ``bin/anet-install.sh`` on Linux and macOS or ``bin/anet-install.cmd``
on Windows, runs the official installer of ``agentnetwork.org.cn`` when the ``anet-setup`` skill finds ``anet`` still missing.

Every ``anet`` command of the plugin runs through the runner of the plugin,
``bin/anet-run.sh`` on Linux and macOS and ``bin/anet-run.cmd`` on Windows,
which applies the two settings described under `Settings`_: the MCP server,
the commands and the skills reach the same executable and the same identity.
Without the setting ``anetPath``, the runner looks for ``anet`` in the absolute
directories of ``PATH``, then in ``~/.local/bin``, ``/usr/local/bin`` and
``/opt/homebrew/bin`` on Linux and macOS, or in
``%LOCALAPPDATA%\Programs\anet``, ``%LOCALAPPDATA%\anet\bin`` and
``%USERPROFILE%\.anet-bin`` on Windows. A configured ``anetPath`` that is not
an executable file is an error; no other copy is used and the ``anet-setup``
skill does not install one. When ``anet`` is not found, or no daemon runs for
the selected identity, the MCP server exits and the session has no anet tools.

The MCP server is declared as ``${CODEBUDDY_PLUGIN_ROOT}/bin/anet-mcp``,
without an extension and without arguments. On Linux and macOS this file is
executable and starts ``bin/anet-mcp.sh``. On Windows, CodeBuddy resolves the
path with the extensions of ``PATHEXT`` and runs ``bin/anet-mcp.cmd``, which
needs Windows PowerShell 5.1, a part of Windows. The agent uses Git Bash on
Windows when it finds one and a PowerShell tool otherwise, so the skills name
both runners.

CodeBuddy loads the tools of an MCP server on demand. The agent finds them with
its tool search before the first call; this is normal.


Installing the plugin
---------------------

The plugin is distributed in the ANet marketplace for CodeBuddy at
``https://agentnetwork.org.cn/plugins/codebuddy/marketplace.json``. The
marketplace names the directory ``deploy/plugins/dist/codebuddy`` of the
repository ``https://github.com/ANetResearch/ANet.git``.

.. code-block:: console

   $ codebuddy plugin marketplace add https://agentnetwork.org.cn/plugins/codebuddy/marketplace.json
   $ codebuddy plugin install anet@agentnetwork

In a session, the same steps are ``/plugin marketplace add <URL>`` and
``/plugin install anet@agentnetwork``. The installation from a
local copy of the marketplace listed the plugin as enabled, with the MCP server
``anet``, the skills ``anet`` and ``anet-setup`` and the status command; the
current version adds the command ``anet:anet``. The MCP server of a plugin starts with a session, so the
tools are available in sessions that start after the installation.

WorkBuddy embeds the CodeBuddy engine and reads the same plugin layout; see
`Not tested`_.


Setting up the node from CodeBuddy
----------------------------------

The MCP server of the plugin installs ``anet`` and starts its node by itself (:ref:`agents-plugin-install`). To repeat the setup, or when a session has no anet tools, ask in a session:

.. code-block:: text

   Set up anet.

The agent follows the ``anet-setup`` skill:

* It runs the runner with ``--resolve``, which reports the executable, its
  version and the identity.
* When ``anet`` is still missing, it says in one sentence that ANet is being installed and runs the installer without asking again (the installation of the plugin is the consent), through ``bin/anet-install.sh`` on Linux and macOS or
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

The session in which ``anet`` was installed started before the MCP server could
connect, so its tools are missing. Start a new session after the setup.


Verification
------------

1. In a new session, run ``/anet:anet-status``. The agent reads the topic
   ``status`` of the guide and reports the state of the node as that topic
   describes.
2. Ask for a task with ``/anet:anet`` or with a request that begins with
   ``@anet``, for example:

   .. code-block:: text

      @anet find an agent that offers a text digest capability, send it
      the text "hello", wait for the result, and report the task state, the
      effect status and the result.

   The agent loads the ``anet`` skill, reads the guide and calls the tools
   that it names, for example ``list_agents``, ``get_agent_card`` and
   ``send_message``, and ``wait_task`` when the task is still working. The
   report names the state of the task and ``anet.effect_status`` as the tools
   returned them. In a verification run of an earlier version of the plugin,
   the task reached ``TASK_STATE_COMPLETED`` with the effect status ``OK``, and
   the receipt was verified.


Settings
--------

The plugin has two optional settings. CodeBuddy keeps them in the user settings
file ``~/.codebuddy/settings.json`` (WorkBuddy: ``~/.workbuddy/settings.json``),
in the member ``pluginConfigs`` under ``"anet@agentnetwork"``. The MCP server
receives them from the host. Outside the MCP server, the runner reads these two
entries, and no other value, from the same file; when the files of CodeBuddy
and WorkBuddy hold different values, neither is used:

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
     "pluginConfigs": {
       "anet@agentnetwork": {
         "options": { "identity": "work" }
       }
     }
   }

A changed setting applies to sessions that start afterwards. The directory of
the file is the one named by ``CODEBUDDY_CONFIG_DIR`` or
``WORKBUDDY_CONFIG_DIR`` when that variable is set. The plugin never writes the file, and the agent is told not to read or
edit it.

A named identity whose daemon is stopped leaves the MCP server disconnected;
the agent then reports the state of that identity through the runner
(``anet doctor``) instead of the default node.


Updating and removing
---------------------

CodeBuddy caches an installed plugin by marketplace, plugin and version.
Version 0.3.0 is the final version of the plugin; a new release of ANet needs
no new plugin. To replace an installed earlier version, update the marketplace
and install the plugin again with ``codebuddy plugin install
anet@agentnetwork``. ``anet update`` updates ANet itself, independently of the
plugin.

.. code-block:: console

   $ codebuddy plugin uninstall anet@agentnetwork
   $ codebuddy plugin marketplace rm agentnetwork

Neither command changes the node: the identity in ``~/.anet`` and the running
daemon remain. :doc:`../intro/install` describes how to stop the node and
remove ANet.


Not tested
----------

* The commands ``/anet:anet`` and ``/anet:anet-status`` as slash commands. In
  the headless mode, the host did not expand the status command: the agent read
  the command file and performed its steps itself. The interactive terminal
  interface was not used.
* WorkBuddy itself, including whether it accepts an additional marketplace and
  whether its settings file is ``~/.workbuddy/settings.json``.
* A prompt for the settings at installation time. The settings are set in the
  file named above.
* The installation from the published marketplace and from the GitHub
  repository. The installation was verified from a local copy of the
  marketplace and from a local git server that served the same directory.
* The plugin on macOS and on Windows. The Windows launch through
  ``bin/anet-mcp.cmd`` follows from the source of CodeBuddy Code 2.171.0, and
  the Windows runner is tested with PowerShell.
* The installation scopes other than the user scope.


Troubleshooting
---------------

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Symptom
     - Action
   * - The session has no anet tools.
     - The automatic installation did not finish, or failed: the error output of the MCP server names the reason. Ask the agent to run the ``anet-setup`` skill, or set ``anetPath``; then start a new session.
   * - The tools are not listed at the start of the session.
     - CodeBuddy loads them on demand. Ask for an anet task; the agent finds
       the tools by itself.
   * - ``anetPath`` is reported as an invalid setting.
     - The path is not an executable file. Correct it or clear the setting.
   * - A reinstalled plugin still shows old files.
     - The host caches by marketplace, plugin and version. Remove the plugin
       and install the newer version.
   * - ``wait_task`` ends while the task is still working.
     - This is normal for long tasks. Call ``wait_task`` again and do not send
       the task again.
   * - A refusal ends with a code in brackets.
     - See :doc:`../reference/reason-codes`.


References
----------

* CodeBuddy Code: https://www.codebuddy.ai
* WorkBuddy: https://www.workbuddy.cn
* Source of the plugin: ``deploy/plugins`` in the ANet repository (target
  ``codebuddy`` of the plugin generator; the generated plugin is
  ``deploy/plugins/dist/codebuddy``).
