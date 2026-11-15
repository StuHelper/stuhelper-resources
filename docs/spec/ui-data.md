---
type: spec
status: Accepted
updated: 2026-11-15
---

# 原型资料列表与计数

## 列表扩展

现有资料列表增加 sort=newest 和 file_type。newest 按选中版本的 created_at、资料 ID 降序，file_type 是小写文件扩展名，不含点，最长十二个字母或数字。原有 sort=id 行为保留。

列表增加 total，表示应用筛选后、分页前的总数。query 同时匹配标题和描述。当前课程的合并别名由 catalog 批量解析，列表与计数使用相同归属。最新排序游标绑定账号范围、筛选与排序，不能跨条件重用。

公开、管理与本人列表的 after 参数保留既有 2000 字符上限。最新排序游标在此上限内校验格式、筛选范围和分页位置，超过上限返回 resources_invalid_input。

公开列表只包含已发布的资料与当前已发布版本。分类、文件名、类型、大小、课程关联和上传时间继续来自现有 Resource 与 Version。上传时间是版本创建时间，未伪造成其他时间。

## 可见计数

GET /api/v1/resources/counts 要求 school_id，可附 course_ids、category_id、file_type 和 query。course_ids 最多一百项，重复值去重。total 是当前筛选总数，courses 为请求的各课程数量，包括零值。

列表和计数复用 Repository 的同一筛选条件。统计的是用户可发现的资料元数据，下载资格继续由下载接口独立校验。未发布、下架和封禁内容不进入公开列表及计数。

每次请求检查当前学校准入。course_ids 通过 catalog 公开批量接口检查所属学校和当前状态，其他学校被拒绝。没有数据与无权限分别返回，不用零计数掩盖授权或依赖错误。

计数没有跨请求缓存。一个聚合 SELECT 返回筛选总数及所有请求课程的计数，不逐门查询。公开列表的记录与 total 来自同一 SELECT 快照，已有详情及下载授权仍在返回时执行。

catalog 的本人最近课程通过同一 Counts 服务获取数量。最近访问记录由 catalog 拥有，本模块不保存浏览历史。

## 本人统计与既有能力

GET /api/v1/resources/me/summary 只返回本人 resource_count 与 published_count。前者与无筛选的本人资料列表一致，包括有版本的草稿、处理中和其他本人状态，后者为已发布子集。

本人数量只要求登录，不依赖当前校园资格。主体来自服务端认证，不能通过 owner、用户 ID 或其他字段读取他人统计。

分类标签复用 categories，文件预览复用 preview，单个下载复用 download。批量下载向现有 packages 提交 resource_ids，再查询包状态并下载各分卷。每份资料仍按当前权限检查。

## 验证

| 场景 | 条件 |
| --- | --- |
| RES-UI-01 | 最新与文件类型筛选、分页、可见数量一致、本人统计和撤权 |
| RES-UI-02 | HTTP 正常、登录拒绝、跨学校、空 ID、上限和未知文件类型 |
| RES-UI-03 | 三个列表接受有效的 2000 字符游标并正确翻页，超长游标返回 400 |
