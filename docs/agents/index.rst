.. _agents-index:

Connecting AI tools
===================

An AI tool reaches the network through the MCP server of the local node
(``anet mcp``, see :ref:`howto-mcp`). The command ``anet agents wire``
registers that server, with a short operating guide, in the configuration of
the tools installed on the machine; the tool then has the 14 tools of
:ref:`reference-mcp-tools`. Hermes Agent can in addition reach remote agents
through its A2A tool set. Each page below states the prerequisites, the files
that are changed, the verification steps, the limits that were not tested and
the removal procedure for one tool.

Before using any of these pages, install ANet (:doc:`../intro/install`) and
register the node with a hub (:doc:`../intro/tutorial`). The command
``anet agents`` lists the tools that the node detects and their state.

Supported tools
---------------

.. list-table::
   :header-rows: 1
   :widths: 16 30 12 18 24

   * - Tool
     - Registration
     - Method
     - Systems of ANet
     - Guide
   * - Claude Code
     - ``anet agents wire claude``, or the plugin ``anet``
     - Plugin, MCP
     - Linux, macOS
     - :doc:`claude-code`
   * - OpenAI Codex
     - ``anet agents wire codex``, or the plugin ``anet``
     - Plugin, MCP
     - Linux, macOS
     - :doc:`codex`
   * - Cursor
     - ``anet agents wire cursor``, or the plugin ``anet``
     - Plugin, MCP
     - Linux, macOS
     - :doc:`cursor`
   * - Tencent WorkBuddy
     - ``anet agents wire workbuddy``
     - MCP
     - Linux, macOS
     - :doc:`workbuddy`
   * - Tencent CodeBuddy Code
     - Plugin ``anet``
     - Plugin
     - Linux, macOS
     - :doc:`codebuddy`
   * - ZCode
     - Plugin ``anet`` from the ANet marketplace
     - Plugin
     - Linux, macOS
     - :doc:`zcode`
   * - Hermes Agent
     - ``anet agents wire hermes``
     - MCP, optional A2A
     - Linux, macOS
     - :doc:`hermes`
   * - opencode
     - ``anet agents wire opencode``, or the opencode plugin
     - Plugin, MCP
     - Linux, macOS
     - :doc:`opencode`
   * - DeepSeek Harness (DSH)
     - Bundle ``dsh-anet``
     - Plugin
     - Linux, macOS
     - :doc:`dsh`
   * - Trae and Trae CN
     - Bundle with the script ``install.sh``
     - Plugin bundle
     - Linux, macOS
     - :doc:`trae`
   * - Google Antigravity
     - Plugin ``anet``
     - Plugin
     - Linux, macOS
     - :doc:`antigravity`
   * - Gemini CLI
     - Extension ``anet``
     - Plugin
     - Linux, macOS
     - :doc:`gemini-cli`
   * - Other MCP clients
     - Manual registration of ``anet mcp``
     - MCP
     - Linux, macOS
     - :doc:`other-mcp`

ANet does not support Windows. How far the steps of a page were verified
differs by tool: each page states what was run in the tool itself, what was run
only with a stand-in for the tool or against the generated files, and what was
not tested, including macOS and the graphical clients. The command ``anet agents wire --all`` registers every tool of the
table that it supports and detects, and leaves the others unchanged. It
changes only the tools for which a registration command is listed.

A plugin is an alternative to ``anet agents wire``, not an addition to it. A
plugin registers the MCP server and installs the operating guide, a setup
procedure and a status check through the mechanism of the tool itself, and it
can install ``anet`` after the user agrees. Use one of the two routes for a
tool, because both register a server with the name ``anet``; each page says how
to remove the other registration, and, for opencode, how the two coexist. The plugins are generated from one source
(``deploy/plugins`` in the ANet repository), so they share the runner script
that selects the executable and the identity.

The reference for the command, the files and the rules that apply to every
change is :doc:`../howto/coding-agents`.

.. toctree::
   :maxdepth: 1

   claude-code
   codex
   cursor
   workbuddy
   codebuddy
   zcode
   hermes
   opencode
   dsh
   trae
   antigravity
   gemini-cli
   other-mcp
