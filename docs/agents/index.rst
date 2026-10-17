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
   :widths: 16 26 12 22 24

   * - Tool
     - Registration command
     - Method
     - Systems of ANet
     - Guide
   * - Claude Code
     - ``anet agents wire claude``
     - MCP
     - Linux, macOS
     - :doc:`claude-code`
   * - OpenAI Codex
     - ``anet agents wire codex``
     - MCP
     - Linux, macOS
     - :doc:`codex`
   * - Cursor
     - ``anet agents wire cursor``
     - MCP
     - Linux, macOS
     - :doc:`cursor`
   * - Tencent WorkBuddy
     - ``anet agents wire workbuddy``
     - MCP
     - Linux, macOS
     - :doc:`workbuddy`
   * - Hermes Agent
     - ``anet agents wire hermes``
     - MCP, optional A2A
     - Linux, macOS
     - :doc:`hermes`
   * - opencode
     - ``anet agents wire opencode``
     - MCP
     - Linux, macOS
     - :doc:`opencode`
   * - Other MCP clients
     - Manual registration of ``anet mcp``
     - MCP
     - Linux, macOS
     - :doc:`other-mcp`

ANet does not support Windows. The steps of every page were verified on
Linux; each page lists what was not tested on macOS and in the graphical
clients. The command ``anet agents wire --all`` registers every tool of the
table that it detects and leaves the others unchanged.

The reference for the command, the files and the rules that apply to every
change is :doc:`../howto/coding-agents`.

.. toctree::
   :maxdepth: 1

   claude-code
   codex
   cursor
   workbuddy
   hermes
   opencode
   other-mcp
