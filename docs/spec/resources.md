---
type: spec
status: Accepted
updated: 2026-11-10
---

# 资料分类、发布与交付

## 分类与资料

分类配置持久化保存，名称支持八种界面语言。管理员可以新增、改名、排序或停用分类，并配置字段、选项、必填、范围、筛选和分组。全站分类管理需要独立权限，学校维护权限不能隐含修改全站分类。

分类字段支持文本、整数和选项。每类最多二十字段，选项最多一百项，分类最多一百项。可选年份缺失时保持未知。未声明字段、错误类型、越界整数和旧配置修订会被拒绝。

资料包含标题、简介、标签、来源、课程关联和文件版本。提交时保存分类快照。停用分类保留旧资料展示，新提交改用有效分类。待审或准备发布的更正会生成新版本，迟到审核不能覆盖更新后的版本。

原创资料记录接受的条款版本与摘要，转载资料记录已知来源或明确的未知状态。转载不能提交原创授权。课程、学期、开课和教师通过 catalog 公开接口校验，不复制学术数据表。

## 发布与访问

上传和更正依当前动作资格执行。资料提交后进入审核，审核通过后由持久化任务请求媒体发布。新版本文件准备完成前，原发布版本保持可用。切换成功后只向普通用户提供当前版本，并异步释放旧媒体引用。

公开与受限是访问条件。改变文件保护要求时需要匹配用途的新文件。后台执行发布前再次检查当前版本、分类修订和管理范围，任务租约过期的执行者不能确认结果。

下载分别检查当前身份资格和访问权。管理员限于授权范围；单独授权、购买和会员各自生效，某项失效不会撤销其他权益。普通下架仅保留当前合格已购用户的交付，安全停用停止全部下载。

下架、投诉和异议先保存申请与待发送任务，提交成功只表示申请已登记。投诉正文、联系方式与证据不包含在公开资料中。申请固定到提交时的资料版本，申请人可查看审核任务和公开决定理由。投递失败后可沿用原键重试，复核需指定已有决定。账务补发、替代交付或退款由对应业务接口承接。

资料内容、目录和分类读取要求当前内容准入。分类管理员可按独立管理权限读取分类，普通登录不能读取分类或其他公共内容。本人资料、申请与管理接口继续按各自归属和授权检查。

## 目录与下载包

目录归属学校，可以进一步关联课程。管理员调整父目录时检查循环、学校与课程范围和子树深度。层级最多十六，单次遍历最多一千目录，每个目录最多一百子目录；超限返回明确错误。

资料包按当前权限选择文件，忽略无权访问的资料，不在清单中列出缺项。按文件数与字节上限拆成多个包，保存文件名、大小与实际包清单。清单读取和每包下载再次检查权限与当前版本，变更后要求重新生成。

单包最多一百文件、四 GiB，单文件最多一 GiB，一次最多一百包。实际配置可以更低。包清单一至六十分钟过期，过期快照可分批清理。资料模块不缓存文件字节。

## 持久化与运行

资料、分类修订、授权、目录、申请、任务、幂等结果、审计与 outbox 由 resources 独立拥有。运行身份不能读取其他模块数据，也不能修改审计、分类历史或幂等结果。迁移身份与运行身份分离。

业务写入、审计和待发送事件在同一事务提交。同一幂等键重试返回原结果，内容不同则拒绝。任务使用稳定外部键恢复响应丢失，限定重试次数并保留失败状态，管理员可恢复同一发布任务。

数据库业务记录最多一百万条、幂等记录最多一千万条、活动任务最多十万条。达到配置上限后拒绝新增操作，不扩大配额。业务与审计记录保留策略由运营确认，当前只自动清理过期资料包快照。

## 验收场景

| 场景 | 预期结果 |
| --- | --- |
| RES-01 | 分类改名保留快照，年份按配置筛选，拒绝未定义字段与跨范围管理 |
| RES-02 | 待审时旧版本可用，更新后迟到审核被拒绝，切换后释放旧引用 |
| RES-03 | 独立权益、资格撤回、普通下架及安全停用分别生效 |
| RES-04 | 并发重试只创建一次资料和审计，不同内容的同键请求被拒绝 |
| RES-05 | 被拒绝的上传不查询学术对象，也不调用媒体 |
| RES-06 | 目录拒绝循环，下载包只含授权资料，下载前重新检查权限 |
| RES-07 | 来源与原创授权按条件保存，下架申请通过持久化任务交付 |
| RES-08 | 远端成功但响应丢失时，恢复同一发布，不重复外部效果 |
| RES-09 | 独立运行身份不能越界或修改审计，outbox 失败回滚业务写入 |
| RES-10 | HTTP 按契约拒绝缺失与额外字段，成功写入遵循契约状态，生成 SDK 可以读取资料 |

审核员通过当前学校范围权限查看待审版本及原文件，读取前重新检查权限，普通用户不能使用审核入口。本人申请使用分页回执列表，详情读取 moderation 的当前处理结果，证据不复制到公开资料。

| 场景 | 预期结果 |
| --- | --- |
| RES-11 | 审核详情与文件只对当前授权审核员开放；单独授权列表包含对应用户身份键，仅管理权限可读 |

| RES-12 | 分类读取要求内容准入，资格撤销或依赖故障时拒绝；分类管理员保留维护能力 |

本人上传资料的目录关联名称按登录与归属查询，每批最多 100 个 ID。申请回执不授予目录关联查询权。

投诉只有提交时通过内容准入且目标公开可见才保存标题。回执保留当时标题，后续资格、资料版本及同键重试不改变标题；历史回执没有标题依据时不补发。申请回执不包含正文、文件和预览。

| 场景 | 预期结果 |
| --- | --- |
| RES-13 | 资格失效后本人上传关联名称仍可读，其他账号被拒绝；投诉标题仅按提交时准入保存，重试和后续资格变化不补发 |

预览端点只交付已获下载权限的原文件，内联响应设置沙箱策略。预览、下载和媒体引用都重新检查当前准入、资格、权益与资料状态。资料详情返回 available_methods 和 unlock_reason，获准购买时提供 purchase 方式。没有访问权申请入口。

| 场景 | 预期结果 |
| --- | --- |
| RES-14 | 未授权预览与下载均不读取文件；错误明确缺失或不可用条件，本人元数据在资格故障时仍可读 |

账号注销保留原作者、来源、许可证、正文和已发布文件，仅移除上传账号的公开关联。受信模块端口登记永久账号标记，后续新上传被拒绝。当前公开 DTO 不含上传者身份，内部归属记录保留供审核。

| 场景 | 预期结果 |
| --- | --- |
| RES-15 | 注销后版权元数据与文件保留，无上传账号公开关联，重复处理无额外效果且拒绝后续新上传 |

| RES-16 | 同一列表请求按学校和动作只查询一次当前资格与权限，下一次请求重新读取事实，资料逐项保留当前状态与权益判断 |

| RES-17 | 投诉按账号限制每分钟 10 次新提交，同键重试不重复占用，超限返回 429 且不写入，其他账号与下一窗口不受影响 |

## 公开审计查询

AUDIT-01：postgres.Repository.ReadAudit(ctx, auditquery.Request) 返回 auditquery.Batch。backend 适配器负责认证，将当前授权与查询范围求交，再传入 Scopes。Repository 按范围并集与全部条件过滤后分页。

时间为左闭右开区间，每次最多 101 条，按时间、模块、ID 字节序降序。Before 使用全局位置。Snapshot 固定 PostgreSQL 已提交成员，排除回填和延迟提交；删除或修改审计使旧快照失效，调用方须重新查询。

摘要只含模块、内部对象和操作者标识、时间、操作、原因、结果与范围。学校或群不明时使用 *，仅对应全局权限可见。没有原因时为空，超出 512 字节或含控制字符的原因省略；无法证明结果时为 unknown。

表结构直接定义最终形态，不转换历史数据。审计与业务同事务写入。运行身份需要读取 audit_retention，清理修订由固定 schema 的触发器更新。快照超过 256 字节或无效时明确报错。

postgres/audit_query_test.go 的 TestPublicAuditQuery 使用独立 PostgreSQL 数据库，验证条件筛选、范围、空原因、unknown、脱敏、回填与延迟提交、清理失效和取消。OpenAPI 路径保持现状，本接口供可信 Go 装配使用。

| 场景 | 验证条件 |
| --- | --- |
| AUDIT-01 | 公开审计按授权过滤、稳定分页、脱敏及快照失效；运行身份不能删除审计 |

## 搜索来源与事件投递

contracts/discovery-event.schema.json 固定 search 9e791c6 的 discovery.v1 约定，来源提交和校验值记录在相邻 source.json。投影只有公开标题、学校、课程与教师归属、修订、可见状态和时间，不包含正文、作者、认证材料或评分。

搜索文档使用当前已发布版本，kind 为 resource。未发布替代版本不会覆盖公开标题或关联。发布、版本替换、下架、安全停用、恢复和删除决定均逐对象写入；现有删除决定执行下架，并保留不可见墓碑。

每次来源对象保存都在业务事务内追加 discovery_history，并写入 resources.discovery.v1 outbox 与 pg_notify('resources_outbox', '')。不可见文档是墓碑，标题为空且不携带课程和教师关联。原始对象删除后，墓碑仍保留。

事件采用 CloudEvents 1.0。source 为 resources，subject 等于 data.target.id，事件 ID 由对象 ID 与修订确定。相同请求重放沿用业务幂等回执，不重复写入投影事件。普通业务事件也使用该封套，只携带业务修订。

模块导出 OutboxChannel。postgres.Repository.PendingOutbox(ctx, limit, excludeSubjects) 返回待发送 Envelope，每批 1 至 100 条，最多排除 1000 个主题。nil 与空集合等价，排除在 LIMIT 前执行，同主题按写入顺序读取。

outbox 只用 sent_at 表示确认状态，未发送部分索引支持空闲与积压查询。读取不认领、不确认事件。ConfirmOutbox(ctx, id) 在所有订阅消费者持久提交后调用，重复确认保留首次时间，不存在的 ID 返回 not_found。

backend relay 负责 LISTEN、启动和重连补读，以及每 60 秒的未发送索引兜底。失败按 5 秒起的指数退避重试，最长一小时，累计 24 小时转待处理。excludeSubjects 接收退避和待处理主题，消费者按事件 ID 去重。

Service.DiscoveryCurrent(ctx, principal, ids) 接收可信请求人与 1 至 100 个 ID。按当前 discover 权限逐学校过滤，外部授权完成后重新读取发布状态，避免返回授权等待期间已下架的资料。拒绝返回空集合，依赖故障返回错误。

旧 PendingEvents 和 AcknowledgeEvent 已移除。Envelope.Data 为 json.RawMessage，relay 按 Type 解码。来源表直接定义 sent_at 最终结构，不保留旧 delivered 字段或接口签名。

postgres.Repository.DiscoverySnapshot(ctx, cursor, limit) 供受信重建任务调用，返回 Items 和 Next，单页 1 至 100 条。空 cursor 开始，空 Next 结束。不可变历史与 PostgreSQL 可见性快照固定首轮成员，分页按 ID 字节序递增。

分页期间的修改、删除和延迟提交不会替换首轮版本。游标五分钟有效，历史修改或清理会使旧游标失效，必须中止重建。历史与墓碑不自动清理，保留策略须结合消费者重放期限单独确认。

本次只新增公开 Go 来源接口，不新增或修改 OpenAPI 路径。运行身份需要读取 discovery_history、discovery_retention，追加 discovery_history，并读取、写入和确认 outbox。历史的修改与删除保留给迁移或维护身份。

| 场景 | 验证条件 |
| --- | --- |
| RES-DISCOVERY-01 | 发布与下架、替代版本、墓碑、Schema 校验、回滚和固定快照 |
| RES-DISCOVERY-02 | 授权期间下架后不返回旧发布状态 |
| RES-OUTBOX-01 | 提交通知、回滚无通知、主题排除、积压与幂等确认 |

## 购买来源与会员覆盖

访问权来源包括管理员范围、单独授权、购买来源和会员投影。管理员在管理范围内例外，普通用户仍须通过当前准入和下载动作资格。购买与会员不会绕过资格；资格恢复后原权益继续参与判断。

GET /api/v1/resources/coverage/{id} 读取 billing 套餐 ID 对应的实际资料范围。PUT 同路径使用 Idempotency-Key、revision 和 coverage.manage 全站权限替换配置。空 rules 表示无覆盖，未配置返回 not_found。

每套覆盖最多 100 条规则。规则之间取并集，每条规则内 school_id 必填，directory_id 与 category_id 可选且同时匹配。目录精确匹配，不包含子目录；目录必须属于指定学校。配置更新后下一次访问立即生效。

会员投影按 owner、plan_id 保存，访问时检查 valid_from <= 当前时间 < valid_until。到期无需清理任务。单独授权、会员与各 order_id 的购买来源分别撤销。普通下架仅保留已购交付，停用拒绝全部交付。

Service.UnlockEligibility(ctx, principal, resourceID) 返回 (UnlockDecision, error)。购买资格使用 resources.purchase，下载资格继续使用 resources.download.restricted。

Authority 接口分别接收模块内动作 purchase 与 download.restricted，由宿主映射到 platform 动作。

购买与下载条件可分别配置。购买资格不满足仍返回 requirements_missing；依赖故障仍返回错误。内容准入、可售状态与已有访问权检查保持独立，购买条件不会撤销既有权益。

Reason 包括 eligible、already_accessible、unauthenticated、not_found、resource_unavailable、requirements_missing 与 not_purchasable。依赖故障返回错误，不能扣费。

资料详情在有访问权时返回 download，在满足购买条件时返回 purchase。purchase_available 表示资源模块已支持购买协作；backend 仍须确认 billing 可用并合并价格。本模块不保存商业条款或支付资料。

Service.Consume(ctx, Envelope) 是可信 relay 入口。只接受 billing 的三类事件，严格拒绝未知字段。backend 验证生产方并转换本地 DTO，不导入 billing 私有实现。消费方 Schema 与生产方 Schema 须在装配时核对。旧 Dependencies.Entitlements 查询已移除。

| 事件 | data 字段 |
| --- | --- |
| billing.resource-unlock.paid.v1 | order_id、owner、resource_id、revision |
| billing.resource-unlock.refunded.v1 | order_id、owner、resource_id、revision |
| billing.membership.changed.v1 | owner、plan_id、valid_from、valid_until、revision |
| resources.entitlement.granted.v1 | order_id、owner、resource_id、revision |
| resources.entitlement.rejected.v1 | order_id、owner、resource_id、revision、reason |

封套使用 specversion、id、source、type、subject、time、datacontenttype、data。owner 包含 issuer 与 subject，时间使用 RFC 3339。revision 为正整数。

消费按来源对象忽略旧修订，inbox 按 billing 事件 ID 去重，退款先到时保存撤销记录以阻止旧 paid 恢复权益。

paid 在事务内检查当前资料与版本可交付性。资料不存在、下架或停用时返回 rejected，reason 为 resource_unavailable。正常情况保存购买来源并发 granted。权益、inbox、outbox 和提交通知同事务，失败后可重投原事件。

每个事件 Schema 保存于 contracts 对应事件名文件，SHA-256 记录在 entitlement-events.sha256.json。

billing 文件是本仓消费方约束，已按 P7a 当前 Schema 核对 owner 对象与 application/json 内容类型。发送沿用 PendingOutbox、ConfirmOutbox 与 resources_outbox 通道。

运行身份需要 coverage、purchases、memberships 的 SELECT/INSERT/UPDATE，以及 inbox 的 SELECT/INSERT。权益消费不读取其他模块的表。购买来源不自动清理，保留供退款、重投与账号查询使用。

| 场景 | 验证条件 |
| --- | --- |
| RS-05 | 多来源不重复购买，资格撤销暂停访问，退款仅撤销对应购买来源 |
| RS-06 | 会员仅按当前配置范围生效，覆盖修改立即生效 |
| RS-15 | 会员到期即时失效，旧修订不恢复已退款权益 |
| RS-28 | 下架与停用拒绝 paid，outbox 失败回滚权益与 inbox，重试恢复 |
