# 正文并发提交 HTTP 回归

这是隔离服务器 HTTP 契约验收，不是浏览器操作或真实模型竞争验收。

## 环境与约束

`web/draft-conflict.integration.mjs` 固定仅连接 `http://127.0.0.1:16081`，使用测试可信身份头、cookie/CSRF 和新随机客户端 ID。服务器必须挂载隔离数据且 `NOVEL_ALLOWED_ENDPOINTS=`；不得转发到正式服务。脚本通过实际 HTTP 创建独有 `HTTP防覆盖验收-<timestamp>` 作品和纯合成草稿，只修改该作品，不调用模型、MCP、全局设置或删除。真实浏览器编辑另记，不把 API 写入当成 UI 验收。

```sh
node --check web/draft-conflict.integration.mjs
node web/draft-conflict.integration.mjs
```

私有证据仅写 `.native-test-data/http-conflict/<timestamp>/report.json`，0700 目录/0600 文件，不记录 cookie/CSRF、grant、会话租约、服务器路径或正文。

## 已执行结果

运行 `bash-519`，2026-11-02T01:19:26Z，exit 0，**7/7 PASS，0 付费派发**：

1. 同一基线同时提交两个不同正文，恰有一个成功；另一个返回 `DRAFT_CONTENT_CONFLICT`，当前正文是成功者。
2. 旧调用缺少正文基线拒绝，正文不变。
3. 过期项目租约不能保存，正文不变。
4. 冻结定稿后另一次人工保存先成功，迟到定稿返回 `committed:false` / `FINALIZATION_COMMIT_CONFLICT`；服务器保持新人工正文及工作稿状态，候选请求文本保留。
5. 新定稿缺少已保存基线拒绝。
6. 当前基线定稿可提交；响应丢失后的完全相同旧请求即使未携带新基线字段，也幂等返回原 finalizationId。
7. 定稿后即使正文基线相等，也不能通过普通保存重开或覆盖只读正文。

私有报告：`.native-test-data/http-conflict/2026-11-02T01-19-26-574Z/report.json`。脚本结束显式关闭自己的项目，不删除合成作品。没有发起定稿后 AI 编排，不能据此宣称后处理、真实模型竞争或多人协作通过。

另外，`web/domain-tests.config.mjs` 的 113 项确定性关键回归覆盖事务回滚、空正文/Unicode 精确比较、修稿来源/目标失效、定稿幂等和客户端完成事件保留新输入；该配置已加入干净镜像构建门。安全/传输/书库隐藏 74 项 Node 测试和隔离 HTTP 安全 32 项亦通过。扩大套件尚有两个失败套件，详见 [CONCURRENT-EDITING.md](CONCURRENT-EDITING.md)，不冒称全套测试全绿。
