---
type: spec
status: Accepted
updated: 2026-11-21
---

# 资料列表与计数

## 列表筛选与分页

资料列表默认按 ID 升序排列。`sort=newest` 按选中版本的创建时间和资料 ID 降序排列。`file_type` 接受不含点的小写文件扩展名，最多十二个 ASCII 字母或数字。

公开、管理与本人资料列表均支持可选的 teacher_id、term_id、tag 和 access（public 或 restricted）筛选。公开与管理列表传入 teacher_id 或 term_id 时通过 catalog 校验学校归属，本人列表仅按归属过滤，不依赖当前准入。

`total` 是筛选后、分页前的总数。`query` 匹配标题和描述。公开、管理列表和计数通过 catalog 的 `CourseReferences` 批量解析课程合并别名。本人列表按资料上保存的课程 ID 筛选。

公开、管理与本人列表的 `after` 及 `next_cursor` 契约上限为 2000 字符。按 ID 排序时，游标为资料 ID。按最新排序时，游标绑定账号范围、筛选项和分页位置。超长或不匹配的游标返回 `resources_invalid_input`。

公开列表只展示已发布资料的当前版本。分类、文件名、类型、大小和课程关联来自 `Resource` 与 `Version`，上传时间取版本创建时间。

## 可见计数

GET /api/v1/resources/counts 要求 school_id，可附 course_ids、course_id、category_id、directory_id、teacher_id、term_id、tag、access、attributes、file_type 和 query。

`course_ids` 最多一百项，服务端在校验数量后去重。`total` 为筛选总数，`courses` 返回各请求课程的计数，包含零值。

指定 `category_id` 且分类配置了有效的 `group_by` 字段时，响应可包含 `group_by`、`groups` 与 `ungrouped_count`。整数分组按数值降序排列，选项按配置顺序排列，文本按字节序排列。缺失值计入未分组计数。

列表和计数复用 Repository 的同一筛选条件。统计的是用户可发现的资料元数据，下载资格继续由下载接口独立校验。未发布、下架和封禁内容不进入公开列表及计数。

每次请求检查当前学校准入。`course_ids` 通过 catalog 公开批量接口检查学校归属和状态。计数为零表示筛选后没有符合条件的资料。授权或依赖失败返回对应错误。

计数每次查询都按当前数据与授权计算。一个聚合 SELECT 返回筛选总数及全部请求课程的计数。公开列表的记录与 `total` 使用同一 SELECT 快照。

目录服务可通过 `Counts` 获取本人最近访问课程的资料计数。最近访问记录由 catalog 维护。

## 本人统计与交付

GET /api/v1/resources/me/summary 只返回本人 resource_count 与 published_count。前者与无筛选的本人资料列表一致，包括有版本的草稿、处理中和其他本人状态，后者为已发布子集。

本人数量只要求登录，不依赖当前校园资格。主体来自服务端认证，不能通过 owner、用户 ID 或其他字段读取他人统计。

分类使用 `categories`，预览和单文件下载分别使用 `preview`、`download`。批量下载向 `packages` 提交 `resource_ids`，再查询包清单并下载分卷。每份资料都按当前权限检查。

## 验收场景

| 场景 | 条件 |
| --- | --- |
| RES-UI-01 | 最新与文件类型筛选、分页、可见数量一致、本人统计和撤权 |
| RES-UI-02 | HTTP 正常、登录拒绝、跨学校、空 ID、上限和未知文件类型 |
| RES-UI-03 | 三个列表均接受 2000 字符的有效最新排序游标，超长游标返回 400 |
| RES-UI-04 | 教师、学期、标签与访问条件筛选在列表和计数一致生效，分类配置驱动动态分组统计 |
