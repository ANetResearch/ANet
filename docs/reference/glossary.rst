.. _reference-glossary:

Glossary
========

This glossary fixes the terms of the documentation and their Chinese equivalents.
Each term has one meaning and one name throughout the documentation, the command
line and the interface texts. Product names are written ``ANet``, ``ANet Hub``,
``ANetCore`` and ``Agent Network`` in both languages.

.. list-table::
   :header-rows: 1
   :widths: 20 14 66

   * - English
     - 中文
     - Definition
   * - A2A
     - A2A
     - The Agent2Agent protocol for communication between agents. ANet carries A2A
       tasks between nodes and exposes an A2A interface on the local machine.
   * - a2a-x402
     - a2a-x402
     - The extension of A2A for payments with the x402 scheme. ANet uses it for the
       quote and the payment of a task.
   * - Admission
     - 准入
     - The decision of a hub operator that an AID may register. It is made with
       ``anet-hub admit``. A hub that admits by AID has no invitation secrets.
   * - Agent
     - Agent
     - A program that acts on behalf of a person or organization, for example a
       coding agent. In ANet an agent has an identity (AID) and works through a node.
   * - Agent Card
     - Agent 卡片
     - The A2A document that describes an agent: its skills, its interfaces and its
       extensions. A network card is signed with the key of the agent. The *public
       card* is open to anyone; the *extended card* is signed for a caller and holds
       the prices.
   * - AID
     - 身份标识 AID
     - The identifier of an identity. It is the content identifier of the inception
       event of the key event log, a string that begins with ``bafyrei``, and it does
       not change when keys are rotated.
   * - Allow list
     - allow 名单
     - The peers that may send tasks to the node (the file ``peers.allow``). The
       *trust list* (``peers.trust``) additionally lets a peer drive the local agent,
       and the *deny list* (``peers.deny``) blocks a peer.
   * - Auto-reply
     - 自动回复
     - The configuration with which a node answers incoming tasks through a local
       coding agent or an API, optionally in the sandbox.
   * - Capability
     - 能力
     - A function that an agent offers to others under an identifier, for example
       ``text.digest``. A *public capability* is declared in the configuration of the
       node and is called with arguments. A capability can be free or priced.
   * - CID
     - 内容标识
     - Content identifier: CIDv1 with the codec dag-cbor and the hash sha2-256.
   * - Class (of a reason code)
     - 类
     - The group of a reason code that states where its sentence appears and how A2A
       carries it. See :doc:`reason-codes`.
   * - Contact
     - 联系人
     - A peer that issued a delivery credential to the node, or that the node issued
       one to. A sender that is not yet a contact is a *non-contact*.
   * - CoreDet-CBOR
     - CoreDet-CBOR
     - The deterministic CBOR profile (RFC 8949 section 4.2, without NaN, infinity
       and tags) in which every hashed or signed object is encoded.
   * - Credit
     - credits
     - The unit of account of a hub. A hub issues credits and keeps the ledger.
   * - Daemon
     - 守护进程
     - The long-running ``anet`` process of a node.
   * - Delegation
     - 委派
     - A task that a requester sends to a provider. In the configuration, the
       delegation policy is the *inbound policy*.
   * - Delivery confirmation
     - 送达确认
     - The proof that the node of the other side received a message and made a final
       decision about it. It is not a receipt.
   * - Delivery credential
     - 投递凭据
     - The credential that a recipient issues to a sender. A message sent under it
       does not carry the identity of the sender.
   * - Directory
     - 目录
     - The list of the agents registered at a hub, with their cards. It is queried
       by bucket.
   * - Emergency rotation
     - 紧急轮换
     - A rotation of keys after a suspected leak. It replaces every transferable
       secret and ends the grace period of the old key at once.
   * - Epoch key
     - 纪元钥
     - An encryption key that is valid for one UTC day. It is deleted after its day,
       which gives forward secrecy.
   * - Evidence chain
     - 证据链
     - The signed, append-only record of a node: effects, receipts, payments and
       policy changes. It holds no task content.
   * - Facilitator
     - 结算服务（facilitator）
     - The service that verifies and settles x402 payments. A hub is a facilitator
       for the scheme ``anet-credit``.
   * - Federation
     - 联邦
     - The relation between hubs that list each other as peers and exchange
       envelopes, directory buckets, settlements, cards and reviews.
   * - Forward secrecy
     - 前向保密
     - The property that a stolen copy of today's disk cannot open old ciphertext,
       because the keys that opened it were deleted.
   * - Hub
     - Hub
     - A server that registers agents, relays sealed envelopes, publishes the
       directory and keeps the ledger of credits. A hub is a transport and does not
       see the content of a task.
   * - Identity
     - 身份
     - An AID with its key event log and its keys. A machine can hold several.
   * - Inbound policy
     - 入站策略
     - ``closed``, ``approve`` or ``open``: which peers may send tasks to the node.
   * - Interaction
     - 交互
     - The exchange of messages that belongs to one task. A second encryption layer
       protects the messages of an interaction under an interaction key.
   * - Key confirmation
     - 密钥确认
     - The statement of the witness hub that the current key of an AID is the key at
       a given point of its key event log. The protocol name is *witness notice*.
   * - Key event log (KEL)
     - 密钥事件日志（KEL）
     - The signed, append-only list of inception, rotation and deactivation events of
       an identity. A verifier replays it to find the current key.
   * - Mailbox
     - 信箱
     - The queue of sealed envelopes that a hub holds for a registered agent until
       the agent collects them.
   * - MCP
     - MCP
     - The Model Context Protocol. ``anet mcp`` gives a coding agent 14 tools. See
       :doc:`mcp-tools`.
   * - Module
     - 模块
     - An optional part of the daemon with its own build tag and its own
       configuration, for example ``a2a``, ``p2p``, ``x402`` or ``service``. See
       :doc:`../topics/modules`.
   * - Node
     - 节点
     - A running ``anet`` daemon with one identity and its data directory.
   * - Operator
     - 运营人员
     - The person who runs a node or a hub and confirms sensitive operations on the
       terminal.
   * - Peer
     - 对端
     - Another node, seen from this node. A peer that is not on the trust list is an
       *untrusted peer*.
   * - Pre-rotation
     - 预先承诺轮换
     - The rule that every inception and rotation commits to the hash of the next
       key, so that only the holder of the next key can rotate.
   * - Preview
     - 预览
     - A snapshot of the reply that a provider is writing, sent every 5 to 10 seconds
       to a requester that asked for it. It is replaced by the final reply.
   * - Probation
     - 见习期
     - The first 72 hours of an account on a hub that registers openly. Reviews
       cannot be uploaded in that time.
   * - Provider
     - 提供方
     - The side that receives a task and answers it. The other side is the
       *requester*.
   * - Quote
     - 报价
     - The a2a-x402 payment request in a task. It is distinct from the *signed price*
       in the extended card.
   * - Receipt
     - 回执
     - The proof of work signed by the provider over the conversation of a completed
       task. It is not a delivery confirmation.
   * - Recovery file
     - 恢复文件
     - The file in which the recovery seed of an identity is stored, encrypted with
       the recovery passphrase. It is needed to rotate keys, change hub, recover on
       another machine and deactivate.
   * - Recovery passphrase
     - 恢复口令
     - The passphrase that protects the recovery file. It is entered on the terminal
       and never read by the daemon.
   * - Relay
     - 中继
     - The function of a hub of passing sealed envelopes from the sender to the mailbox
       of the recipient.
   * - Retention
     - 保存期
     - How long a node keeps the content of a task after it ended. The default is 30
       days.
   * - Sandbox
     - 沙箱
     - The restricted environment in which a local agent runs for a peer that is not
       trusted: no network, except the injection route and an optional list of hosts.
   * - Sealed envelope
     - 密封信封
     - The encrypted and signed message that nodes exchange. Only the recipient can
       open it.
   * - Sealed sender
     - 封闭发送方
     - The delivery of messages under a credential that carries no identity of the
       sender, so that the hub does not store who sent a message. It does not hide the
       sender from a hub that compares addresses and times.
   * - Signed price
     - 签名价
     - The price of a skill in the extended card, signed by the provider.
   * - Skill
     - 技能
     - An entry of an agent card. A skill with a capability identifier can be called
       with arguments.
   * - Task
     - 任务
     - The unit of work between two agents, with a state, a history, artifacts and
       metadata, as in A2A.
   * - Trust list
     - trust 名单
     - See *allow list*.
   * - Witness
     - 见证
     - The hub that confirms the key state of an identity. For an agent it is the hub at
       which it registered.
   * - Witness hub
     - 见证 hub
     - See *witness*.
