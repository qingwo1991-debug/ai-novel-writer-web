# Go 服务器凭据兼容（迁移中）

实现源码：`backend-go/internal/secrets/`。[GitHub Actions 41378784128](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41378784128) 已通过双向原实现对照、单元测试、race、编译与 smoke；全套 113 顶层 / 345 含子用例、0 fail、0 skip。本模块尚未连接全局配置、LLM profile 或生产数据。

## 不重新生成旧凭据

- AES-256-GCM：`ANWSEC01 || nonce(12) || tag(16) || ciphertext`。
- AAD：`AI-Novel-Writer/server-secret/v1`；明文 UTF-8 最大 64 KiB。
- 只从显式调用者参数读取密钥，不自动读取环境、默认作者目录或模型配置。
- `LoadExistingKey` 仅读取；`LoadOrCreateKey` 只在缺失时创建。损坏、大小不符、权限不安全等既有文件明确失败，不覆盖、不改权限、不重新生成。
- Linux 固定目录描述符，final component 使用真实 `openat(O_NOFOLLOW)`；密钥要求当前 UID、0600、普通文件、单硬链接、32 字节。
- 创建采用独占临时文件、fsync、无覆盖 hard-link 发布、去除临时链接、目录 fsync；并发 Go 创建者通过目录 flock 串行，最终读取同一密钥。

密钥仍须与私人数据卷一起备份。此加密不防止能读取密钥的系统管理员。比原实现更严格的单链接/精确 0600 校验会失败关闭，不能默默更改既有密钥；与旧 Node 同时创建密钥不是正式支持的双写场景。

## 双向原实现对照

GitHub CI 专用 `backend-go/contract/secret-contract.mjs` 使用原 `createServerSecretStorage`，创建**公开固定合成测试 key**和三组密文：空串、中文/emoji/NUL、64 KiB 边界。

1. 原 TypeScript 加密 → Go 解密比对。
2. Go 随机 nonce 加密 → 原 TypeScript 解密比对。
3. 单元测试覆盖篡改、错误 key、格式/长度、nonce、UTF-8 替换、密钥权限/链接/创建竞争/坏文件不覆盖。

测试 key 仅为公开 fixture，不是凭据；不读取/上传真实作品、服务器 key 或 provider secret。Node 只作 CI oracle，不进入 Go 服务运行时。

## 仍需完成

全局数据目录准入、已有配置解密/显示脱敏/保存、provider 与 embedding 配置、导入来源身份 key 和其他 application key 的逐用途兼容仍需连接与验收。`secrets` 通过不能视为 `config:*`、`llm:*` 等操作已迁移。
