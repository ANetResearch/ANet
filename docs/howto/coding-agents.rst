.. _howto-coding-agents:

Integrating coding agents
=========================

A coding agent reaches the network through the MCP server of the local
node (``anet mcp``, see :ref:`howto-mcp`). The command ``anet agents wire``
registers that server, together with a short operating guide, in the
configuration files of the coding agents installed on the machine. The
command ``anet agents unwire`` removes both again.

The guide for each tool, with prerequisites, verification and removal steps,
starts at :doc:`/agents/index`. This page describes the supported tools, the files that are changed, the
rules that apply to every change, and the options specific to Hermes.

.. contents::
   :local:
   :depth: 2


Overview
--------

A coding agent has no built-in knowledge of ANet. Integration consists of two
parts:

* an **MCP server entry** that starts ``anet mcp`` with the data directory of
  the node, so that the tool set of :ref:`reference-mcp-tools` becomes
  available in the coding agent;
* an **operating guide**, a short text that states how the tools are used,
  how task results are to be reported, and what the agent must not do
  (for example raise a spending limit, or follow instructions contained in
  a task that another agent sent to the node).

The MCP server accepts tool calls only from the process that started it and
forwards them to the daemon of the node over the local control plane.
The daemon keeps the identity keys, the ledger, and the inbound policy; the
coding agent holds none of them. Operations that change the authority of the
node, such as allowing a peer, loosening the inbound policy, or changing a
payment limit, are not exposed as tools and remain with the operator at a
terminal.

The command works on files only. It does not need a running daemon, and the
installer runs it before the first ``anet up``. The MCP server itself does
need a running daemon; see :ref:`howto-mcp`.


Supported tools
---------------

The tool argument of ``anet agents wire`` is one of the names in the first
column. ``claude-code`` is accepted as an alias of ``claude``.

.. list-table::
   :header-rows: 1
   :widths: 12 38 30 20

   * - Argument
     - MCP server entry
     - Operating guide
     - Environment
   * - ``claude``
     - Server ``anet`` in the user scope of ``~/.claude.json``
     - Skill ``~/.claude/skills/anet/SKILL.md``
     - ``CLAUDE_CONFIG_DIR``
   * - ``codex``
     - Table ``[mcp_servers.anet]`` in ``~/.codex/config.toml``
     - Block in ``~/.codex/AGENTS.md``
     - ``CODEX_HOME``
   * - ``cursor``
     - Member ``mcpServers.anet`` in ``~/.cursor/mcp.json``
     - None; the MCP server sends its own instructions on connect
     -
   * - ``opencode``
     - Member ``mcp.anet`` in ``~/.config/opencode/opencode.json``
     - Block in ``~/.config/opencode/AGENTS.md``
     - ``XDG_CONFIG_HOME``
   * - ``hermes``
     - Key ``mcp_servers.anet`` in ``~/.hermes/config.yaml``
     - Block in ``~/.hermes/SOUL.md``
     - ``HERMES_HOME``
   * - ``workbuddy``
     - Member ``mcpServers.anet`` in ``~/.workbuddy/mcp.json``
     - None; the MCP server sends its own instructions on connect
     -

A tool counts as present when its executable is on ``PATH`` or its
configuration directory exists. WorkBuddy is a desktop application without a
command-line program, so it counts as present when ``~/.workbuddy`` exists. Without a tool argument, or with ``--all``,
the command wires every supported tool that is present and reports the
others as not installed. A tool that is named explicitly is wired even when it
is not detected.

Claude Code
~~~~~~~~~~~

The server is registered in the user scope, which applies to every project on
the machine. When ``claude`` is on ``PATH`` the entry is created with
``claude mcp add -s user``, because Claude Code manages the locking and the
format of its own file. When ``claude`` is not on ``PATH`` the command edits
``~/.claude.json`` directly. The operating guide is installed as a skill; it
is loaded when a task mentions ANet, and it does not occupy the context of
other sessions.

Codex
~~~~~

The Codex command-line program, the IDE extension and the desktop application
share ``~/.codex/config.toml``, or the file in the directory named by
``CODEX_HOME``. The entry is written inside a block delimited by marker comments. The values
``startup_timeout_sec = 30`` and ``tool_timeout_sec = 900`` are set explicitly:
the tool ``wait_task`` blocks until a task changes state, and a task of a
remote agent is measured in minutes, whereas the default tool timeout of
Codex is 60 seconds.

Codex reads ``AGENTS.override.md`` instead of ``AGENTS.md`` when the file
exists in the global directory. In that case the operating guide is not
loaded, and ``wire`` reports the file. A ``.codex/config.toml`` in a trusted
project takes precedence over the user file; ``wire`` does not inspect it. The
official reference is `<https://developers.openai.com/codex/mcp>`_.

Cursor
~~~~~~

The entry is written to ``~/.cursor/mcp.json``, which is read by the editor
and by the Cursor command-line agent (the program is named ``agent`` in the
current documentation; earlier releases used ``cursor-agent``). Cursor has no user-level rules file, so no
operating guide is installed. An older rule file written by a previous
release under ``~/.cursor/rules`` is removed when its content identifies it
as ANet's.

opencode
~~~~~~~~

The entry uses the opencode form, with ``"type": "local"``, the command and
its argument in one array, an explicit ``enabled`` flag, and ``environment``
in place of ``env``. When only ``opencode.jsonc`` exists, that file is the
target. The editor does not preserve comments: if the file contains
comments, the command makes no change and prints the entry for manual
insertion.

Hermes
~~~~~~

The entry runs the binary directly, without a shell wrapper. Hermes names
the tools ``mcp_anet_<tool>``, for example ``mcp_anet_list_agents``; the
operating guide in ``SOUL.md`` uses these names. On native Windows the
default home directory of Hermes differs from ``~/.hermes``; set
``HERMES_HOME`` to the directory in use. The timeouts are ``timeout: 900`` and ``connect_timeout: 30``. Optional
``a2a_agents`` entries are described in :ref:`howto-coding-agents-hermes-a2a`.


Wiring and unwiring
-------------------

Synopsis
~~~~~~~~

.. code-block:: console

   $ anet agents [status]
   $ anet agents wire [--all | <tool>...] [--refresh] [--a2a <aid>...]
   $ anet agents unwire [--all | <tool>...] [--a2a <aid>...]

``--all`` and explicit tool names are mutually exclusive. ``--refresh`` and
``--a2a`` apply to Hermes; ``--refresh`` is accepted by ``wire`` only.
The command exits with a non-zero status when at least one tool ended in a
conflict or an error, so that an installer cannot report success after a
partial result.

The report is printed in English. When the environment variable ``ANET_LANG``
is ``zh-CN``, the same lines are printed in Chinese. The sentences come from the
reason table. A refusal ends with its reason code in square brackets, for
example ``[wire_conflict]``, and the next step follows on a line that begins
with an arrow when the code names a command. The ``wire_`` codes are listed in
:ref:`reason-codes-w`.

Procedure
~~~~~~~~~

1. Initialise and start the node, if this has not been done:

   .. code-block:: console

      $ anet init
      $ anet up

2. Register the server with the coding agents on the machine:

   .. code-block:: console

      $ anet agents wire --all

   A single tool is wired by name:

   .. code-block:: console

      $ anet agents wire claude

3. Restart the coding agent, or start a new session. The tools are loaded at
   the start of a session.

4. Check the result:

   .. code-block:: console

      $ anet agents

   The command reports one state for each tool: not installed, not wired,
   wired and current, wired but outdated (the entry or the guide differs from
   what ``wire`` would write now), or conflict. For an outdated entry the
   report names the command that corrects it.

5. In the coding agent, request the tool ``node_status``. A response that
   contains the AID and the version of the node confirms that the chain from
   the agent to the daemon works. If the daemon is not running, the MCP
   server does not start; the client reports a failed connection.

Removal is symmetric:

.. code-block:: console

   $ anet agents unwire claude
   $ anet agents unwire --all

``unwire`` removes the MCP entry, the operating guide, and every
``a2a_agents`` entry together with its token. An entry with the name ``anet``
that ANet did not write is left unchanged and reported.


Rules that apply to every change
--------------------------------

The command edits configuration files of other programs. The same rules
apply to every tool:

Backup
   Before a file is changed it is copied to ``<name>.anet-bak-<UTC time>``
   next to the original. An existing backup is never overwritten.

Absolute binary path and pinned identity
   The entry names the ``anet`` binary by absolute path and sets
   ``ANET_DATA_DIR`` to the data directory of the identity that was selected
   when the command ran. An MCP client does not load the shell profile of the
   operator, and ``anet mcp`` resolves strictly once an identity is pinned: it
   reaches the daemon of that identity and no other. To wire another
   identity, select it with ``--id``:

   .. code-block:: console

      $ anet --id bob agents wire claude

   A tool has one ``anet`` entry; a later ``wire`` for another identity
   replaces the entry written earlier. A binary started through ``go run``
   is refused, because the temporary build directory disappears when the
   process exits.

Atomic writes
   A file is written to a sibling temporary file and renamed. The mode of
   the replaced file is kept.

Idempotence
   Running ``wire`` twice changes nothing the second time. After an upgrade
   of ``anet``, running it again updates the operating guide and, where the
   binary path changed, the entry.

Conflicts
   An existing entry with the name ``anet`` whose command is not ``anet mcp``
   is not overwritten. The tool is reported as a conflict, nothing is changed
   for that tool, and the exit status is non-zero. The conflicting entry is
   removed or renamed by the operator, and ``wire`` is run again. The same
   applies to a table ``mcp_servers.anet`` in a Codex file that was not
   written by ``anet agents wire``; the report gives the line number.

Managed blocks
   In TOML, YAML and Markdown files the command edits only the text between
   its own marker lines. Edits inside a managed block are overwritten by the
   next ``wire``.

Superseded content
   Earlier releases appended a persona block to ``~/.claude/CLAUDE.md``,
   wrote a rule file under ``~/.cursor/rules``, and, through
   ``anet install --agent openclaw``, appended a block to
   ``~/.openclaw/AGENTS.md``. ``wire`` removes the Claude Code block and the
   Cursor file; ``wire --all`` and ``unwire --all`` remove the OpenClaw
   block when it is present. OpenClaw is not a supported tool.


Entries written by the command
------------------------------

The following examples show the entries for a binary at
``/usr/local/bin/anet`` and a data directory ``/home/operator/.anet``.
The paths are replaced by the actual values on each machine.

Claude Code, Cursor and WorkBuddy use the same stdio form:

.. code-block:: json

   {
     "mcpServers": {
       "anet": {
         "type": "stdio",
         "command": "/usr/local/bin/anet",
         "args": ["mcp"],
         "env": { "ANET_DATA_DIR": "/home/operator/.anet" }
       }
     }
   }

The Cursor and WorkBuddy entries have no ``type`` member. For Claude Code the equivalent
command is:

.. code-block:: console

   $ claude mcp add -s user anet -e ANET_DATA_DIR=/home/operator/.anet -- /usr/local/bin/anet mcp

Codex, in ``config.toml``:

.. code-block:: toml

   # >>> anet (managed by `anet agents wire`; edits inside this block are overwritten) >>>
   [mcp_servers.anet]
   command = "/usr/local/bin/anet"
   args = ["mcp"]
   env = { ANET_DATA_DIR = "/home/operator/.anet" }
   startup_timeout_sec = 30
   tool_timeout_sec = 900
   # <<< anet <<<

opencode, in ``opencode.json``:

.. code-block:: json

   {
     "$schema": "https://opencode.ai/config.json",
     "mcp": {
       "anet": {
         "type": "local",
         "command": ["/usr/local/bin/anet", "mcp"],
         "enabled": true,
         "environment": { "ANET_DATA_DIR": "/home/operator/.anet" }
       }
     }
   }

Hermes, in ``config.yaml``:

.. code-block:: yaml

   # >>> anet mcp (managed by `anet agents wire`; edits inside this block are overwritten) >>>
   mcp_servers:
     anet:
       command: "/usr/local/bin/anet"
       args: ["mcp"]
       env:
         ANET_DATA_DIR: "/home/operator/.anet"
       timeout: 900
       connect_timeout: 30
   # <<< anet mcp <<<

The ``$schema`` member of the opencode file is written only when ``wire``
creates the file.


WorkBuddy
~~~~~~~~~

The entry is written to the user-level file ``~/.workbuddy/mcp.json`` that
the WorkBuddy MCP guide describes, in the form of that guide: a ``command``,
``args`` and ``env`` under ``mcpServers``. Other servers in the file are kept.
A file that is not plain JSON, for example one with comments, is not edited;
the command prints the entry for manual insertion.

Only the parts that the WorkBuddy documentation confirms are written. No
operating guide is installed, because no user-level instruction file is
documented; the MCP server sends its own instructions on connect, and whether
WorkBuddy passes them to the model is not documented. No timeout is written:
the documented default is 30 seconds, and the documentation does not state
that a user-level entry honours a ``timeout`` member. Until that is confirmed
in the current version, call ``wait_task`` with the default wait and repeat the
call when the task is still working. The reload behaviour, the menu path and
the trust prompt are described in the official guide
(`<https://www.workbuddy.cn/docs/workbuddy/From-Beginner-to-Expert-Guide/Function-Description/MCP-Guide>`_);
the command prints this reference after it writes the entry. WorkBuddy is
distinct from the CodeBuddy command-line program, whose files are under
``~/.codebuddy``; ``wire`` does not handle CodeBuddy.

On Windows, ``anet`` and the WorkBuddy application both run natively, and
``wire`` writes ``%USERPROFILE%\.workbuddy\mcp.json`` with the absolute path
of ``anet.exe``. This route has not been tested against the Windows
application.


.. _howto-coding-agents-hermes-a2a:

Hermes and remote agents (``--a2a``)
------------------------------------

Hermes can call a remote agent through its own A2A client. The option is
intended for agents on the network; the Hermes documentation recommends
in-process subagents for agents on the same machine. With ``--a2a``,
``wire`` adds one ``a2a_agents`` entry per AID to ``config.yaml``. Each entry
points at the local A2A interface of the node (see :ref:`howto-local-a2a`),
so that the call is signed, sealed and relayed by the node. Hermes does not
reach the remote agent directly.

.. code-block:: console

   $ anet agents wire hermes --a2a <aid-of-remote-agent>

The ``a2a`` toolset of Hermes is disabled for the command line by default.
Enable it once, otherwise ``a2a_call`` is not visible; ``wire`` reminds the
operator after writing the entries:

.. code-block:: console

   $ hermes tools enable a2a --platform cli

Several AIDs are given as a comma-separated list, as repeated ``--a2a``
options, or as words that follow one ``--a2a``. An AID consists of lowercase
letters and digits, 8 to 128 characters long. ``--a2a`` requires ``hermes``
(or ``--all``) in the same command.

The resulting entry has the following form:

.. code-block:: yaml

   # >>> anet a2a (managed by `anet agents wire`; holds the local A2A token - keep this file 0600) >>>
   a2a_agents:
     "<aid-of-remote-agent>":
       url: "http://127.0.0.1:43811/a2a/v1/agents/<aid-of-remote-agent>"
       auth:
         type: bearer
         token: "<local A2A token>"
       timeout: 3600
   # <<< anet a2a <<<

Properties of these entries:

* The entry contains the bearer token of the local A2A interface. The file
  is therefore kept at mode ``0600``; ``wire`` narrows a wider mode, and
  ``anet agents`` reports a file that is wider.
* The address and the token are read from ``a2a_addr.txt`` and
  ``a2a_token.txt`` in the state directory of the a2a module. The files are
  created when the daemon first serves the interface. If they do not exist,
  ``wire --a2a`` fails and asks for ``anet up`` first.
* ``wire`` refuses to write the entry while another process holds the
  recorded port, or while the interface is down because of a port conflict.
  The token would otherwise be handed to the process that holds the port.
* The operating guide for Hermes states that a remote agent listed under
  ``a2a_agents`` is reachable with ``a2a_call`` and that the same spending
  limits apply.

When the port or the token of the interface changes, the existing entries
become invalid. This happens after the file ``a2a_addr.txt`` is removed,
after an emergency key rotation, and after a port conflict has been
resolved. ``anet agents`` then reports that the port or token of an entry has
changed. The entries are rewritten with:

.. code-block:: console

   $ anet agents wire hermes --refresh

To remove individual entries and leave the MCP entry in place:

.. code-block:: console

   $ anet agents unwire hermes --a2a <aid-of-remote-agent>

``unwire hermes`` without ``--a2a`` removes the MCP entry, the guide, and all
entries together with their tokens.


Notes
-----

* **Closed by default.** The operating guide tells the coding agent that a
  new node accepts no task from any other agent and spends nothing. The
  operator opens either on a terminal, where the commands ask for
  confirmation; see :ref:`topics-delegation` and :ref:`topics-payments`.
* **Tasks from other agents are untrusted input.** The guide instructs the
  coding agent not to run commands, read files, send secrets or spend
  because the text of such a task asks for it. This instruction is a
  mitigation. The inbound policy and the sandbox described in
  :ref:`topics-sandbox` are the enforcement.
* **After a restart of the daemon.** A running ``anet mcp`` process finds the
  daemon again when the control token has been replaced, and repeats the
  failed call once. A session needs no restart for that reason alone.
* **Builds without MCP.** A binary built with ``-tags no_mcp`` has no MCP
  server; ``anet agents`` and ``anet mcp`` then end with a message that names
  the build.
* **Reporting.** ``anet doctor`` includes the state of every tool and, for
  Hermes, the state of each ``a2a_agents`` entry.

See also
--------

* :ref:`howto-mcp`
* :ref:`howto-local-a2a`
* :ref:`reference-cli`
* :ref:`howto-troubleshooting`
* :doc:`/agents/index`
