.. _contributing:

Contributing
============

ANet consists of three repositories, ``ANet`` (the node program, the command line and these
documents), ``ANetHub`` (the hub) and ``ANetCore`` (the protocol and cryptography kernel). This
page describes how to build and test them, what is expected of a change, and how contributions
are licensed.

.. contents::
   :local:
   :depth: 2


Ground rules
------------

* Changes to the protocol are discussed in an issue before a pull request is opened. This
  covers wire formats, signatures and content identifiers. Protocol bytes do not change after
  a release: key numbers, labels and codes are never reused, and every change of the register
  (``wire/registry.json`` in ANetCore) is made together with the specification and the affected
  golden vectors.
* Bug fixes, documentation, tests and portability fixes are welcome without prior discussion.
* A change that alters a sentence shown to a person, a command, a configuration key or a reason
  code updates the documentation in the same pull request.
* Do not report vulnerabilities in public issues. See :ref:`reference-security` and ``SECURITY.md``.


Building and testing
--------------------

The programs are written in Go (version 1.26 or later) and are pure Go: ``CGO_ENABLED=0`` and no
build tag are needed for the default build.

.. code-block:: console

   $ ./build.sh            # builds ./anet
   $ ./build.sh --check    # gofmt, go vet, go test and the build-tag checks, then the build

The race detector needs cgo and a long timeout, because the tests of the daemon alone run for more
than ten minutes under it:

.. code-block:: console

   $ CGO_ENABLED=1 go test -race -timeout 45m ./...

Build tags
~~~~~~~~~~

Tags come in two directions. A *subtractive* tag removes a module that is built in by default:
``no_anetlink``, ``no_p2p``, ``no_blackboard``, ``no_org``, ``no_cas``, ``no_service``, ``no_mcp``,
``no_x402``, ``no_a2a``, ``no_preview`` and ``no_autoreply``. An *additive* tag adds a module that is absent by default: ``shell``.
``go test ./...`` does not see code behind an additive tag, so run ``go vet`` and ``go test`` with
``-tags shell`` as well. ``bash scripts/tagcheck.sh all`` checks every tag by the number of symbols
in its own direction; ``build.sh --check`` and the continuous integration both call it.
The console has no tag yet; ``scripts/tagcheck.sh pending`` lists the modules without one. A new optional
module gets its tag in that script and a row in ``.github/workflows/ci.yml``. See
:doc:`topics/modules`.

Tests
~~~~~

Tests live next to the code. The daemon package has an in-memory fake hub for end-to-end tests without
a real hub. Wire objects are pinned by golden vectors in ``ANetCore/golden``: the bytes of an encoding
are the oracle, and two repositories that share an object (for example the C1 interface between ``anet``
and ANetLink) carry byte-identical copies of the vectors.

Continuous integration
~~~~~~~~~~~~~~~~~~~~~~

Two jobs run tests that depend on the machine. The job ``sandbox`` runs twice, on the host with
``bubblewrap`` and the AppArmor profile of the repository, and in a Debian 12 container; the tests that
must pass are listed in the job, and a skip of one of them is a failure. The job ``premises`` runs the
tests whose premise is the machine (a user that is not root, the reference A2A SDK, a build without a
feature), and a skip there is also a failure. Jobs for macOS and for a second account (``peercred``) cover
the other platforms.

Style
~~~~~

* Code is ``gofmt``-clean and follows the comment style of the surrounding files.
* The reason table (``internal/a2ashape/reasons.go``) is the only place for sentences that are shown to
  a person. The command line does not write reason sentences of its own.
* Sentences are written in the style described in the next section.


Adding or retiring a reason code
--------------------------------

A reason code is a stable interface (see :ref:`reason-codes-lifecycle`). It is added and retired
deliberately, in one commit that holds the code, the register and the documentation.

To add a code:

#. Add the row to the reason table in ``internal/a2ashape/reasons.go``, with both templates and the next
   step, and add the code to the registry of user reasons in ANetCore if the wire registry requires it.
#. Add one line to ``internal/a2ashape/codes.txt``, in sorted place, with the version being prepared as
   *since* and the status ``active``. The failure of ``TestCodeRegisterMatchesTheTable`` prints the exact
   line when it is missing.
#. Regenerate the reference with ``docs/_tools/gen_reason_codes.py`` and add the Chinese entries of the new
   sentences to the catalog of ``reference/reason-codes``.
#. Name the code in the entry of the release in ``docs/news.rst``.

To retire a code, remove its row, change its line in ``codes.txt`` to ``retired`` with the retiring version
and, if there is one, the replacement code, and regenerate the reference. Never delete the line and never
give the name to another event. A hub reason code follows the same steps in ``internal/wire3/reasons.go`` and
``internal/wire3/codes.txt`` of ANetHub, and ``docs/_tools/gen_hub_reasons.py``. A name that is a code of both
the hub and the node must be listed as mirrored in ``docs/_tools/check_code_collisions.py``, which
``docs/_tools/build-site.sh`` runs.


Documentation
-------------

The documentation is written in reStructuredText with Sphinx and the Read the Docs theme. English is the
source language; Chinese is translated with ``sphinx-intl`` from gettext catalogs.

.. code-block:: console

   $ pip install -r docs/requirements.txt
   $ make -C docs html
   $ SPHINX_LANGUAGE=zh_CN make -C docs html

The build runs with ``-W --keep-going``, so every warning fails it. A page that adds a command, a
configuration key or a code must give its source in the code or in the specification. The pages of
the reference are generated where the source of the programs has the data: the command index, the
reason codes, the hub codes and the wire tables (the scripts are in ``docs/_tools``).

Style
~~~~~

* Write in a formal, restrained register. Use noun phrases as headings and complete declarative
  sentences as explanations. Do not use colloquial language, exclamation marks, rhetorical questions or
  emoji. Do not personify the software.
* Do not address the reader directly in descriptions. Use roles such as operator, caller and node. An
  imperative is acceptable for a step in a procedure.
* Use the terms of the :ref:`glossary <reference-glossary>`, and introduce a term at its first use.
* Write times in UTC and in ISO 8601, and amounts with the unit credits.
* Use ``hub.example.com`` for hosts and an obviously fictitious value for an AID. Do not publish keys,
  tokens or the names of internal hosts.
* Examples are complete and runnable. Mark commands with ``.. code-block:: console`` and structured data
  with ``json``.
* Cross-reference with ``:ref:`` and ``:doc:``.

Translation
~~~~~~~~~~~

The Chinese text is written to read naturally and not word for word, in the same register. Extract and
update the catalogs with:

.. code-block:: console

   $ make -C docs gettext
   $ sphinx-intl update -p docs/_build/gettext -l zh_CN

Terms follow the glossary. Command lines, configuration keys, codes and paths are not translated.

In Chinese text, inline markup that touches a Chinese character needs an escaped space (a backslash and a space)
before or after it, because docutils does not recognise the start or the end of the markup
otherwise. After every ``sphinx-intl update``, run the script that sets these escapes:

.. code-block:: console

   $ python3 docs/_tools/fix_po_markup.py


Licensing of contributions
--------------------------

ANet is released under the ANet Open Source License (see :doc:`license`). By submitting a contribution,
the contributor agrees that it is provided under that license, including the contributor terms: Agent
Network Research may adjust the license, may use the contribution commercially, including in its hosted
hub and cloud operations, and may submit it to an A2A project under the Apache License 2.0 alone.
Contributions to the A2A specification material in ``docs/a2a/`` are licensed under the Apache License 2.0.


Security reports
----------------

Report a suspected vulnerability by email to ``hi@anet0.com`` with the subject ``[SECURITY]``.
