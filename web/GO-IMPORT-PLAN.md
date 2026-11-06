# 导入一件真事：把"选文件"换成"上传"，其余不动

> 结论先说：**导入不是"再接线一下"**。它在桌面端是"原生选择框 + 进程内 inspection 令牌"两件事
> 绑在一起的，浏览器两件都没有。可行的改法是把这两件事拆开，各用浏览器自己的做法替代，
> 而不是去模拟一个原生选择框。

## 一、桌面端为什么这样写

`electron/controllers/import-controller.ts:322-323` 的注释写得很清楚：

> Selection, bounded reading, and inspection are one main-process operation.
> The renderer receives only the final inspection token and safe display facts.

也就是说 `dialog:select-novel-files` 一次做完三件事：

1. 弹原生框选文件；
2. 在主进程里按上限读取（**字节从不过 IPC**，省内存也防注入）；
3. 解析并放进 `importInspectionStore`，只把**令牌 + 安全展示信息**给渲染进程。

令牌是进程内的、绑定 `webContents.id` 的、有有效期的（`import-inspection-store.ts:191`）。
下游 `db:import-run-prepare-inspection`、`db:import-run-author-preview` 都靠 `peek`/`consume` 拿回章节。

所以 `dialog:*` 这 8 个名字在浏览器里**永远绑不上**——不是没写，是语义不存在。

## 二、Go 这边其实已经有一半

| 桌面端的东西 | Go 里的对应物 | 状态 |
| --- | --- | --- |
| 进程内 inspection 存储 | `internal/importsource.Store`：`Inspect` / `Peek` / `Consume` / `RevokeCaller`，带 receipt、CAS、按 caller 吊销 | **已存在并有测试** |
| 格式解析 | `internal/importsource`：`parser.go`、`epub.go`、`legacy.go`、`identity.go` | 已存在 |
| 把字节送进服务器 | `POST /api/uploads`（multipart，`?path=相对项目路径`），`synthetic_project_files_linux.go:191` | **已存在并已用于前端** |

缺的不是能力，是**中间那一步**：没人把"上传到项目目录的文件"交给 `Inspect`。

## 三、改法：一个操作，两步走

```
浏览器 <input type="file">
   │  POST /api/uploads?path=import/卷一.txt     ← 已有，字节落进项目目录
   ▼
db:import-run-prepare-inspection（改造）
   │  服务端从项目目录读回受限数量的字节
   │  → importsource.Store.Inspect(binding, captured)
   ▼
   返回 { inspectionId, summary }               ← 令牌仍然只在服务端存储里
   ▼
db:import-run-author-preview（已绑定，直接传 chapters）
db:draft-import-finalized-batch（已绑定，整批一个事务）
```

要点：

1. **令牌依然不落客户端**，客户端拿到的是 id，服务端靠 cookie + 标签页会话把它绑回 caller，
   这和桌面端用 `webContents.id` 绑定是同一个思路，只是换成了 cookie。
2. **字节仍有上限**。桌面端靠"主进程读"控制峰値；这里靠 `POST /api/uploads` 的项目内路径 +
   服务端读取上限，两者都不能省。
3. **不做"列出宿主目录"**。上传只写进当前租约的项目目录，`path` 是相对路径，越界即拒——
   这条已有实现和测试，不要绕过它。

## 四、为什么不直接把 `dialog:select-novel-files` 绑成"返回上传列表"

因为那会让一个"选择"操作产生副作用（读盘、解析、占内存），而它的名字和参数都表示它是无副作用的。
如果要做，应该另开一个名字（例如 `import:upload-and-inspect`），把语义写在名字里。
这需要先确认前端愿意改调用点——**这是唯一需要你决定的地方**：

- 方案 A（推荐）：保留 `dialog:*` 永远 501，前端在导入处改调 `/api/uploads` + `db:import-run-prepare-inspection`。
- 方案 B：新增一个操作名专门做"上传即检查"，前端只改一处调用名。

两个方案的服务端工作量几乎一样，差别只在**前端要改多少**。

## 五、你那 8.2G 正文的现实路径

不建议一次性把 8.2G 全塞进一次导入：

1. 先按卷/文件分批上传（`POST /api/uploads` 每个文件一次，前端已有进度条可用）；
2. 每批走 `prepare-inspection` → `author-preview` → `draft-import-finalized-batch`，
   一个批次一个事务，失败只回滚那批；
3. 导入期间不阻塞写作——批次是独立事务，写到一半的章节不会被后续批次影响。

这样单批的内存峰值是"一个文件"，不是"整本书"。
