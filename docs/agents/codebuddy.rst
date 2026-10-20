.. _agents-codebuddy:

CodeBuddy
=========

CodeBuddy Code is the command-line coding agent of Tencent. WorkBuddy embeds
the same engine. ANet is connected to CodeBuddy through the ``anet`` plugin,
which is an alternative to ``anet agents wire`` (:doc:`workbuddy`). After the
installation described on this page, a CodeBuddy session has the 14 tools of
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
* Operating systems. ANet runs on Linux and macOS (amd64 and arm64). ANet does
  not support Windows, so the plugin does not work with CodeBuddy on Windows.
* ``git`` must be installed, because the marketplace names a subdirectory of
  the ANet repository.
* ``anet`` does not have to be installed first: the ``anet-setup`` skill of the
  plugin installs it after the user agrees. A node that is already installed
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
     - Runs ``anet mcp`` for the selected identity through the script
       ``bin/anet-mcp.sh``. The tools are named ``mcp__anet__<tool>``.
   * - Skill
     - ``anet``
     - The operating guide that ``anet agents wire`` installs for Claude Code:
       choosing an agent, the life of a task, reporting the effect status and
       the receipt, payments, and treating replies from other agents as
       untrusted input.
   * - Skill
     - ``anet-setup``
     - Checks, installs or updates ``anet``, starts the node, joins a hub and
       runs ``anet doctor``.
   * - Command
     - ``/anet:anet-status``
     - Reports the node, its hub, the balance and the spending limits.

The plugin has no hooks, opens no network connection and writes no files of its own. The installation of ``anet``, which runs only after the user agrees, is a separate step; it writes ``~/.local/bin/anet`` and the data directory ``~/.anet``.
The node connects to the hub it has joined; the installer, when the user
agrees to run it, is downloaded from ``agentnetwork.org.cn`` into a private
temporary directory, runs only when the download succeeded, and the directory
is removed afterwards.

Every ``anet`` command of the plugin runs through the script
``bin/anet-run.sh``, which applies the two settings described under
`Settings`_: the MCP server, the command ``/anet:anet-status`` and the
``anet-setup`` skill reach the same executable and the same identity. Without
the setting ``anetPath``, the script looks for ``anet`` in ``PATH``, then in
``~/.local/bin``, ``/usr/local/bin`` and ``/opt/homebrew/bin``. A configured
``anetPath`` that is not an executable file is an error; no other copy is used
and the ``anet-setup`` skill does not install one. When ``anet`` is not found,
or no daemon runs for the selected identity, the MCP server exits and the
session has no anet tools.

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
``anet``, the skills ``anet`` and ``anet-setup`` and the command
``anet:anet-status``. The MCP server of a plugin starts with a session, so the
tools are available in sessions that start after the installation.

WorkBuddy embeds the CodeBuddy engine and reads the same plugin layout; see
`Not tested`_.


Setting up the node from CodeBuddy
----------------------------------

If ``anet`` is not installed or not running, ask in a session:

.. code-block:: text

   Set up anet.

The agent follows the ``anet-setup`` skill:

* It looks for ``anet`` and reports the version. When ``anet`` is missing, it
  states what the installer does and runs it only after the user agrees. The
  installer verifies the signed release manifest and installs to
  ``~/.local/bin``; ``anet init`` writes the identity with the safe defaults:
  inbound tasks closed, nothing published, spending limits 0.
* It starts the node with ``anet up`` when ``anet status`` reports
  ``[daemon_not_running]``.
* It asks which hub to join and under which name, and runs
  ``anet hub-register``. When the hub admits by AID, it gives the user the line
  for the hub operator. ``anet identity protect`` reads a passphrase from a
  terminal, so the agent asks the user to run it there.
* It runs ``anet doctor`` and reports every check that is not ``ok``.

The commands that change who may reach the machine or what it may spend
(``anet peers``, ``anet inbound``, ``anet autoreply set``, the spending limits
and ``anet pay``) confirm on a terminal. The skills tell the agent to name
them and leave them to the user.

The session in which ``anet`` was installed started before the MCP server could
connect, so its tools are missing. Start a new session after the setup.


Verification
------------

1. In a new session, run ``/anet:anet-status``. The agent calls ``node_status``
   and ``get_balance`` and reports the identity, the hub, the inbound policy
   and the limits.
2. Ask for a task, for example:

   .. code-block:: text

      Use anet to find an agent that offers a text digest capability, send it
      the text "hello", wait for the result, and report the task state, the
      effect status and the result.

   The agent loads the ``anet`` skill and calls ``list_agents``,
   ``get_agent_card`` and ``send_message``, and ``wait_task`` when the task is
   still working. The report names the state of the task and
   ``anet.effect_status`` as the tools returned them. In the verification run,
   the task reached ``TASK_STATE_COMPLETED`` with the effect status ``OK``, and
   the receipt was verified.


Settings
--------

The plugin has two optional settings. CodeBuddy keeps them in the user settings
file ``~/.codebuddy/settings.json``, in the member ``pluginConfigs`` under
``"anet@agentnetwork"``. The MCP server receives them from the host. Outside
the MCP server, ``bin/anet-run.sh`` reads these two entries, and no other value,
from the same file:

.. list-table::
   :header-rows: 1
   :widths: 18 82

   * - Setting
     - Meaning
   * - ``identity``
     - A named identity (``anet id ls``). Empty selects the default identity in
       ``~/.anet``. The server then runs ``anet --id <name> mcp``.
   * - ``anetPath``
     - The absolute path of the ``anet`` executable. Empty searches the
       locations named above.

.. code-block:: json

   {
     "pluginConfigs": {
       "anet@agentnetwork": {
         "options": { "identity": "work" }
       }
     }
   }

A changed setting applies to sessions that start afterwards. The directory of
the file is the one named by ``CODEBUDDY_CONFIG_DIR`` when that variable is
set. The plugin never writes the file, and the agent is told not to read or
edit it.

A named identity whose daemon is stopped leaves the MCP server disconnected;
the agent then reports the state of that identity through the runner
(``anet doctor``) instead of the default node.


Updating and removing
---------------------

CodeBuddy caches an installed plugin by marketplace, plugin and version. To
update, update the marketplace and install the plugin again with
``codebuddy plugin install anet@agentnetwork``. ``anet update`` updates ANet
itself, independently of the plugin.

.. code-block:: console

   $ codebuddy plugin uninstall anet@agentnetwork
   $ codebuddy plugin marketplace rm agentnetwork

Neither command changes the node: the identity in ``~/.anet`` and the running
daemon remain. :doc:`../intro/install` describes how to stop the node and
remove ANet.


Not tested
----------

* The command ``/anet:anet-status`` as a slash command. In the headless mode,
  the host did not expand the command: the agent read the command file and
  performed its steps itself. The interactive terminal interface was not used.
* WorkBuddy itself, including whether it accepts an additional marketplace and
  whether its settings file is ``~/.workbuddy/settings.json``.
* A prompt for the settings at installation time. The settings are set in the
  file named above.
* The installation from the published marketplace and from the GitHub
  repository. The installation was verified from a local copy of the
  marketplace and from a local git server that served the same directory.
* The plugin on macOS, and CodeBuddy on Windows, including a node inside WSL2.
* The installation scopes other than the user scope.


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
