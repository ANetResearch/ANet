.. _ops-hub:

Running a hub
=============

A hub is the server side of an ANet network. Nodes register their identity
and signed Agent Card with a hub, deposit sealed envelopes in it for other
nodes, and settle credit payments through it. A hub relays envelopes it
cannot open and keeps no task content. The concepts are described in
:ref:`topics-hubs`; this page covers installing, starting and operating the
``anet-hub`` and ``anet-hub-admin`` programs.

.. contents::
   :local:
   :depth: 2


Components
----------

ANetHub provides two programs. Each has its own data directory; the operator
plane reads the data directory of the public hub.

.. list-table::
   :header-rows: 1
   :widths: 22 58 20

   * - Program
     - Role
     - Default listen address
   * - ``anet-hub``
     - The public hub: registry, relay, federation, settlement, reviews and
       the embedded web interface. Operator subcommands (``admit``,
       ``grant``, ``restore`` and others) are part of the same program.
     - ``:8088``
   * - ``anet-hub-admin``
     - The operator plane: an administration console and API under
       ``/admin``, run as a separate process. See
       :ref:`ops-hub-admin`.
     - ``127.0.0.1:8078``

Both programs are static binaries (pure Go, no C library). Nodes need
``anet`` 0.3.0 or later to use a hub of this release; ``anet-hub -version``
prints the hub wire version and the node version it requires.

.. code-block:: console

   $ anet-hub -version
   anet-hub 0.3.0 (wire 3, nodes need anet >= 0.3.0; commit 1a2b3c4, built 2026-10-16T00:00:00Z)


Building
--------

Build from a checkout of the ANetHub repository:

.. code-block:: console

   $ bash scripts/build.sh                 # anet-hub and anet-hub-admin, with the commit stamped in
   $ SKIP_WEBUI=1 bash scripts/build.sh    # Go only; embeds the committed web interface

Without the script, a plain Go build produces the same programs:

.. code-block:: console

   $ CGO_ENABLED=0 go build -o anet-hub ./cmd/anet-hub
   $ CGO_ENABLED=0 go build -o anet-hub-admin ./cmd/anet-hub-admin

Federation is compiled in by default. A hub for use inside one organisation,
with federation removed entirely, is built with the ``no_federation`` tag:

.. code-block:: console

   $ CGO_ENABLED=0 go build -tags no_federation -o anet-hub ./cmd/anet-hub

A ``no_federation`` build has no ``/fed/v3`` routes and no ``clear``
subcommand. ``/stats`` reports the modules compiled into a running hub.


Starting a hub
--------------

A hub listens on a loopback address and is exposed through a reverse proxy
that terminates TLS (see :ref:`ops-hub-proxy`).

.. code-block:: console

   $ anet-hub --addr 127.0.0.1:8088 \
              --data /var/lib/anet-hub \
              --public-url https://hub.example.com

The first start creates the hub's own identity (an AID and a key event log)
and the review tag key inside the data directory, and prints their
identifiers:

.. code-block:: text

   anet-hub identity: bafyrei...
   hub: made the review tag key (tag_key_id 1)
   anet-hub review tag key: tag_key_id 1
   anet-hub modules: federation
   anet-hub 0.3.0 (commit 1a2b3c4) listening on 127.0.0.1:8088 (data: /var/lib/anet-hub)

The hub identity is published at ``GET /hub/identity``. Peer hubs pin it, and
the settlement receipts the hub signs verify against it. A hub that has no
recovery passphrase prints a notice at every start; see
:ref:`ops-hub-recovery`.

The program stops on ``SIGINT`` or ``SIGTERM``, closing the listener within
five seconds.

Command-line options
~~~~~~~~~~~~~~~~~~~~

Options may be written with one or two leading dashes.

.. list-table::
   :header-rows: 1
   :widths: 27 20 53

   * - Option
     - Default
     - Meaning
   * - ``--addr``
     - ``:8088``
     - HTTP listen address. Use a loopback address when a reverse proxy
       fronts the hub.
   * - ``--data``
     - ``./.anet-hub``
     - Data directory (SQLite store, hub identity, keys). It must be
       writable by the account that runs the hub.
   * - ``--public-url``
     - empty
     - The hub's public base URL. It is reported as ``homeHub`` in
       ``/a2a/v1/agents`` and compared with the relay URL of the A2A cards
       the hub admits. When empty, the origin of each request is used.
   * - ``--version``
     - ``false``
     - Print the version line and exit.
   * - ``--relay-max-envelope``
     - 96 MiB
     - Largest sealed envelope ``/relay/send`` accepts, in bytes. Larger
       envelopes receive ``413``. The proxy's request-body limit must be at
       least this value.
   * - ``--relay-mailbox-messages``
     - ``5000``
     - Undelivered envelopes one recipient may have queued. Further sends
       receive ``507``.
   * - ``--relay-mailbox-bytes``
     - 1 GiB
     - Undelivered envelope bytes one recipient may have queued. Further
       sends receive ``507``.
   * - ``--relay-ttl``
     - ``336h`` (14 days)
     - How long an undelivered envelope is kept before it is deleted. The
       maximum is 14 days 22 hours 55 minutes; a larger value stops the
       start, because an envelope kept longer could outlive the only key
       that opens it.
   * - ``--relay-poll-budget``
     - 48 MiB
     - Cumulative envelope bytes one ``/relay/poll`` response returns. The
       first message is always returned.
   * - ``--register-rate``
     - ``10``
     - Per-client-address token refill of ``/register``, per minute.
       Exhausted buckets receive ``429``.
   * - ``--register-burst``
     - ``20``
     - Per-client-address token bucket size of ``/register``.

The send rate limits, the directory lookup limits and the other
sealed-sender settings are fixed values, not options. A recipient that needs
a larger share of the mailbox is given one with ``anet-hub quota``
(see :ref:`ops-hub-quota`).

Options of earlier releases that performed an action (``-grant``,
``-invite-new``, ``-clear`` and the like) have become subcommands. A command
line that still uses one is refused with exit status 2 and names the
replacement:

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - Removed option
     - Replacement
   * - ``-grant``, ``-reason``
     - ``anet-hub grant``
   * - ``-invite-required``
     - ``anet-hub admission``
   * - ``-invite-new``, ``-label``, ``-invite-uses``, ``-invite-days``
     - ``anet-hub admit``
   * - ``-invite-list``
     - ``anet-hub admissions``
   * - ``-invite-revoke``
     - ``anet-hub admissions revoke``
   * - ``-clear``, ``-amount``, ``-payee``, ``-peer-endpoint``
     - ``anet-hub clear``
   * - ``-due``
     - ``anet-hub due``
   * - ``-repair-ledger``
     - none; the ledger keeps 14 days, so no gap remains to repair
   * - ``-relay-send-rate``, ``-relay-send-burst``, ``-keys-lookup-rate``,
       ``-keys-lookup-burst``
     - none; these are fixed values


Data directories of other releases
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Release 0.3.0 migrates no data. A data directory written by 0.2 or earlier
is refused with exit status 1 and is left unchanged; move the directory away
and start again to create a new identity. A data directory written by a
newer ``anet-hub`` is refused as well; upgrade the program and do not delete
the directory. A second ``anet-hub`` process on the same data directory is
refused for as long as the first one runs.


.. _ops-hub-proxy:

TLS and the reverse proxy
-------------------------

The hub does not terminate TLS. Run it on a loopback address and place a
reverse proxy in front. Three requirements apply to the proxy.

* **Request-body limit.** The proxy's limit applies before the hub's. Set
  ``client_max_body_size`` to at least the value of ``--relay-max-envelope``.
* **Client address.** The hub rate-limits registration and enforces its
  per-network quotas by client address. It reads the address from the
  ``X-Real-IP`` header. A proxy that does not set this header makes every
  client share the proxy's bucket.
* **No access log.** Request lines name agents (``/agents/<aid>/...``). A
  retained access log would reconstruct which address contacted which
  agent, long after the hub has deleted the envelope. Disable the access log
  and keep the error log at ``crit``.

The following server block satisfies all three and can be used as a
starting point. Certificates are issued here with an ACME client; any other
source works equally.

.. code-block:: nginx

   server {
       listen 80;
       listen [::]:80;
       server_name hub.example.com;
       access_log off;
       error_log  /var/log/nginx/hub.error.log crit;

       location /.well-known/acme-challenge/ { root /var/www/certbot; }
       location / { return 301 https://$host$request_uri; }
   }

   server {
       listen 443 ssl http2;
       listen [::]:443 ssl http2;
       server_name hub.example.com;

       ssl_certificate     /etc/letsencrypt/live/hub.example.com/fullchain.pem;
       ssl_certificate_key /etc/letsencrypt/live/hub.example.com/privkey.pem;
       ssl_protocols       TLSv1.2 TLSv1.3;

       access_log off;
       error_log  /var/log/nginx/hub.error.log crit;

       add_header X-Content-Type-Options nosniff always;
       add_header X-Frame-Options SAMEORIGIN always;
       add_header Referrer-Policy strict-origin-when-cross-origin always;

       location / {
           proxy_pass http://127.0.0.1:8088;
           proxy_http_version 1.1;
           proxy_set_header Host              $host;
           proxy_set_header X-Real-IP         $remote_addr;
           proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
           proxy_set_header X-Forwarded-Proto $scheme;
           client_max_body_size 96m;
           proxy_read_timeout 300s;
           proxy_send_timeout 300s;
       }
   }

Plain HTTP is not suitable for a hub reachable from the Internet. Some
networks alter unencrypted traffic, and nodes send their registration and
mailbox requests to the address they were given.

Verify the installation from another host:

.. code-block:: console

   $ curl -s https://hub.example.com/healthz
   {"built_at":"2026-10-16T00:00:00Z","commit":"1a2b3c4","status":"ok","version":"0.3.0"}
   $ curl -s https://hub.example.com/stats
   {"agents":0,"federated_agents":0,"tasks_completed":0,"reviews":0,"avg_rating":0,"modules":["federation"]}

A node then joins with ``anet hub-register https://hub.example.com``
(see :ref:`topics-hubs` for the join states a node reports).


.. _ops-hub-admission:

Admission
---------

Admission decides which identities may register. It has two modes, shown and
changed with ``anet-hub admission``.

``closed``
   The mode of a new hub. Only identities on the admission list, and
   identities arriving during an open sign-up window, may register.

``open``
   Any identity may register after passing three gates: a proof of work of
   22 bits, a daily quota per client network (IPv6 clients are counted per
   /32), and a 72-hour probation. Registration itself pays no credit; an
   account that completes its probation receives 20 trial credits, capped
   per network and by a daily budget for the whole hub. These values are
   fixed and are the same on every hub. An organisation behind a single
   address admits identities by AID instead.

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub admission
   Sign-up: admission list only (anet-hub admit).
   $ anet-hub --data /var/lib/anet-hub admission open
   Sign-up: open (proof of work, daily quota per network, 72-hour probation).

Changing the mode affects new registrations only. Registered nodes remain
registered.

Admitting an identity
~~~~~~~~~~~~~~~~~~~~~

A node that is not yet admitted prints the command the operator must run
(``anet hub-register`` reports the state ``hub_pending_admission`` and retries
in the background). The operator admits the AID the node reports:

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub admit bafyrei... --label "build host 3"
   Admitted bafyrei...; it finishes joining on its own once it runs anet hub-register.

Further forms:

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub admit bafyrei... --grant 50
   Admitted bafyrei...; the 50-credit grant is paid when it first registers.
   $ anet-hub --data /var/lib/anet-hub admit --file aids.txt
   Admitted 12 AIDs from aids.txt; 3 of them are already registered.

``--file`` reads one AID per line. A line that is not a valid AID stops the
import and nothing from the file is admitted. A grant is paid once per AID;
admitting the same AID again does not pay it again. The command works while
the hub is running.

Sign-up windows
~~~~~~~~~~~~~~~

A sign-up window admits newcomers without listing their AIDs, for a limited
time and number. A window lasts at most 7 days and admits at most 10000
identities. Accounts that join through a window are subject to proof of work
and to the 72-hour probation.

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub admit --window 2h --max 50 --label workshop
   The sign-up window is open until 10-16 14:00 (at most 50; proof of work required; new accounts are on probation for 72 hours).
   $ anet-hub --data /var/lib/anet-hub admit --window off
   The sign-up window is closed (31 joined through it).

Only one window can be open at a time. The duration is written as a Go
duration (``90m``, ``2h``) or in days (``3d``, ``1d12h``).

Inspecting and revoking
~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub admissions
   Sign-up: admission list only (anet-hub admit).
   bafyrei...	50	build host 3
   $ anet-hub --data /var/lib/anet-hub admissions --json
   $ anet-hub --data /var/lib/anet-hub admissions revoke bafyrei...
   Revoked the admission of bafyrei... and deregistered it; its 50 credits stay on the ledger.

Each line of the list holds the AID, the grant (a trailing ``*`` marks a
grant already paid) and the label, separated by tabs, so the list can be
edited and fed back to ``admit --file``. Revoking removes the admission and
deregisters the account if it is registered; its balance stays on the ledger.
On an open hub the identity can register again; revoking is not a ban.

Accounts that have registered but never collected their mail for seven days
are deregistered automatically.


.. _ops-hub-quota:

Quotas for one recipient
~~~~~~~~~~~~~~~~~~~~~~~~

Every recipient has a share of its mailbox reserved for messages from
senders that are not its contacts. The defaults are 30 first contacts per
minute, 500 non-contact messages and 64 MiB of non-contact data. An
operator can raise these for one AID, for example for a popular public
capability; values below the default are refused.

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub quota bafyrei... --non-contact-msgs 2000
   --first-contacts-per-min	30	(default)
   --non-contact-msgs	2000	(default 500)
   --non-contact-mib	64	(default)

Without options the command shows the current values. The hub applies a
change within a minute. A value must be a whole number from 1 to
2147483647.

Credits granted by the operator
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub grant bafyrei... --amount 500 --reason "pilot allowance"
   Granted 500 credits to bafyrei... (pilot allowance); balance now 500.

The account must be registered on this hub. Issuance, supply and clearing
are described in :ref:`ops-facilitator`.


Operator subcommands
--------------------

The operator subcommands open the hub's database directly. All of them
accept ``--data DIR`` and read the same data directory the hub runs with.
Except where noted they work while the hub is running.

.. list-table::
   :header-rows: 1
   :widths: 38 62

   * - Subcommand
     - Purpose
   * - ``admit``, ``admissions``, ``admission``
     - Admission list, sign-up windows and registration mode
       (:ref:`ops-hub-admission`).
   * - ``quota <AID>``
     - Raise the non-contact quotas of one recipient.
   * - ``grant <AID> --amount N [--reason R]``
     - Issue credits to a registered account.
   * - ``supply``
     - Print issued, redeemed and outstanding credit, the latest published
       supply epoch, and the amounts due to and owed by peer hubs.
   * - ``due [--json]``
     - Print what this hub owes each peer hub.
   * - ``clear <peer-aid> --amount N [--peer-endpoint URL]``
     - Discharge part of what this hub owes a peer hub. Not available in a
       ``no_federation`` build.
   * - ``redemptions [--json]``
     - List the redemptions of the last 14 days.
   * - ``identity status|setup-recovery|rotate``
     - Inspect and protect the hub identity. ``setup-recovery`` and
       ``rotate`` require a stopped hub and a terminal.
   * - ``restore <backup>``
     - Restore the database from a backup. Requires a stopped hub
       (:ref:`ops-backup`).

A subcommand pointed at a directory that holds no hub refuses to run and
changes nothing. The subcommands print sentences in the language selected by
``LC_ALL``, ``LC_MESSAGES`` or ``LANG`` (the first one set): Chinese when
the value starts with ``zh``, English otherwise. Lists and figures are
printed as tab-separated data and are not translated.


.. _ops-hub-recovery:

Hub identity and recovery passphrase
------------------------------------

The hub signs settlement receipts, the notices that confirm the key state of
its agents, and every request to a peer hub, with its own identity. As for a
node identity, the key that will sign next is not stored in the data
directory: it is derived from a recovery seed that is encrypted under a
passphrase and stored in a recovery file. Until a passphrase is set, a leak
of the data directory would allow someone else to take over the hub identity
and the operator could not take it back. A hub without a passphrase starts,
and prints the following notice at every start and in ``identity status``:

.. code-block:: text

   The hub's recovery passphrase is not set: if the data directory leaks, the hub identity cannot be taken back; run anet-hub identity setup-recovery while the hub is stopped.

Set it once, with the hub stopped and from a terminal:

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub identity setup-recovery
   Recovery passphrase: ********
   Type it again: ********
   Recovery file written to /home/anet-hub/.anet-recovery/ab12cd34ef56.anetrec (passphrase-encrypted). Keep a copy somewhere else.
   The hub identity was rotated: key number 0 → 1; it takes effect at the next start, and peers and nodes pick it up within 10 minutes.

The passphrase needs at least 12 characters and is read from the terminal
only; it is never accepted on the command line. The recovery file is written
to ``~/.anet-recovery/<last 12 characters of the AID>.anetrec`` of the
account that ran the command, or to the path given with ``--recovery-file``.
The file is useless without the passphrase; store a copy on separate
media.

.. code-block:: console

   $ anet-hub --data /var/lib/anet-hub identity status
   aid	bafyrei...
   key_seq	1
   recovery	set
   recovery_file	/home/anet-hub/.anet-recovery/ab12cd34ef56.anetrec	present

Rotation replaces the hub's signing key. ``identity rotate`` performs one
planned rotation; ``--new-recovery`` also replaces the recovery seed and
passphrase; ``--emergency`` is for a data directory that may have leaked. It
performs the rotation without a grace period for the retired keys, implies
``--new-recovery`` and regenerates the secrets that live in the hub data
directory. Secrets that the program cannot replace itself, such as the
administration token, are listed for the operator to replace.

.. v030-doc-verify: KEL-12


.. _ops-hub-admin:

The operator console
--------------------

``anet-hub-admin`` provides the administration console and API for a hub. It
runs as a separate process beside the public hub and reads the same database.
It never reads relay payloads, which the hub itself cannot open, and it holds
no channel to any other host.

.. code-block:: console

   $ ADMIN_TOKEN="$(openssl rand -hex 32)" anet-hub-admin \
         --addr 127.0.0.1:8078 \
         --hub-data /var/lib/anet-hub \
         --data /var/lib/anet-hub-admin

.. list-table::
   :header-rows: 1
   :widths: 27 30 43

   * - Option
     - Default
     - Meaning
   * - ``--addr``
     - ``127.0.0.1:8078``
     - Listen address. Keep it on loopback; a reverse proxy publishes it.
   * - ``--hub-data``
     - required in practice
     - The public hub's data directory (``hub.db``).
   * - ``--data``
     - required in practice
     - The operator plane's own data directory (``admin.db``,
       ``officials.json``).
   * - ``--base``
     - ``/admin``
     - URL base path.
   * - ``--snapshot-every``
     - ``5m``
     - Interval of statistics snapshots; ``0`` turns them off.
   * - ``--check-token``
     - ``false``
     - Apply the start-up rules to ``$ADMIN_TOKEN``, exit with status 0 or
       1 and print no value. Deployment scripts use it before a restart.
   * - ``--version``
     - ``false``
     - Print the version line and exit.

The built-in defaults of ``--hub-data`` and ``--data`` are paths of one
reference deployment and are unlikely to exist on another host. Pass both
options explicitly.

The operator token
~~~~~~~~~~~~~~~~~~

The token is read from the ``ADMIN_TOKEN`` environment variable and has no
default. An unset or empty token, or a placeholder such as ``CHANGE_ME``,
stops the start. A token that appears in the project's published source is
accepted, but the process logs a warning at start and every hour. Generate a
random token of at least 32 hexadecimal characters and keep it in a file
readable by the service account only (see :ref:`ops-deployment`).

.. code-block:: console

   $ ADMIN_TOKEN="CHANGE_ME" anet-hub-admin --check-token; echo $?
   ADMIN_TOKEN is a placeholder (for example CHANGE_ME from the unit template): refusing to start the admin surface. Set a random token in the unit's environment and restart.
   1

Login and API access
~~~~~~~~~~~~~~~~~~~~

The console page at ``/admin`` is public; the login happens inside it. The
API under ``/admin/api`` is available only with the token as a bearer
credential. Failed credential checks, from the login route and from bearer
authentication alike, are limited to 20 per client address per minute; further
attempts receive ``429``. Requests that carry no credential are answered with
``401``.

.. code-block:: console

   $ curl -s -X POST https://hub.example.com/admin/api/login \
          -H 'Content-Type: application/json' -d "{\"token\":\"$ADMIN_TOKEN\"}"
   {"ok":true}
   $ curl -s https://hub.example.com/admin/api/overview \
          -H "Authorization: Bearer $ADMIN_TOKEN"

The remote address is taken from ``X-Real-IP`` when present, so publish the
console only through a proxy that sets this header. A location block for
the console, to be placed in the TLS server block before ``location /``:

.. code-block:: nginx

   location ^~ /admin {
       proxy_pass http://127.0.0.1:8078;
       proxy_http_version 1.1;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_read_timeout 300s;
       proxy_buffering off;
       client_max_body_size 1m;
   }

Functions
~~~~~~~~~

.. list-table::
   :header-rows: 1
   :widths: 40 60

   * - API route (under ``/admin/api``)
     - Function
   * - ``GET /overview``
     - Totals, database size, statistics snapshots, the number of official
       agents and the last audit entries.
   * - ``GET /agents[?q=&tier=]``, ``GET /agents/{aid}``
     - Registered agents, including agents that are not listed publicly,
       with review counts and mailbox backlog (a row count; payloads are not
       read).
   * - ``POST /agents/{aid}/moderate``
     - Mark an agent for attention or clear the mark.
   * - ``DELETE /agents/{aid}``
     - Revoke the admission of the agent and deregister it. The balance
       stays on the ledger. On an open hub the agent can register again.
   * - ``GET /official``, ``POST /official``, ``DELETE /official/{id}``
     - Register of official agents (``id``, ``aid``, ``hub``, ``caps`` and
       descriptive labels).
   * - ``GET /capabilities``, ``GET /discover``
     - Capabilities of registered agents.
   * - ``GET /reviews``
     - Reviews as rating, comment and receipt CID only.
   * - ``GET /audit``
     - Audit trail of every administrative action.
   * - ``GET /deleted``
     - Deleted accounts, with ``?format=admit`` producing a list that
       ``anet-hub admit --file`` accepts, which undoes a batch of deletions.

There is no route that restores a deleted registration from an archive,
because that would write an old key history back into the registry. To undo
a deletion, run ``anet-hub admit <AID>``; the node re-registers the next time
``anet hub-register`` runs. Other paths under ``/admin/api/`` answer ``404``
with a JSON body after authentication. The console itself is served as a
single page. ``GET /admin/healthz`` reports the version, commit and build time
of the operator plane without authentication, so that a deployment can confirm
which build is running.

The official-agent register is read at start from ``officials.json`` in the
operator plane's data directory; a missing file means no official agents. An
entry that still carries ``runtime``, ``monitor``, ``ops`` or ``datasets``
fields is refused, and the process does not start. The operator plane starts,
stops or updates no agent; those tasks belong to separate tooling outside the
hub host.

.. v030-doc-verify: GUI-1

The console is being redesigned as a navigation sidebar with sections for
overview, nodes, registration, federation, settlement and audit, admission
and quota, and logs, with sortable and filterable tables. Delivery
confirmations and admission state (proof of work, quota, probation) are shown
there.

Two options of earlier releases, ``--restore-agents-from`` and
``--keep-only-agents``, are removed. Each is refused with exit status 2 and
names its replacement (``anet-hub restore`` and the console's delete
function or ``anet-hub admissions revoke``).


Upgrading a hub
---------------

#. Take a backup (:ref:`ops-backup`).
#. Stop the service.
#. Replace the binaries. Replace ``anet-hub-admin`` in the same step; the
   operator plane refuses a hub data directory written by a newer release.
#. Start the service and check ``/healthz`` for the new version.

Release 0.3.0 uses hub wire 3. A node that declares an older wire version is refused with status ``426``
and the reason ``wire_too_old``, which names the required wire version and
node release; they cannot use a 0.3.0 hub, and a
0.3.0 node does not use an older hub.
