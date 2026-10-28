# StuHelper Resources

资料模块管理分类、课程关联、资料版本、审核发布、访问授权和目录下载包。文件字节交给 media，学术对象通过 catalog 校验，审核通过 moderation 处理。各模块使用独立数据库身份。

课件、往年题、教材和习题来自可修改的初始配置。分类使用稳定 ID，字段支持文本、整数和选项。往年题的年份只是配置中的整数型字段，可筛选和分组，业务逻辑不根据分类名称分支。

分类修订和发布版本保留快照。新版本待审时继续展示和下载旧版本。下载和打包检查当前资格、访问权及资料状态，普通下架保留已购交付，安全停用阻止全部下载。

## 检查与接入

```bash
make setup
make generate
bash scripts/check-with-postgres.sh
```

检查使用独立 PostgreSQL 集群，覆盖事务、权限、并发幂等、审核连续性和恢复。OpenAPI 生成 Go 与 TypeScript 客户端，HTTP 同时按该契约校验请求。

迁移身份执行 postgres.Migrate，运行身份通过 CheckRuntime 检查。服务通过 Dependencies 接入当前身份与资格、目录、文件和审核接口。账务是可选能力，未接入时不提供购买功能，单独授权仍可使用。

当前本仓测试使用可控外部接口。应用装配、页面、真实账号与生产验收在对应仓库分别记录。完整规则见[模块规格](docs/spec/resources.md)，许可证为 AGPL-3.0-only。
