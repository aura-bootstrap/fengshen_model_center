# 峰神模型中心（fengshen_model_center）

峰神切片生成器的本地模型中心：只读盘点本地模型资产、登记外置权重路径、核验二进制组件交付证据，并在本机回环地址提供管理界面与只读 HTTP 汇总报告。本仓库是从 `fengshen_slicer` 独立出来的自包含项目，不依赖其工作副本即可构建、测试与运行。中心与所有执行器之间的通信只走本机回环 HTTP/WS，旧 stdio/CLI 协议被隔离在 shim 进程内部。

## 能力边界

- 只读扫描与容量统计：逻辑磁盘大小、文件身份归并、重复候选线索，按需 SHA-256。
- 权重与执行组件分开：源码、开发 venv 不等于已交付；组件清单只做声明与完整性证据，不安装、不构建、不执行推理。
- 12 个稳定本地权重目录项（含双口径识别与分片组成规则）。
- 本机回环 HTTP 管理界面；API 需要页面内注入的会话令牌。
- 内外隔离：对外只有中心 API；内部执行器经 `fengshen-shim` 以回环 HTTP/WS 访问，每实例一次性令牌与严格限额。

## 目录结构

```text
.
├─ cmd/
│  ├─ fengshen-model-center/    # 中心主程序（package main）与内嵌 web/
│  ├─ fengshen-shim/            # 执行器适配进程（whisper-stream/whisper-batch/timbre/haar/llama）
│  └─ build/                    # manifest/delivery/engine-trust/verify-lock 生成器
├─ internal/
│  ├─ bindings/                 # 权重绑定与事务
│  ├─ componentclient/          # 中心 → shim 的 HTTP/WS 客户端（令牌、限额、事件）
│  ├─ componentprotocol/        # 内部协议消息、严格 JSON、限额与校验
│  ├─ components/               # 组件清单、证据与启动报告
│  ├─ highlightclient/          # 高光评分/转写/媒体 HTTP 客户端
│  ├─ integration/              # 三端只读集成（会话、快照、健康）
│  ├─ mediaclient/
│  ├─ registry/                 # 外置路径登记存储
│  ├─ runtimeadapter/           # 编排：Whisper/音色/Haar/llama 全部经 shim 调用
│  ├─ runtimeassets/            # 运行时资产分块存储
│  ├─ runtimehttp/              # 对外运行时上传/下载 HTTP
│  ├─ runtimejobs/              # 运行时任务与配额
│  ├─ runtimeprocess/           # 受控进程监督（含 shim 与 llama）
│  ├─ safefs/                   # Windows 安全文件系统边界
│  ├─ shim/                     # shim 的 HTTP/WS 服务与旧协议桥
│  └─ upstream/                 # 复制自 generation_server 的依赖闭包（非测试源码）
├─ vendor/                      # 唯一第三方依赖（WebSocket 库）离线 vendored
├─ assets/haar/                 # OpenCV Haar 正/侧脸模型
├─ components-src/              # llama-runtime / whisper-batch / whisper-stream 原生源码
├─ installer/                   # PowerShell 构建/测试脚本
├─ sidecar/                     # 冻结 runner；权重与 venv 不入库
├─ docs/specs/                  # 迁移前的需求/设计/任务文档
├─ models/                      # 开发态默认扫描根（空）
└─ .github/workflows/ci.yml
```

## 构建

要求：Windows x64、Go 1.25+（`go.mod` 已固定语言版本）。唯一第三方依赖是 HTTP/WS 所需的 `github.com/coder/websocket`，已 vendored 到 `vendor/`，离线可构建。

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
.\fengshen-model-center.exe --root <开发根或安装根> --data <登记数据目录> --listen 127.0.0.1:18120
```

常用参数：

| 参数 | 说明 |
|---|---|
| `--root` | 开发根或受控安装根；开发态需包含 `sidecar` 与 `models` 目录 |
| `--root-kind` | `development`（默认）或 `installed` |
| `--data` | 独立路径登记目录，默认 `%AppData%\FengshenSlicer\model-center` |
| `--listen` | 仅本机回环监听地址，默认 `127.0.0.1:18120` |
| `--integration-data` | 三端可信发现目录；空则不启用集成 |
| `--install-root`、`--center-sha256`、`--delivery-sha256` | 安装态可信启动参数 |
| `--component-info`、`--haar-component-info` | 仅输出元数据，不扫描、不加载模型 |

开发态默认扫描根：`sidecar/seed-vc/checkpoints`、`models`、`~/.cache/torch/hub/checkpoints`，以及本机权重库 `W:\model_weght_hub`（目录存在时自动加入；可用 `FENGSHEN_MODEL_WEIGHT_HUB` 覆盖）。权重不入库，也可通过界面登记外置路径。

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
node --test cmd/fengshen-model-center/web/app.test.cjs cmd/fengshen-model-center/web/evidence.test.cjs
```

CI 在 `windows-latest` 运行以上三类检查，并附加仓库守卫：Go 源码禁止 `fengshen-slicer` 导入、禁止权重/二进制扩展名、禁止大于 100 MiB 的文件。

## 组件构建（可选，需本地工具链）

`installer/` 下的脚本用于生成受控组件与交付清单，要求 MSVC x64、CMake、Ninja 及锁定的上游归档：

- `build_model_center.ps1`：构建中心与 `fengshen-shim.exe`、生成清单/交付、可选冻结 Demucs/Seed-VC、可选构建原生组件
- `build_whisper_batch.ps1`、`build_whisper_stream.ps1`、`build_llama_runtime.ps1`：原生组件
- `build_model_center_test.ps1`：本地回归
- `test_model_center_runtime_http.ps1`：运行时 HTTP 测试

这些脚本默认把产物写到仓库外的全新目录，不随仓库提交。

## 上游代码同步

`internal/upstream/` 复制自 `fengshen_slicer` 的 `generation_server`（`facehaar` 与 `highlight` 的 8 个子包）。同步规则、来源提交与步骤见 [docs/upstream-sync.md](docs/upstream-sync.md)。迁移基线为 `fengshen_slicer` 提交 `3572089`（2026-11-08）及其工作区在途的模型中心改动，迁移日期 2026-11-10。

## 与 fengshen_slicer 的关系

本仓库承载模型中心的服务、界面、组件清单与构建工具；`fengshen_slicer` 中的三端客户端仍保留原副本。切换方式（整体替换或子模块）是后续独立任务，本仓库不修改 `fengshen_slicer`。

## 许可

暂未选择开源许可证，保留所有权利。
