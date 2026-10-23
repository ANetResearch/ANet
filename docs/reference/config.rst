.. _reference-config:

Configuration
=============

A node reads its configuration from ``config.json`` in its data directory. A hub
reads command-line flags and, for federation, ``federation.json`` in its data
directory. This page lists every key, its default and the way to change it.

The configuration has few keys on purpose. A key exists only when someone needs a
value other than the default and the value cannot be derived from other
information. Internal timings, limits that form a security boundary and the
location of files inside the data directory are constants and not settings.

.. contents::
   :local:
   :depth: 2


How the configuration is changed
--------------------------------

``anet init`` writes the defaults and never changes a value that is already set.
Three rules govern the entry points.

* A key that changes trust, spending or identity is changed only through the
  command line, and the command asks for confirmation on the terminal. Without a
  terminal it refuses with ``tty_required``.
* Every other key is edited in ``config.json``. The daemon reads the file when it
  starts, so a change takes effect after a restart. A key that has a command takes
  effect at once when it is set through that command.
* The MCP tools never change the configuration.

The daemon refuses to start when ``config.json`` contains a removed key, and
prints the sentence ``config_key_removed`` with the replacement
(:ref:`reference-config-removed`). It also refuses to start when a value fails
validation (``config_invalid``). ``anet doctor`` reports removed keys under
``config.removed`` before the daemon is started.

.. code-block:: console

   $ anet init
   $ anet doctor --json

A complete minimal ``config.json`` after a join looks as follows. Keys that have
their default value need not appear.

.. code-block:: json

   {
     "control_addr": "127.0.0.1:46215",
     "hub_url": "https://hub.example.com",
     "name": "coder",
     "caps": ["translate"],
     "inbound": {"policy": "closed", "public_capabilities": []},
     "payments": {"auto_max": 0, "agent_max": 0, "agent_daily_max": 0,
                  "explicit_max": 10, "daily_max": 50}
   }

The port in ``control_addr`` is an example. ``anet id new`` assigns a free port.


Top-level keys
--------------

``control_addr``
   Default: ``127.0.0.1:<assigned>``. The listen address of the control plane. It
   differs per identity so that several identities coexist on one machine. It is
   written by ``anet id new`` and rarely edited.

``hub_url``
   Default: empty. The hub of the node. It is written only by ``anet hub-register``.

``name``, ``caps``
   Default: empty. The display name and the capability tags that the directory
   shows. Set with ``anet hub-register --name`` and ``--caps``.

``summary``, ``readme``
   Default: empty. The self-description of the agent. Set with ``anet profile set``.

``no_response_after``
   Default: ``"15m"``. One value governs two limits: how long a delegation may stay
   unconfirmed before it fails with ``delivery_unconfirmed``, and how long, after
   the other node confirmed it, the other side may stay silent before the task
   ends with ``no_response``. Long tasks need a larger value. The value ``"0"``
   waits for the full message lifetime of 14 days. Valid values are a Go duration
   or ``"0"``. The delivery line of a task names the time at which the node gives
   up.

``stream_preview``
   Default: ``"off"``. Whether this node sends previews as a provider. The values
   are ``off``, ``on`` and ``with_reasoning``. Set with ``anet preview``;
   ``with_reasoning`` needs terminal confirmation. See :doc:`../topics/preview`.

``content_retention``
   Default: ``"30d"``. How long the node keeps the content of a task after the
   task ends. Valid values are 1 to 3650 days written as ``<N>d``, or ``forever``.
   Calls to public capabilities keep their content for 7 days regardless. Set
   with ``anet retention set``, which asks for confirmation.

``without``
   Default: not set. An array of module names that are switched off. A listed
   module never starts, and the node handles its absence in the same way as for a
   module that is not compiled in. Names must be known module names and must not
   repeat; otherwise the file fails validation with ``config_invalid``. The key
   is edited in ``config.json`` or with ``anet init --without`` (names are added,
   never removed) and ``anet init --with`` (one name is removed). The control
   plane, MCP and the local A2A interface have no way to write it. See
   :doc:`../topics/modules`.

``inbound``
-----------

``inbound.policy``
   Default: ``closed``. Which peers may send tasks. ``closed`` accepts tasks only
   from peers on the allow list, ``approve`` holds tasks of other peers until an
   operator approves them, and ``open`` accepts tasks from anyone. Set with ``anet
   inbound policy``; loosening needs terminal confirmation.

The lists are the files ``peers.allow``, ``peers.trust`` and ``peers.deny`` in the
data directory. They are managed with ``anet peers``. The queue of tasks that wait
for approval holds at most 200 tasks, at most 3 per peer, for 72 hours. These are
constants.

``inbound.public_capabilities``
   Default: ``[]``. The capabilities that this node offers to peers, as an array
   of objects. Each object has these keys.

   .. list-table::
      :header-rows: 1
      :widths: 28 14 58

      * - Key
        - Default
        - Description
      * - ``id``
        -
        - Required. The capability identifier that callers use.
      * - ``per_caller_per_min``
        - 60
        - Calls per caller and minute.
      * - ``per_caller_per_day``
        - 2000
        - Calls per caller and UTC day.
      * - ``global_per_min``
        - 1200
        - Calls per minute from all callers.
      * - ``max_inflight``
        - 16
        - Calls running at the same time.
      * - ``per_caller_inflight``
        - 1
        - Calls of one caller running at the same time; at least 1 and at most
          ``max_inflight``.
      * - ``max_args_bytes``
        - 4096
        - Largest size of the arguments object.
      * - ``files``
        - none
        - The files that a call may carry: ``count`` (1 to 8), ``max_bytes``
          (1 byte to 64 MiB) and ``types`` (at least one content type that the Go
          function ``http.DetectContentType`` can detect). The capability must be
          provided by the ``service`` module. The declared type of a file is not
          trusted; the node detects the type from the content.
      * - ``discard``
        - ``false``
        - The node does not keep the files of a call, and it clears the arguments
          and the result when the task ends. This restricts only the storage of
          ``anet``, not the behavior of the backend program.
      * - ``allow_only``
        - ``false``
        - The capability is offered only to peers on the allow list or the trust
          list. It does not appear on any card or registry entry that a hub can
          read.

The declaration of a capability is described in :doc:`../topics/capabilities`.

``inbound.admit``
   Default: not set. Delegates the admission of text tasks from peers that are on
   no list to a capability of the ``service`` module (since 0.3.9). An object with
   these keys.

   .. list-table::
      :header-rows: 1
      :widths: 28 14 58

      * - Key
        - Default
        - Description
      * - ``capability``
        -
        - Required. A capability of ``modules.service`` that is marked
          ``"internal": true``.
      * - ``cache_seconds``
        - 30
        - How long the decision for one peer is kept, from 1 to 300 seconds. Both
          admissions and refusals are kept. A question that fails is kept for
          5 seconds.

   For a text task from a peer that is not on the allow, trust or deny list, the
   node calls the capability with the header ``X-ANet-Caller`` set to the AID of
   the peer and the body ``{}``. Only the reply ``{"admit": true}`` admits the
   peer. Any other reply, an error, a service that does not answer within
   5 seconds or a capability that is not served admits nobody. An admitted peer's
   task is accepted as the task of a peer on the allow list would be, under every
   policy. Its turns carry ``Admitted`` for the auto-reply module
   (see ``auto_reply.admitted``), and its tasks carry ``anet.admitted: true`` for
   A2A backends. Admission grants nothing else: payments, the trust list and the
   peer lists are unchanged. The deny list is read first and always takes
   precedence. Capability calls are not affected. The decision is made again for
   every turn, every message and every delivery to a backend, so a peer that the
   service no longer admits is refused once its cached decision expires.

   Set with ``anet inbound admit set <capability> [--cache-seconds N]`` and
   removed with ``anet inbound admit off``. The capability must not also be in
   ``inbound.public_capabilities``. A value that breaks these rules is refused
   with ``config_invalid``, and the daemon does not start with it.


``auto_reply``
--------------

The block is absent by default, which means that the auto-reply is off.

.. list-table::
   :header-rows: 1
   :widths: 28 22 50

   * - Key
     - Default
     - Description
   * - ``backend``
     - ``openai``
     - ``exec`` starts a local coding agent, ``openai`` calls an
       OpenAI-compatible API. Set with ``anet autoreply set --backend``.
   * - ``agent``
     -
     - Required for ``exec``: ``cursor``, ``claude``, ``codex``, ``opencode``,
       ``openclaw`` or ``hermes``.
   * - ``untrusted``
     - ``off``
     - ``off`` or ``sandbox``. Whether tasks of peers that are not trusted are
       answered, and in the sandbox.
   * - ``trusted``
     - ``direct``
     - ``direct`` runs the agent as the user of the node for trusted peers;
       ``sandbox`` runs it in the sandbox. Changing from ``sandbox`` to ``direct``
       is confirmed on the terminal.
   * - ``admitted``
     - ``off``
     - How the ``exec`` backend runs the tasks of peers that ``inbound.admit``
       admits (since 0.3.9). ``off`` treats them as the tasks of peers that are
       not trusted, as ``untrusted`` says; ``direct`` runs the agent as the user
       of the node, without the sandbox; ``sandbox`` runs it in the sandbox,
       within the run budget. Set with ``anet autoreply set --admitted``;
       turning on ``direct`` is confirmed on the terminal. A trusted peer is
       governed by ``trusted`` only.
   * - ``api_base``, ``model``, ``system_prompt``
     - empty
     - Settings of the ``openai`` backend; ``model`` also overrides the model of
       an ``exec`` agent.
   * - ``api_key``
     - empty
     - Write-only. Set with ``--api-key-stdin``. The value is not shown by
       ``anet autoreply show``, the control plane or the logs.
   * - ``work_dir``
     - a directory in the data directory
     - Working directory of runs in ``direct`` mode. An invalid path is reported
       as ``work_dir_invalid``.
   * - ``command``
     - found on ``PATH``
     - Path of the agent program.
   * - ``extra_args``
     - ``[]``
     - Additional arguments for the agent program.
   * - ``openclaw_agent``
     - ``main``
     - The ``--agent`` value for ``openclaw``.
   * - ``max_history``
     - 20
     - Conversation turns sent to the backend.
   * - ``api_timeout_seconds``
     - 180
     - Timeout of one backend call. It is also the wall-clock limit of a run in
       the sandbox.
   * - ``sandbox.egress_allow``
     - ``[]``
     - Exact host names that sandboxed runs may reach on port 443 through
       CONNECT. Every listed host is an outbound channel, so adding one is
       confirmed on the terminal. See :doc:`../topics/sandbox`.
   * - ``sandbox.runs_per_day``
     - 20
     - Runs per local calendar day for peers that are not trusted. A value of 0
       means that no run is made for them; the task stays in the inbox. An invalid
       value is reported as ``budget_invalid``.
   * - ``sandbox.runs_per_peer_per_day``
     - 5
     - The same limit for one peer.
   * - ``runner``, ``model_proxy``, ``admitted_budget``
     - absent
     - Direct runs as another local account, the model proxy and the budget of
       admitted peers. See :ref:`reference-config-runner`.

The interval at which the daemon looks for tasks (5 seconds) and the cap of 30
automatic messages per interaction are constants.

Every write of the block is recorded as the evidence event ``anet.policy.changed``.
The event names the key as a whole when its summary differs, and there is one
record for each audited field: ``auto_reply.trusted``, ``auto_reply.admitted``,
``auto_reply.sandbox.egress_allow``, ``auto_reply.sandbox.runs_per_day``,
``auto_reply.sandbox.runs_per_peer_per_day`` and ``auto_reply.api_key_set``. Turning
the block on or off is recorded in the same way against the defaults. A block that
is rejected is answered with the sentence of the module (``config_invalid``,
``budget_invalid``, ``work_dir_invalid``, ``api_base_insecure`` or
``egress_allow_*``) and status 400, and a conflict with the inbound policy with 409.
A daemon that finds an ``auto_reply`` block while the auto-reply module is not
available refuses to start (``module_absent.configured``).

.. _reference-config-runner:

``auto_reply.runner``, ``auto_reply.model_proxy``, ``auto_reply.admitted_budget``
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

These blocks apply to the ``exec`` backend only and are absent by default. The
procedure is described in :doc:`../howto/runner`.

.. list-table::
   :header-rows: 1
   :widths: 34 16 50

   * - Key
     - Default
     - Description
   * - ``runner.socket``
     -
     - Absolute path of the Unix socket of ``anet runner serve``. When the block
       is set, every direct run (trusted or admitted peer) is sent to the runner
       as a job. Not available on Windows (``runner_unsupported``).
   * - ``runner.expected_user``
     - the owner of the socket
     - The account the runner runs as, by name or uid. The listener on the
       socket must be this account; otherwise nothing is sent
       (``peer_uid_mismatch``).
   * - ``runner.socket_group``
     -
     - A group whose members may write a directory on the path of the socket.
       The path rules are those of the service backends (see the backend
       policy below).
   * - ``model_proxy.listen``
     -
     - Loopback address and port of the listener, for example
       ``127.0.0.1:47811``. Port 0 selects a free port.
   * - ``model_proxy.upstreams[].name``
     -
     - Name of the upstream in the route ``/anet-upstream/<name>/``: lower-case
       letters, digits, ``-`` and ``_``, at most 32 characters, unique, and not
       ``anthropic``, ``openai`` or ``custom``. At most 8 upstreams.
   * - ``model_proxy.upstreams[].api_base``
     -
     - Address of an OpenAI-compatible chat completions service: ``https``, or
       ``http`` on the loopback interface (``api_base_insecure`` otherwise).
   * - ``model_proxy.upstreams[].api_key_file``
     - none
     - Absolute path of a file whose first line is the key of the service. It is
       read by the node when the proxy starts; the key never reaches a run.
   * - ``model_proxy.upstreams[].model``
     - none
     - When set, every request is pinned to this model.
   * - ``model_proxy.upstreams[].max_output_tokens``
     - 32768
     - The highest output limit of a forwarded request, from 1 to 131072. A
       request that asks for more, or for no limit, is sent with this limit. A
       sandboxed run keeps the limit of 8192.
   * - ``admitted_budget.runs_per_day``
     - 50
     - Runs of admitted peers per local calendar day whose model requests are
       forwarded. Later runs receive ``model_budget_spent`` for each model
       request.
   * - ``admitted_budget.runs_per_peer_per_day``
     - 10
     - The same limit for one peer. It may not exceed ``runs_per_day``
       (``budget_invalid``).
   * - ``admitted_budget.requests_per_run``
     - 100
     - Model requests of one run of an admitted peer. A run of a trusted peer may
       make up to 1000.

A run that the node starts directly, or sends to the runner, receives the
variables ``ANET_MODEL_BASE``, ``ANET_MODEL_KEY``, ``ANET_TURN_URL`` and
``ANET_TURN_TOKEN`` (see :ref:`reference-config-run-env`). A sandboxed run
receives none of them.


``payments``
------------

.. list-table::
   :header-rows: 1
   :widths: 28 12 60

   * - Key
     - Default
     - Description
   * - ``auto_max``
     - 0
     - Largest amount in credits that the node pays by itself for one task.
   * - ``agent_max``
     - 0
     - Largest amount for one payment that a coding agent submits through MCP.
   * - ``agent_daily_max``
     - 0
     - Total per day for payments that a coding agent submits.
   * - ``explicit_max``
     - 10
     - Largest amount for one payment that the operator confirms with ``anet pay``.
   * - ``daily_max``
     - 50
     - Total per day over all payments. Automatic payments count toward it.

All five keys are changed only with ``anet payments set``, which asks for
confirmation. The list of payees for automatic payments is the file
``payees.allow`` in the data directory, managed with ``anet payees``. The list is
always in force. A quote that is higher than the price in the signed extended
card of the provider is not paid automatically.


``modules``
-----------

Optional modules read their configuration under ``modules.<name>``. See
:doc:`../topics/modules` for the modules that exist and for the build tags that
leave a module out.

``modules.a2a``
   The local A2A interface and the A2A backends that the node reaches.

   ``backends[]``
      Objects with ``match`` (which peers or tasks use the backend), ``url``,
      ``token_file``, ``accept_untrusted`` and ``toolless``. A backend that
      listens on a Unix socket also accepts the keys of the *backend policy*
      below.

   ``retry``
      ``max_interval`` (default ``2m``) and ``give_up_after`` (default ``10m``).

``modules.anetlink``
   The connection to an ANetLink process, which serves capabilities through the C1
   interface. The interface runs over a Unix socket only.

   ``socket``
      Default: ``$XDG_RUNTIME_DIR/anetlink/c1.sock``. Required when
      ``XDG_RUNTIME_DIR`` is not set.

   The keys of the backend policy ``expected_user`` and ``socket_group`` apply.
   ``allow_tcp: true`` is refused with ``config_invalid``.

``modules.service``
   Capabilities that are served by local HTTP or Unix-socket programs.

   ``capabilities[]``
      Objects with ``id``, ``url``, ``price``, ``description``, ``protocol``,
      ``name``, ``tags``, ``examples``, ``input_modes``, ``output_modes``,
      ``timeout_ms``, ``token_file``, ``internal``, ``agent_tool`` and
      ``input_schema``. ``url`` is
      ``unix:///path/to/socket[:/request/path]`` (recommended) or ``http(s)://``
      with ``allow_tcp``. ``price`` is an amount in credits; a capability without
      a price is free. A listed price is published in the signed extended card
      only. For a capability that declares ``files``, ``input_modes`` follows from
      the declaration and must not contradict it.

      ``internal: true`` (since 0.3.9) marks a capability that only this node
      calls, such as the capability of ``inbound.admit``. It appears on no card
      and in no registration, and a call from the network is answered as for a
      capability that the node does not have. An internal capability has no
      price and is not an agent tool.

      ``agent_tool: true`` (since 0.3.9) marks a capability that an agent which
      runs for a peer may call as that peer. The node sends the call to the same
      backend with the same token as a call from the network, with
      ``X-ANet-Caller`` set to the AID of the peer and ``X-ANet-Via`` set to the
      origin of the call, for example ``agent``. A capability without the flag
      cannot be called this way (``not_agent_tool``). A peer on the deny list
      cannot call it at all. ``input_schema`` is an optional JSON Schema object,
      at most 16 KiB, that describes the arguments for the agent together with
      ``description``.

   ``timeout_ms``, ``token_file``
      Defaults for all capabilities (the default timeout is 2 minutes). The token
      file must have an absolute path, may use ``$VAR`` and ``${VAR}``, and must
      not be readable by other users.

``modules.p2p``
   Direct connections between nodes.

   ``socket``
      The socket of the peer process.

   ``advertise``
      Default: empty. The ``host:port`` address that contacts dial to reach the
      node directly. It is given only to contacts that are on a list and never
      appears on a card that anyone can read.

   ``lan``
      Default: ``false``. Permits direct connections to private addresses. Use it
      only in a laboratory on one network.

``modules.x402``
   The payment module has no setting in 0.3. The balance proofs of the hub are
   checked automatically every 6 hours.

``modules.cas``, ``modules.shell``, ``modules.blackboard``, ``modules.org``
   Optional modules with their own settings. They are unchanged in 0.3.

Backend policy (all Unix-socket backends)
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

A module that connects to a local backend over a Unix socket checks the user of
the process that listens on the socket.

``expected_user``
   User name or numeric uid that must own the listening process. Not set by
   default.

``socket_group``
   Group of the socket. Required when more than one other user may connect. Not
   set by default.

``peer_check``
   Default: ``""`` (strict). The value ``"path-only"`` selects an explicit
   degraded mode for platforms where the peer user cannot be read. Only the
   directory chain is then checked.

``allow_tcp``
   Not set by default. Permits ``http(s)`` backends on TCP. It is refused for
   ``modules.anetlink``.


The data directory
------------------

The data directory is ``<home>/``, ``<home>/ids/<name>/`` or the directory named by
``ANET_DATA_DIR`` (see :doc:`cli`). The files that matter for operation are:

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - File or directory
     - Content
   * - ``config.json``
     - The configuration described on this page.
   * - ``peers.allow``, ``peers.trust``, ``peers.deny``
     - The peer lists, one AID per line, with mode ``0600``. The daemon reads them
       for every decision, so an edit takes effect at the next message without a
       restart. A missing file is an empty list.
   * - ``payees.allow``
     - The payee list for automatic payments.
   * - ``interactions.db``
     - The store of tasks and the evidence chain. The SQLite ``user_version`` is 3.
   * - ``enc_keys.cbor``
     - The encryption key ring; the format version ``v`` is 2.
   * - ``modules/<name>/``
     - Private state of a module. The local A2A interface keeps its address in
       ``a2a_addr.txt`` and its bearer token in ``a2a_token.txt`` there.

A data directory that an earlier or later release wrote is refused: version 0.2
data with ``identity_v02``, and data of a newer release with ``datadir_too_new``.
See :doc:`../upgrading`.


Environment variables
---------------------

.. list-table::
   :header-rows: 1
   :widths: 24 76

   * - Variable
     - Effect
   * - ``ANET_HOME``
     - The home directory that holds the identities. Default ``~/.anet``.
   * - ``ANET_ID``
     - Selects the identity, like ``--id``.
   * - ``ANET_DATA_DIR``
     - Names one raw data directory outside the home layout.
   * - ``ANET_LANG``
     - Selects the language of sentences: Chinese when the value begins with
       ``zh`` (any letter case), English otherwise. The locale variables
       ``LC_ALL``, ``LC_MESSAGES`` and ``LANG`` are not consulted.
   * - ``ADMIN_TOKEN``
     - The credential of ``anet-hub-admin``.

.. _reference-config-run-env:

Variables of an agent run
~~~~~~~~~~~~~~~~~~~~~~~~~

The environment of a direct run of the ``exec`` backend holds no ``ANET_*``
variable of the daemon. The node adds only the variables of the run itself:

.. list-table::
   :header-rows: 1
   :widths: 24 76

   * - Variable
     - Value
   * - ``ANET_OUTBOX``
     - The directory whose files are sent with the reply.
   * - ``ANET_MODEL_BASE``
     - ``http://<model_proxy listener>/anet-upstream``, when the model proxy has
       an upstream.
   * - ``ANET_MODEL_KEY``
     - The key of the run for the model proxy, valid while the run is under way.
   * - ``ANET_TURN_URL``
     - ``http://<model_proxy listener>/anet-turn/v1``, for an inbound task of a
       trusted or admitted peer when the service module has ``agent_tool``
       capabilities.
   * - ``ANET_TURN_TOKEN``
     - The turn token of the run, bound to the peer, the interaction and the
       run.

A variable ``ANET_INVITE`` from earlier releases is ignored. ``anet doctor`` reports it
under ``config.removed`` with level ``warn``, and the daemon does not refuse to
start because of it.


Hub configuration
-----------------

Command-line flags
~~~~~~~~~~~~~~~~~~

The flags of ``anet-hub`` are listed in :doc:`cli`. Limits that depend on the scale
of the hub are constants. The quota of a single recipient is raised per AID with
``anet-hub quota``; no global flag exists for it.

Registration policy and constants
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

A new hub admits agents by AID (``closed``). ``anet-hub admission open`` lets any
agent register, protected by three gates: a proof of work of 22 bits, a daily
quota per address range (IPv6 addresses are counted per /32), and a probation
period of 72 hours in which an account cannot upload reviews. A new account
receives no credits at registration. After the probation period the hub issues 20
credits, bounded by daily budgets per address range and for the whole hub. These
values are constants and identical on every hub.

``federation.json``
~~~~~~~~~~~~~~~~~~~

The file ``federation.json`` in the data directory of a hub enables federation.
When the file is absent, federation is off.

.. code-block:: json

   {
     "delivery": "allowlist",
     "discovery": "allowlist",
     "home": "https://hub.example.com",
     "peers": [
       {"aid": "bafyrei...peer-hub-aid", "endpoint": "https://hub2.example.net"}
     ],
     "witness": "on"
   }

``delivery``
   ``off`` or ``allowlist``. With ``allowlist`` the hub relays envelopes to and
   from the listed peers.

``discovery``
   ``off`` or ``allowlist``. Switches independently of ``delivery``: a hub may
   carry the traffic of a peer without publishing its directory to it.

``home``
   The public endpoint of this hub. It is sent as the routing hint on every card
   that the hub serves.

``peers``
   The peer hubs, each with ``aid`` and ``endpoint``. Peers are configured
   statically; there is no automatic discovery of hubs.

``witness``
   ``on`` (the default when absent) or ``off``. Whether the hub pins the heads of
   the issuance chains of its peers. The setting has an effect only when peers
   are listed.

See :doc:`../ops/federation`.


ANetLink
~~~~~~~~

The program ``anetlinkd`` takes its settings with ``--config`` and has these keys
for the C1 interface.

``c1.socket`` (``--c1-socket``)
   Default: ``$XDG_RUNTIME_DIR/anetlink/c1.sock``, which is also the default of
   ``modules.anetlink.socket``. Both sides connect without further settings.

``c1.allow_users`` (``--c1-allow-user``, repeatable)
   Default: empty, which allows only the user of ``anetlinkd``. Required when the
   daemon and ``anetlinkd`` run as different users.

``c1.group`` (``--c1-group``)
   Default: not set. Required when more than one other user connects.

``c1.remote``
   Default: empty, which refuses remote calls for all capabilities.

.. _reference-config-removed:

Removed keys
------------

A removed key in ``config.json`` stops the daemon with ``config_key_removed``. This
is not a migration. It prevents an old file from being ignored without notice.

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Removed
     - Replacement
   * - ``pricing``; ``payments.publish_prices``
     - Prices are given only by the signed extended card. Remove the key.
   * - ``modules.x402.voucher_addr``, ``voucher_url``
     - The voucher gate is gone. Remove both.
   * - ``modules.x402.witness_hub``, ``witness_every_seconds``
     - Balance checks run automatically. Remove both.
   * - ``providers``, ``providers.anetlink``
     - Use ``modules.anetlink``.
   * - ``accept_delegations``
     - Use ``anet inbound policy``.
   * - ``rotation_grace``
     - The grace period of a scheduled rotation is fixed at 1 hour.
   * - ``inbound.allow_file``, ``deny_file``, ``trust_file``
     - The lists are ``peers.allow``, ``peers.deny`` and ``peers.trust`` in the data
       directory.
   * - ``inbound.reject_notice``
     - Refusals travel with the delivery confirmation.
   * - ``inbound.pending``
     - The queue limits are fixed: 200 tasks, 3 per peer, 72 hours.
   * - ``public_capabilities[].evidence``
     - Evidence keeps content identifiers only.
   * - ``auto_reply.require_image``, ``usage_hint``
     - Turn the vision service into a public capability that takes files.
   * - ``auto_reply.error_reply``
     - When the backend fails, the requester sees the standard ``backend_failed``
       sentence.
   * - ``auto_reply.poll_interval_seconds``, ``max_auto_replies``
     - Fixed values.
   * - ``payments.payees_file``
     - The list is ``payees.allow``; manage it with ``anet payees``.
   * - ``modules.anetlink.timeout_s``
     - Fixed value.
   * - ``expected_uid`` in ``modules.service``, ``modules.a2a.backends[]`` and
       ``modules.anetlink``
     - Use ``expected_user``, which also accepts a numeric uid.
   * - ``modules.p2p.dial_timeout_ms``, ``reach_timeout_ms``
     - Fixed values.
   * - ``modules.taskboard``
     - The shared task board of the hub was removed. Use ``anet delegate``.

The hub flags that were removed are listed in :doc:`cli`.
