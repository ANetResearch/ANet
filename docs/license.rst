.. _license:

License
=======

ANet, ANetCore and ANetHub are released under the **ANet Open Source License**, which is the Apache
License 2.0 with additional conditions. The software and the documentation that are published in the three
repositories are covered. The text of the file ``LICENSE`` in each repository is the license; this page is a
summary and does not replace it.

.. contents::
   :local:
   :depth: 1


What the license permits
------------------------

The Apache License 2.0 applies in full, including the grant of the copyright license (section 2) and of the
patent license (section 3). The following uses are expressly permitted:

* Commercial use, including as a back end of other applications. This covers embedding the ``anet`` daemon,
  the ``anet`` command line or the ANetCore packages in a product.
* Running daemons and hubs on one's own infrastructure, without a limit on the number of nodes.
* Modification and redistribution under the terms of the Apache License 2.0 and the conditions below. A
  copy of the license that is given to recipients is the entire document, additional conditions included.


Additional conditions
---------------------

Condition 1a: multi-tenant hosted hub
   Without written authorization of the producer, the source code must not be used to operate a
   *multi-tenant hosted hub*: a hub that is offered as a service, publicly or commercially, to several unrelated
   organizations or individuals for registering their agents and relaying their messages. Two parties are unrelated
   when neither controls the other and they are not under common control. Two cases do not need authorization:

   * a hub used by one individual, or only within one organization, including the entities that control it, that
     it controls or that are under common control with it, and their employees and contractors acting for them;
   * a non-commercial federated hub: a hub that peers, through the federation protocol, with at least one hub
     operated by the producer, charges no fee for registration, relay, discovery or settlement, and is not offered
     as part of, or to promote, a paid product or service.

Condition 1b: logo and copyright information
   In the user interfaces of ANet, the ANet logo and the copyright information must not be removed or
   changed. Names or marks of the redistributor may be added alongside. The user interfaces are the web page of
   a hub, the operator console of a hub, the local console of the daemon and the output of the command line
   programs (for example the version, help and banner lines). The condition does not apply to uses that involve
   none of these interfaces, such as the use of ANetCore as a library or the operation of the daemon behind another
   application.

Condition 2: contributors
   A contributor agrees that the producer can adjust the license to be stricter or more relaxed, that the
   contribution may be used commercially, including in the hosted hub and the cloud operations of the producer, and
   that the producer may submit it to an A2A upstream project under the Apache License 2.0 alone.

Condition 3: contributions to the A2A protocol
   The specification material in ``docs/a2a/`` of the ANet repository, and the parts of ANet that the producer
   submits to an A2A upstream project (in the form submitted), are licensed under the Apache License 2.0 alone, and
   the additional conditions do not apply to them. Nothing in the license restricts the implementation of the A2A
   protocol, or of those specifications, in software that is not derived from the source code of ANet, or the use
   of the identifiers that the specifications define.


Third-party components
----------------------

The programs link third-party libraries under their own licenses. The notices that those licenses require
are part of the source repositories.


Commercial license and questions
--------------------------------

For a commercial license, or for any question about the license, write to ``hi@anet0.com``.
