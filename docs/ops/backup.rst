.. _ops-backup:

Backup and recovery
===================

This page describes what to save from a hub and from a node, how to restore
it, and what a restore changes for the other parties. Two properties of ANet
shape the procedures. Identity is split across places: the data directory
holds the keys in use, and the key that will sign next is derived from a
recovery seed that is kept in a separate recovery file, encrypted under a
passphrase. And a restored copy is detected: an identity whose data has gone
back to an older state is noticed by its hub and by its peers.

.. contents::
   :local:
   :depth: 2


What to save from a hub
-----------------------

.. list-table::
   :header-rows: 1
   :widths: 30 14 56

   * - Item
     - Required
     - Content and notes
   * - ``hub.db``
     - yes
     - Registry, key histories, ledger window, supply epochs, mailboxes. The
       maintenance job writes a backup without the relay content (see
       below); do not copy the file by hand while the hub runs, because the
       write-ahead log (``hub.db-wal``) belongs to it.
   * - ``hub_identity.kel``
     - yes
     - The hub identity file. A restore needs it: without the identity the
       hub would start under a new AID, and the ledger, which is kept on the
       hub's own account, would no longer be its ledger.
   * - Hub recovery file
     - yes
     - ``<home>/.anet-recovery/<last 12 characters of the AID>.anetrec``, or
       the path given to ``--recovery-file``. Encrypted under the recovery
       passphrase. Store it away from the host (see
       :ref:`ops-hub-recovery`). The passphrase is not recoverable.
   * - ``federation.json``
     - if used
     - The peer table (:ref:`ops-federation`).
   * - ``kel_hwm``
     - no
     - A counter file that lets the hub detect an older ``hub.db``. Restoring
       the pair together is correct; restoring ``hub.db`` alone is detected
       at the next start.
   * - ``keys/review_tag.key``
     - no
     - The key that makes reviewer tags. By design, no backup made by the
       maintenance job holds it. When the file is missing at start, a new key
       is made and the reviewer counts start again under a new key number.
   * - ``federation.db``
     - no
     - Pinned peer key histories and cursors. Fetched again.
   * - ``admin.db``, ``officials.json``
     - if used
     - The operator plane's own data directory: the audit trail, moderation
       marks and the official-agent register.
   * - Proxy configuration, ``admin.env``
     - yes
     - Outside the data directory.

``hub.db`` holds balances and a 14-day window of ledger entries. Treat a
backup like the database: restrict access to the account that runs the hub.

The maintenance job
~~~~~~~~~~~~~~~~~~~

``hub-db-roll.sh`` (installed in :ref:`ops-deployment`) runs daily. It

* checkpoints the write-ahead log on every run, so that page images of
  deleted rows do not remain in ``hub.db-wal``;
* on Sundays writes ``hub-backup-YYYYMMDD.db`` in the data directory, keeps
  the newest two backups, and vacuums the database when more than a fifth of
  its pages are free;
* on every run deletes backups that are 14 days old or older, by the date in
  their names, so that ledger records are on disk at most 14 days in the
  database and 14 days in a backup;
* appends its actions to ``roll.log`` in the data directory.

A backup holds everything except the tables whose rows only matter while the
hub runs: ``relay_message`` (the sealed envelopes), ``relay_redelivery``,
``dereg_event``, ``relay_dack`` and ``notice_cache``. The content of the
mailboxes is therefore never in a backup. The backup keeps the revocations of
delivery credentials, the quotas raised with ``anet-hub quota``, and the
balance leaves of the supply epochs of the last 14 days.

To write a backup outside the Sunday schedule, set ``FORCE_WEEKLY``. A backup
that already exists for the current date is kept, not rewritten:

.. code-block:: console

   # sudo -u anet-hub env HUB_DATA_DIR=/var/lib/anet-hub FORCE_WEEKLY=1 \
         /opt/anet-hub/bin/hub-db-roll.sh
   # tail -n 3 /var/lib/anet-hub/roll.log

Copy the backup file to separate storage; a backup beside the database
protects against a damaged file, not against a lost disk.


Restoring a hub
---------------

Restoring a hub replaces its database with a backup. Everything that happened
after the backup is lost, including credit movements. Mailboxes are empty
after a restore.

#. Stop the hub.
#. Put ``hub_identity.kel`` (and ``federation.json``, if used) in the data
   directory if it is a new one.
#. Run the restore subcommand with the backup file, or with the directory
   that holds it. A directory selects its newest ``hub-backup-YYYYMMDD.db``.
#. Start the hub.

.. code-block:: console

   # systemctl stop anet-hub.service
   # sudo -u anet-hub anet-hub --data /var/lib/anet-hub restore /srv/backups/hub-backup-20261011.db
   Restored the hub database from /srv/backups/hub-backup-20261011.db (42 agents); once the hub starts, each agent's key confirmation pauses for up to 30 minutes, or until its node connects again if it is offline.
   # systemctl start anet-hub.service

The restore subcommand refuses, and changes nothing, in these cases:

.. list-table::
   :header-rows: 1
   :widths: 36 64

   * - Message
     - Cause and action
   * - ``The hub is running, so nothing was changed``
     - Stop the hub and run the command again.
   * - ``No hub backup (hub-backup-<date>.db) was found``
     - The path is not a backup file or a directory holding one.
   * - ``There is no hub identity in <path>``
     - The data directory has no hub identity. Put the original identity
       file back, or point ``--data`` at the hub's data directory.
   * - ``... is not a whole hub backup from this version of anet-hub``
     - The file is damaged or is not a hub database. Use another backup.
   * - ``... was written by a newer anet-hub``
     - Upgrade ``anet-hub`` and run the command again.

What a restore does to the network
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

A hub signs a notice that confirms the current key state of each agent.
After a restore, the hub's database may be older than the key histories that
nodes have since published. To prevent an old key from being confirmed as
current, the hub signs no notice for an agent until

* it accepts a strictly longer key history for the agent from its node, or
* 30 minutes have passed since the hub began to serve and the node sends a
  request signed with the key at the top of the stored history.

Each agent is released on its own. Nodes report ``This node's hub was just restored
from a backup and is checking identity records; nothing to do, it takes at
most 30 minutes.`` An owner who is online is released within seconds. An
agent that rotated after the backup and whose node stays offline remains held
until it connects.

The same quarantine applies if ``hub.db`` is put back by hand: at the next
start the hub finds that its change counter is lower than the one in
``kel_hwm`` and holds every agent in the same way.

Mailboxes are empty after a restore. A sender asks the hub whether an envelope
was collected; the hub answers ``unknown`` for an envelope it no longer has,
and the sender's node sends the message again.

After a restore, nodes can compare their own payment record with the hub's
ledger with ``anet reconcile`` and check the supply chain with
``anet audit-hub`` (see :ref:`reference-cli`).

When the hub identity itself is lost
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

If the data directory is lost together with ``hub_identity.kel``, the hub
cannot be continued under its AID by copying files. A new data directory
creates a new identity, which is a new hub: new network name, empty ledger,
and peers that must list its new AID. If the recovery file and passphrase are
available, a rotation under them proves control of the identity; see
:ref:`ops-hub-recovery`.


What to save from a node
------------------------

A node's data directory is ``~/.anet`` (or ``$ANET_HOME``) for the default
identity, ``~/.anet/ids/<name>`` for others, or the directory named by
``ANET_DATA_DIR``. Within one directory:

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - File or directory
     - Content
   * - ``identity.kel``
     - The identity: key event log and the keys in use.
   * - ``enc_keys.cbor``
     - The ring of encryption keys. Keys of past days are deleted when they
       can no longer open any message (forward secrecy).
   * - ``mbox.key``
     - The mailbox signing key.
   * - ``config.json``
     - Configuration.
   * - ``interactions/``
     - The local delegation log: tasks and, for the retention period, their
       content.
   * - ``evidence.ael.jsonl``
     - The evidence chain. It is append-only and is what receipts are
       checked against.
   * - ``peers.allow``, ``peers.deny``, ``peers.trust``, ``payees.allow``
     - The lists that decide whom the node serves and whom it pays.
   * - ``hub_join.json``, ``key_state_notice.cbor``
     - Hub join state and the latest signed key-state notice.
   * - ``modules/``
     - Private state of modules: the address and the token of the local A2A
       interface, ``modules/autoreply/sessions.json`` (the map from an
       interaction to the session of the local agent) and
       ``modules/x402/liab_proof.jsonl`` (the balance proofs of the hub that the
       node has verified, one line for each epoch, mode ``0600``).
   * - ``daemon.lock``
     - An empty file on which a running daemon holds an exclusive lock. It is
       not part of a backup. The operating system releases the lock when the
       process ends, however it ends.
   * - ``recovery_local.key``, ``recovery_local.anetrec``
     - Present only while the identity has no recovery passphrase. Together
       they can rotate the identity (see below).
   * - ``control_token.txt``, ``modules/a2a/a2a_token.txt``
     - Tokens of the local interfaces. They are replaced by deleting them
       and restarting; they do not need to be saved.
   * - ``daemon.log``
     - The node's log.

A command that changes files under a running daemon (``anet retention set``, the
steps of ``anet identity`` that work on files, ``anet init --without``) first
looks at the lock. When the lock is held and the control interface does not
answer within a few seconds, a daemon is there and busy, and the command stops
with ``daemon_not_answering`` and changes nothing. When the daemon answers, the
command uses it; ``anet init --without`` and ``--with`` stop with
``daemon_running`` instead.

A backup of this directory contains private keys and the task content kept
during the retention period (30 days after a task ends by default, 7 days for
public calls). Keep it as carefully as a password store: mode ``0700``, on
encrypted storage.

The recovery file is not in the data directory. It is
``~/.anet-recovery/<last 12 characters of the AID>.anetrec``, written with
mode ``0600``, and removing an identity with ``anet identity rm --purge`` does
not delete it. It is useless without the passphrase, and the passphrase is
not stored anywhere by ANet. Save the file and remember the passphrase in
separate places.

A backup procedure
~~~~~~~~~~~~~~~~~~

Stop the node so that the files are consistent, copy the directory, and
start the node again:

.. code-block:: console

   $ anet stop
   $ (umask 077; tar -C ~ -czf "anet-backup-$(date -u +%Y%m%dT%H%M%SZ).tar.gz" .anet)
   $ anet up

Keep the archive on a different device from the node, and keep a copy of the
recovery file with a different custodian from the archive. For a node run by
systemd, replace ``anet stop`` and ``anet up`` with ``systemctl stop`` and
``systemctl start`` of the service.


Restoring a node
----------------

Three situations differ in their consequences.

**Moving to another machine.** Stop the node on the old machine, copy the
whole data directory to the new one, and start it there. Do not start the
data on the old machine again. Tasks in progress continue.

**Restoring from a backup.** The state after the backup is lost. At the next
contact with the hub the node finds that its data is older than the last key
state it published, ends the tasks that were in progress, and generates its
encryption keys again. The tasks end with the reason ``local_state_lost``,
and their other party is notified; new tasks are not affected. The node
shows the following warning for 7 days:

.. code-block:: text

   At 10-16 09:30 this node found its data had gone back to an older state and ended 2 tasks in progress; if you did not restore a backup or copy the data directory, this identity is running elsewhere and its data may have leaked: run anet identity rotate --emergency.

The sentence appears in ``anet status`` and as the check ``local_state`` in
``anet doctor``.

A backup that is newer than the latest key publication, at most about a day
old, is not detected as a rollback. A node with no hub configured does not
perform the check.

.. code-block:: console

   $ anet stop
   $ mv ~/.anet ~/.anet.old
   $ tar -C ~ -xzf anet-backup-20261011T020000Z.tar.gz
   $ anet up
   $ anet doctor

**Only the recovery file remains.** The identity can be recovered from the
recovery file and its passphrase into a new data directory. The old task
history is not restored; the new directory has no interactions, so no
rollback is reported.

.. code-block:: console

   $ anet identity recover ~/Downloads/ab12cd34ef56.anetrec --name recovered
   Recovery passphrase: ********
   Recovering also replaces the recovery passphrase.
   New recovery passphrase: ********
   Type it again: ********
   Recovered as local identity recovered (data directory ~/.anet/ids/recovered). The new recovery file is ~/.anet-recovery/ab12cd34ef56.anetrec; keep a copy somewhere else. The old recovery file no longer works.

A recovery file that has no passphrase cannot be used this way. It belongs to
an identity that joined a hub without a terminal and works only together with
its own data directory, from which ``anet identity protect`` sets a passphrase
(see :ref:`topics-identity`).

Never run two copies of one identity
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Two data directories of the same identity that run at the same time compete
for one mailbox at the hub. Each finds that its data is older than the state
the other published, ends its tasks, and one copy can read and acknowledge
messages the other needed. The second detection within 7 days is reported as
a failure (``local_state.repeated``). If a rollback warning appears and no
backup was restored and no directory copied, the data directory may have been
copied by someone else. Run:

.. code-block:: console

   $ anet identity rotate --emergency

The emergency rotation invalidates the signing key of the other copy, replaces
every secret that can be copied from the data directory, ends tasks in progress
with the reason ``key_compromise``, and sets a new recovery passphrase. It
needs the recovery passphrase and a terminal.


Damaged or incompatible data directories
----------------------------------------

The node refuses to start in a data directory it cannot use and leaves the
directory unchanged:

``This data directory is from an earlier release (anet 0.2 or AgentNetwork 1.x), and 0.3 does not migrate old data; move it away and run anet up to create a new identity.``
   Release 0.3 does not migrate old identities. Move the directory away; from
   0.3.5, ``install.sh`` renames it to ``~/.anet-legacy-<version>-<time>``.
   The new identity must join a hub again.

``This data directory was written by a newer anet, which this version cannot open; run anet update and start again (do not delete the directory).``
   The data is valid. Upgrade the program.

``The identity file ... is damaged and cannot be read, so anet did not start``
   Restore ``identity.kel`` from a backup, or recover the identity from its
   recovery file with ``anet identity recover``.

The hub refuses a damaged or incompatible data directory in the same way (see
:ref:`ops-hub`).


Deletion and backups
--------------------

ANet deletes content by schedule: task content at the end of its retention
period, encryption keys when they can no longer be used, and hub ledger
entries after 14 days. These deletions act on the files in use. Copies in
backups, snapshots of a copy-on-write file system, and blocks left on a
solid-state device are outside what the software can delete. Apply the
retention period to backups as well: expire the archives of a node and the
backups of a hub on a schedule that matches what is promised to users. The
maintenance job does this for hub backups, which it keeps for at most 14
days.
