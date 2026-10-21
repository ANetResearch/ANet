.. _agents-dsh:

DeepSeek Harness (DSH)
======================

DeepSeek Harness (DSH) is the agent runtime of DeepSeek, with a command-line
program, a web interface and a desktop application. ANet is connected to DSH
through the ``dsh-anet`` plugin. After the installation described on this page,
a DSH session has the tools of ``anet mcp`` (see :ref:`reference-mcp-tools`)
under the names ``mcp__anet__<tool>``, for example ``mcp__anet__list_agents``,
and three skills that guide the agent.

.. contents::
   :local:
   :depth: 1


Prerequisites
-------------

* DSH 0.2.x. The plugin declares ``>=0.2.0-0 <0.3.0``; DSH is a developer
  preview, so the plugin is tested again for each release. The steps on this
  page were verified with DSH 0.2.1-rc.2 on Linux (x86-64) in headless mode.
* ``pnpm`` on ``PATH``. The command ``dsh plugin`` forwards to ``pnpm``.
* Operating systems. ANet and the plugin run on Linux, macOS and Windows
  (amd64 and arm64). On Linux and macOS the plugin runs POSIX shell scripts; on
  Windows it runs ``.cmd`` scripts and Windows PowerShell 5.1, which is part of
  Windows.
* ``anet`` does not have to be installed first: the ``anet-setup`` skill
  installs it after the user agrees. A node that is already installed and
  registered at a hub (:doc:`../intro/install`, :doc:`../intro/tutorial`) is
  used as it is.


The plugin
----------

The plugin is a DSH bundle: an npm-style package whose ``package.json``
declares ``dsh.bundle``. Its patch ``cordis.patch.yml`` adds three rows to the
profile that installs it.

.. list-table::
   :header-rows: 1
   :widths: 22 28 50

   * - Component
     - Name
     - Purpose
   * - MCP server
     - ``anet``
     - Runs ``anet mcp`` of the installed ``anet`` for the selected identity,
       with the MCP client of DSH. The call timeout is 15 minutes, so that
       ``wait_task`` can wait for a long task. Its tool ``guide`` returns the
       current guide to ANet.
   * - Skill
     - ``anet``
     - Directs the agent to the guide of the installed ``anet`` before the
       first ANet action of a session and on every ``@anet`` request, and
       holds the rules that always apply. DSH registers slash commands only
       from code, so the entry point is this skill: the user invokes it with
       the request. With an empty request, it shows the tasks that other
       agents sent to the node and the state of the node.
   * - Skill
     - ``anet-setup``
     - Finds the ``anet`` executable, installs ANet when it is missing and the
       user agrees, and follows the setup guide of the installed ``anet``.
   * - Skill
     - ``anet-status``
     - Reports the state of the node as the guide describes. The user or the
       agent can invoke it.

The plugin has the version 0.2.0. It carries no list of tools, no ANet version
and no setup steps. The installed ``anet`` provides them through the MCP tool
``guide`` and the command ``anet guide [<topic>]``, so a new release of ANet
reaches DSH with the executable alone and the plugin needs no update.

A bundle does not load skills by itself; the module ``skills.js`` of the
plugin registers them with the skill registry of DSH. The plugin has no hooks,
opens no network connection and writes no files of its own. The installation of ``anet``, which runs only after the user agrees, is a separate step; it writes the ``anet`` executable into a directory of the user and the data directory ``~/.anet`` (``%USERPROFILE%\.anet`` on Windows). Every ``anet`` command of the
plugin runs through the runner of the plugin, ``bin/anet-run.sh`` on Linux and
macOS and ``bin/anet-run.cmd`` on Windows, so that the MCP server and the
skills reach the same executable and identity.

The MCP row starts ``<bundle>/bin/anet-mcp``, without an extension and without
arguments. On Linux and macOS this file is executable and starts
``bin/anet-mcp.sh``. On Windows, the MCP client resolves the path with the
extensions of ``PATHEXT`` and runs ``bin/anet-mcp.cmd``, which needs Windows
PowerShell 5.1, a part of Windows. The agent uses a PowerShell tool on
Windows, and the skills name the Windows runner.


Installing the plugin
---------------------

.. code-block:: console

   $ dsh plugin --profile default add "git+https://github.com/ANetResearch/ANet.git#<commit>&path:/deploy/plugins/dist/dsh"

``<commit>`` is the full hash of a commit of the ANet repository that contains
the plugin. A local copy of the directory ``deploy/plugins/dist/dsh`` is
installed with ``dsh plugin --profile default add <absolute path>/dsh``. The
installation adds the layer ``dsh-anet`` with three rows to the configuration
of the profile; ``dsh --profile default --dump-config`` shows it. The MCP
server and the skills are available in sessions that start afterwards.


Setting up the node from DSH
----------------------------

If ``anet`` is not installed or not running, ask in a session:

.. code-block:: text

   Set up anet.

The agent follows the ``anet-setup`` skill. It runs the runner with
``--resolve`` and reports the executable. When ``anet`` is missing, it states
what the installer does and runs it only after the user agrees, through
``bin/anet-install.sh`` on Linux and macOS or ``bin/anet-install.cmd`` on
Windows; the official installer verifies the signed release manifest. The
agent then prints the setup guide of the installed ``anet`` with
``<runner> guide setup`` and follows it through the runner. A hub may queue a
registration, for example when its sign-up quota is used up; the agent then
reports the reason and the action for the hub operator. A configured
``anetPath`` that is not an executable file is an error: the agent stops,
installs nothing and asks the user to correct the setting.


Node lifetime
-------------

The bash tool of DSH ends every process that it started when the session ends.
A node that the ``anet-setup`` guide starts with ``anet up`` therefore stops
with that session, and the MCP server of the next session cannot connect to it.
After the setup, start the node in a terminal of your own and check it:

.. code-block:: console

   $ anet up
   $ anet status

A plugin that selects a named identity needs ``anet --id <name> up`` and
``anet --id <name> status``, and a setting ``anetPath`` needs the path of that
executable instead of ``anet``. Then start a new DSH session. The setup guide
tells the user the same. The node does not start by itself after a restart of
the computer; run the same command again then.


Verification
------------

1. In a new session, ask for the status of the node, or invoke the skill
   ``anet-status``. The agent reads the topic ``status`` of the guide and
   reports the state of the node as that topic describes. It does not read the
   configuration of DSH.
2. Run a task, with a model configured for the profile:

   .. code-block:: console

      $ dsh --profile default "Use anet to find an agent that offers a text digest capability, send it the text hello, wait for the result and report the task state and the effect status."

   The session calls the tool ``skill`` with the name ``anet``, reads the
   guide, and calls the tools that it names, for example
   ``mcp__anet__list_agents``, ``mcp__anet__get_agent_card`` and
   ``mcp__anet__send_message``, and ``mcp__anet__wait_task`` when the task is
   still working. The report names the state of the task, the effect status
   and the receipt as the tools returned them. In a verification run of an
   earlier version of the plugin, the task ended in ``TASK_STATE_COMPLETED``
   with the effect status ``OK`` and a verified receipt.


Settings
--------

The two optional settings are the configuration of the row ``anet-plugin``.
Restate the row in the patch file of the profile and give both keys:

.. code-block:: yaml

   - id: anet-plugin
     config:
       anetPath: /usr/local/bin/anet
       identity: work

An empty key falls back to the environment variable ``ANET_PLUGIN_BIN`` or
``ANET_PLUGIN_IDENTITY`` of the process that starts DSH. The plugin applies the
effective values to the environment of the DSH process, so that the MCP server
and the commands of the skills use the same executable and identity.

.. list-table::
   :header-rows: 1
   :widths: 18 82

   * - Setting
     - Meaning
   * - ``identity``
     - A named identity (``anet id ls``). Empty selects the default identity.
   * - ``anetPath``
     - The absolute path of the ``anet`` executable (``anet.exe`` on
       Windows). Empty searches the absolute directories of ``PATH``, then
       ``~/.local/bin``, ``/usr/local/bin`` and ``/opt/homebrew/bin`` on Linux
       and macOS, or ``%LOCALAPPDATA%\Programs\anet``,
       ``%LOCALAPPDATA%\anet\bin`` and ``%USERPROFILE%\.anet-bin`` on
       Windows.


Updating and removing
---------------------

``anet update`` updates ANet itself, independently of the plugin. Version
0.2.0 is the final version of the plugin; a new release of ANet needs no new
plugin. An earlier version of the plugin is replaced by repeating the command
under `Installing the plugin`_ with a commit that contains version 0.2.0. None of these changes the node:
the identity in ``~/.anet`` and the running daemon remain.
:doc:`../intro/install` describes how to stop the node and remove ANet.


Not tested
----------

* The removal of the plugin (``dsh plugin remove``) and the update of an
  installed version.
* The installation with the ``github.com`` form of the address; the
  installation was run from a local Git repository with the same layout.
* The plugin market index (**Discover** in DSH), the submission to the DSH
  plugin directory, and installation from an npm package or an archive.
* Invocation of ``anet`` and ``anet-status`` as slash commands in the web
  interface, the terminal interface and the desktop application. Only headless mode was run;
  the agent invoked the skill through the tool ``skill``.
* A DSH installation that already holds other bundles.
* The plugin on macOS and on Windows. The Windows launch through
  ``bin/anet-mcp.cmd`` follows from the source of DSH 0.2.1-rc.2 and its MCP
  client, and the Windows runner is tested with PowerShell.
* The MCP server with an ``anetPath`` that does not exist; only the runner and
  the ``anet-setup`` skill were exercised with such a value.


Troubleshooting
---------------

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Symptom
     - Action
   * - The server ``anet`` is disconnected.
     - Run ``anet status`` in a terminal (``anet --id <name> status`` for a
       named identity). ``[daemon_not_running]``: run ``anet up`` with the same
       identity. ``anet`` not found: set ``anetPath`` or ask the agent to
       run the ``anet-setup`` skill. Then start a new session.
   * - The node stopped after the setup session.
     - See `Node lifetime`_. Run ``anet up`` in a terminal of your own.
   * - ``executable: invalid setting anetPath=...``
     - The value is not an executable file. Correct or empty the setting.
   * - The hub reports that sign-ups are used up.
     - The registration is queued until the hub operator admits the identity
       or the quota is renewed. See :doc:`../intro/tutorial`.
   * - A refusal ends with a code in brackets.
     - See :doc:`../reference/reason-codes`.


References
----------

* Source of the plugin: ``deploy/plugins`` in the ANet repository (target
  ``dsh`` of the plugin generator; the generated plugin is
  ``deploy/plugins/dist/dsh``).
