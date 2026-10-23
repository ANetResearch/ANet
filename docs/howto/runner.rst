.. _howto-runner:

Running the agent as another account
====================================

A node that answers tasks with a local coding agent (``auto_reply.backend`` set
to ``exec``) runs that agent directly, as the account of the node, for trusted
peers, and in the sandbox for other peers (see :doc:`../topics/sandbox`). A
direct run gives the agent the network, the tools and the files of that
account. This is appropriate when the peers are the operator's own agents. It
is not appropriate when the peers are many people, for example the members of
an organisation, each of whom can steer the agent into running any program.

This page describes how to run the agent of direct runs as a separate local
account, how to keep the model keys with the node, and how the agent uses the
capabilities of the node on behalf of the peer it serves. Three settings work
together:

``auto_reply.runner``
   Sends every direct run to ``anet runner serve``, a process that runs as
   another account and starts the agent there.
``auto_reply.model_proxy``
   A listener of the node on the loopback interface that forwards the model
   requests of a run to the configured model services and adds their keys. The
   agent holds a key that is valid for one run only.
``anet turn-mcp``
   An MCP server, started by the agent, that offers the capabilities of the
   service module marked ``agent_tool`` and calls them as the peer of the run.

The runner is available on Linux and macOS. On other platforms the command and
the setting are refused with ``runner_unsupported``.

.. contents::
   :local:
   :depth: 2


What the separation provides
----------------------------

With the three settings in place, the following holds even though a peer can
make the agent run arbitrary programs:

* The agent runs as the runner account. It cannot read the identity keys, the
  control token, the configuration or the data of the service module, which
  belong to the account of the node.
* The agent never holds a model key of the operator. Its environment contains
  the address of the model proxy and a random key that the proxy accepts only
  while the run is under way. The proxy adds the key of the model service
  itself.
* The agent acts on the node only as the peer whose task it serves. The turn
  token in its environment is bound by the node to the caller, the interaction
  and the run. A request names a capability and its arguments, never a caller.
* Every connection to the runner is checked with the peer credentials of the
  Unix socket, in both directions. The runner accepts jobs only from the
  account of the node, and the node sends jobs only to a listener that runs as
  the expected account.

The following risks remain and are accepted by this design. Every program that
the agent starts runs as the runner account, so it can read the work
directories of other peers on the runner, and, while another run is under way,
the environment of that run, including its run key and turn token. With that
token it can act as the other peer until that run ends. Writes that a service
receives through the turn tools carry ``X-ANet-Via: agent`` so that the service
can mark them. Operators who cannot accept these risks use the sandbox instead.


Preparing the accounts
----------------------

The examples use two accounts: ``anet-org`` runs the node, ``anet-hermes`` runs
the runner and the agent. The socket lives in a directory that both can reach
and only ``anet-hermes`` and root can change.

.. code-block:: console

   # useradd --system --create-home anet-hermes
   # groupadd anet-runner
   # usermod -aG anet-runner anet-org
   # install -d -o anet-hermes -g anet-runner -m 0750 /run/anet-runner

The socket is created with mode ``0660``. With ``--socket-group anet-runner``
the runner gives it that group, so that ``anet-org`` can connect. Directories
on the path that are writable by a group are accepted by the node only when
``auto_reply.runner.socket_group`` names that group, or when the group is the
private group of the owner.

Install the agent (for example hermes) for ``anet-hermes``, and remove any model
key from its configuration and environment: the agent receives its model
address and key from the run.


Starting the runner
-------------------

Run the runner as ``anet-hermes``, typically as a systemd user unit of that
account:

.. code-block:: ini

   [Unit]
   Description=ANet runner

   [Service]
   ExecStart=/usr/local/bin/anet runner serve --socket /run/anet-runner/hermes.sock \
       --allow-user anet-org --socket-group anet-runner \
       --agent hermes=/home/anet-hermes/.local/bin/hermes --max-jobs 4
   Restart=on-failure

   [Install]
   WantedBy=default.target

The options are described in :ref:`reference-cli-runner`. The runner uses no
data directory and no identity. It writes one log line per job, with the short
form of the peer, the agent, the exit and the duration; it never logs the
conversation, the reply or the environment.

Each job runs in a work directory of the runner account:

``per-peer`` (default)
   ``<cache>/anet/runner/<peer>/``, where ``<peer>`` is the last 16 characters
   of the AID of the peer. The directory is kept for ``--keep-days`` days after
   its last job (14 by default), so that a peer finds the results of earlier
   tasks. ``<cache>`` is ``$XDG_CACHE_HOME`` or ``~/.cache`` on Linux and
   ``~/Library/Caches`` on macOS.
``per-run``
   A new directory for every job, removed when the job ends.

Each job also gets an input directory for the conversation and its images, and
an outbox; both are removed when the job ends. Files that the agent writes to
``$ANET_OUTBOX`` are returned to the node and sent with the reply, within the
limits of the outbox (16 files, 64 MiB in total).


Configuring the node
--------------------

Add the three blocks to the ``auto_reply`` block of the node and keep the model
keys in files that only the account of the node can read:

.. code-block:: json

   "auto_reply": {
     "backend": "exec",
     "agent": "hermes",
     "untrusted": "off",
     "runner": {
       "socket": "/run/anet-runner/hermes.sock",
       "expected_user": "anet-hermes",
       "socket_group": "anet-runner",
       "max_jobs": 4
     },
     "model_proxy": {
       "listen": "127.0.0.1:47811",
       "upstreams": [
         {"name": "plan", "api_base": "https://ark.cn-beijing.volces.com/api/plan/v3",
          "api_key_file": "/home/anet-org/.config/anet-keys/plan.key"},
         {"name": "api", "api_base": "https://ark.cn-beijing.volces.com/api/v3",
          "api_key_file": "/home/anet-org/.config/anet-keys/api.key"}
       ]
     },
     "admitted_budget": {"runs_per_day": 50, "runs_per_peer_per_day": 10, "requests_per_run": 100}
   }

The keys are described in :ref:`reference-config-runner`. Restart the daemon,
or apply the block with the control plane; ``anet autoreply show --json`` then
lists the runner, the listener of the model proxy and the names of the
upstreams, and never a key or a key file.

A direct run, for a trusted peer or for a peer that ``inbound.admit`` admitted,
is then sent to the runner as a job instead of being started by the node.
Sandboxed runs are unchanged and do not use the runner, the model proxy or the
turn tools.

Direct runs proceed side by side: the node sends at most ``runner.max_jobs``
jobs at once (4 by default; set it to the ``--max-jobs`` of the runner), and at
most one per peer. A long task of one peer therefore does not delay the tasks
of other peers. A further task of the same peer, or a task that finds every
place taken, starts when a place becomes free.


Configuring the agent
---------------------

A run receives four variables in its environment:

.. list-table::
   :header-rows: 1
   :widths: 26 74

   * - Variable
     - Value
   * - ``ANET_MODEL_BASE``
     - ``http://127.0.0.1:<port>/anet-upstream``. The upstream named ``plan``
       is at ``${ANET_MODEL_BASE}/plan``.
   * - ``ANET_MODEL_KEY``
     - The key of the run, sent as ``Authorization: Bearer``.
   * - ``ANET_TURN_URL``
     - ``http://127.0.0.1:<port>/anet-turn/v1``, the address of the turn tools.
   * - ``ANET_TURN_TOKEN``
     - The turn token of the run.

The model variables are present when the model proxy has at least one
upstream. The turn variables are present for an inbound task of a trusted or
admitted peer when the service module of the node declares at least one
capability with ``"agent_tool": true``.

A hermes configuration (``~/.hermes/config.yaml`` of ``anet-hermes``) that uses
them, with the model names of the deployment added where hermes expects them:

.. code-block:: yaml

   model:
     provider: anet-plan
     base_url: ${ANET_MODEL_BASE}/plan
   providers:
     anet-plan:
       base_url: ${ANET_MODEL_BASE}/plan
       key_env: ANET_MODEL_KEY
     anet-api:
       base_url: ${ANET_MODEL_BASE}/api
       key_env: ANET_MODEL_KEY
   mcp_servers:
     org:
       command: anet
       args: ["turn-mcp"]
       env:
         ANET_TURN_URL: ${ANET_TURN_URL}
         ANET_TURN_TOKEN: ${ANET_TURN_TOKEN}

``anet turn-mcp`` treats a variable that is empty, or that still holds the text
``${...}`` because it was not set, as absent. In that case it starts, offers no
tools, and its instructions state that no turn tools are available.


The model proxy
---------------

The proxy forwards ``POST /anet-upstream/<name>/chat/completions`` and
``GET /anet-upstream/<name>/models`` to the upstream ``<name>``: the paths of
the OpenAI-compatible chat completions API, relative to ``api_base``. It
applies the request policy of the sandbox's model route. The body must be one
uncompressed JSON object; the output limit of a request is at most 8192 tokens;
server-side tools, MCP connectors and media given by URL are refused; the
credentials and cookies that the agent sends are dropped and the key of the
upstream is added. When an upstream sets ``model``, every request is pinned to
that model; otherwise the model of the request is forwarded.

Every refusal is answered in the error format of the model service, with a
sentence that begins with ``anet:``, so that the agent can report what it could
not do. A request without the key of a run that is under way is answered with
status 401 and ``model_proxy_unauthorized``. The key of a run is random and is
accepted only from the start of the run until its end. Every local account can
reach the listener; without such a key it receives the refusal and nothing
else. Requests and answers are not logged or stored.

The key is the authority; the connections are bounded besides, so that no
local account can occupy the listener. On Linux the node determines the
account of each connection from the socket table of the kernel and closes a
connection of any account other than its own and the runner account
(``expected_user``, or the owner of the socket when it is not set) as soon as it
is accepted. Each account holds at most 64 connections, and the listener at
most 128. A request must send its header within 5 seconds and its body within
30 seconds; a run reads one request body at a time, so that a run that sends
its body slowly delays only its own requests. An idle connection is closed
after 30 seconds. The answer of the model service is not bounded in time and
may be streamed for as long as the run lasts. On other platforms only the
bounds on the listener and the timeouts apply.

A run of a trusted peer may make up to 1000 model requests. A run of an
admitted peer may make ``admitted_budget.requests_per_run`` requests, and the
runs of admitted peers are counted per local calendar day against
``runs_per_day`` and ``runs_per_peer_per_day``. A task of an admitted peer that
finds the day's budget used up is refused before its run starts, with the
reason ``budget`` and the time until the next local midnight, as a sandboxed
run is; neither the agent nor a job on the runner is started. A run that has no
place in the budget for another reason (the model proxy was configured while
it was under way) receives status 429 and ``model_budget_spent`` for each model
request. After a restart of the daemon the counts of the day are read back from
the evidence chain.

A change of the ``model_proxy`` block restarts the listener; a run that is under
way at that moment loses its key.


The turn tools
--------------

The turn tools are the capabilities of the service module of the node that its
configuration marks ``"agent_tool": true``. The node serves them on the
listener of the model proxy:

``GET /anet-turn/v1/tools``
   The tools: for each, its name for a model (the capability with every
   character other than letters, digits, ``-`` and ``_`` replaced by ``_``, at
   most 64 characters), the capability, its description and the JSON Schema of
   its arguments.
``POST /anet-turn/v1/call``
   ``{"capability": "board.post", "args": {...}}``. The node calls the
   capability as the peer of the run, with ``X-ANet-Caller`` set to the AID of
   that peer and ``X-ANet-Via`` set to ``agent``. The answer is
   ``{"result": ...}``, or ``{"error": {"code": ..., "message": ...}}`` with the
   code and message of the service.

Both require ``Authorization: Bearer <turn token>``. The token is minted for one
run and is refused (``turn_unauthorized``) once the run has ended. A request
with any member other than ``capability`` and ``args``, or with arguments that
are not a JSON object, is refused with ``turn_call_invalid``; a capability that
is not a tool of the run with ``turn_tool_unknown``. A run makes at most 200
calls (``turn_calls_spent``).

``anet turn-mcp`` presents these tools to the agent as MCP tools. It sends the
token only to an ``http`` address on the loopback interface.


Checking the setup
------------------

#. ``anet autoreply show --json`` lists ``runner`` and ``model_proxy`` with the
   address of the listener.
#. Send a task from a trusted peer. The runner logs one line for the job, and
   the node records ``anet.autoreply.invoked`` with ``mode`` set to ``direct``.
#. A runner that the node cannot reach, or that runs as an account other than
   ``expected_user``, makes the turn fail with ``backend_failed`` for the peer;
   the log of the node names the reason, for example ``peer_uid_mismatch``.
#. A job for an agent that the runner does not run is refused with
   ``runner_agent_not_allowed``; a job that the runner does not understand,
   usually because the node and the runner are of different releases, with
   ``runner_job_invalid``.
