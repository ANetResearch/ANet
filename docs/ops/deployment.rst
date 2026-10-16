.. _ops-deployment:

Deployment
==========

This page gives complete deployment samples for the two long-running
components: a hub (``anet-hub`` with the optional ``anet-hub-admin``) and a
node (the ``anet`` daemon). Each is shown as a set of systemd units and as a
container image. The samples are starting points; the host names, paths and
account names are examples.

.. contents::
   :local:
   :depth: 2


Principles
----------

* **Dedicated account.** Run each component under its own unprivileged
  account. The data directory holds private keys and is created with mode
  ``0700``.
* **Loopback listeners.** A hub listens on a loopback address and a reverse
  proxy terminates TLS (see :ref:`ops-hub-proxy`). A node's control plane and
  local A2A interface are loopback-only and must not be published.
* **Secrets in files.** Tokens are kept in files readable by the service
  account only and are never placed in a unit file, an image or a command
  line.
* **Write access to the data directory only.** The units below mount
  the file system read-only and open the data directory for writing.
* **One data directory, one process.** Two processes on the same hub data
  directory, or two copies of one node identity running at the same time, are
  both faults (see :ref:`ops-backup`).


A hub with systemd
------------------

The installation uses one account, ``anet-hub``, for the hub, the operator
plane and the maintenance job. The operator plane writes to the hub's
database (to deregister an agent), so it needs the same account.

Account and files
~~~~~~~~~~~~~~~~~

.. code-block:: console

   # useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin anet-hub
   # install -d -m 0755 /opt/anet-hub/bin
   # install -m 0755 anet-hub anet-hub-admin /opt/anet-hub/bin/
   # install -m 0755 deploy/hub-db-roll.sh /opt/anet-hub/bin/
   # install -d -m 0700 /etc/anet-hub
   # (umask 077; printf 'ADMIN_TOKEN=%s\n' "$(openssl rand -hex 32)" > /etc/anet-hub/admin.env)
   # apt-get install --yes sqlite3

``deploy/hub-db-roll.sh`` is in the ANetHub repository. It needs the
``sqlite3`` command. The data directories are created by systemd from the
``StateDirectory=`` lines of the units below.

Hub unit
~~~~~~~~

``/etc/systemd/system/anet-hub.service``:

.. code-block:: ini

   [Unit]
   Description=ANet Hub
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   User=anet-hub
   Group=anet-hub
   StateDirectory=anet-hub
   StateDirectoryMode=0700
   ExecStart=/opt/anet-hub/bin/anet-hub \
     --addr 127.0.0.1:8088 \
     --data /var/lib/anet-hub \
     --public-url https://hub.example.com
   Restart=on-failure
   RestartSec=5
   LimitNOFILE=65535
   UMask=0077

   NoNewPrivileges=yes
   ProtectSystem=strict
   ReadWritePaths=/var/lib/anet-hub
   ProtectHome=yes
   PrivateTmp=yes
   PrivateDevices=yes
   ProtectKernelTunables=yes
   ProtectKernelModules=yes
   ProtectKernelLogs=yes
   ProtectControlGroups=yes
   ProtectClock=yes
   ProtectHostname=yes
   RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
   RestrictNamespaces=yes
   RestrictRealtime=yes
   RestrictSUIDSGID=yes
   LockPersonality=yes
   MemoryDenyWriteExecute=yes
   SystemCallArchitectures=native
   CapabilityBoundingSet=
   AmbientCapabilities=

   [Install]
   WantedBy=multi-user.target

Operator-plane unit
~~~~~~~~~~~~~~~~~~~

The operator plane reads the token from ``/etc/anet-hub/admin.env``. systemd
reads that file before it drops privileges, so the file can be owned by
``root`` with mode ``0600``. The unit is omitted entirely on a hub that does
not use the console.

``/etc/systemd/system/anet-hub-admin.service``:

.. code-block:: ini

   [Unit]
   Description=ANet Hub operator plane
   After=network-online.target anet-hub.service
   Wants=network-online.target

   [Service]
   Type=simple
   User=anet-hub
   Group=anet-hub
   StateDirectory=anet-hub-admin
   StateDirectoryMode=0700
   EnvironmentFile=/etc/anet-hub/admin.env
   ExecStart=/opt/anet-hub/bin/anet-hub-admin \
     --addr 127.0.0.1:8078 \
     --hub-data /var/lib/anet-hub \
     --data /var/lib/anet-hub-admin
   Restart=on-failure
   RestartSec=3
   UMask=0077

   NoNewPrivileges=yes
   ProtectSystem=strict
   ReadWritePaths=/var/lib/anet-hub /var/lib/anet-hub-admin
   ProtectHome=yes
   PrivateTmp=yes
   PrivateDevices=yes
   ProtectKernelTunables=yes
   ProtectKernelModules=yes
   ProtectKernelLogs=yes
   ProtectControlGroups=yes
   ProtectClock=yes
   ProtectHostname=yes
   RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
   RestrictNamespaces=yes
   RestrictRealtime=yes
   RestrictSUIDSGID=yes
   LockPersonality=yes
   MemoryDenyWriteExecute=yes
   SystemCallArchitectures=native
   CapabilityBoundingSet=
   AmbientCapabilities=

   [Install]
   WantedBy=multi-user.target

Without an ``ADMIN_TOKEN`` line in the file, or with a placeholder value, the
service does not start. Check a token before restarting the service:

.. code-block:: console

   # sh -c 'set -a; . /etc/anet-hub/admin.env; set +a; /opt/anet-hub/bin/anet-hub-admin --check-token' && echo accepted

Maintenance job
~~~~~~~~~~~~~~~

``hub-db-roll.sh`` checkpoints the write-ahead log, writes the weekly backup
and prunes old backups (see :ref:`ops-backup`). It runs once a day.

``/etc/systemd/system/hub-db-roll.service``:

.. code-block:: ini

   [Unit]
   Description=anet-hub database maintenance
   After=anet-hub.service

   [Service]
   Type=oneshot
   User=anet-hub
   Group=anet-hub
   Environment=HUB_DATA_DIR=/var/lib/anet-hub
   ExecStart=/opt/anet-hub/bin/hub-db-roll.sh
   Nice=10
   IOSchedulingClass=idle
   UMask=0077
   NoNewPrivileges=yes
   ProtectSystem=strict
   ReadWritePaths=/var/lib/anet-hub
   ProtectHome=yes
   PrivateTmp=yes
   PrivateDevices=yes
   ProtectKernelTunables=yes
   ProtectKernelModules=yes
   ProtectControlGroups=yes
   RestrictAddressFamilies=AF_UNIX
   LockPersonality=yes
   CapabilityBoundingSet=

``/etc/systemd/system/hub-db-roll.timer``:

.. code-block:: ini

   [Unit]
   Description=Daily anet-hub database maintenance

   [Timer]
   OnCalendar=*-*-* 04:30:00
   Persistent=true

   [Install]
   WantedBy=timers.target

Start everything
~~~~~~~~~~~~~~~~

.. code-block:: console

   # systemctl daemon-reload
   # systemctl enable --now anet-hub.service anet-hub-admin.service hub-db-roll.timer
   # systemctl status anet-hub.service
   # curl -s http://127.0.0.1:8088/healthz
   {"built_at":"2026-10-16T00:00:00Z","commit":"1a2b3c4","status":"ok","version":"0.3.0"}

Then set the recovery passphrase of the hub identity. The hub must be
stopped for this step, and the command must run in a terminal. Choose a
recovery-file location outside the data directory:

.. code-block:: console

   # systemctl stop anet-hub.service
   # install -d -o anet-hub -g anet-hub -m 0700 /etc/anet-hub/recovery
   # sudo -u anet-hub /opt/anet-hub/bin/anet-hub --data /var/lib/anet-hub \
         identity setup-recovery --recovery-file /etc/anet-hub/recovery/hub.anetrec
   # systemctl start anet-hub.service

Copy ``hub.anetrec`` to separate storage. Later ``identity status`` and
``identity rotate`` commands name the same file with ``--recovery-file``,
because the service account has no home directory. Other operator
subcommands are run the same way, as the service account:

.. code-block:: console

   # sudo -u anet-hub /opt/anet-hub/bin/anet-hub --data /var/lib/anet-hub admissions

Finally publish the hub through the reverse proxy configuration shown in
:ref:`ops-hub-proxy`, and request the certificate for the host name.


A hub in a container
--------------------

The project does not publish a container image. The following
``Containerfile`` builds a minimal one around a static ``anet-hub`` binary
(built with ``CGO_ENABLED=0``). The image has no shell; it holds the binary
and an empty, writable ``/data`` directory.

.. code-block:: dockerfile

   FROM debian:12-slim AS prepare
   RUN mkdir /data

   FROM gcr.io/distroless/static-debian12:nonroot
   COPY --from=prepare --chown=65532:65532 /data /data
   COPY anet-hub /usr/local/bin/anet-hub
   VOLUME /data
   EXPOSE 8088
   ENTRYPOINT ["/usr/local/bin/anet-hub", "--addr", ":8088", "--data", "/data"]

Build and start it. The port is published on the loopback interface of the
host only, so the reverse proxy remains the single public face:

.. code-block:: console

   $ CGO_ENABLED=0 go build -o anet-hub ./cmd/anet-hub
   $ docker build -f Containerfile -t anet-hub:0.3.0 .
   $ docker volume create anet-hub-data
   $ docker run -d --name anet-hub --restart unless-stopped \
         -p 127.0.0.1:8088:8088 -v anet-hub-data:/data \
         anet-hub:0.3.0 --public-url https://hub.example.com
   $ curl -s http://127.0.0.1:8088/healthz
   {"built_at":"unknown","commit":"unknown","status":"ok","version":"0.3.0"}

Arguments after the image name are appended to the entry point, so
``--public-url`` and the other options of :ref:`ops-hub` can be given there.
Operator subcommands run in a second container on the same volume. Set the
recovery passphrase with the hub container stopped, and keep the recovery file
on a separate bind mount owned by the image's user (uid 65532):

.. code-block:: console

   $ docker stop anet-hub
   $ install -d -o 65532 -g 65532 -m 0700 recovery
   $ docker run --rm -it --entrypoint /usr/local/bin/anet-hub \
         -v anet-hub-data:/data -v "$PWD/recovery:/recovery" anet-hub:0.3.0 \
         --data /data identity setup-recovery --recovery-file /recovery/hub.anetrec
   $ docker start anet-hub
   $ docker run --rm --entrypoint /usr/local/bin/anet-hub \
         -v anet-hub-data:/data anet-hub:0.3.0 --data /data admissions

The hub writes its log to standard error; ``docker logs anet-hub`` shows it.


A node with systemd
-------------------

A node that must run without a logged-in user (a provider of public
capabilities, for example) is an ``anet daemon`` under its own account. The
data directory is ``ANET_DATA_DIR``. The command-line tool refuses to
talk to a daemon that runs under another user, so commands for the node are
run as its account.

.. code-block:: console

   # useradd --system --home-dir /var/lib/anet-node --shell /usr/sbin/nologin anet-node
   # install -d -m 0755 /opt/anet/bin
   # install -m 0755 anet /opt/anet/bin/anet

``/etc/systemd/system/anet-node.service``:

.. code-block:: ini

   [Unit]
   Description=ANet node
   Wants=network-online.target
   After=network-online.target

   [Service]
   Type=simple
   User=anet-node
   Group=anet-node
   StateDirectory=anet-node
   StateDirectoryMode=0700
   RuntimeDirectory=anet-node
   RuntimeDirectoryMode=0700
   Environment=HOME=/var/lib/anet-node
   Environment=ANET_DATA_DIR=/var/lib/anet-node/data
   Environment=XDG_RUNTIME_DIR=/run/anet-node
   ExecStart=/opt/anet/bin/anet daemon
   Restart=on-failure
   RestartSec=5s
   UMask=0077

   NoNewPrivileges=yes
   CapabilityBoundingSet=
   AmbientCapabilities=
   ProtectSystem=strict
   ReadWritePaths=/var/lib/anet-node
   ProtectHome=yes
   PrivateTmp=yes
   PrivateDevices=yes
   ProtectKernelTunables=yes
   ProtectKernelModules=yes
   ProtectKernelLogs=yes
   ProtectControlGroups=yes
   ProtectClock=yes
   ProtectHostname=yes
   ProtectProc=invisible
   RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
   RestrictNamespaces=yes
   RestrictRealtime=yes
   RestrictSUIDSGID=yes
   LockPersonality=yes
   MemoryDenyWriteExecute=yes
   SystemCallArchitectures=native
   SystemCallFilter=@system-service
   SystemCallFilter=~@privileged
   SystemCallErrorNumber=EPERM

   [Install]
   WantedBy=multi-user.target

``RestrictNamespaces=yes`` and the system-call filter prevent the sandbox of
the auto-reply module from starting, because the sandbox creates namespaces.
A node that answers tasks with a sandboxed local agent needs these two lines
relaxed (see :ref:`topics-sandbox`).

A small wrapper runs the command-line tool as the node's account with the same
environment:

``/usr/local/bin/anet-node``:

.. code-block:: sh

   #!/bin/sh
   exec runuser -u anet-node -- env \
     HOME=/var/lib/anet-node \
     ANET_DATA_DIR=/var/lib/anet-node/data \
     XDG_RUNTIME_DIR=/run/anet-node \
     /opt/anet/bin/anet "$@"

.. code-block:: console

   # chmod 0755 /usr/local/bin/anet-node
   # systemctl daemon-reload
   # systemctl enable --now anet-node.service
   # anet-node status
   # anet-node hub-register https://hub.example.com --name provider-1

``hub-register`` asks for the recovery passphrase on the terminal and writes
the recovery file to ``/var/lib/anet-node/.anet-recovery/``, outside the data
directory. Copy that file to separate storage (see :ref:`ops-backup`). Without
a terminal, registration still completes, and the node reports that the
identity has no recovery passphrase until ``anet identity protect`` is run in
a terminal.

A node in a container
---------------------

The same approach works for a node. The image holds a static ``anet`` binary,
a writable ``/data`` directory for the node's data and a writable
``/recovery`` directory that receives the recovery file. Keep the two on
separate volumes so that they can be backed up and stored separately.

.. code-block:: dockerfile

   FROM debian:12-slim AS prepare
   RUN mkdir /data /recovery

   FROM gcr.io/distroless/static-debian12:nonroot
   COPY --from=prepare --chown=65532:65532 /data /data
   COPY --from=prepare --chown=65532:65532 /recovery /recovery
   COPY anet /usr/local/bin/anet
   ENV ANET_DATA_DIR=/data HOME=/recovery
   VOLUME ["/data", "/recovery"]
   ENTRYPOINT ["/usr/local/bin/anet"]
   CMD ["daemon"]

.. code-block:: console

   $ CGO_ENABLED=0 go build -o anet ./cmd/anet
   $ docker build -f Containerfile -t anet-node:0.3.0 .
   $ docker run -d --name anet-node --restart unless-stopped \
         -v anet-node-data:/data -v anet-node-recovery:/recovery anet-node:0.3.0
   $ docker exec -it anet-node anet hub-register https://hub.example.com --name provider-1
   $ docker exec anet-node anet doctor

The control plane and the local A2A interface listen on the container's own
loopback interface and are not published; commands reach them through
``docker exec``. Do not publish those ports.


Checklist
---------

Before a hub accepts nodes from the Internet:

#. ``/healthz`` answers through the public name over HTTPS, and the version
   is the one installed most recently.
#. The proxy sets ``X-Real-IP``, allows request bodies of at least
   ``--relay-max-envelope``, and keeps no access log.
#. The hub identity has a recovery passphrase, and a copy of the recovery
   file is stored away from the host.
#. The operator plane, when used, answers only through the proxy and its
   token is neither a placeholder nor shared with another system.
#. The daily maintenance job has run once (``roll.log`` in the data
   directory) and a backup can be restored on a test host
   (:ref:`ops-backup`).
#. ``anet-hub admission`` shows the intended mode, and ``federation.json``
   lists exactly the intended peers (:ref:`ops-federation`).
