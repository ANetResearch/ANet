.. _versioning:

Versioning policy
=================

This page states what the version numbers of ANet promise, which compatibility surfaces exist and
how a change reaches users.

.. contents::
   :local:
   :depth: 2


Components and numbers
----------------------

A release is made of four components, each with its own number.

.. list-table::
   :header-rows: 1
   :widths: 26 20 54

   * - Component
     - Release 0.3
     - Content
   * - ANetCore
     - v0.16.0
     - The protocol and cryptography kernel (Go module). ``ANet`` and ``ANetHub`` require the same
       version in their ``go.mod``.
   * - ``anet``
     - 0.3.0
     - The node program: daemon, command line, MCP server and local interfaces.
   * - ANetHub
     - 0.3.0
     - The hub and the operator console.
   * - ANetLink
     - 0.2.0
     - The bridge to local devices and services. It speaks the C1 interface, version 1.

``anet version`` prints the version, the commit, the time of the build and the modules that are
compiled into the binary. ``anet-hub --version`` prints the release, the hub wire version and the
first ``anet`` release that speaks it.


Compatibility surfaces
----------------------

Compatibility is defined per surface, each with its own version that is checked at run time.

.. list-table::
   :header-rows: 1
   :widths: 22 24 54

   * - Surface
     - Version in 0.3
     - Check
   * - Hub wire
     - ``X-ANet-Wire: 3``
     - Sent on every request and response. A different value is refused with ``wire_too_old`` (426) or
       ``wire_too_new`` (400).
   * - Sealed envelope
     - ``v = 2``
     - Any other value is refused by the hub (``bad_envelope``) and by the receiver.
   * - Key event log
     - ``anet.kel/2``
     - A log without the tag does not replay.
   * - Direct frames
     - ``v: 3``
     - A frame of another version is not delivered.
   * - C1 (``anet`` and ANetLink)
     - version 1, path ``/v1``
     - ``/v0`` answers 426 ``c1_version``.
   * - Data directory
     - ``user_version = 3``, ``enc_keys.cbor`` ``v = 2``, ``anet.identity/2``
     - An older data directory is refused with ``identity_v02``, a newer one with ``datadir_too_new``.
   * - Configuration
     - the keys of :doc:`reference/config`
     - A removed key stops the daemon (``config_key_removed``).
   * - A2A
     - A2A 1.0
     - The local interface and the cards follow A2A 1.0. ANet adds extensions in the namespace
       ``agentnetwork.org.cn``.

A key number, a label, a code or a route that was removed is not reused for another meaning.


Release series
--------------

* A change that breaks a surface (wire, identity, data or configuration) is made only in a release that
  increases the minor number, such as 0.3.0. All such changes of a series are made in its first release.
* The patch releases of a series (0.3.x) do not break any surface of the table above. A node and a hub of
  one series interoperate in any combination of patch versions, and can be upgraded in any order. A patch
  release fixes defects and adds features that do not change a surface, for example modules that are
  completed after the first release.
* The notices for removed commands and options stay through the series. After it, a removed command is
  treated as an unknown command.
* The notes of a release list every change of a surface, and :doc:`upgrading` states what has to be done.

.. v030-doc-verify: statement on patch releases of 0.3.x (release plan: modules that are not complete follow as 0.3.x without breaking compatibility)


Releases and updates
--------------------

A release is described by ``release.json`` with the version, the commit, the signing time, the expiry,
the SHA-256 of every archive and binary, the module set of each variant and the fingerprint of the next
release key. The file is signed with the release key of the project in the SSH signature format.
``install.sh`` and ``anet update`` verify the signature, the expiry and that the version is not older than
the installed one before they install. The host from which archives are downloaded is not trusted. The key
and its pre-committed successor are published in ``SECURITY.md`` of the repository.

A node that has no support for a feature of a hub, or a hub that needs a newer ``anet``, says so in one
sentence with the release to install (``required_anet`` in the answer of the hub).


Deprecation
-----------

A feature is removed in the release that increases the minor number. The release notes list it, the
removal is answered with a sentence that names the replacement, and the replacement exists in the release
that removes it.


Documentation versions
----------------------

The documentation is published for the current release and describes the behavior of that release. The
English text is the source and the Chinese text is a translation of it.
