# 峰神模型中心（fengshen_model_center）

峰神切片生成器的本地模型中心：按用户清单盘点本地模型资产、核验二进制组件交付证据，并在本机回环地址提供只读 HTTP 汇总报告与无头 JSON API。中心没有 Web 界面，也不做页面会话认证。本仓库是从 `fengshen_slicer` 独立出来的自包含项目，不依赖其工作副本即可构建、测试与运行。中心与所有执行器之间的通信只走本机回环 HTTP/WS，旧 stdio/CLI 协议被隔离在 shim 进程内部。

## 能力边界

- 只读扫描与容量统计：逻辑磁盘大小、文件身份归并、重复候选线索，按需 SHA-256。
- 权重与执行组件分开：源码、开发 venv 不等于已交付；组件清单只做声明与完整性证据，不安装、不构建、不执行推理。
- 12 个稳定本地权重目录项（含双口径识别与分片组成规则）。
- 无 Web 界面；管理操作走同机回环 JSON API（扫描、校验、组件证据），不设页面会话令牌。
- 内外隔离：对外只有中心 API；内部执行器经 `fengshen-shim` 以回环 HTTP/WS 访问，每实例一次性令牌与严格限额。三端集成、运行时与绑定接口继续使用内部会话授权。

## 目录结构

```text
.
├─ cmd/
│  ├─ fengshen-model-center/    # 中心主程序（package main，无 Web 界面）
│  ├─ fengshen-shim/            # 执行器适配进程（whisper-stream/whisper-batch/timbre/haar/llama）
│  └─ build/                    # manifest/delivery/engine-trust/verify-lock 生成器
├─ external/                    # 非 Go 代码与资源（模型权重不在此仓库）
│  ├─ model-center/             # 中心构建、契约测试与运行时 HTTP 脚本
│  ├─ whisper/
│  │  ├─ batch/                 # 原生源码、CMake、回归测试与 build.ps1
│  │  └─ stream/                # 原生源码、CMake、协议/VAD 测试与 build.ps1
│  ├─ llama/
│  │  └─ runtime/               # 原生源码、CMake、锁文件与 build.ps1
│  └─ timbre/                   # Demucs/Seed-VC 冻结 runner 与测试
├─ internal/
│  ├─ bindings/                 # 权重绑定与事务
│  ├─ componentclient/          # 中心 → shim 的 HTTP/WS 客户端（令牌、限额、事件）
│  ├─ componentprotocol/        # 内部协议消息、严格 JSON、限额与校验
│  ├─ components/               # 组件清单、证据与启动报告
│  ├─ highlightclient/          # 高光评分/转写/媒体 HTTP 客户端
│  ├─ integration/              # 三端只读集成（会话、快照、健康）
│  ├─ mediaclient/
│  ├─ registry/                 # 清单快照类型（不再持久化）
│  ├─ runtimeadapter/           # 编排：Whisper/音色/Haar/llama 全部经 shim 调用
│  ├─ runtimeassets/            # 运行时资产分块存储
│  ├─ runtimehttp/              # 对外运行时上传/下载 HTTP
│  ├─ runtimejobs/              # 运行时任务与配额
│  ├─ runtimeprocess/           # 受控进程监督（含 shim 与 llama）
│  ├─ safefs/                   # Windows 安全文件系统边界
│  ├─ shim/                     # shim 的 HTTP/WS 服务与旧协议桥
│  └─ upstream/                 # 复制自 generation_server 的依赖闭包（非测试源码）
└─ .github/workflows/ci.yml
```

`external/` 按场景存放不属于 Go 业务代码、也不在模型权重库中的构建输入与资源。模型权重仍只由 `%USERPROFILE%\.fengshen\model_manifest.json` 指向。

## 构建

要求：Windows x64、Go 1.25+（`go.mod` 已固定语言版本）。唯一第三方依赖是 HTTP/WS 所需的 `github.com/coder/websocket`，由 `go.mod`/`go.sum` 锁定；首次构建前执行 `go mod download`（或配置 GOPROXY），仓库不包含 `vendor/`。

```powershell
go build -o fengshen-model-center.exe ./cmd/fengshen-model-center
go build -o fengshen-shim.exe ./cmd/fengshen-shim
```

构建工具（清单/交付/信任）：

```powershell
go run ./cmd/build -mode manifest -root <目录> -template <模板>
```

## 运行

```powershell
.\fengshen-model-center.exe --root <开发根或安装根> --data <绑定数据目录> --listen 127.0.0.1:18120
```

常用参数：

| 参数 | 说明 |
|---|---|
| `--root` | 项目根或受控安装根；模型文件路径全部来自用户模型清单 |
| `--root-kind` | `development`（默认）或 `installed` |
| `--data` | 绑定存储目录，默认 `%AppData%\FengshenSlicer\model-center` |
| `--listen` | 仅本机回环监听地址，默认 `127.0.0.1:18120` |
| `--integration-data` | 三端可信发现目录；空则不启用集成 |
| `--install-root`、`--center-sha256`、`--delivery-sha256` | 安装态可信启动参数 |
| `--component-info`、`--haar-component-info` | 仅输出元数据，不扫描、不加载模型 |

开发态与安装态**只使用**用户模型清单 `%USERPROFILE%\.fengshen\model_manifest.json`（`entries`：`model_id` / `path` / `bytes` / `sha256`）。安装器在安装时调用中心生成该文件；开发和本地运行也使用同一文件，可手动执行 `fengshen-model-center.exe --write-model-manifest "D:\weights;D:\other"` 生成。中心按清单条目盘点与绑定，不再扫描目录，也不再使用 `registrations.json` 或 `resource-layout.json`。

## 内部通信与隔离（HTTP/WS）

```
引擎/管理端 ─HTTP→ 模型中心 ─HTTP/WS→ fengshen-shim ─旧协议→ 执行器
                              ├─ whisper-stream（WS，NDJSON 语义）
                              ├─ whisper-batch（HTTP 异步作业）
                              ├─ timbre demucs/seedvc（HTTP 异步作业）
                              ├─ haar（HTTP 推理）
                              └─ llama（HTTP 代理；shim 启动 llama-server 并持有上游 key）
```

- 中心进程不再读取执行器 stdout 作为数据通道、不再拼接 CLI 业务参数、不再进程内直调推理，也不再直接访问 llama-server。
- 每个 shim 仅监听 `127.0.0.1` 随机端口；令牌经 `FENGSHEN_SHIM_TOKEN` 注入，仅在内存与环境块中使用，不落盘、不进日志。
- 内部接口拒绝缺失/重复/混合认证头与代理头，限制控制 JSON、WS 帧、图像与产物大小；会话取消或过期会终止 shim 进程树并释放端口。
- 权重与输入仍为经验证的本地只读路径绑定；流式 PCM 走 WS，Haar 图像走 HTTP 请求体，作业产物经 HTTP 下载；llama 的 `/health` 与 `/v1/chat/completions` 经 shim 代理，上游内部 key 不出 shim。

## 测试

```powershell
go vet ./...
go test ./...
```

CI 在 `windows-latest` 运行以上检查，并附加仓库守卫：Go 源码禁止 `fengshen-slicer` 导入、禁止权重/二进制扩展名、禁止大于 100 MiB 的文件。

## 组件构建（可选，需本地工具链）

`external/` 下的脚本用于生成受控组件与交付清单，要求 MSVC x64、CMake、Ninja 及锁定的上游归档：

- `external/model-center/build.ps1`：构建中心与 `fengshen-shim.exe`、生成清单/交付、可选冻结 Demucs/Seed-VC、可选构建原生组件
- `external/whisper/batch/build.ps1`、`external/whisper/stream/build.ps1`、`external/llama/runtime/build.ps1`：原生组件
- `external/model-center/build_test.ps1`：本地回归
- `external/model-center/test_runtime_http.ps1`：运行时 HTTP 测试

这些脚本默认把产物写到仓库外的全新目录，不随仓库提交。

## 上游代码同步

`internal/upstream/` 复制自 `fengshen_slicer` 的 `generation_server`（`facehaar` 与 `highlight` 的 8 个子包）。同步规则、来源提交与步骤见 [docs/upstream-sync.md](docs/upstream-sync.md)。迁移基线为 `fengshen_slicer` 提交 `3572089`（2026-11-08）及其工作区在途的模型中心改动，迁移日期 2026-11-10。

## 与 fengshen_slicer 的关系

本仓库承载模型中心的服务、组件清单与构建工具；`fengshen_slicer` 中的三端客户端仍保留原副本。切换方式（整体替换或子模块）是后续独立任务，本仓库不修改 `fengshen_slicer`。

## 许可

暂未选择开源许可证，保留所有权利。
