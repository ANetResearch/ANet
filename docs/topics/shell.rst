.. _topics-shell:

The shell module
================

The ``shell`` module lets a node run commands that its operator has approved, on request of
callers that the operator has listed. A typical use is a development board or a remote
machine on which a service has to be restarted, a log read or a firmware flashed from the
other end of the network, with the output returned to the caller and the permission
revocable at any time.

The module is the only one that turns a request from the network into a command on the host.
It is therefore *additive*: it is not part of the default build, and it is included only in
the build with the tag ``shell`` (``install.sh --shell``, ``install.ps1 -Shell`` on Windows, or
``./build.sh --with shell``). See :doc:`modules`.

.. contents::
   :local:
   :depth: 2


Three separate decisions
------------------------

Whether a node runs remote commands depends on three decisions, each of which is made on
purpose.

1. *The build.* A binary without the tag ``shell`` contains no code for it. A build with the
   tag registers no capability and runs no command until the second decision is made.
2. *The configuration.* The module acts only when ``config.json`` has a block
   ``modules.shell`` that defines at least one command or sets ``allow_arbitrary``. A
   binary that contains the module and has no block is inactive.
3. *The list of callers.* A block with commands and an empty list of callers refuses every
   call. An empty list means nobody, not everybody.

A caller must also pass the inbound policy of the node. The default policy is ``closed``, so
the caller has to be on ``peers.allow`` (``anet peers allow <aid>``) before a call reaches the
module. The two lists have different tasks: ``peers.allow`` decides who may hand tasks to the
machine, and the list of the module decides which of them may run commands. Do not list a
``shell.*`` capability in ``inbound.public_capabilities``. That would admit strangers and leave
the list of the module as the only barrier.

A call without the identity of a caller is refused unless ``allow_local`` is ``true``. The
empty value of the identity must not mean "local and trusted", because a call site that omits
the identity would then bypass the whole list without a trace.


Configuration
-------------

.. code-block:: json

   {
     "modules": {
       "shell": {
         "commands": {
           "restart-app": {"run": "systemctl restart myapp", "description": "Restart the application"},
           "tail-log":    {"run": "journalctl -u myapp -n 200 --no-pager"},
           "flash":       {"run": "/opt/tools/flash.sh", "args": true, "timeout_s": 600}
         },
         "allow_file": "/etc/anet/shell-allow",
         "timeout_s": 30,
         "max_output_bytes": 65536
       }
     }
   }

.. list-table::
   :header-rows: 1
   :widths: 32 68

   * - Key
     - Meaning
   * - ``commands.<name>.run``
     - The command line. It is run through the interpreter, so that pipes, redirections and
       ``&&`` can be used. The name must not contain a space, ``@`` or ``/``, because it
       becomes part of a capability identifier.
   * - ``commands.<name>.description``
     - A description for the capability list.
   * - ``commands.<name>.args``
     - ``true`` lets the caller append arguments. The default is ``false``.
   * - ``commands.<name>.dir``
     - The working directory.
   * - ``commands.<name>.timeout_s``
     - The time limit of this command. It overrides the global value.
   * - ``allow``
     - The AIDs that may call, as an inline list. It is read at the start.
   * - ``allow_file``
     - A file with the AIDs that may call, one for each line; ``#`` starts a comment. It is
       read again before every call. ``allow`` and ``allow_file`` are mutually exclusive; both
       together fail when the configuration is loaded.
   * - ``allow_local``
     - Admits calls without the identity of a caller. The default is ``false``.
   * - ``allow_arbitrary``
     - Offers ``shell.exec``, which runs any command that the caller sends. The default is
       ``false``.
   * - ``timeout_s``
     - The global time limit in seconds. The default is 30 and the largest value is 86,400 (24
       hours).
   * - ``max_output_bytes``
     - The largest size of the output that is returned. The default is 65,536 and the largest
       value is 4 MiB.
   * - ``shell``
     - The interpreter. The default is ``/bin/sh``, and ``powershell`` on Windows (see
       :ref:`topics-shell-windows`).

The node writes a warning to standard error at the start when no caller is allowed, and when
``allow_arbitrary`` is on.


Capabilities
------------

.. list-table::
   :header-rows: 1
   :widths: 28 72

   * - Capability
     - Behavior
   * - ``shell.list``
     - Reports the commands that the node defines and the number of callers on the list. A
       remote caller receives only the number, not the AIDs, so that an accepted caller does not
       learn which other machines the node trusts. A local call receives the full list.
   * - ``shell.run@<name>``
     - Runs one defined command.
   * - ``shell.exec``
     - Runs the command that the caller sends. It exists only while ``allow_arbitrary`` is
       ``true``; otherwise it is neither on the capability card nor available by name.

The names of the commands become capability identifiers such as ``shell.run@restart-app``,
and a node announces the capabilities that it answers. Anyone who can browse the directory of
a hub can therefore see which commands the machine offers. The list of callers still guards
them. A command whose existence should not be known has a name that carries no information.


Revoking access
---------------

The file named by ``allow_file`` is read again before every call and is not cached. Deleting
a line, or the whole file, takes effect with the next call and needs no restart. A missing file
is an empty list, which refuses everybody. It is neither an error nor permission for all. The
inline list ``allow`` changes only when the configuration is loaded again, so a production
node should use ``allow_file``.


Behavior
--------

* *No privilege escalation.* A command runs as the user of the daemon. A command that needs
  root requires a daemon that runs as root, which is a decision of the installation and is
  visible in the unit file. The module calls neither ``sudo`` nor ``setuid``.
* *Arguments do not become commands.* With ``args: true``, each argument of the caller is
  quoted and appended. The command line belongs to the operator, the arguments to the
  network, and the two are never mixed.
* *The time limit ends the process group.* Ending only the shell would leave the processes
  that it started on the machine.
* *The output is bounded.* A longer output is cut, and the output states the cut.
* *The effect status is accurate.* Exit status 0 is ``OK``. Any other status is ``FAILED`` with
  the exit status and the standard error. A command that is killed after the time limit is
  ``FAILED``, and the message states that it was killed.
* *Every run is on the evidence chain.* The event ``anet.shell.command`` records the caller,
  the command, the exit status and the duration. A refused call is recorded as
  ``anet.shell.refused``, with the AID of the caller. The output is not part of the chain.

The line that a ``run`` contains is not restricted. The operator who writes it is trusted, and
the trust boundary lies between what the operator wrote and what the network sent.


.. _topics-shell-windows:

Windows
-------

The module runs on Windows with the same access control: the caller must be on
``peers.allow`` and on the list of the module, and ``allow_local`` stays off. The installer
parameter ``-Controller`` writes such a configuration for exactly one AID, without a
confirmation: the parameter on the command line is the consent (see
:ref:`intro-install-windows-controller`).

*Interpreters.* The key ``shell`` accepts the following values. Any other value is a
configuration error.

.. list-table::
   :header-rows: 1
   :widths: 32 68

   * - Value
     - Interpreter
   * - ``powershell`` (the default)
     - ``%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe``. The path is fixed and
       never looked up on ``PATH``.
   * - ``pwsh``
     - PowerShell 7 (``pwsh.exe``).
   * - ``cmd``
     - ``cmd.exe``.
   * - An absolute path
     - A path to ``powershell.exe``, ``pwsh.exe``, ``cmd.exe``, ``sh.exe`` or ``bash.exe``.

*Invocation.* PowerShell receives the command as ``-EncodedCommand``, with ``-NoProfile``,
``-NonInteractive`` and ``-ExecutionPolicy Bypass``, and writes its output as UTF-8. A command
that would exceed the length limit of a Windows command line is refused.

*Arguments.* The arguments of a command with ``args: true`` are quoted for PowerShell: each
argument is enclosed in single quotes, and the single quote characters inside it, including the
typographic ones, are doubled. ``cmd`` does not accept arguments; a command with ``args: true``
is refused with that interpreter.

*Characters refused in arguments.* Windows runs a batch file (``.bat`` or ``.cmd``, including the
launchers that npm installs) through ``cmd.exe``, which interprets its arguments again with its
own rules. Quoting by the interpreter therefore does not protect an argument that reaches a batch
file. With every interpreter, an argument that contains one of the characters
``" % ! ^ & | < > ( )``, a carriage return or a line feed is refused, and the command does not
run. Commands whose arguments need these characters are to be declared without ``args``, with
the complete command line in ``run``.

*Process trees.* Each command runs in a Job object. On a timeout or a cancellation the whole
process tree is terminated. A process that a command leaves running survives the command, as
on Linux and macOS, but ends when the daemon ends.

*Working directory.* The default working directory is the profile directory of the user.

*Privileges.* Commands run as the user that runs the node. The node runs as the logged-on
user and never as a Windows service, and the module uses no elevation mechanism. When the
installer runs in an elevated window, it starts the node with standard user rights if Windows
provides them, and states otherwise (see :ref:`intro-install-windows`).

A configuration for Windows:

.. code-block:: json

   {
     "modules": {
       "shell": {
         "shell": "powershell",
         "commands": {
           "app-status": {"run": "Get-Process -Name myapp", "description": "Report the application process"},
           "tail-log":   {"run": "Get-Content -Tail 200 $env:LOCALAPPDATA\\MyApp\\app.log"},
           "disk-free":  {"run": "Get-PSDrive -PSProvider FileSystem"}
         },
         "allow_file": "C:\\Users\\operator\\.anet\\shell-allow",
         "timeout_s": 30
       }
     }
   }
