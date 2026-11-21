---
type: guide
status: Accepted
updated: 2026-11-21
---

# Resources 工程约定

遵循 [StuHelper 工作区约定](https://github.com/StuHelper/stuhelper/blob/develop/AGENTS.md)。resources 维护资料、分类、目录、版本、访问权、申请、审计和投影事件。文件及发布引用通过 media 操作，学术关联由 catalog 校验，审核决定来自 moderation。SQL 放在 `postgres`，迁移身份与运行身份分开。

分类与字段使用持久化配置。新接口先修改 OpenAPI，再生成 SDK。依赖类型与能力要求遵循 `module.yaml`。在相应规格中维护场景，并在 `scenario_tests` 登记可定位到实际函数的测试。

依照 [README](README.md#验证) 启动独立 PostgreSQL 集群并执行本仓检查。使用合成账号、供应商凭据替身和测试文件。
