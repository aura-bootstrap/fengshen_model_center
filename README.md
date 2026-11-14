# 峰神模型中心（fengshen_model_center）

峰神切片生成器的本地模型中心：按用户清单盘点本地模型资产、核验二进制组件交付证据，并在本机回环地址提供只读 HTTP 汇总报告与无头 JSON API。中心没有 Web 界面，也不做任何令牌或会话鉴权；仅监听本机回环地址，供内部进程直接调用。本仓库是从 `fengshen_slicer` 独立出来的自包含项目，不依赖其工作副本即可构建、测试与运行。中心与所有执行器之间的通信只走本机回环 HTTP/WS，旧 stdio/CLI 协议被隔离在 shim 进程内部。

## 能力边界

- 只读扫描与容量统计：逻辑磁盘大小、文件身份归并、重复候选线索，按需 SHA-256。
- 权重与执行组件分开：源码、开发 venv 不等于已交付；组件清单只做声明与完整性证据，不安装、不构建、不执行推理。
- 12 个稳定本地权重目录项（含双口径识别与分片组成规则）。
- 无 Web 界面；管理操作走同机回环 JSON API（扫描、校验、组件证据），无令牌、无会话鉴权。
- 内外隔离：对外只有中心 API；内部执行器经 `fengshen-shim` 以回环 HTTP/WS 访问，无令牌，仅保留严格限额与进程边界。三端集成、运行时与绑定接口直接可用，不再做权限校验。

## 目录结构

```text
.
├─ cmd/
│  ├─ fengshen-model-center/    # 中心主程序（package main，无 Web 界面）
│  ├─ fengshen-shim/            # 执行器适配进程（whisper-stream/whisper-batch/timbre/haar/llama）
│  └─ build/                    # manifest/delivery/verify-lock 生成器
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
│  ├─ componentclient/          # 中心 → shim 的 HTTP/WS 客户端（限额、事件）
│  ├─ componentprotocol/        # 内部协议消息、严格 JSON、限额与校验
│  ├─ components/               # 组件清单、证据与启动报告
│  ├─ highlightclient/          # 高光评分/转写/媒体 HTTP 客户端
│  ├─ integration/              # 三端只读集成（发现、快照、健康）
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
├─ tests/livecheck/             # 运行实例的全模型/API 深检工具
├─ scripts/                     # 一键构建、bin 平铺安装、启动/停止与测试
├─ build/                       # 本地构建产物（gitignore，不随仓库）
├─ bin/                         # 本地可运行部署目录（gitignore，不随仓库）
└─ docs/                        # 上游同步等说明
```

`external/` 按场景存放不属于 Go 业务代码、也不在模型权重库中的构建输入与资源。模型权重仍只由 `model_manifest.json` 指向：正常模式落三渠道 slicer 用户目录 `~/.fengshen/slicer/{user,dev,admin}/`（同步写、读首存在）；显式 `--state-dir`/`FENGSHEN_STATE_DIR`（沙盒隔离）时落状态目录。

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

## 一键构建

运行 `scripts\build.bat`（双击或在仓库根执行），进入循环菜单。`scripts/` 下只提供 bat 入口，不再保留对应的 `.ps1` 版本：

1. 构建全部（中心 + shim + 可用的原生/冻结组件）
2. 仅构建中心与 shim
3. 构建 whisper-batch
4. 构建 whisper-stream
5. 构建 llama-runtime
6. 冻结 Demucs / Seed-VC
7. 设置依赖路径与版本
8. 清理 build 目录
9. 安装到 bin（全部或单个部分）
0. 退出

所有构建产物只写入 `build\<选项>-<时间戳>\`，`build/` 已加入 `.gitignore`。原生构建前可在菜单 7 设置 Whisper 依赖目录与 llama 源码归档；冻结还需要 Python 输入目录与输入锁文件。也可通过 `FENGSHEN_WHISPER_DEPS`、`FENGSHEN_LLAMA_SOURCE_ARCHIVE`、`FENGSHEN_FREEZE_INPUTS`、`FENGSHEN_FREEZE_INPUT_LOCK` 预置。直接执行单个菜单项可用 `scripts\build.bat 2` 预选菜单项 2。

安装与构建分离：菜单 9 可取 `build\` 下各部分最近一次完整构建，把成品平铺复制到 `bin\`，同名文件直接覆盖，不创建 `components\`、`model-center\` 等层级。也可直接执行 `scripts\install.bat [all|center|whisper-batch|whisper-stream|llama-runtime|timbre]`，或追加构建目录参数只安装指定产物，例如 `scripts\install.bat center build\center-20261110-171830-755`。

## bin 部署

`bin\` 与外部 `model_root` 配合即可启动本地服务；安装采用平铺结构，只放可执行文件，不带组件包与版本目录：

```text
bin\
├─ fengshen-model-center.exe
├─ fengshen-shim.exe
├─ whisper-batch.exe
├─ whisper-stream.exe
├─ llama-runtime.exe
├─ llama-server.exe
├─ demucs-runner.exe
└─ seedvc-runner.exe
```

前两个文件是中心运行所需；其余文件在安装对应部分后出现。`demucs-runner` / `seedvc-runner` 是 PyInstaller onedir，平铺安装只复制入口 exe，依赖文件仍保留在 `build\` 产物中。

```powershell
scripts\run.bat
scripts\run.bat W:\model_root 127.0.0.1:18120
```

`scripts\run.bat` 直接读取 `bin\fengshen-model-center.exe`，在后台启动并把中心 PID 写入状态目录 `model_center.pid`；输出写入状态目录 `logs\model_center.out.log` / `model_center.err.log`。状态目录安装态为客户端安装根（`bin\model-center` 上两级），开发态为仓库根，`FENGSHEN_STATE_DIR` 可覆盖。重复执行会检测已在运行的实例并直接返回。启动脚本不复制进 `bin\`。

停止服务：执行 `scripts\stop.bat`，它会读取 PID 文件并结束中心进程（含子进程），随后删除 PID 文件。

状态数据不复制进 `bin\`：清单落三渠道用户目录（显式隔离时落状态目录；仅当清单不存在时才用给定 `model_root` 生成，启动时旧状态目录清单自动迁移），运行数据写入状态目录 `model-center\`。`model_root` 只读使用，不复制、不修改。重新安装会覆盖 `bin\` 根下的同名文件。

## 运行

```powershell
.\fengshen-model-center.exe --root <开发根或安装根> --data <绑定数据目录> --listen 127.0.0.1:18120
```

常用参数：

| 参数 | 说明 |
|---|---|
| `--root` | 项目根或受控安装根；模型文件路径全部来自用户模型清单 |
| `--root-kind` | `development`（默认）或 `installed` |
| `--state-dir` | 状态目录（日志、运行数据）；默认安装态为客户端安装根、开发态为仓库根，`FENGSHEN_STATE_DIR` 可覆盖；显式指定时清单也落这里（隔离模式） |
| `--data` | 绑定存储目录，默认 `<状态目录>\model-center` |
| `--listen` | 仅本机回环监听地址，默认 `127.0.0.1:18120` |
| `--integration-data` | 三端发现目录；空则不启用集成 |
| `--install-root` | 安装态组件根目录前缀 |
| `--component-root` | 组件根目录；空则使用 `<install-root>\model-center\components`（安装部署用；本地 `bin` 运行不传） |
| `--component-info`、`--haar-component-info` | 仅输出元数据，不扫描、不加载模型 |

开发态与安装态使用模型清单 `model_manifest.json`（`entries`：`model_id` / `path` / `bytes` / `sha256`），落 `~/.fengshen/slicer/{user,dev,admin}/`——中心同时服务三渠道，写时三目录同步、读按 user→dev→admin 取首存在；显式 `--state-dir`/`FENGSHEN_STATE_DIR` 时落状态目录。安装器在安装时调用中心生成该文件；开发和本地运行也使用同一文件，可手动执行 `fengshen-model-center.exe --write-model-manifest "D:\weights;D:\other"` 生成。中心按清单条目盘点与绑定，不再扫描目录，也不再使用 `registrations.json` 或 `resource-layout.json`。

## 内部通信与隔离（HTTP/WS）

```
引擎/管理端 ─HTTP→ 模型中心 ─HTTP/WS→ fengshen-shim ─旧协议→ 执行器
                              ├─ whisper-stream（WS，NDJSON 语义）
                              ├─ whisper-batch（HTTP 异步作业）
                              ├─ timbre demucs/seedvc（HTTP 异步作业）
                              ├─ haar（HTTP 推理）
                              └─ llama（HTTP 代理；shim 启动 llama-server）
```

- 中心进程不再读取执行器 stdout 作为数据通道、不再拼接 CLI 业务参数、不再进程内直调推理，也不再直接访问 llama-server。
- 每个 shim 仅监听 `127.0.0.1` 随机端口，不校验令牌；内部接口限制控制 JSON、WS 帧、图像与产物大小；会话取消或关闭会终止 shim 进程树并释放端口。
- 运行时请求只用 `X-Model-Center-Session-ID`/`X-Model-Center-Task-ID` 做任务归属，它们不是凭据；任务被显式取消或服务关闭时释放资产与进程。
- 权重与输入仍为经验证的本地只读路径绑定；流式 PCM 走 WS，Haar 图像走 HTTP 请求体，作业产物经 HTTP 下载；llama 的 `/health` 与 `/v1/chat/completions` 经 shim 代理，llama-server 仅绑定回环地址且不设 key。

## 测试

```powershell
go vet ./...
go test ./...
```

仓库不配置 CI workflow；以上检查需在本地执行。仓库守卫另行约定：Go 源码禁止 `fengshen-slicer` 导入、禁止权重/二进制扩展名、禁止大于 100 MiB 的文件。

## 测试

```powershell
scripts\test.bat
scripts\test.bat -MinFirstPartyCoverage 80 -MinPackageCoverage 50
```

执行顺序：

1. `go vet ./...`；
2. `go test -count=1 -covermode=atomic ./...`，不使用测试结果缓存；
3. 排除 `internal/upstream/`（复制依赖闭包）与 `tests/livecheck`（测试工具）后的第一方覆盖率门禁；
4. 每个有测试的 Go 包单独覆盖率门禁；
5. 生成 `build\full-test\coverage.html`；
6. 构建契约；
7. 检查 `bin\` 中已安装的中心与 shim（需先执行 `scripts\install.bat center`）；
8. 读取状态目录 `model_center.pid`；若进程不存在则调用 `scripts\run.bat` 启动并重读 PID，然后通过 `/api/state`、`/api/report`、`/api/components` 做进程健康测试；
9. 运行 `tests/livecheck`：扫描全部清单模型，逐模型分页核对文件数、字节数和状态，校验 report 的 present/composition 汇总，并覆盖公开 API 与错误语义；默认只读元数据，`-VerifyHashes` 才读取全部权重做 SHA-256 校验。

测试启动的实例会保留运行，不会在测试结束时停止。`-SkipProcess` 跳过第 8、9 步，`-SkipDeep` 只跳过第 9 步，`-VerifyHashes` 让第 9 步做全量哈希，`-SkipBuildContract` 跳过第 6 步。

默认门禁：第一方覆盖率 ≥ 70%，单包 ≥ 40%。当前基线：第一方 70.4%，componentclient 82.8%，componentprotocol 86.4%，modelmanifest 87.5%。覆盖率报告不宣称 100%：OS 差异分支、错误注入路径和 `internal/upstream` 复制闭包仍会留白，门禁用于防止明显回退。

## 组件构建（可选，需本地工具链）

`external/` 下的脚本用于生成受控组件与交付清单，要求 MSVC x64、CMake、Ninja 及锁定的上游归档：

- `external/model-center/build.ps1`：构建中心与 `fengshen-shim.exe`、生成清单/交付、可选冻结 Demucs/Seed-VC、可选构建原生组件
- `external/whisper/batch/build.ps1`、`external/whisper/stream/build.ps1`、`external/llama/runtime/build.ps1`：原生组件
- `external/model-center/build_test.ps1`：本地回归

这些脚本接受仓库外的全新目录或 `<repo>\build\...`；一键脚本统一写入 `build\`，该目录不随仓库提交。

## 上游代码同步

`internal/upstream/` 复制自 `fengshen_slicer` 的 `generation_server`（`facehaar` 与 `highlight` 的 8 个子包）。同步规则、来源提交与步骤见 [docs/upstream-sync.md](docs/upstream-sync.md)。迁移基线为 `fengshen_slicer` 提交 `3572089`（2026-11-08）及其工作区在途的模型中心改动，迁移日期 2026-11-10。

## 与 fengshen_slicer 的关系

本仓库承载模型中心的服务、组件清单与构建工具；`fengshen_slicer` 中的三端客户端仍保留原副本，切换时需同步去掉旧会话令牌并改用无鉴权的直连接口。切换方式（整体替换或子模块）是后续独立任务，本仓库不修改 `fengshen_slicer`。

## 许可

暂未选择开源许可证，保留所有权利。
