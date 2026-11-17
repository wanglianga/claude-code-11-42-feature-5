# 演示报告

> 由 `make demo` 在本地 PostgreSQL 18 上实际执行生成（本次含历史状态一致性复核）；51/51 断言通过。

## 0. 报文契约注册

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| declaration/declaration | ✅ | registered v1 compat=strict fields=3 |
| scanner/scan | ✅ | registered v1 compat=backward fields=3 |
| screening/screening | ✅ | registered v1 compat=strict fields=4 |
| imaging/evidence | ✅ | registered v1 compat=backward fields=2 |
| review/manual_review | ✅ | registered v1 compat=strict fields=3 |
| stowage/container_stow | ✅ | registered v1 compat=backward fields=2 |
| manifest/flight_manifest | ✅ | registered v1 compat=backward fields=4 |
| custody/handover | ✅ | registered v1 compat=backward fields=3 |
| custody/package_change | ✅ | registered v1 compat=strict fields=4 |

## 1. 干净货件 AWB12345678901：暂定→正式分阶段发布

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| declaration | ✅ | 1 package declared for AWB 12345678901 |
| publish provisional @02:30 | ✅ | kind=provisional status=verified completeness=1.000 consumable=false |
| publish formal @03:00 | ✅ | kind=formal status=verified completeness=1.000 consumable=true diff=v1->v2, 3 new inputs |
| as-of publication 02:35 read | ✅ | v1 provisional status=verified |
| publish container AKE12345MU | ✅ | status=verified derived_from=1 shipment(s) |
| publish leg CA1234/2026-11-17/PEK-SHA | ✅ | status=verified derived_from=1 shipment(s) |
| replay 978a2417 | ✅ | published=verified recomputed=verified match=true |
| replay 3962a0ca | ✅ | published=verified recomputed=verified match=true |

## 2. 重复补传 + 拆包旧证据 AWB12345678902

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| identical retransmission | ✅ | outcome=duplicate_identical duplicate_of=da1b2305-9ed5-4a3f-b082-feaa9ea229c1 |
| conflicting retransmission | ✅ | outcome=duplicate_conflicting error=true (original da1b2305-9ed5-4a3f-b082-feaa9ea229c1 kept) |
| review correction | ✅ | review supersedes device verdict da1b2305 |
| split at 04:25 | ✅ | PKG-DUP-1 -> PKG-SPLIT-1A + PKG-SPLIT-1B (new composition version, not a mutation) |
| late scan citing retired package | ✅ | arrived 6 minutes after the split; cites PKG-DUP-1 which no longer exists as a version |
| children re-screened | ✅ | both split children carry their own screening verdict + evidence archive |
| publish split shipment v1 | ✅ | status=pending_evidence completeness=0.833 consumable=false |
| publish split shipment v2 (after re-declaration) | ✅ | status=verified completeness=0.983 diff=v1->v2, status pending_evidence -> verified, 1 new inputs |
| quality after split | ✅ | 1 cross-package/misaligned findings |
| stale scan disposition | ✅ | disposition=superseded_package; event arrived after the package was retired by a split/merge; evidence is misaligned and is not attributed to any child |
| composition versions | ✅ | 2 versions: [v1(PKG-DUP-1) v2(G-SPLIT-1A+G-SPLIT-1B)] |
| replay split v1 | ✅ | published=pending_evidence recomputed=pending_evidence |
| replay split v2 | ✅ | published=verified recomputed=verified |

## 3. 跨来源冲突 AWB65432100003：冲突待裁定而非代选

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| publish conflict shipment | ✅ | status=conflict_pending conflicts=1 consumable=false |
| wait_complete with open sources | ✅ | skipped=true reason=wait_complete: watermark not closed (missing sources [declaration manifest scanner screening]); no snapshot published |
| forced formal publish | ✅ | kind=formal status=conflict_pending consumable=false |

## 4. 舱单缺证据 AWB98765400004：不得签发确认状态，只输出质量告警与待补证

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| publish manifest-gap shipment | ✅ | status=pending_evidence consumable=false pending=[departure_handover_missing manifest_required_evidence_missing] |

## 5. 历史状态一致性复核：货件↔集装器↔航段的多份记录

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| scope scope-clock-skew | ✅ | shipment=AWB70000000001 systems=[platform reading declaration] cutoff=2026-11-17T22:00:00Z |
| case case-97bcc8dd | ✅ | 截图A：货件待补证 vs 集装器完整确认 |
| import scope-clock-skew | ✅ | imported=7 excluded=0 late=0 |
| run case-97bcc8dd | ✅ | verdict=both_explain assertions=8 constraints=38 checks=13 budget_exhausted=false |
| publish clock-skew case v1 | ✅ | verdict=both_explain assertions=8 examples=6 material=7 |
| scope scope-multi | ✅ | shipment=AWB70000000008 extra=[{shipment AWB70000000009}] systems=[platform declaration] |
| case case-43e23077 | ✅ | 同时比较多个货件：存在一套共同可行顺序 |
| import scope-multi | ✅ | imported=4 excluded=0 late=0 |
| run case-43e23077 | ✅ | verdict=consistent assertions=7 constraints=22 checks=10 budget_exhausted=false |
| scope scope-combo | ✅ | shipment=AWB70000000002 systems=[platform reading declaration] cutoff=2026-11-17T22:00:00Z |
| case case-d8b665c1 | ✅ | 截图B：两张截屏各自成立但组合不可能 |
| import scope-combo | ✅ | imported=3 excluded=0 late=0 |
| run case-d8b665c1 | ✅ | verdict=contradicted assertions=5 constraints=16 checks=10 budget_exhausted=false |
| scope scope-read-early | ✅ | shipment=AWB70000000003 systems=[platform reading] cutoff=2026-11-17T22:00:00Z |
| case case-b9696c92 | ✅ | 读取引用了尚未发布的版本 |
| import scope-read-early | ✅ | imported=2 excluded=0 late=0 |
| run case-b9696c92 | ✅ | verdict=contradicted assertions=2 constraints=12 checks=2 budget_exhausted=false |
| scope scope-restart | ✅ | shipment=AWB70000000004 systems=[scanner] cutoff=2026-11-17T22:00:00Z |
| case case-cc6bc581 | ✅ | 重启后序号复用 |
| import scope-restart | ✅ | imported=4 excluded=0 late=0 |
| run case-cc6bc581 | ✅ | verdict=undetermined assertions=1 constraints=13 checks=1 budget_exhausted=false |
| scope scope-late-old | ✅ | shipment=AWB70000000005 systems=[platform reading] cutoff=2026-11-17T22:00:00Z |
| case case-eecc1d8d | ✅ | 迟到日志与旧案卷 |
| import scope-late-old | ✅ | imported=1 excluded=0 late=1 |
| run case-eecc1d8d | ✅ | verdict=undetermined assertions=1 constraints=4 checks=1 budget_exhausted=false |
| scope scope-late-new | ✅ | shipment=AWB70000000005 systems=[platform reading] cutoff=2026-11-18T01:00:00Z |
| case case-fc146610 | ✅ | 迟到日志进入新复核 |
| import scope-late-new | ✅ | imported=2 excluded=0 late=0 |
| scope scope-shared | ✅ | shipment=AWB70000000006 systems=[platform declaration] cutoff=2026-11-17T22:00:00Z |
| case case-88fcf49d | ✅ | 范围外共享前置被保留 |
| import scope-shared | ✅ | imported=2 excluded=1 late=0 |
| run case-88fcf49d | ✅ | verdict=undetermined assertions=2 constraints=10 checks=3 budget_exhausted=false |
| scope scope-transload | ✅ | shipment=AWB70000000006B systems=[declaration platform] cutoff=2026-11-17T22:00:00Z |
| case case-6bc0c02c | ✅ | 换装：历史归属沿当时引用判断 |
| import scope-transload | ✅ | imported=3 excluded=0 late=0 |
| run case-6bc0c02c | ✅ | verdict=contradicted assertions=5 constraints=17 checks=12 budget_exhausted=false |
| scope scope-stitch | ✅ | shipment=AWB70000000006C systems=[platform] cutoff=2026-11-17T22:00:00Z |
| case case-8dba06ea | ✅ | 成员来自一份快照、计数来自另一份 |
| import scope-stitch | ✅ | imported=2 excluded=0 late=0 |
| run case-8dba06ea | ✅ | verdict=contradicted assertions=5 constraints=14 checks=10 budget_exhausted=false |
| scope scope-image | ✅ | shipment=AWB70000000006D systems=[platform] cutoff=2026-11-17T22:00:00Z |
| case case-1fcc5129 | ✅ | 依赖原图才能确认的内容 |
| import scope-image | ✅ | imported=1 excluded=0 late=0 |
| run case-1fcc5129 | ✅ | verdict=undetermined assertions=3 constraints=4 checks=1 budget_exhausted=false |
| scope scope-budget | ✅ | shipment=AWB70000000007 systems=[platform] cutoff=2026-11-17T22:00:00Z |
| case case-6fcdfeba | ✅ | 预算不足 |
| import scope-budget | ✅ | imported=8 excluded=0 late=0 |
| run case-6fcdfeba | ✅ | verdict=undetermined assertions=3 constraints=34 checks=2 budget_exhausted=true |
| run case-97bcc8dd | ✅ | verdict=both_explain assertions=5 constraints=36 checks=9 budget_exhausted=false |

## 6. 收尾校验：账本不可变 + 所有版本可复演 + 顶层约束

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| append-only guard | ✅ | UPDATE raw_events rejected by trigger: ERROR: table public.raw_events is append-only: UPDATE is not permitted (SQLSTATE P0001) |
| replay all snapshots | ✅ | 9/9 snapshots replay deterministically |
| status coverage | ✅ | map[conflict_pending:2 pending_evidence:2 verified:5] |

## 断言检查

| 检查 | 结果 | 依据 |
| --- | --- | --- |
| provisional published before sources closed | ✅ | early_provisional mode must publish at the cutoff even if sources are open |
| formal version follows provisional with explicit diff | ✅ | the second publication must be a new version superseding the provisional one |
| verified clean shipment is consumable-confirmed | ✅ | a formal verified snapshot is the only shape downstream may treat as confirmed |
| point-in-time query returns the then-readable provisional version | ✅ | as-of must reproduce what downstream could have read at that time |
| leg snapshot aggregates shipments across the leg | ✅ | the leg-level publication folds every shipment whose container flies that leg |
| clean shipment version replays deterministically | ✅ | every published version must be reproducible from its pinned inputs |
| clean shipment version replays deterministically | ✅ | every published version must be reproducible from its pinned inputs |
| identical retransmission suppressed as duplicate | ✅ | same idempotency content must not create a second event |
| conflicting retransmission quarantined, original retained | ✅ | a re-used idempotency key with new content must not overwrite |
| correction references the event it replaces | ✅ | supersedes relation is what makes the ruling auditable |
| split shipment does not silently inherit the retired parent's verdict | ✅ | the children were never covered by the original declaration, so v1 cannot be verified |
| re-declaration converges the split shipment in a new version | ✅ | post-split re-declaration is what makes the successor packages contributable |
| late old-package scan is misaligned, not auto-transferred | ✅ | a package retired by a split must not have its evidence silently attached to a child |
| stale scan appears in no child's evidence set | ✅ | the misaligned event must be retained for audit but absent from every successor's verdict inputs |
| split produced a new composition version | ✅ | the parent membership must be retired and the two children added as a new version |
| split shipment publication replays deterministically | ✅ | history including the split, the correction and the re-declaration must reproduce |
| split shipment publication replays deterministically | ✅ | history including the split, the correction and the re-declaration must reproduce |
| contradictory device results publish as conflict_pending | ✅ | without handover evidence the platform must not pick a verdict |
| conflict snapshot is not consumable-confirmed | ✅ | foreign systems must not treat a conflict as a confirmed status |
| wait_complete postpones publication until sources close | ✅ | the wait strategy deliberately holds the batch so no provisional value reaches downstream |
| even a forced formal snapshot keeps conflict_pending and is not consumable | ✅ | forcing the gate must never turn an unresolved conflict into a confirmed status |
| conflict snapshot replays deterministically | ✅ | the conflict set and decision must be reproducible |
| manifest citing missing required evidence cannot yield a confirmed status | ✅ | the snapshot must publish only quality warnings and pending-evidence items |
| manifest evidence gap is surfaced as a pending/blocking item | ✅ | a missing required evidence index must be visible to the operator |
| manifest-gap snapshot replays deterministically | ✅ | the refusal to confirm must be reproducible from history |
| clock-error case keeps both readings open | ✅ | shipment claim verdict=both_explain (clock interval straddles the published transition) |
| clock-error case keeps both order examples | ✅ | one example shows the pending reading holding, another shows it not holding |
| targeted solver agrees with exhaustive enumeration | ✅ | 2 small-scale exhaustive cross-check(s), all agree |
| a multi-shipment comparison needs one common order, and finds it | ✅ | both shipments verified at the same instant in one shared assignment |
| each screenshot is individually explicable | ✅ | both single claims are both_explain against the uncertain instant |
| combined screenshots are impossible at one instant | ✅ | combination verdict=contradicted with 3 cited record(s) |
| combination conflict is not presented as a minimal root cause | ✅ | conflict set explicitly states it is sufficient but not claimed minimal |
| read citing an unpublished snapshot is a proven contradiction | ✅ | the read would have to precede the version it cites; no order allows it |
| read contradiction cites the original records | ✅ | 2 records named in the conflict set |
| reused sequence after restart does not order records | ✅ | boot-2 numbers 1->2 are ordered; boot-1 number 5 is not compared with them |
| log received after the cutoff is a late arrival, not folded into the old case | ✅ | imported=1 late=1 (publication on time, read late) |
| old case does not see the late read | ✅ | the read is absent from the old cutoff's assertions; a new scope is required |
| a new review opened later accepts the late log | ✅ | imported=2 late=0 in the new scope |
| out-of-scope shared prerequisite is not deleted | ✅ | hub record is excluded from scope, but the publication's dependency on it is reported as unresolved |
| scope boundary does not silently clean up the publication | ✅ | no claim is fabricated from material the scope cannot see |
| historical attribution follows the transload reference, not the current container | ✅ | claiming the later container at 08:10 is contradicted; at that instant the package was in AKE-TL-A |
| membership from one snapshot and counts from another are not stitched | ✅ | no single published version has count=3 with membership {PKG-ST-1,PKG-ST-2} |
| image-dependent content is not claimed verified from metadata | ✅ | claim stays undetermined; existing time relations are left intact |
| budget exhaustion is undetermined, never a contradiction | ✅ | verdict=undetermined exhausted=true checks_used=2 |
| budget exhaustion states that no contradiction is not proof | ✅ | no contradiction found is not a proof of consistency |
| constraint check reports the base material as satisfiable before any claim | ✅ | constraints=32 checks=1 feasible=true |
| field projection does not change the compatibility verdict | ✅ | internal=both_explain exported=both_explain integrity=verdict_recomputed_from_same_material |
| old review reproduces from the old material | ✅ | verdict=both_explain material_hash=57784e51ecea |
| narrowing permission keeps a boundary relation instead of deleting it | ✅ | container publication is exported as a boundary record with its necessary relation |
| ledger rejects overwrite | ✅ | UPDATE on raw_events is blocked by the DB |
| all published versions replay deterministically | ✅ | each downstream state must be reproducible by historical version |

**51/51 checks passed**
