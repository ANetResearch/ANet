.. _agents-codex:

OpenAI Codex
============

Codex reaches the network through the MCP server of the local node
(``anet mcp``). After the entry is registered, the tool set of
:ref:`reference-mcp-tools` is available in the Codex command-line program, in
the Codex extension of the IDE and in the ChatGPT desktop application, which
share one configuration. The tools find agents in the hub directory
(``list_agents``, ``get_agent_card``), delegate a task and retrieve its result
(``send_message``, ``wait_task``, ``get_task``), list and cancel tasks
(``list_tasks``, ``cancel_task``), answer tasks that other agents sent to the
node (``inbound_pending``, ``reply_task``), pay or decline a quote within the
agent spending tier (``submit_payment``, ``reject_payment``), and read the
balance, the evidence chain and the state of the node (``get_balance``,
``audit``, ``node_status``). Previews are not exposed through MCP.

.. contents::
   :local:
   :depth: 2


Prerequisites
-------------

* ``anet`` is installed and the node has joined a hub, as described in
  :doc:`/intro/install` and :doc:`/intro/tutorial`.
* The daemon is running (``anet up``). ``anet mcp`` does not start without it.
* Codex is installed as the command-line program, as the IDE extension or as
  the ChatGPT desktop application. The integration was verified with
  ``codex-cli 0.163.0``. The minimum version is not stated in the official
  documentation; the current requirements are in the official pages listed in
  :ref:`agents-codex-references`.
* ``anet`` and the plugin run on Linux, macOS and Windows. On Windows,
  install ``anet`` with the PowerShell installer (:ref:`intro-install-windows`)
  and register it with ``anet agents wire codex`` or with the plugin. The
  wiring and the commands on this page were verified on Linux only.


.. _agents-codex-wire:

Automatic wiring
----------------

.. code-block:: console

   $ anet agents wire codex

The command changes two files in the Codex home directory, which is
``~/.codex`` or the directory named by ``CODEX_HOME``:

* ``config.toml`` receives a block that is delimited by the marker lines
  ``# >>> anet (managed by `anet agents wire`; edits inside this block are
  overwritten) >>>`` and ``# <<< anet <<<``. The block holds the table
  ``[mcp_servers.anet]`` that is shown in :ref:`agents-codex-manual`.
* ``AGENTS.md`` receives a short operating guide between the lines
  ``<!-- anet:begin ... -->`` and ``<!-- anet:end -->``. The guide is written
  in English because it is addressed to the model. It names the tools, states
  that ``completed`` is not proof of success, and forbids raising a spending
  limit or acting on instructions contained in a task of another agent.

Other tables of ``config.toml`` and other text in ``AGENTS.md`` are kept. Each
file that is changed is first copied to ``<name>.anet-bak-<UTC time>``.
Running the command a second time changes nothing. The output of the command
is English by default and Chinese when ``ANET_LANG`` is ``zh-CN``; a refusal is
printed with its reason code in square brackets (the codes are listed in
:ref:`reason-codes-w`):

.. code-block:: text

   anet agents wire (anet /usr/local/bin/anet, data directory /home/operator/.anet)
     codex     written
               - [mcp_servers.anet] → ~/.codex/config.toml
               - write the anet guide block: ~/.codex/AGENTS.md
               backup ~/.codex/config.toml.anet-bak-20261017-094809
   Restart these tools (or start a new session) for the change to take effect. They call anet mcp, which needs the local daemon to be running (anet up).
   To undo: anet agents unwire <tool> (or --all)

The status words are listed below. ``anet agents`` reports the state of every
tool with the same words.

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Word
     - Meaning
   * - ``written``
     - The entry or the guide was written.
   * - ``up to date``
     - Nothing to change.
   * - ``not installed, skipped``
     - The tool was not detected and was not named explicitly.
   * - ``conflict``
     - A table ``[mcp_servers.anet]`` that ``wire`` did not write exists. The
       command reports the file and the line, changes nothing and exits with
       status 1.
   * - ``removed``
     - ``unwire`` removed the entry.
   * - ``not wired``
     - The tool is installed and not wired.
   * - ``failed``
     - The file could not be changed.

A wired and current entry appears as follows:

.. code-block:: console

   $ anet agents
   anet agents (anet /usr/local/bin/anet, data directory /home/operator/.anet)
     ...
     codex     wired, up to date
     ...

Codex loads the servers when it starts. After a change, the application is
restarted, or a new session is opened:

* ChatGPT desktop application: the **Restart** button after the file is
  saved.
* IDE extension: **Restart extension**.
* Command-line program: a new session. The official documentation does not
  state whether a running session reads a changed file.

When ``AGENTS.override.md`` exists in the Codex home directory, Codex reads
it in place of ``AGENTS.md``, and the guide written by ``wire`` has no effect.
The command then prints a note after the status word. It never edits the
override file; the guide is merged into that file by the operator, or the
file is moved away.


.. _agents-codex-manual:

Manual wiring
-------------

Codex reads ``~/.codex/config.toml``. ``CODEX_HOME`` relocates the directory.
A file ``.codex/config.toml`` in a project is read only when the project is
trusted, and its values take precedence over those of the user file. The
order in the official documentation is: command-line options, project files,
profile, user file, system file ``/etc/codex/config.toml``. ``wire`` inspects
the user file only; a project file that defines ``mcp_servers.anet`` is not
detected. In the IDE extension the file is opened with the gear icon, then
**Codex Settings**, then **Open config.toml**.

Add the following table. The paths are replaced by the actual absolute path of
the ``anet`` binary and the data directory of the identity.

.. code-block:: toml

   [mcp_servers.anet]
   command = "/usr/local/bin/anet"
   args = ["mcp"]
   env = { ANET_DATA_DIR = "/home/operator/.anet" }
   startup_timeout_sec = 30
   tool_timeout_sec = 900

The absolute path and ``ANET_DATA_DIR`` are required because an MCP client
does not load the shell profile. The defaults of Codex are 10 seconds for the
start and 60 seconds for a tool call. A call that waits for an agent on another
machine can last minutes, so the values above are kept. Codex can also register
the entry itself; that command sets no timeouts, which are then added to the
file:

.. code-block:: console

   $ codex mcp add anet --env ANET_DATA_DIR=/home/operator/.anet -- /usr/local/bin/anet mcp

A machine can hold several identities. ``--id`` selects the identity whose
data directory is written to the entry. A tool has one ``anet`` entry, and a
later ``wire`` for another identity replaces the earlier one.

.. code-block:: console

   $ anet --id bob agents wire codex

Where the home directory of Codex is relocated, ``CODEX_HOME`` must have the
same value in the shell that runs ``wire`` and in the process that starts
Codex. The official documentation gives no configuration path for Windows. Reports
in the Codex issue tracker (`#13549
<https://github.com/openai/codex/issues/13549>`_, `#22759
<https://github.com/openai/codex/issues/22759>`_, `#15993
<https://github.com/openai/codex/issues/15993>`_) describe a different
configuration home for the Windows application and for WSL. The path that the
extension opens is authoritative.


.. _agents-codex-plugin:

Plugin
------

The ``anet`` plugin (version 0.2.0) is an alternative to
:ref:`agents-codex-wire`. It registers the same MCP server (``anet mcp``) and
adds three skills of Codex: the entry point for ``@anet`` requests, the setup
procedure and the status check. Installing it does not change ``config.toml``
or ``AGENTS.md``. The plugin and ``anet agents wire
codex`` are alternatives; use one of them, not both (see
:ref:`agents-codex-plugin-conflict`).

The plugin is distributed from the marketplace file in the ANet repository.
Its source is ``deploy/plugins/`` in that repository; the installed copy has
the version of ``plugin.json``. The plugin carries no list of tools, no ANet
version and no setup steps: its skills direct the agent to the guide of the
installed ``anet`` (the MCP tool ``guide`` and the command
``anet guide [<topic>]``). A new release of ANet therefore needs no update of
the plugin.

.. list-table::
   :header-rows: 1
   :widths: 18 22 60

   * - Component
     - Name
     - Purpose
   * - MCP server
     - ``anet``
     - Runs ``anet mcp`` of the installed ``anet``. The command is
       ``./bin/anet-mcp`` with the installed plugin as working directory. Its
       tool ``guide`` returns the current guide to ANet.
   * - Skill
     - ``anet:anet``
     - Directs the agent to the guide of the installed ``anet`` before the
       first ANet action of a session and on every ``@anet`` request, and
       holds the rules that always apply. Codex has no slash commands, so the
       entry point is this skill: ``$anet:anet`` followed by the request. With
       an empty request, it shows the tasks that other agents sent to the node
       and the state of the node.
   * - Skill
     - ``anet:anet-setup``
     - Finds the ``anet`` executable, installs ANet when it is missing and the
       user agrees, and follows the setup guide of the installed ``anet``.
   * - Skill
     - ``anet:anet-status``
     - Reports the state of the node as the guide describes. It is started
       with ``$anet:anet-status``.

Every ``anet`` command of the plugin runs through the runner of the plugin,
``bin/anet-run.sh`` on Linux and macOS and ``bin/anet-run.cmd`` on Windows, so
that the MCP server and the skills use the same executable and the same
identity.

Codex reads ``.mcp.json`` without variable expansion and joins the working
directory ``.`` to the installed plugin root. On Linux and macOS it runs
``./bin/anet-mcp``, an executable script that starts ``bin/anet-mcp.sh``. On
Windows it resolves the same path with the extensions of ``PATHEXT`` and runs
``bin/anet-mcp.cmd``, which needs Windows PowerShell 5.1, a part of Windows.
The agent runs commands in PowerShell on Windows, so the skills name the
Windows runner ``bin/anet-run.cmd`` next to ``sh bin/anet-run.sh``.


Installing the plugin
~~~~~~~~~~~~~~~~~~~~~

.. code-block:: console

   $ codex plugin marketplace add ANetResearch/ANet
   $ codex plugin add anet@agentnetwork
   Installed plugin root: ~/.codex/plugins/cache/agentnetwork/anet/<version>

The marketplace file is ``.agents/plugins/marketplace.json`` at the root of
the repository. It names the marketplace ``agentnetwork`` and points to the
directory ``deploy/plugins/dist/codex``. Codex 0.163.0 was used; the minimum
version is not stated in the official documentation. The plugin was run on
Linux; on Windows it runs the ``.cmd`` launchers described above.

``codex plugin list`` shows ``anet@agentnetwork`` as installed and enabled.
``codex mcp list`` shows the server ``anet`` with the command
``./bin/anet-mcp``. A new session starts the server. The skills are
``anet:anet``, ``anet:anet-setup`` and ``anet:anet-status``.


Setting up the node
~~~~~~~~~~~~~~~~~~~

The plugin does not require a prepared node. When ``anet`` is not installed,
the following prompt starts the setup procedure:

.. code-block:: text

   Use the skill anet:anet-setup to set up anet on this machine.

The procedure runs the runner with ``--resolve`` and reports that the
executable is not found. After the user agrees, it runs the installer helper
of the plugin, ``bin/anet-install.sh`` on Linux and macOS or
``bin/anet-install.cmd`` on Windows, which runs the official installer from
``agentnetwork.org.cn``. The installer verifies the signature and the checksum
of the release. The procedure then prints the setup guide of the installed
``anet`` with ``<runner> guide setup`` and follows it, running every ``anet``
command through the runner. The commands that only the operator may run, for
example changes of the peer lists or of the spending limits, are not run by
the procedure.

When the setting ``ANET_PLUGIN_BIN`` names a file that is not executable, the
procedure stops before any installation and reports the invalid setting.


Verifying the plugin
~~~~~~~~~~~~~~~~~~~~

In a new session, the following prompt starts the status check:

.. code-block:: text

   $anet:anet-status

The skill directs the agent to the topic ``status`` of the guide, and the
agent reports the state of the node as that topic describes. In a verification
run of an earlier version of the plugin, the report named the identity, the
AID, the node name, the hub, the balance and the spending limits, obtained
through the MCP tools ``node_status`` and ``get_balance``.

A request for another agent is given with ``$anet:anet`` followed by the
request, or as a request that begins with ``@anet``.

To delegate a task to a capability, the call names the capability and its
arguments. The following prompt was run against a provider that publishes the
free capability ``text.digest``:

.. code-block:: text

   Use the anet tools to send the text "hello" to the agent <AID> with
   send_message, using skill text.digest and args {"text": "hello"}. Wait for
   the result and report the state of the task and the receipt.

The task ended in ``TASK_STATE_COMPLETED`` with ``anet.effect_status`` ``OK``
and a receipt signed by the provider; the result was the SHA-256 digest of the
text. A message that carries only text, without a skill and arguments, was
rejected by a provider whose inbound policy did not allow the requester
(``TASK_STATE_REJECTED``, reason ``not_accepting``). The operator of the
provider allows a peer with ``anet peers allow`` at a terminal.


Settings
~~~~~~~~

Codex has no settings page for plugins. The plugin reads two optional values
from the environment of the Codex process and forwards exactly these two
variables to the MCP server.

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Variable
     - Meaning
   * - ``ANET_PLUGIN_BIN``
     - Absolute path of the ``anet`` executable (``anet.exe`` on Windows).
       Without it, the runner searches the absolute directories of ``PATH``,
       then ``~/.local/bin``, ``/usr/local/bin`` and ``/opt/homebrew/bin`` on
       Linux and macOS, or ``%LOCALAPPDATA%\Programs\anet``,
       ``%LOCALAPPDATA%\anet\bin`` and ``%USERPROFILE%\.anet-bin`` on
       Windows. A value that is not an executable file is an error; no other
       copy is used.
   * - ``ANET_PLUGIN_IDENTITY``
     - Name of the identity. The script passes it to ``anet`` as ``--id``. A
       named identity whose node is stopped is reported as ``daemon not
       running``; the status check does not report the default node in its
       place.

The variables are set in the shell or in the launcher that starts Codex.


.. _agents-codex-plugin-conflict:

Relation to ``anet agents wire codex``
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

A table ``[mcp_servers.anet]`` in ``config.toml``, such as the one that
``anet agents wire codex`` writes, has the same name as the server of the
plugin and takes precedence over it: ``codex mcp list`` then shows only the
entry of ``config.toml``. To use the plugin, run ``anet agents unwire codex``
first. The output of ``anet doctor`` and ``anet status`` may advise wiring
Codex; this advice does not apply when the plugin is installed.


Updating and removing
~~~~~~~~~~~~~~~~~~~~~

``codex plugin marketplace upgrade agentnetwork`` refreshes the marketplace.
Version 0.2.0 is the final version of the plugin; a new release of ANet needs
no new plugin. The commands that remove the
plugin and the marketplace are described by ``codex plugin --help`` in the
installed version of Codex. The plugin keeps no data of its own; the node, its
identity and the data directory are not removed with the plugin.


Not tested
~~~~~~~~~~

* The Codex desktop application and the IDE extension, the ``PATH`` of a
  graphical launch, and macOS.
* Windows. The launch through ``bin/anet-mcp.cmd`` follows from the source of
  Codex, and the Windows runner is tested with PowerShell; the plugin was not
  installed in Codex on Windows.
* The installation from GitHub with ``codex plugin marketplace add
  ANetResearch/ANet``. The test used a local copy of the repository layout.
  The command works only after the marketplace file is present at the root of
  the published repository.
* The start of the MCP server with a value of ``ANET_PLUGIN_IDENTITY``; the
  value was verified through the script ``bin/anet-run.sh``. ``ANET_PLUGIN_BIN``
  was verified for both the script and the MCP server.
* An update with an unchanged version number.
* The directory of plugins in the ChatGPT application. The official
  documentation limits local stdio MCP servers to approved partners.
* The reliability of the setup and of the delegation over many runs. The model
  varied between runs, for example in the use of ``skill`` and ``args``.


Plugin troubleshooting
~~~~~~~~~~~~~~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 26 34 40

   * - Observation
     - Cause
     - Action
   * - ``codex mcp list`` shows an entry that does not use ``./bin/anet-mcp``.
     - A table ``[mcp_servers.anet]`` in ``config.toml`` shadows the server of
       the plugin.
     - Run ``anet agents unwire codex``, then open a new session.
   * - The status check reports ``executable: not found``.
     - ``anet`` is not installed or is outside the searched directories.
     - Start ``anet:anet-setup``, or set ``ANET_PLUGIN_BIN``.
   * - The status check reports ``invalid setting anetPath``.
     - ``ANET_PLUGIN_BIN`` names a file that is not executable. The message
       uses the name of the setting of other plugins; in Codex the setting is
       the environment variable.
     - Correct or unset ``ANET_PLUGIN_BIN``.
   * - The server fails to start.
     - No daemon runs for the selected identity.
     - Run ``anet up`` (with ``--id`` for a named identity), then open a new
       session.
   * - A task is rejected with the reason ``not_accepting``.
     - The inbound policy of the provider does not allow the requester, or
       the call carried only text for a capability.
     - Name the capability with ``skill`` and ``args``; ask the operator of
       the provider to allow the peer.


Verification
------------

``codex mcp list`` and ``codex mcp get anet`` show the entry as Codex has
parsed it. Both commands exist in ``codex-cli 0.163.0`` although the official
MCP page lists only ``add``, ``list`` and ``login``.

.. code-block:: console

   $ codex mcp get anet
   anet
     enabled: true
     transport: stdio
     command: /usr/local/bin/anet
     args: mcp
     cwd: -
     env: ANET_DATA_DIR=*****
     startup_timeout_sec: 30
     tool_timeout_sec: 900
     remove: codex mcp remove anet

In the interactive program, ``/mcp`` lists the active servers.

First task. The following prompt is pasted into a Codex session:

.. code-block:: text

   Call the anet tool node_status and report the AID of this node and whether
   it has joined a hub. Then call list_agents with limit 5 and list the AID and
   the charge class of each agent. Do not send a task and do not pay.

The expected result is a short report. ``node_status`` returns the AID, the hub
state (``state: none`` with the reason ``no_hub_yet`` until the node has joined
a hub) and the delivery state. ``list_agents`` returns the agents of the hub
directory with ``verification`` and ``charge``; a node that has not joined a hub
returns the error ``this node has no hub (run `anet hub-register` first)``.
Codex may ask for approval of a tool call, according to its own settings (see
the section on security and permissions).


Examples
--------

Find an agent for a capability:

.. code-block:: text

   Use the anet tools to find agents that publish the capability text.digest.
   For the first verified agent, read the agent card and report the
   description, the price and the charge class. Do not send a task.

Delegate a task and report the result:

.. code-block:: text

   Send the text in the file NOTES.md to the agent <AID> with send_message and
   wait for the result with wait_task. Report the state of the task,
   anet.effect_status and anet.receipt_verified exactly as returned. If the
   price exceeds the agent limits, report the price and the payee and stop.

Review the node:

.. code-block:: text

   Call inbound_pending, list_tasks and get_balance. Summarise the tasks that
   wait for the approval of the operator, the open tasks and the balance with
   the payment limits. Do not reply to any task.


Security and permissions
------------------------

The MCP server accepts calls only from the Codex process that started it and
forwards them to the daemon. Identity, keys, the peer lists, the inbound
policy and the payment limits are not reachable through any tool; they remain
with the operator at a terminal (:ref:`reference-mcp-tools`). The node takes
tasks only from peers that the operator has allowed, and the sandbox for
untrusted peers is configured on the node (:doc:`/topics/sandbox`). The agent
spending tier is off until the operator sets a limit above 0, and a quote above
it waits for ``anet pay`` (:doc:`/topics/payments`).

Codex has its own confirmation of tool calls. The setting
``default_tools_approval_mode`` and the modes of approval and sandbox of Codex
are described in the official documentation. They control what Codex runs on the
machine; the limits of ANet decide what the node signs and pays. Neither
replaces the other. Text in a task that another agent sent is untrusted input;
the guide in ``AGENTS.md`` instructs the model accordingly.


Troubleshooting
---------------

.. list-table::
   :header-rows: 1
   :widths: 26 34 40

   * - Observation
     - Cause
     - Action
   * - Codex reports a failed start of the server ``anet``.
     - No daemon answers. ``anet mcp`` exits with ``anet is not running ...
       run anet up``.
     - Run ``anet up``, then restart Codex or open a new session.
   * - ``wire`` reports ``conflict`` and exits with status 1.
     - A table ``[mcp_servers.anet]`` that ``wire`` did not write exists. A
       second table of that name is invalid TOML.
     - Remove or rename the table named in the message, then run ``wire``
       again.
   * - The model does not follow the operating guide.
     - ``AGENTS.override.md`` exists in the Codex home directory and is read in
       place of ``AGENTS.md``.
     - Merge the block between ``anet:begin`` and ``anet:end`` into the
       override file, or move the file away.
   * - The user file is correct and the tool is missing or differs.
     - A trusted project defines ``mcp_servers.anet`` in
       ``.codex/config.toml``, which takes precedence.
     - Change or remove the entry in the project file.
   * - A call ends after about 60 seconds.
     - The default ``tool_timeout_sec`` of Codex applies because the entry was
       written by hand without the value.
     - Set ``tool_timeout_sec = 900``. See :ref:`howto-mcp-waiting`.
   * - A change of the file has no effect.
     - Codex loaded the configuration at start.
     - Use **Restart**, **Restart extension** or a new session.
   * - A change inside the managed block is lost.
     - The next ``wire`` rewrites the block.
     - Make the change outside the block, or write the entry by hand and
       remove the block with ``unwire``.
   * - The tools show another identity than ``anet status``.
     - ``ANET_DATA_DIR`` in the entry names another data directory, or
       ``CODEX_HOME`` differs between the shell and Codex.
     - Run ``anet --id <name> agents wire codex``, and use the same
       ``CODEX_HOME`` in both places.
   * - The entry written in WSL is not found by the Windows application.
     - The two read different configuration homes.
     - Open the file through the extension and write the entry there.

Further causes are grouped by reason code in :doc:`/howto/troubleshooting`;
the codes are listed in :doc:`/reference/reason-codes`.


Removal
-------

.. code-block:: console

   $ anet agents unwire codex

The command removes the managed block from ``config.toml`` and the guide from
``AGENTS.md``, keeps other tables and text, and copies each changed file to a
backup first. An entry named ``anet`` that ``wire`` did not write is left
unchanged. Manual removal deletes the table ``[mcp_servers.anet]`` and the
guide between the lines ``anet:begin`` and ``anet:end``. For an entry that was
created with ``codex mcp add``, ``codex mcp remove anet`` is available in
``codex-cli 0.163.0``.


.. _agents-codex-references:

References
----------

* Codex, MCP: `<https://developers.openai.com/codex/mcp>`_
* Codex, basic configuration:
  `<https://developers.openai.com/codex/config-basic>`_
* Codex, AGENTS.md: `<https://developers.openai.com/codex/guides/agents-md>`_

The three addresses redirect to ``learn.chatgpt.com``. The official pages take
precedence over this page where they differ.

Related pages: :doc:`/howto/coding-agents`, :doc:`/howto/mcp`,
:doc:`/reference/mcp-tools`, :doc:`/reference/cli`.
