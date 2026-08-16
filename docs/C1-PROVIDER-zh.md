# C1 — CapabilityProvider 合同（能力提供者）

> anet4 五合同之一（总纲见 anet 母仓 docs/K207）。Go 包：`provider/`。

## 一句话

**daemon 获得可调用能力的唯一门。** 原生 agent、外部 agent 连接器、物理世界
runtime（ANetLink）都从这一个接口进来。

## 红线（org 教训）

daemon 不得知道 provider 背后是什么——尤其**不得知道"设备"这个概念**。
provider 声明能力、可被调用、返回效果，仅此三件事过界。

## 形态

进程内：实现 `provider.CapabilityProvider`，`Registry.Register` 注册。
进程外（独立部署的 anetlinkd）：daemon 侧一个 shim 实现同一接口、内部走
UDS——对 daemon 代码零差别。wire 载荷类型全部来自 ANetCore
（`effect.Effect`、`tsir.EffectRecord`），杜绝跨仓类型漂移。

## 语义要点

- `Invoke` 返回 `effect.Effect`：**UNVERIFIED 不是失败**——"命令已送达但物理
  效果在当前信任级不可验证"与 FAILED 严格区分（信任记账依赖这个区分）。
- 能力解析：精确匹配优先，然后按 `.` 边界向上走（注册 `light` 可服务
  `light.onoff`；`lightning` 永不匹配 `light`）。
- 注册原子性：任何冲突（重复 ID / 能力已被他人持有）导致整次注册不生效。
- `Describe` 返回 CAS 描述对象的 CID，daemon 原样挂上 ADP 卡，不解释内容。
