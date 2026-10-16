.. _ops-monitoring:

Monitoring
==========

A deployment is monitored at three levels: the state of each node, reported
by ``anet status`` and ``anet doctor``; the process-level health of a hub,
reported by its HTTP endpoints and logs; and the state of the hub's ledger
and federation, reported by the operator subcommands. This page lists the
commands, the shape of their output, and the signals that call for action.

.. contents::
   :local:
   :depth: 2


Node
----

``anet status``
~~~~~~~~~~~~~~~

``anet status`` prints one line per topic, each with a label, a sentence
and, where a next step exists, an indented line that starts with an arrow.
The sentences are the same as those returned by the MCP tool ``node_status``
and the local A2A interface.

.. code-block:: console

   $ anet status
   identity  default …7hq2abcd, key confirmed by https://hub.example.com 4 minutes ago.
   hub       Registered at https://hub.example.com.
   inbound   Takes tasks only from the 0 peers on the allow list; 0 public capabilities; auto-reply off.
   payments  Automatic and agent payments are off (limit 0); manual payments (anet pay): ≤10 each, 50 left today.
   privacy   Task content retention is 30 days, counted from when a task ends (public calls: 7 days).

The lines appear in this order, and a line appears only when its condition
holds:

.. list-table::
   :header-rows: 1
   :widths: 14 86

   * - Topic
     - Condition and content
   * - ``identity``
     - Unless the node has not joined a hub. The state of the identity: key
       confirmed, confirmation ageing, stopped, deactivated.
   * - ``recovery``
     - When the identity has no recovery passphrase. Followed by
       ``→ anet identity protect``.
   * - ``hub``
     - Always. Registered, waiting for admission, retrying, refused, or a
       wire version that does not match.
   * - ``delivery``
     - When outbound messages are waiting for confirmation or are stuck.
   * - ``inbound``
     - Always. Policy, the number of allowed peers and public capabilities,
       and whether auto-reply is on.
   * - ``sandbox``
     - When untrusted or trusted callers are served by a sandbox.
   * - ``payments``
     - Always. Limits and what has been paid today.
   * - ``privacy``
     - Always. The retention period, or a clock warning, or a detected
       rollback of the node's data.
   * - ``preview``
     - When preview streaming is on.

``anet status --json`` prints the complete status object with the same
sentences in its ``summary`` member. A node whose daemon is not running prints
a single sentence and the next step, ``anet up``.


``anet doctor``
~~~~~~~~~~~~~~~

``anet doctor`` reports what the node is set up to do and which entry points
are open. It reads the data directory, writes nothing and needs no running
daemon; checks that need runtime data report ``unknown`` when the daemon is
stopped. Every finding has an identifier, a status and a sentence; a finding
that is not ``ok`` also names the command that resolves it.

.. code-block:: console

   $ anet doctor
   anet doctor — /home/node/.anet

   [info] version  anet 0.3.0 (commit 1a2b3c4, built 2026-10-16T00:00:00Z)
   [ok] version.signature  verified: signed release manifest, key SHA256:...
   [info] modules  a2a,anetlink,blackboard,cas,mcp,org,p2p,service,x402
   [ok] config  /home/node/.anet/config.json
   [ok] identity  bafyrei...
   [ok] control  daemon running, control plane at 127.0.0.1:39811
   [ok] identity.format  /home/node/.anet/identity.kel
   [warn] hub  This hub (https://hub.example.com) needs its operator to admit your AID first; send this line to the operator: anet-hub admit bafyrei... (once they do, this node finishes joining on its own).
     → anet-hub admit bafyrei...
   [ok] inbound  policy closed; allow 0, trust 0, deny 0; public capabilities 0
   [ok] si5  all fresh-install safety defaults in place

The status values are:

``ok``
   The check passed.
``info``
   A fact, or a setting that differs from a fresh installation. It is the
   operator's choice and does not fail the report.
``warn``
   An attention-worthy condition that does not stop the node.
``fail``
   A condition that breaks the node or exposes a credential.
``unknown``
   The check could not be made, usually because the daemon is not running.

The exit status is ``1`` if any check is ``fail``, and ``0`` otherwise. With
``--json`` the report is a single object whose ``schema`` member is
``anet.doctor/1``. Its ``checks`` array holds objects with ``id``, ``status``,
``reason`` (the code of the sentence), ``detail`` and ``hint``; ``ok`` is
``false`` when a check failed. The identifiers are stable.

.. code-block:: console

   $ anet doctor --json | jq -r '.checks[] | select(.status=="warn" or .status=="fail") | "\(.status) \(.id) \(.hint)"'
   warn hub anet-hub admit bafyrei...

Checks
^^^^^^

Checks that do not apply to the node's configuration are not shown; for
example the ``sandbox`` checks appear only when a sandbox is in use.

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Identifier
     - Subject
   * - ``version``, ``version.signature``
     - The version, and whether the binary was installed from a signed
       release.
   * - ``modules``
     - Modules compiled in, and modules turned off in the configuration. A
       configuration that refers to a module that is not available is a
       ``fail``.
   * - ``config``, ``config.removed``
     - The configuration file: permissions and parseability, and keys that
       no longer exist.
   * - ``si5``
     - The safety defaults of a fresh installation; the keys that differ
       are listed.
   * - ``control``, ``control.token``
     - The local control plane and its token.
   * - ``identity``, ``identity.format``
     - The AID, and whether the identity file is readable and of this
       release.
   * - ``identity.witness``
     - Whether the hub confirms the node's current key state, and how fresh
       the confirmation is. ``fail`` when the confirmation is too old, or
       when the identity was stopped or deactivated elsewhere.
   * - ``identity.recovery``
     - The recovery file: present, mode ``0600``, or the warning that no
       passphrase has been set.
   * - ``identity.rotation``
     - A key rotation that has not been confirmed yet.
   * - ``hub``, ``hub.probation``
     - Registration state; probation and the trial credit, if any. A new account
       that came in through open registration is on probation for 72 hours, and
       the trial credits are paid after it.
   * - ``delivery.outbox``
     - Outbound messages that are stuck or unconfirmed.
   * - ``tasks.no_response``
     - The time after which a task with no reply from its peer fails.
   * - ``sender_privacy.*``
     - The mailbox key, the sessions and delivery credentials, and the hub's
       behaviour towards sender hiding.
   * - ``clock``
     - The difference between the computer's clock and the hub's. The finding
       ``clock.ts_retries`` is a warning when, in the last hour, the hub found the
       signing time of this node behind its floor more than ten times; the clock
       may be jumping, and NTP should be enabled. A skewed clock or a clock that
       could not be checked becomes a warning only after ten minutes without a
       confirmed clock; deletion of expired keys and content is then slowed
       until the hub can be reached. The remedy is to fix the system time.
   * - ``keys.epoch``
     - Encryption keys published ahead of time, and expired keys kept longer
       than necessary.
   * - ``retention``
     - The retention period of task content. On a copy-on-write file system the
       report adds that deleted data may remain in snapshots.
   * - ``local_state``
     - A detected rollback of the node's data in the last 7 days. ``warn``
       the first time, ``fail`` when repeated.
   * - ``inbound``, ``inbound.lists``
     - Inbound policy and lists.
   * - ``capabilities.*``
     - Public capabilities: file handling, in-flight limits, capabilities
       open only to lists.
   * - ``preview``
     - Preview streaming.
   * - ``payments``, ``payments.audit``, ``payments.reviews``, ``payments.card``, ``payments.unchargeable``
     - Payment limits and payees; the result of the last check of the balance
       proof of the hub (``payments.audit``, repeated every 6 hours); reviews
       waiting for upload, and a warning for reviews that have waited for more
       than 7 days; the extended card that carries prices; priced capabilities
       on a node that cannot take payment.
   * - ``auto_reply``, ``sandbox``, ``sandbox.*``
     - The auto-reply configuration and, when a sandbox is in use, its
       readiness, upstream configuration, network egress, budget and limits.
   * - ``backends.peer_check``, ``backend.transport``, ``a2a.backends``
     - Backends of the service module.
   * - ``anetlink.socket``, ``anetlink.publish``
     - ANetLink connection and published capabilities.
   * - ``a2a``, ``a2a.port``, ``a2a.token``
     - The local A2A interface.
   * - ``agents``, ``agents.<tool>``, ``hermes.*``
     - The wiring of coding agents (see :ref:`howto-coding-agents`).
   * - ``official``
     - The signed list of official agents.
   * - ``p2p.lan``
     - Information that direct delivery dials addresses of the local network.

The sentence of every ``warn`` and ``fail`` is a code of the reason table, so
the same event is described in the same words by ``anet status``, MCP and the
local A2A interface. See :ref:`reference-reason-codes`.

Logs and records
~~~~~~~~~~~~~~~~

The node writes ``daemon.log`` in its data directory. ``anet logs`` prints the
last 80 lines; ``anet logs 200`` prints 200, and ``anet logs --all`` prints
the whole file. A node run by systemd also writes to the journal. The log
holds operational messages and no task content.

``anet audit`` reads the node's evidence chain and checks every record. It
needs no daemon.

.. code-block:: console

   $ anet logs 5
   $ anet audit --since 24h --type anet.payment.settled
   $ anet audit --export ./evidence-export


Hub
---

Endpoints
~~~~~~~~~

``GET /healthz`` reports that the process answers and which build it is. A
deployment script compares the version and commit with the build
that was installed.

.. code-block:: console

   $ curl -fsS https://hub.example.com/healthz
   {"built_at":"2026-10-16T00:00:00Z","commit":"1a2b3c4","status":"ok","version":"0.3.0"}

``GET /stats`` reports the number of registered agents, of agents learned from
federation peers, of published receipts and reviews, the average rating and
the modules compiled in. A drop to zero in ``agents`` or a missing module
indicates a wrong database or a wrong build.

.. code-block:: console

   $ curl -fsS https://hub.example.com/stats
   {"agents":42,"federated_agents":7,"tasks_completed":130,"reviews":55,"avg_rating":4.6,"modules":["federation"]}

``GET /hub/identity`` returns the hub's AID and key history; the AID is
constant for the life of the hub. The operator plane answers
``GET /admin/healthz`` with its version, commit and build time without
authentication.

The hub keeps no access log. The reverse proxy must keep none either (see
:ref:`ops-hub-proxy`), so that the number of requests is observed with
``/stats`` and the proxy's own error log and metrics, not with a log of request
lines.

The hub's log
~~~~~~~~~~~~~

The hub writes its log to standard error; a systemd unit sends it to the
journal.

.. code-block:: console

   # journalctl -u anet-hub.service --since "1 hour ago"

The log has no request lines and names no sender. Lines that call for
attention:

.. list-table::
   :header-rows: 1
   :widths: 46 54

   * - Message
     - Meaning and action
   * - ``The hub's recovery passphrase is not set ...``
     - At every start until ``anet-hub identity setup-recovery`` is run
       (:ref:`ops-hub-recovery`).
   * - ``hub: ERROR: supply epoch N..M not published: ...``
     - The signed supply chain missed an epoch. Check disk space and the
       database; the hub retries.
   * - ``hub: ledger window: deleted N rows past their time``
     - Routine; hourly deletion of ledger rows older than 14 days.
   * - ``hub: relay TTL purge deleted N undelivered envelope(s) ...``
     - Routine; envelopes nobody collected.
   * - ``hub: inactive sweep deregistered N account(s) ...``
     - Routine; registered accounts that never collected mail in 7 days.
   * - ``hub: no key confirmation is signed for an agent until ...``
     - The hub started on a restored database and holds key confirmations
       for a time (:ref:`ops-backup`). ``hub: every agent is out of the
       restore quarantine`` ends it.
   * - ``hub: ... treating the database as an older copy``
     - ``hub.db`` is older than the counter file; quarantine applies.
   * - ``hub: federation: peer ... serves a key history that contradicts the
       one pinned for it``
     - A peer's key history was rewritten. The hub keeps the pinned one.
       Contact the peer's operator before continuing to trust it.
   * - ``hub: federation peer ... answered with ..., not hub wire 3``
     - A peer runs another wire version. The hub stops talking to it for a
       time and resumes when it upgrades.
   * - ``hub: federation A2A card sync ...: HTTP N``
     - A peer's card stream fails; check reachability and the peer table on
       both sides.
   * - ``hub: cross-hub settlement ... not cleared``
     - A settlement forwarded to a peer is not completed yet. The merchant
       retries; see :ref:`ops-facilitator`.
   * - ``hub: fed_settle signed by <AID>: <reason>``
     - A peer's settlement request was refused with that reason.
   * - ``SECURITY: ADMIN_TOKEN is a credential this software published ...``
     - Operator plane only. The token is a published value; replace it.

Operator readouts
~~~~~~~~~~~~~~~~~

These subcommands are cheap and may be run from a monitoring job as the
service account. Each opens the database next to the running hub.

.. list-table::
   :header-rows: 1
   :widths: 34 66

   * - Command
     - Signal
   * - ``anet-hub supply``
     - ``outstanding`` against ``issued`` and ``redeemed``; ``latest_epoch``
       advances once per UTC day; ``events_since_publish`` grows between
       epochs.
   * - ``anet-hub due``, ``anet-hub supply`` ``due`` / ``owed`` lines
     - Obligations between hubs. A figure that grows without clearing needs
       ``anet-hub clear`` (:ref:`ops-federation`).
   * - ``anet-hub redemptions``
     - Redemptions to be handled within 14 days.
   * - ``anet-hub admissions``, ``anet-hub admission``
     - The registration mode and any open sign-up window.
   * - ``anet-hub identity status``
     - Recovery passphrase set, recovery file present.

The operator plane's ``GET /admin/api/overview`` reports totals, the size of
the hub database and recent statistics snapshots, and ``GET /admin/api/audit``
lists administrative actions. The size of the database and of the data
directory's free disk space are the first values to alarm on.


Probes
------

A hub probe, to be run every minute from a timer or a monitoring agent:

.. code-block:: sh

   #!/bin/sh
   # exit status 0: healthy; 1: unhealthy
   url="${1:-https://hub.example.com}"
   expected="${2:-0.3.0}"
   body="$(curl -fsS --max-time 10 "$url/healthz")" || exit 1
   printf '%s' "$body" | jq -e --arg v "$expected" '.status == "ok" and .version == $v' >/dev/null || exit 1
   curl -fsS --max-time 10 "$url/stats" | jq -e '.modules | length > 0' >/dev/null || exit 1

A node probe, run as the node's account (the wrapper of :ref:`ops-deployment`):

.. code-block:: sh

   #!/bin/sh
   # exit status 0: no failed check; 1: at least one failed check or no daemon
   report="$(anet-node doctor --json)" || exit 1
   printf '%s' "$report" | jq -e '.ok' >/dev/null || exit 1

``anet doctor`` exits with status 1 when any check has failed, so the exit
status alone suffices where a shell condition is enough. Alarm on the
following, at least:

* a hub whose ``/healthz`` does not answer through the public name, or whose
  version differs from the installed one;
* free disk space on the data directory of a hub below 15 percent;
* a node whose ``doctor`` reports ``fail``, in particular
  ``identity.witness`` (confirmation too old, or the identity stopped
  elsewhere) and ``local_state`` (a repeated rollback, which means a second
  copy of the identity);
* a missing daily maintenance run (``roll.log`` not updated in 36 hours);
* ``supply`` whose ``latest_epoch`` does not advance for more than a day.
