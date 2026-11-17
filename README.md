# 航空货运安检事件数据质量与状态服务平台

> Aviation Cargo Security-Event Data-Quality & Status Publication Platform — Go + PostgreSQL

本平台从**运单申报系统、收货扫描枪、安检设备、图像索引库、人工复核台、集装器台账、航班舱单**等异构来源汇聚关于同一货件的安检事件，按**货件 / 包装 / 集装器 / 航段**发布**经验证的安检数据状态**及其**证据完整度**，并提供从发布版本到原始报文、图谱关系、质量规则与人工裁定的**可复演血缘**。

> **平台边界**：本平台**不执行**物理安检、货物放行、配载或异常处置。业务系统（安检、放行、配载）只**消费**本平台发布的状态数据与证据链。

---

## 1. 为什么需要它（问题陈述）

现有接口只保存"当前状态"，于是：

- 拆包/并包后，**迟到的旧包装扫描**会被错误地算到新子件上；
- 同一设备事件被**补传**，或复核回写，**覆盖**了旧证据；
- 两台设备在相近时间给出**相反结论**，接口只能选一个存下来；
- 舱单引用了"已验证"状态，但**证据索引缺失**，下游却照常放行；
- 审计问"当时为什么发布这个状态"，没人能复现。

本平台的做法是：**原始事件只追加不覆盖**（数据库触发器禁止 `UPDATE/DELETE/TRUNCATE`），身份关系**版本化**，状态由**证据融合**得出且**固定输入版本**，发布分**暂定 / 正式**两阶段，任一发布版本都可**逐位复演**。

---

## 2. 核心能力与领域模型

### 2.1 报文契约注册器
每类 (`source`, `message_type`) 登记一个契约版本，声明：业务键、设备事件号、发生/接收时间、包装修订、证据索引、**字段必填性**与**兼容策略**（`strict` / `backward` / `free`）。缺失必填字段**不拒绝**事件（货运迟到更正是常态），而是降低证据完整度并进入规则检查；类型/枚举错误与 `strict` 未声明字段则拒绝（`422`）。

### 2.2 原始事件账（append-only）
按 `source + 幂等键（或设备事件号）` 追加。去重语义：

| 场景 | 结果 |
| --- | --- |
| 相同幂等键 + 相同内容 | `duplicate_identical`（200，幂等抑制，原始事件保留） |
| 相同幂等键 + 不同内容 | `duplicate_conflicting`（409，原事件保留，新内容隔离） |
| 首次通过契约校验 | `accepted`（201） |

所有接收尝试（含重复/冲突/拒绝）都写入 `intake_attempts`，供重复报文规则使用。

### 2.3 货件身份图谱（版本化）
关系 `shipment↔package`、`package↔container`、`container↔leg` 带**生效区间**与**知识时刻**（事件接收时间）。拆包/并包/换装**只追加新关系版本**，旧关系通过追加**注销行**关闭，从不修改原行。因此：

- 任意时刻可查询"当时有效的包装组合版本"；
- 拆包后迟到的旧包装扫描被判定为 **`superseded_package`（失配证据）**，**绝不转移给任一子件**，但仍保留在账本中可审计。

### 2.4 事件时间归并器
依据**业务阶段**（申报→收货→送检交接→安检→归档→复核→装机→舱单）、**设备事件号**、**包装位置**为每条事件建立**可解释时序**；即使设备时钟漂移，也不会把安检排到装载之后。每条记录都带排序依据、时钟漂移量与失配说明。

### 2.5 状态融合器（四种数据状态）
将申报、设备结论、人工结论、证据完整度融合为：

- `verified`（已验证）
- `pending_evidence`（待补证）
- `conflict_pending`（冲突待裁定）
- `not_publishable`（不可发布）

关键规则：
- **矛盾不代选**：相近时间相互矛盾的设备结论若无交接证据分隔，发布 `conflict_pending`；
- **人工裁定生效**：带 `supersedes_event_id` 的复核事件为更正，被更正事件不参与融合；
- **证据完整度** = 六个维度的加权和（运单申报覆盖、交接记录、设备结论、证据索引+归档、人工复核、身份一致性），默认 `verified` 门槛 **0.85**；
- 结果固定写引用输入版本（事件序号、图谱关系版本、规则键、权重）。

**四种状态的判定语义**（互斥、有优先级）：

| 状态 | 语义 | 典型触发 |
| --- | --- | --- |
| `conflict_pending` | 冲突待裁定 | 相近时间相反设备结论且无交接分隔；不同包装件结论不一致且无人工统一 |
| `not_publishable` | 不可发布 | 无任何可归属证据；阻断规则命中（如证据与被拆并后包装件失配） |
| `pending_evidence` | 待补证 | 可通过**补证**修复的缺口：缺证据索引/归档、缺人工复核、未达完整度阈值、舱单引用的必要证据未到 |
| `verified` | 已验证 | 各包装件结论一致、证据完整度达标、无阻断项与实质待补证项 |

> `pending_evidence`（可补证）与 `not_publishable`（不可救）刻意区分：舱单缺证据属于前者——数据齐了就能确认，但当**下**绝不冒充确认状态。

### 2.6 质量规则引擎（六类）
| 规则码 | 检查 |
| --- | --- |
| `duplicate_retransmission` | 字节相同补传 / 同键不同内容 / 同内容不同键 |
| `cross_package_reference` | 跨包装引用、拆并后旧证据失配 |
| `missing_handover` | 安检/装载前缺少交接记录 |
| `device_clock_drift` | 到达与发生时间偏差超阈值或为负 |
| `status_regression` | 接收序上工作流阶段倒流且无显式更正 |
| `inconsistent_source_encoding` | 同一业务键在不同来源编码不一致（如 `123-45678901` vs `12345678901`） |

### 2.7 快照发布器（暂定 / 正式）
按水位线策略分阶段发布：

- **`wait_complete`**：水位线满员（全部必需来源已关闭）且交接条件满足后才发布；**迟到设备报文会延后整批货件的可用时间**，但下游少处理暂定值。
- **`early_provisional`**：关闭点先发布**暂定快照**，来源补齐后再生成**正式版本**，两者各自固定输入版本并带**显式差异**（`diff`：状态/结论/完整度变化、新增待补证、已解决冲突、新增输入事件…）。

下游可按用途选择：要时效读暂定，要完整读正式；`consumable_confirmed` **仅在正式且 `verified`** 时为 `true`。

### 2.8 审计回放器
从发布版本回溯：原始事件（按序号+内容哈希校验）→ 图谱关系版本 → 六类规则 → 证据对齐 → 状态融合，**重跑同一引擎**并比对**结果哈希**。`deterministic_match=true` 表示该时点可逐位复演。

### 2.9 历史状态一致性复核（本次新增）

单份快照能否算出来是一回事；**多份历史记录之间能否同时成立**是另一回事。复核模块回答后者，且不重跑安检规则、不重建投递系统：

- **输入**：随项目导入的虚构日志——原始事件、单系统事实批次（`facts`，仅提供同源同启动区段的发生序锚点）、快照发布记录、消费系统实际读取日志、来源时钟可信误差范围。
- **两个时钟分开**：`occurred`（来源时钟给出的可信区间）与 `received`（平台实际收到时刻）。没有精确发生时刻时只给区间；**不会**直接把截图时间、发布日志时间、扫描时间排成一条线。
- **序号只在同源同区段内有序**：`(system, stream, boot_segment)` 相同的记录才按 `source_sequence` 建立先后；**重启前后编号不比较**；同一区段内编号复用则该段失去数值次序并显式记为未解决。
- **因果引用**：发布依赖的事实必须先于该次发布（`input_event_refs`）；消费读取只能引用**已经存在**的快照版本；证据索引必须被归档事件确认。
- **可见状态**：在待查时刻，状态只能来自该时刻**已发布版本中的有效末项**，并包含其全部前置事实；成员关系、计数必须来自**同一版本**，不能把一份快照的成员与另一份快照的确认数拼成从未发布过的组合。
- **换装历史归属**沿当时引用判断；当前集装器不能代替旧位置。
- **多货件比较**要求存在**一套共同可行顺序**支持整个组合，不能把各自单独可行的版本拼起来。
- **截止点纪律**：`received`/`published_at` 晚于资料截止点的记录进入 `late_arrivals`，只能开启**新的**复核；分析时还会按目标截止点再裁剪一遍。
- **结论四分**：
  - `contradicted`：有证据证明不相容（正向分支全部不可满足，且资料本身可满足）；
  - `both_explain`：存在相容与不相容两种解释（典型：跨设备时钟误差）；
  - `undetermined`：资料不足或依赖未读原图，尚不能判断；
  - `consistent`：已核实范围内始终相容（**不等于**证明真实世界一致）。
- **冲突集合**返回一组已无法同时满足的原始记录与条件；**不声称最小**，也**不声称**唯一根因。
- **按疑问求解**：把每个“这份截屏的状态当时能否成立”拆成少量约束查询（支持相容的形状 + 支持不相容的形状），只保留必要示例；不导出全部排列。完整穷举只作为**小规模对照校验**（`cross_check=true`）使用。
- **预算**：可行查询次数上限；耗尽时区分“已找到反例 / 已证明不存在 / 搜索尚未完成”，并明确 `no contradiction found is not a proof of consistency`。
- **范围裁剪**：保留目标记录的因果前置与共享发布依赖；未授权货件用**只证明必要关系的边界记录**代替明细，连必要关系都不能披露时标记**无法独立复算**。
- **受限导出**：内部分析与受限材料分别保存引用；字段投影**不改变**相容性结论（`projection_integrity` 会核对两者一致）。
- **可复算**：每次运行固定所读记录的 id 清单与哈希；`/reproduce` 用旧资料重算，缺记录或哈希不符会明确报错，不用新资料悄悄替换。

---

## 3. 技术栈

| 项 | 版本 |
| --- | --- |
| Go | 1.26 |
| PostgreSQL | 18 |
| pgx | v5 |
| 文档 | OpenAPI 3.1 + Swagger UI（内嵌静态资源，离线可用） |

## 4. 目录结构

```
cmd/server/           HTTP 服务入口
cmd/demo/             端到端演示与验证（一键运行）
internal/domain/      纯领域逻辑：事件模型、身份图谱、时序、规则、融合、发布（可单测、可复演）
internal/domain/review/ 历史状态一致性复核：约束求解、按疑问分析、穷举对照（纯逻辑）
internal/store/       PostgreSQL 持久层 + append-only schema（含 review_* 追加表）
internal/service/     编排：摄入、发布、查询、回放
internal/api/         HTTP 路由、OpenAPI、Swagger UI
internal/demo/        样例场景（重复补传 / 拆并包 / 冲突 / 证据缺口 / 更正 / 历史复核）
samples/review/       可导入的虚构复核日志与各用例预期结论
internal/api/static/  Swagger UI 静态资源
docs/                 演示报告等
```

---

## 5. 本地启动

### 5.1 前置：PostgreSQL

容器内已提供 PostgreSQL 18（`127.0.0.1:5432`，库/用户/密码均为 `ccdev`）。服务启动时**自动幂等建表**，无需手动迁移。

```bash
# 确认可连接
PGPASSWORD=ccdev psql -h 127.0.0.1 -U ccdev -d ccdev -c 'select version();'
```

如需自定义连接串，设置 `DATABASE_URL`（默认见下）。

### 5.2 直接运行（Go）

```bash
export DATABASE_URL='postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable'
go build -o bin/server ./cmd/server
./bin/server           # 默认监听 :8080
```

或用 Makefile：

```bash
make run               # 构建并启动，默认 :8080
```

健康检查与文档：

```bash
curl -s localhost:8080/healthz
open http://localhost:8080/docs        # Swagger UI（macOS）
# 或浏览器访问 http://localhost:8080/docs
```

- Swagger UI：`http://localhost:8080/docs`
- OpenAPI 3.1 文档：`http://localhost:8080/openapi.json`

### 5.3 Docker Compose（一键启动）

```bash
docker compose up --build -d      # 启动 db + api，API 在 http://localhost:8080
docker compose logs -f api
docker compose --profile demo run --rm demo   # 在容器内跑完整演示
docker compose down -v            # 停止并清空数据卷
```

> Compose 中 `db` 映射到宿主机 `5433` 以避免与本机 5432 冲突；`api` 暴露 `8080`。

---

## 6. 测试账号 / 凭证

本平台是**后端数据服务**，对外接口无登录鉴权（隔离开发环境）。涉及的外部系统凭证：

| 对象 | 用户 / 库 | 密码 | 地址 |
| --- | --- | --- | --- |
| PostgreSQL（本地/Compose） | `ccdev` / 数据库 `ccdev` | `ccdev` | `127.0.0.1:5432`（Compose 宿主机 `5433`） |

演示数据无需账号：`cmd/demo` 会**重置 schema** 后写入 4 个样例货件（用 `go run ./cmd/demo -reset=false` 可保留既有账本、仅在其上追加演示数据）。

---

## 7. 验证方式（本题要求）

### 7.1 一键端到端演示（推荐）

```bash
export DATABASE_URL='postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable'
make demo
# 等价：go run ./cmd/demo -markdown
```

演示会**实际触发**并断言以下场景（任一失败进程非零退出）：

1. **暂定 → 正式分阶段发布**：干净货件先出暂定快照，来源补齐后出正式快照，带显式 diff；时点查询能复现"当时下游能读到暂定版本"；正式 verified 版本 `consumable_confirmed=true`。
2. **重复补传**：相同补传被幂等抑制（`duplicate_identical`）；同幂等键不同内容返回 409 且原事件保留。
3. **拆包后的旧证据**：拆包后迟到的旧包装扫描被判定 `superseded_package`，**不转移给任一子件**；组合关系产生新版本。
4. **人工更正**：复核事件通过 `supersedes_event_id` 更正设备结论，更正链进入快照与回放。
5. **跨来源冲突**：两台设备相近时间相反结论且无交接 → `conflict_pending`，`consumable_confirmed=false`；即使强制正式发布也不改判；`wait_complete` 模式下未满员则**跳过**发布。
6. **舱单缺证据**：舱单引用必需证据索引但账本缺失 → 只能出质量告警与待补证项，**不得**签发确认状态。
7. **账本不可变**：对 `raw_events` 执行 `UPDATE` 被数据库触发器拒绝。
8. **历史版本可复演**：每个已发布快照回放 `deterministic_match=true`。
9. **历史状态一致性复核**（第 5 节，见 7.1b）：跨设备时钟误差、组合不可能、读取早于发布、重启复号、迟到日志、范围外前置、多货件比较、换装归属、快照拼接、原图依赖、预算不足、权限收窄与旧案复算，全部实际计算并断言。

演示输出（节选，完整见 `docs/demo-report.md`）：

```
| publish split shipment v1 | ✅ | status=pending_evidence completeness=0.833 consumable=false |
| publish split shipment v2 (after re-declaration) | ✅ | status=verified completeness=0.983 diff=v1->v2, status pending_evidence -> verified, 1 new inputs |
## 3. 跨来源冲突 AWB65432100003：冲突待裁定而非代选
| publish conflict shipment | ✅ | status=conflict_pending conflicts=1 consumable=false |
...
| clock-error case keeps both readings open | ✅ | shipment claim verdict=both_explain |
| combined screenshots are impossible at one instant | ✅ | combination verdict=contradicted |
| targeted solver agrees with exhaustive enumeration | ✅ | 2 small-scale exhaustive cross-check(s), all agree |
...
**51/51 checks passed**
```

### 7.1b 复核专项演示（本次新增）

`make demo` 的第 5 节实际计算以下结论（任一失败即非零退出）：

| 场景 | 计算结论 |
| --- | --- |
| 跨设备时钟误差 | 两张截屏各自 `both_explain`；穷举对照一致 |
| 两张截屏各自成立、组合不可能 | 各自 `both_explain`，`combination=contradicted` |
| 读取引用了尚未发布的版本 | `contradicted`，冲突集合引用原始记录 |
| 重启后序号复用 | 跨 boot 段无序号约束，仅同段 1→2 有序 |
| 迟到日志 | 旧范围 `late_arrivals=1` 不折入；新范围可接收 |
| 范围外共享前置 | 记录被范围排除，但发布依赖保留为 `unresolved` |
| 多货件比较 | 同一时刻均 `verified` → `combination=consistent` |
| 换装历史归属 | 旧时刻声称新集装器 → `contradicted` |
| 快照拼接 | 成员来自一份、计数来自另一份 → `contradicted` |
| 依赖原图 | `undetermined`，已有时间约束保留 |
| 预算不足 | `budget_exhausted=true`、`undetermined`、明确未证明一致 |
| 权限收窄 + 旧案复算 | 投影后结论不变；旧运行按原资料复现 |

也可单独导入 `samples/review/` 下的虚构日志：

```bash
B=http://localhost:8080
SCOPE=$(curl -s -X POST $B/v1/reviews/scopes -H 'content-type: application/json' \
  -d @samples/review/02-combined-claims-scope.json | jq -r .scope_id)
CASE=$(curl -s -X POST $B/v1/reviews/cases -H 'content-type: application/json' \
  -d "{\"scope_id\":\"$SCOPE\"}" | jq -r .case_id)
curl -s -X POST $B/v1/reviews/scopes/$SCOPE/imports -H 'content-type: application/json' \
  -d @samples/review/02-combined-claims-logs.json | jq '{imported:.imported_count,late:.late_arrivals}'
curl -s -X POST $B/v1/reviews/cases/$CASE/runs -H 'content-type: application/json' \
  -d @samples/review/02-combined-claims-run.json | jq '.run.analysis.summary'
```

`internal/api/review_samples_test.go` 会把这些文件真正导入并断言上表结论，保证样例不是“摆设”。

### 7.2 自动化测试

```bash
go test ./...                  # 单元 + 集成（无数据库时集成自动跳过）
go test ./internal/domain/ -v  # 纯领域逻辑（无需数据库）
go test ./internal/domain/review/ -v   # 复核纯逻辑（无需数据库）
make review-test               # 复核领域 + 复核 HTTP 全流程
```

- `internal/domain/domain_test.go`：键规范化、契约校验、去重键、阶段排序、拆包版本化、迟到证据判定、冲突/交接/复核/更正融合、舱单证据缺口。
- `internal/domain/review/`：约束求解（Bellman-Ford 差分约束）、双时钟区间、同源同段序号、重启复号、换装历史归属、快照拼接、多货件共同顺序、预算耗尽、权限收窄、旧案复算，以及**定向求解 vs 穷举**的小规模一致性对照。
- `internal/api/review_test.go` / `review_samples_test.go`：复核 HTTP 全流程（范围登记→导入→按疑问求解→示例→复算→受限导出），以及真实导入 `samples/review/` 的文件并校验结论。
- `internal/api/api_test.go`：OpenAPI 与 Swagger UI 可达、摄入幂等（201/200/409/422）、水位线发布生命周期、**append-only 触发器**（真实数据库）、回放匹配。集成测试默认连接 `TEST_DATABASE_URL`（缺省为本地 `ccdev`），不可达则跳过。

### 7.3 手动验证（HTTP）

```bash
B=http://localhost:8080

# 1) 注册契约
curl -s -X POST $B/v1/contracts -H 'content-type: application/json' -d '{
  "source":"screening","message_type":"screening","compat":"strict",
  "definition":{"policy":"strict","fields":[
    {"name":"result","spec":{"kind":"enum","required":true,"enum":["CLEAR","REJECT","HOLD","INCONCLUSIVE"]}}]}}'

# 2) 追加理论事件（再次提交相同内容应返回 200 duplicate_identical）
curl -s -X POST $B/v1/sources/declaration/events -H 'content-type: application/json' -d '{
  "message_type":"declaration","idempotency_key":"demo-1",
  "occurred_at":"2026-11-17T02:00:00Z",
  "business_keys":{"awb":"123-45678901"},
  "payload":{"packages":["PKG-X"]}}'

# 3) 声明水位线、关闭来源、发布
WM=$(curl -s -X POST $B/v1/watermarks -H 'content-type: application/json' -d '{
  "scope_type":"shipment","scope_key":"AWB12345678901",
  "cutoff_received_at":"2026-11-17T03:00:00Z",
  "required_sources":["declaration"],"require_handover":false,"mode":"wait_complete"}' | jq -r .id)
curl -s -X POST $B/v1/watermarks/$WM/close -H 'content-type: application/json' -d '{"source":"declaration"}'
SNAP=$(curl -s -X POST $B/v1/publications -H 'content-type: application/json' \
  -d "{\"scope_type\":\"shipment\",\"scope_key\":\"AWB12345678901\",\"watermark_id\":\"$WM\"}" | jq -r .snapshot.id)

# 4) 读取快照与回放
curl -s $B/v1/snapshots/$SNAP | jq '.status, .snapshot_kind, .consumable_confirmed'
curl -s $B/v1/snapshots/$SNAP/replay | jq '.deterministic_match, .summary'
```

### 7.4 复核接口一览（本次新增）

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| POST | `/v1/reviews/scopes` | 登记范围（货件、允许来源、可披露对象、截止点） |
| GET | `/v1/reviews/scopes` · `/v1/reviews/scopes/{id}` | 列出 / 读取范围 |
| GET | `/v1/reviews/scopes/{id}/material` | 范围已导入资料摘要与哈希 |
| POST | `/v1/reviews/scopes/{id}/imports` | 导入日志（事件 / facts / 发布 / 读取） |
| POST | `/v1/reviews/cases` | 建案 |
| POST | `/v1/reviews/cases/{id}/constraints` | **约束检查**（未提疑问）：基础资料本身能否同时满足 |
| POST | `/v1/reviews/cases/{id}/runs` | **按疑问求解**并固定资料、结论、约束与示例 |
| GET | `/v1/reviews/cases/{id}/runs` · `/v1/reviews/runs/{id}` | 列出 / 读取运行 |
| GET | `/v1/reviews/runs/{id}/examples` | 顺序示例（相容与不相容各至少一条） |
| POST | `/v1/reviews/runs/{id}/reproduce` | 用旧资料复算旧复核 |
| POST | `/v1/reviews/runs/{id}/export` | 受限材料导出（字段投影 + 边界记录） |

全部接口均有 OpenAPI 3.1 描述，可在 `http://localhost:8080/docs` 交互试用。

---

## 8. 关键设计决策

- **只用 PostgreSQL**：append-only 账本、图谱、水位线、规则、评估、快照同库；触发器强制不可变，无需额外消息中间件即可复演。
- **双时间语义**：每个图谱关系同时携带**生效时间**（业务时间）与**知识时间**（接收时间），使"某时点下游读到什么"可精确复现。
- **确定性结果哈希**：融合结果 + 固定输入（事件序号/内容哈希、图谱版本、规则键）→ SHA-256；回放比对哈希，引擎漂移或账本被改都可发现。
- **失配证据不转移**：拆包/并包只产生新关系版本；旧包装的迟到证据被显式标记 `superseded_package` 并保留审计，绝不静默归属新子件。
- **确认状态的门槛**：`consumable_confirmed` 当且仅当"正式快照 + verified"；冲突、待补证、不可发布、暂定一律不被下游当作最终结果。
- **历史复核按疑问求解而非全排列**：跨系统交错多时，全排列的等待与中间结果迅速膨胀；复核把“这份状态当时能否成立”拆成支持/反对相容的少量约束查询，只留必要示例。代价是追加问题需再次等待，但它保留约束与求值证据，并能在预算不足时明确说“尚未完成”。
- **两个时钟 + 同源同段序号**：发生时间与接收时间分开建约束；序号只在同源同启动区段内有序。这一条直接排除了“把重启前后编号直接比较”和“按墙钟硬排跨系统顺序”两类误判。
- **有效末项与同版本约束**：待查时刻的可见状态只来自该时刻已发布版本中的有效末项，成员/计数必须同源同版本；换装历史归属沿当时引用。这让“把两份快照拼成从未发布过的组合”变成可证明的矛盾，而不是靠人工眼力。
- **矛盾集合不宣称最小**：求解冲突返回一组足够证明不相容的原始记录与条件，并明确说明它未必最小、也不代表唯一根因。

---

## 9. 已知边界

- 不含登录鉴权 / 多租户，按隔离开发环境设计；
- 冲突窗口与时钟漂移阈值可经环境默认值调整（见 `service.DefaultConfig`），暂未开放为 API 配置端点；
- 发布为**显式触发**（调用 `/v1/publications`），未内置定时调度；生产可对接调度器。
- 航段键形如 `CA1234/2026-11-17/PEK-SHA` 含 `/`，作为路径参数时必须做 URL 编码（`%2F`），例如
  `GET /v1/graph/legs/CA1234%2F2026-11-17%2FPEK-SHA`。
- 复核求解是**定向**的：默认不导出全部排列，只返回回答疑问所需的最少示例；`cross_check=true` 仅在版本数很小（默认 ≤720 排列）时穷举对照，超过上限不宣称一致。
- 复核不读取截图图像本体；依赖原图才能确认的内容标记 `requires_image` 并保持 `undetermined`，但不因此推翻已有时间关系。
- 设备时钟区间需由导入方按其同步协议给出（`basis` 说明来源）；复核不会自行“猜”漂移量。
- 诊断类读接口（`timeline` / `quality` / `assessment` / `graph`）默认以账本中**最新接收时间**为"当前"（而非服务器墙钟），以适配来源时钟可能超前于服务器的现场情况；需要精确时点请显式传 `cutoff_received_at`。
