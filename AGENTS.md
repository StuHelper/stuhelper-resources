# Resources 工程约定

遵循 StuHelper Next 主工作区的 AGENTS.md。resources 拥有资料业务、分类、目录、授权和发布状态，通过公开接口调用 catalog、media、moderation 与可选账务能力。SQL 只在 postgres 中，运行与迁移身份分开。

分类与字段来自持久化配置，禁止在业务判断和页面中按课件、往年题等名称分支。新接口先定义 OpenAPI，再生成 SDK。测试覆盖真实 PostgreSQL、授权、幂等、并发和失败恢复。不得提交真实身份、供应商凭据或用户文件。
