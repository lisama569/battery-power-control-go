# 整车功率预算与电池保护管线 (battery-power-control-go)

一套**本地可运行**的整车功率预算与电池保护管线：用模拟的电压/电流/温度/转速
采样驱动，在同一处理周期内得出自洽、可解释的结论——

- 哪些样本被拒绝、为什么拒绝；
- 电池 SOC 与各信号是否有效；
- 激活了哪些故障、依据哪条阈值；
- 放电/转矩/回馈上限是多少、由哪条约束决定；
- 各驱动与用电单元在本周期拿到多少功率、总额是否守恒。

模块边界（均为并发安全）：

| 包 | 职责 |
|---|---|
| `internal/sensing` | 采样有效性判定（量程/陈旧度/时间间隔/跳变/缺失）与最新状态估计 |
| `internal/battery` | 安时积分 SOC、电池聚合状态与有效性透传 |
| `internal/fault` | 带确认窗口、迟滞与锁存的故障状态机 |
| `internal/derating` | 随温度/SOC/电压/转速/故障单调变化的功率/转矩上限 |
| `internal/budget` | 多单元并发功率预算申请、优先级分配与守恒记账 |
| `internal/pipeline` | 把上述子系统装配成“采样→估计→故障→限幅→预算”的单周期管线 |
| `cmd/demo` | 确定性虚拟时钟驱动的本地演示程序 |

---

## 1. 状态估计与样本有效性 (`internal/sensing`)

每个通道（`voltage/current/temp/rpm`）独立配置量程、最大跳变、新鲜度窗口与
相邻样本时间间隔。样本按**固定优先级**判定，只返回首个命中的、可区分的原因：

| 拒绝原因 | 含义 |
|---|---|
| `out_of_range` | 数值超出 `[Min, Max]` |
| `stale` | 时间戳比当前时钟老于 `MaxAge`，或落在未来 |
| `bad_interval` | 与上一有效样本间隔不在 `[MinInterval, MaxInterval]` |
| `jump` | 与上一有效样本的数值变化超过 `MaxDelta` |
| `unknown_channel` | 通道未注册 |

被拒绝的样本**只记录、不更新估计值**，因此不会静默参与任何下游计算
（`TestRejectedSampleDoesNotMutateEstimate` 专门固化该行为）。一批乱序样本
会先按通道+时间戳稳定排序，再顺序校验，结果可复现。

查询侧：

- 通道从未收到有效样本 → `Valid=false, Reason=missing`；
- 最新有效样本年龄超过 `MaxAge` → `Valid=false, Reason=stale`，读数冻结但不再被信任。

### 电池状态 (`internal/battery`)

SOC 用安时积分更新（放电为正）：

```
soc -= I * dt / CapacityAs
```

- 只有**当前有效**的电流读数参与积分；电流缺失/陈旧时积分冻结、`CurrentValid=false`；
- 电流恢复时若与上次有效样本的缺口超过 `UpdateMaxAge`，**不补积分**，只从此刻继续；
- SOC 钳位在 `[0,1]`；电压/温度/转速的有效性原样透传给故障与限幅层。

---

## 2. 故障判定与处理协议 (`internal/fault`)

故障不是一过阈值就置位，而是“**确认窗口 + 恢复窗口 + 迟滞阈值**”，以避免在
阈值附近反复抖动：

- 触发条件连续保持 `ConfirmWindow`（默认 100ms）才激活；
- 恢复条件连续保持 `ClearWindow`（默认 200ms）且越过迟滞阈值才解除；
- 触发阈值与恢复阈值不同，形成迟滞带（如温度 warn=55/recover=48）。

故障集合：`sensor_timeout / undervoltage / overvoltage / overcurrent /
overtemp / low_soc / soc_cutoff`，严重度为 `warning` 或 `critical`。

过温是两级状态机，互斥地落在三个温度区间：

- `T >= 70` 持续确认 → `critical`；
- `48 < T < 70`：critical 回落到此区间**立即降级为 warning**（不清除、不抖动），
  `T >= 55` 持续确认 → `warning`，`48 < T < 55` 为迟滞维持带；
- `T <= 48` 持续恢复窗口 → 完全解除。

`soc_cutoff` 是**锁存（latched）**故障：即使 SOC 后来回升也不自动清除，必须由
上层在确认安全（如完成充电）后显式调用 `Detector.Reset(...)` /
`Pipeline.ResetLatched(...)`。每条 `ActiveFault` 都带当前值、阈值、起始时间与
中文说明，可直接用于日志解释。

---

## 3. 限功率规则 (`internal/derating`)

每条约束独立给出 `[0,1]` 的可用系数，最终：

```
factor = min(温度系数, SOC系数, 电压系数, 转速系数, 故障系数)
MaxDischargeW = RatedPowerW   * factor
MaxTorqueNm   = RatedTorqueNm * factor
MaxRegenW     = RatedRegenW   * factor
```

默认分段线性曲线（端点外取端点值）：

- 温度：`45C→1.0, 85C→0.0`，随温度**单调不增**；
- SOC：`10%→0.0, 30%→1.0`，随 SOC **单调不减**（SOC 越低功率越低）；
- 电压：`280V→0.0, 340V→1.0`，随电压**单调不减**；
- 转速：`>=16000rpm` 或**转速信号无效**时取保守系数 `0.5`（无效信号不被静默信任）；
- 故障：按当前最高严重度取系数，默认 `warning→0.6`、`critical→0`
  （锁存的 `soc_cutoff` 也在其中，放电与回馈同时归零）。

`Compute` 是**纯函数**：同一输入必出同一结果；多条约束并列命中时按固定顺序
`fault > trace_temp > low_soc > low_voltage > high_rpm` 选出 binding 约束，
结论唯一、不互相矛盾、不抖动。`LimitResult.Active` 保留每条约束的系数、输入值、
阈值与文字依据，`Binding`/`ReasonText` 给出最终解释。

---

## 4. 功率预算分配协议 (`internal/budget`)

`Manager` 用互斥锁串行化所有操作；**一批申请在单个临界区内原子裁决**。

1. 未在本批申请、也未被释放的单元，其既有**预留继续保留**；
2. 本批申请按优先级排序（数值小者优先，同级按 `UnitID` 字典序）；
3. 依次在剩余预算上裁决：
   - `available >= Want` → `granted` 全额；
   - `Min <= available < Want` → `degraded` 降级到可用值；
   - `available < Min` → `rejected`，授予 0W；
4. 同一单元在一批中重复提交 → 后续申请直接拒绝（`同一周期内重复提交申请`）。

### 故障与恢复下的预留守恒

- 故障导致预算收缩（`SetBudget`）时，从**优先级最低**的预留开始回收，直到
  预留总和不超过新预算；
- 故障解除、预算恢复时**不会自动补发**已被回收的预留，单元必须重新申请，
  从根上杜绝“重复计入”；
- 主动放弃的单元用 `Release(ids)` 立即归还预算。

守恒不变式（测试持续校验）：

```
sum(所有预留) == AllocatedW
AvailableW    == TotalBudgetW - AllocatedW >= 0
AllocatedW    <= TotalBudgetW
```

`Pipeline.Step` 每个周期都把预算上限同步为当期 `MaxDischargeW`，因此预算收缩/
恢复与当期限幅结论严格一致。

---

## 5. 管线并发模型 (`internal/pipeline`)

`Step`/`Snapshot`/`ResetLatched` 由管线级互斥锁串行，子系统自身也各自带锁。
一个 `Step` 在同一把锁内完成“采样校验 → 电池估计 → 故障判定 → 限幅 →
预算同步与分配”，返回的 `TickResult` 是自洽的单一版本结论，不会出现
“故障已置位但预算仍旧”之类的撕裂视图。

---

## 6. 本地验证

要求 Go 1.25+（开发环境 Go 1.26 验证通过），无外部依赖。

```bash
# 构建
go build ./...

# 静态检查
go vet ./...

# 全量单测（含竞态检测，结果可复现）
go test ./... -race -count=1 -v

# 只看某个子系统（测试日志会打印输入工况与判定依据）
go test ./internal/budget/ -race -v -run TestPriorityAllocation

# 运行模拟演示（虚拟时钟，确定性场景：巡航 -> 过温 -> 跳变样本被拒 ->
# critical 切断 -> 渐进恢复 -> 预算重新分配）
go run ./cmd/demo
```

测试覆盖要点：

- **并发**：`sensing` 并发摄入/读取、`budget` 12 路 goroutine 混合申请/释放/
  预算变动、`pipeline` 10 路并发 Step，全部在 `-race` 下通过并校验守恒；
- **边界工况**：量程端点、确认/恢复窗口边界、SOC 钳位、预算恰好等于 Want/Min、
  分段曲线端点与中点插值；
- **故障链路**：过温 warning→critical→降级→迟滞恢复、传感器超时、
  `soc_cutoff` 锁存需显式复位、跳变样本被拒后不影响限幅结论；
- **可复现/可区分**：相同输入序列驱动两个独立管线结论必须逐字段一致；
  六类拒绝原因互异；失败信息含具体工况、阈值与判定依据。
