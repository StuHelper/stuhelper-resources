---
type: index
status: Accepted
updated: 2026-11-21
---

# StuHelper Resources

资料模块管理资料分类、学校与课程目录、版本、发布状态、访问权和下载包。分类与字段来自持久化配置。读写按稳定的分类 ID 和配置修订执行，提交资料时保存分类快照。

## 模块边界

[module.yaml](module.yaml) 声明必需依赖 platform、catalog、media、moderation。catalog 提供学术关联，media 管理文件及发布引用，moderation 管理审核和申诉。宿主通过 `Dependencies` 注入当前动作资格及三方公开接口。资料与审核决定分别由各自模块保存。

verification、billing、identity 是可选模块。identity 是 `resources.read`、`resources.write`、`resources.manage` 的能力依赖。内容准入和动作资格读取宿主配置及当前事实。所需事实不可用时，受影响的动作返回暂不可用，本人记录与管理入口仍按各自权限校验。

billing 维护价格、扣费和退款。resources 保存独立授权、购买来源、会员投影和套餐覆盖规则，通过账务事件更新投影。下载及文件预览重新检查资格、权益与资料状态。普通下架保留符合当前资格的已购交付，安全停用停止下载。

## 契约与规格

[OpenAPI](contracts/resources.openapi.json) 定义资料 HTTP 接口，变更后用 `make generate` 更新 Go 和 TypeScript SDK。

| 文档 | 内容 |
| --- | --- |
| [资料分类、发布与交付](docs/spec/resources.md) | 审核发布、下载、账务投影、审计和搜索事件 |
| [按账号信息查询](docs/spec/account-query.md) | Go 查询端口、字段与授权范围 |
| [资料列表与计数](docs/spec/ui-data.md) | 排序、筛选、可见计数与本人统计 |

## 验证

安装仓库声明的工具链与 PostgreSQL 后，在仓库根目录执行与 CI 一致的检查。

```bash
make setup
bash scripts/check-with-postgres.sh
```

检查创建独立的 PostgreSQL 测试集群，并核对生成产物与文档。清单中的 `specifications` 指向场景条目，`scenario_tests` 通过 `文件#函数` 指向现有测试。数据库测试检查角色隔离、事务回滚、授权撤销、重复提交及发布恢复，外部依赖使用受控替身。

## 生产验收

StuHelper v0.1.31 的锁定组合使用 resources v0.1.3，资料页面已通过匿名访问核验。使用真实账号完成上传、审核和权限下载仍待验收。开发分支的变更随对应后端组合晋级。 [正式组合](https://github.com/StuHelper/stuhelper/releases/tag/v0.1.31)

本仓使用 AGPL-3.0-only。
