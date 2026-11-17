# 可导入的虚构复核日志

每个子目录（或 `NN-*.json` 三元组）是一组可直接通过 HTTP 导入的虚构日志：

| 文件 | 用途 |
| --- | --- |
| `*-scope.json` | 复核范围登记：目标货件、允许来源、资料截止点 |
| `*-logs.json` | 原始事件 / 单系统事实批次 / 快照发布记录 / 消费读取日志 |
| `*-run.json` | 定向求解请求：固定目标、待查时刻、资料截止点、并列论断 |

## 一键导入（任一用例）

```bash
B=http://localhost:8080
SCOPE=$(curl -s -X POST $B/v1/reviews/scopes -H 'content-type: application/json' \
  -d @samples/review/01-clock-skew-scope.json | jq -r .scope_id)
CASE=$(curl -s -X POST $B/v1/reviews/cases -H 'content-type: application/json' \
  -d "{\"scope_id\":\"$SCOPE\",\"title\":\"clock skew\"}" | jq -r .case_id)
curl -s -X POST $B/v1/reviews/scopes/$SCOPE/imports -H 'content-type: application/json' \
  -d @samples/review/01-clock-skew-logs.json | jq '{imported:.imported_count,late:.late_arrivals}'
curl -s -X POST $B/v1/reviews/cases/$CASE/runs -H 'content-type: application/json' \
  -d @"samples/review/01-clock-skew-run.json" | jq '.run.analysis.summary'
```

## 各用例预期结论

| 用例 | 预期 |
| --- | --- |
| `01-clock-skew` | 两张截屏各自 `both_explain`（时钟误差导致两种解释），组合仍相容；穷举对照一致 |
| `02-combined-claims` | 两张截屏各自 `both_explain`，`combination` 为 `contradicted`（同一时刻不能解析到两个不同版本） |
| `03-read-before-publish` | 读取记录 `contradicted`，冲突集合引用原始记录 |
| `04-restart` | boot-1 序号不与 boot-2 比较；仅 boot-2 内 1→2 有效 |
| `05-late-log` | 读取 `late_arrivals=1`，旧范围 `imported_count=1`；新范围可接收 |
| `06-shared-prereq` | `packaging_hub` 被范围排除，但发布对它的依赖作为未解决项保留 |
| `07-budget` | `budget_exhausted=true`、结论 `undetermined`、`no contradiction ... is not a proof` |
| `08-multi-shipment` | 两货件在同一时刻均 `verified`，`combination=consistent`（一套共同可行顺序） |
| `09-transload` | 在换装前的时刻声称在新集装器 → `contradicted`；历史归属沿当时引用 |
| `10-snapshot-stitching` | 计数来自 v1、成员来自 v2 的组合 → `contradicted`（从未发布过的组合） |
| `11-image-dependent` | `requires_image` 的内容 → `undetermined`，已有时间约束保留 |
