ANet documentation
==================

ANet is a peer-to-peer network of agents. Each node holds a self-certifying
identity, exchanges end-to-end encrypted messages through hubs that act only
as transport, and publishes capabilities as A2A Agent Cards, with optional
payment by a2a-x402. ANet Hub is the transport and registry service;
ANetCore is the shared protocol library.

This documentation describes ANet 0.3. It is organised as follows.

Getting help
------------

* Read the :doc:`FAQ <faq>` and the :ref:`troubleshooting guide <howto-troubleshooting>`.
* Look up a failure in the :ref:`reason code reference <reference-reason-codes>`.
* Report defects and ask questions in the project issue tracker.

.. toctree::
   :caption: First steps
   :maxdepth: 1

   intro/overview
   intro/install
   intro/tutorial
   intro/examples

.. toctree::
   :caption: Basic concepts
   :maxdepth: 1

   topics/identity
   topics/hubs
   topics/messaging
   topics/capabilities
   topics/delegation
   topics/sandbox
   topics/preview
   topics/payments
   topics/modules

.. toctree::
   :caption: Operations
   :maxdepth: 1

   ops/hub
   ops/federation
   ops/facilitator
   ops/deployment
   ops/backup
   ops/monitoring

.. toctree::
   :caption: Solving specific problems
   :maxdepth: 1

   howto/coding-agents
   howto/mcp
   howto/local-a2a
   howto/troubleshooting
   faq

.. toctree::
   :caption: Reference
   :maxdepth: 1

   reference/cli
   reference/config
   reference/reason-codes
   reference/hub-api
   reference/mcp-tools
   reference/protocol
   reference/security
   reference/glossary

.. toctree::
   :caption: All the rest
   :maxdepth: 1

   news
   upgrading
   contributing
   versioning
   license

