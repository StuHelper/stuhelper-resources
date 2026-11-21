---
type: spec
status: Accepted
updated: 2026-11-21
---

# 资料按账号信息查询

`accountquery` 包定义 Go 查询对象和授权服务。backend 将账号、管理范围和 information-query 请求转换为本模块的类型，并通过受信 Go 端口调用。

```go
func (s accountquery.Service) Query(
    ctx context.Context, principal accountquery.Principal,
    q accountquery.Query,
) ([]accountquery.Record, error)
```

`Kind` 为 `account`。将 issuer 与 subject 分别做无填充 base64url 编码，以冒号连接成 `Value`。学校、群和模块范围均需填写，`*` 表示该维度不限定。查询需提供用途 `Purpose` 和请求标识 `RequestID`。

Limit 为 1–100，After 是上一页最后一条记录 ID。数据库先按当前授权和请求范围过滤，再按 ID 的 C 排序取排他分页。游标不会固定历史快照，后续页重新读取当前权限和记录。

`Fields` 接受 business、identity、document、material、refund_payee 五类选择。资料端口仅拥有 `business` 类别。省略选择或包含 `business` 时返回业务字段，单独选择其他类别时只返回匹配的记录和空字段列表。

`Authorizer.Scopes` 从可信身份和当前授权读取 `resources.account-query` 的学校、群与模块范围。授权由宿主按本模块能力执行。

授权范围与请求范围求交，空交集返回 `ErrForbidden`。授权或数据库故障返回 `ErrUnavailable`，整次查询失败。调用有五秒上限并传递取消信号。

`Query.String` 与 `GoString` 隐去全部参数，错误使用稳定代码。宿主日志只记录脱敏诊断与请求标识。

返回上传资料的内部 ID、状态，以及单独授权和购买来源的资料 ID、撤销和到期状态。学校来自资料记录。记录的群范围为 `*`，仅授权具体群的请求无法读取这些记录。

`unlock` 返回购买来源的 `order_id`、`resource_id` 与 `revoked`。单独授权使用 `grant`，包含到期和撤销状态。退款更新对应订单来源。响应限资料与权益元数据。

`postgres.Repository` 实现内部读取端口 `ReadAccount`。宿主注入 `Repo` 与 `Auth`，通过 `Service.Query` 执行授权与查询。

| 场景 | 验证条件 |
| --- | --- |
| IQ-02 | 本域授权、学校和群范围先过滤、稳定排他分页、查询脱敏及依赖不可用 |

清单为 IQ-02 登记授权、脱敏、分页和写入时间测试。隔离 PostgreSQL 用例验证查询可使用仅获本模块 SELECT 权限的测试角色，并检查初始化幂等及写入回滚。

上传记录、单独授权和购买来源各有 `query_changed_at`。Repository 写入时刷新时间，退款也更新购买来源的查询时间。
