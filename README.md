# 峰神模型中心（fengshen_model_center）

峰神切片生成器的本地模型中心：只读盘点本地模型资产、登记外置权重路径、核验二进制组件交付证据，并在本机回环地址提供管理界面与只读 HTTP 汇总报告。本仓库是从 `fengshen_slicer` 独立出来的自包含项目，不依赖其工作副本即可构建、测试与运行。

## 能力边界

- 只读扫描与容量统计：逻辑磁盘大小、文件身份归并、重复候选线索，按需 SHA-256。
- 权重与执行组件分开：源码、开发 venv 不等于已交付；组件清单只做声明与完整性证据，不安装、不构建、不执行推理。
- 12 个稳定本地权重目录项（含双口径识别与分片组成规则）。
- 本机回环 HTTP 管理界面；API 需要页面内注入的会话令牌。

## 目录结构

```text
.
├─ *.go                  # 中心主程序（package main）
├─ bindings/             # 权重绑定与事务
├─ build/                # manifest/delivery/engine-trust/verify-lock 生成器
├─ components/           # 组件清单、证据与启动报告
├─ highlightclient/      # 高光评分/转写/媒体 HTTP 客户端
├─ integration/          # 三端只读集成（会话、快照、健康）
├─ mediaclient/
├─ registry/             # 外置路径登记存储
├─ runtimeadapter/       # Whisper/Demucs/Seed-VC/llama/人脸运行适配
├─ runtimeassets/        # 运行时资产分块存储
├─ runtimehttp/          # 运行时上传/下载 HTTP
├─ runtimejobs/          # 运行时任务与配额
├─ runtimeprocess/       # 受控子进程
├─ safefs/               # Windows 安全文件系统边界
├─ internal/upstream/    # 复制自 generation_server 的依赖闭包（非测试源码）
├─ web/                  # 内嵌管理界面与 Node 测试
├─ assets/haar/          # OpenCV Haar 正/侧脸模型
├─ components-src/       # llama-runtime / whisper-batch / whisper-stream 原生源码
├─ installer/            # PowerShell 构建/测试脚本
├─ sidecar/              # 冻结 runner；权重与 venv 不入库
├─ docs/specs/           # 迁移前的需求/设计/任务文档
├─ models/               # 开发态默认扫描根（空）
└─ .github/workflows/ci.yml
```

## 构建

要求：Windows x64、Go 1.25+（`go.mod` 已固定语言版本），无需任何第三方 Go 模块。

```powershell
go build -o fengshen-model-center.exe .
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

开发态默认扫描根：`sidecar/seed-vc/checkpoints`、`models`、`~/.cache/torch/hub/checkpoints`。权重不入库，按需通过界面登记外置路径。

## 测试

```powershell
go vet ./...
go test ./...
node --test web/app.test.cjs web/evidence.test.cjs
```

CI 在 `windows-latest` 运行以上三类检查，并附加仓库守卫：Go 源码禁止 `fengshen-slicer` 导入、禁止权重/二进制扩展名、禁止大于 100 MiB 的文件。

## 组件构建（可选，需本地工具链）

`installer/` 下的脚本用于生成受控组件与交付清单，要求 MSVC x64、CMake、Ninja 及锁定的上游归档：

- `build_model_center.ps1`：中心清单/交付、可选冻结 Demucs/Seed-VC、可选构建原生组件
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
