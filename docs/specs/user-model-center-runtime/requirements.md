# Requirements Document

## Introduction

在既有独立本地模型中心上增加受控模型执行能力，让用户版通过本地引擎和中心 HTTP 协议使用已登记的模型，而非继续在客户端配置任意模型 URL、可执行文件和权重绝对路径。中心继续独立进程，资产报告、模型绑定、运行能力、真实推理证据分别表达。用户版已有的本地模型用途全部纳入：Whisper 转写及可选 VAD、本地文本与可选视觉分析、Demucs 两种音源分离、Seed-VC 及三项内部依赖、Haar 正脸/侧脸检测；不是给十二个资产分别制造十二个互不相关的推理 API，也不将音色或人脸检测强行插入每个高光任务。

本规格是对 local-model-center 首期“只盘点、不运行模型”边界的显式后续扩展，仅新建的中心执行路由可以运行模型；原盘点、登记、只读报告和自动依赖检查仍不得触发推理。高光最终导出时长采用独立的服务端版本化策略，不复用 candidate_min_sec/candidate_target_sec/candidate_max_sec。实施目标包含从 `D:\一院烟火，岁岁向南` 的真实素材经用户版完整任务链生成一条可播放高光成片。

本次 Quick Spec 采用以下明确默认值，不等待再次澄清：覆盖用户版全部既有本地模型用途；服务器导出策略初始值为最小 20 秒、默认 60 秒、最大 120 秒；用户输入整数秒，目标成片数为 1；真实验收输出放在指定素材目录下新建且不覆盖已有文件的专用子目录。这些是待实施的规格，不是已实现或已通过推理验证的事实。已完成的基线提交推送为 3572089，后续不自动提交、推送、发布、升版本或部署生产。

## Requirements

### Requirement 1: 全部模型用途与责任边界

**User Story:** As a 使用者, I want 用户版统一通过引擎连接中心使用本地模型, so that 不必在客户端拼接模型路径和服务地址。

#### Acceptance Criteria

1. THE SYSTEM SHALL 使用户版新建中心路由任务通过“Flutter → 本地引擎 → 可信模型中心 HTTP → 受控组件”执行模型，不从 Flutter 直接访问运行组件或传入执行命令。
2. THE SYSTEM SHALL 覆盖 Whisper 批处理与真实流式转写、可选 Whisper VAD、本地文本评分、可选本地视觉、Demucs htdemucs/htdemucs_ft、Seed-VC 套件及 Haar 正脸/侧脸用途；Seed-VC 内部依赖 SHALL 不被伪装为独立用户功能。
3. WHILE 执行高光任务 THEN THE SYSTEM SHALL 只调用实际启用的转写、VAD、文本及视觉能力；音色与 Haar SHALL 按既有相关功能调用，缺少未启用可选项不得阻断无关任务。
4. THE SYSTEM SHALL 保持中心为独立进程，并将 FFmpeg 媒体准备、传统第二阶段出片、任务库、设备授权和服务器业务授权保留在其既有责任方。
5. THE SYSTEM SHALL 保留任意自定义 URL 接入 UI 的隐藏状态，不以本轮中心接入恢复可编辑 URL、shell 参数或客户端权重路径表单；云端文案/视频生成等非本地模型功能不被迁移到中心。

### Requirement 2: 中心模型绑定和执行身份

**User Story:** As a 维护者, I want 中心明确选择每个场景使用的模型及组件, so that 盘点发现的多个文件不会被随机选作执行输入。

#### Acceptance Criteria

1. THE SYSTEM SHALL 由中心持久化版本化场景绑定，包含能力、目录模型 ID、盘点组成组及文件身份、受控组件版本和必要设备参数，并采用 revision 冲突保护及独立数据目录。
2. WHEN 绑定存在多份模型、分片或 projector THEN THE SYSTEM SHALL 要求完整且明确的组成选择；Seed-VC 套件及 Demucs FT 不得跨无关组拼装，不得按第一文件、最小体积或文件名猜测兼容性。
3. THE SYSTEM SHALL 在管理版既有模型/中心区域编辑和查看本机中心绑定，明确其仅作用于当前机器；服务器保存的共享策略不得包含本机绝对路径，用户版只选择可用能力而不编辑绑定。
4. WHEN 新建中心路由任务 THEN THE SYSTEM SHALL 冻结绑定 revision、模型/组件身份和协议版本；执行前复核身份，文件改变、登记变化或组件不符时返回稳定错误，不隐式替换模型。
5. THE SYSTEM SHALL 区分文件存在、绑定完整、组件交付、受控启动、实际请求成功五类证据；原模型汇总报告存在三态的含义保持不变。
6. THE SYSTEM SHALL 不读取消费者 API Key、不将中心权重路径写回消费者历史配置，且不得自动迁移或清空历史绑定之外的配置与秘密引用。

### Requirement 3: 运行组件的真实交付

**User Story:** As a 使用者, I want 已启用模型有真正能执行的组件, so that 文件已发现不会被误报成推理已接通。

#### Acceptance Criteria

1. THE SYSTEM SHALL 为文本和视觉提供受控的 llama.cpp 兼容执行组件及协议适配，锁定构建输入、文件摘要、平台和许可证；视觉模型 SHALL 显式绑定所需 projector，不因同属 GGUF 即认定可用。
2. THE SYSTEM SHALL 实现并构建真正支持 fengshen.whispercpp.stream/v1 的原生流式入口，与既有稳定提交、时间戳、背压、取消和恢复契约一致；普通 whisper-cli 或 Go 调用端存在不得代替此实现。
3. THE SYSTEM SHALL 以预编译原生程序或 PyInstaller onedir 等独立二进制组件交付 Demucs 和 Seed-VC，解析中心选择的本地权重，不依赖宿主 Python/venv、首次联网下载或首启编译。
4. THE SYSTEM SHALL 复用项目已有 Go Haar 检测逻辑并提供中心 HTTP 适配，绑定实际加载的 XML 身份；不得把 Python/OpenCV 环境或 XML 文件存在当成人脸推理通过。
5. WHEN 构建输入、平台运行库、模型格式或 GPU 条件不满足 THEN THE SYSTEM SHALL 逐能力报告具体缺口，不创建占位程序、不伪造启动或推理报告、不静默切换云端。
6. THE SYSTEM SHALL 分别保留构建、无模型入口测试、无宿主开发环境测试及真实推理结果；一个组件的成功不得使其他组件或全部十二项自动显示通过。

### Requirement 4: 受信运行会话与媒体范围

**User Story:** As a 重视本机隐私的使用者, I want 只有实际任务能授权中心读取必要输入并执行, so that 只读报告能力不会变成任意程序或文件访问入口。

#### Acceptance Criteria

1. THE SYSTEM SHALL 沿用中心命名管道身份确认、构建绑定、字面量回环和无代理/重定向约束，并为运行任务签发独立短期、限定客户端进程/实例/任务/能力的凭据；只读凭据和浏览器管理凭据不能调用运行 API。
2. WHEN 用户显式提交或恢复已授权的中心任务 THEN THE SYSTEM SHALL 才建立运行会话；自动 status、report、deps 和打开管理页不得启动模型、加载权重或发起首次全量摘要。
3. THE SYSTEM SHALL 让引擎通过有界媒体上传取得任务内 opaque asset ID，运行请求只接受 asset ID、绑定引用和受限参数，不接受任意本机输入/输出路径、URL、模块名、命令或环境变量。
4. THE SYSTEM SHALL 校验媒体长度、格式、解码尺寸、资产归属和响应大小，采用流式接收及受限临时目录，禁止跨任务读取和模型文件下载；所有原素材只读。
5. THE SYSTEM SHALL 对管理、只读和运行 API 分别鉴权，保持 Host/Origin/Fetch-Site 与静态资源隔离；运行 token 不进入 URL、日志、磁盘、任务快照或模型子进程命令行。
6. THE SYSTEM SHALL 维持原设备与功能授权，在既有授权失败时不执行模型；本机运行会话不取代服务器授权，不使中心报告在线成为任务放行依据。

### Requirement 5: 异步执行、取消与进程所有权

**User Story:** As a 使用者, I want 长任务可观察和可取消, so that 模型阻塞不会冻结客户端或遗留失控进程。

#### Acceptance Criteria

1. THE SYSTEM SHALL 提供异步运行作业、逐阶段事件、结果查询和幂等取消，并将作业从启动 HTTP 请求生命周期分离；任务标识及重试标识 SHALL 防止未知请求结果导致重复执行。
2. THE SYSTEM SHALL 设定可测试的并发、排队、单次超时、输入/输出大小和临时容量上限，资源不足时排队或明确拒绝，不并发加载全部大模型。
3. WHEN 取消、超时、客户端会话过期或中心关闭 THEN THE SYSTEM SHALL 停止接收新输入、取消排队和执行中的所属作业、回收自身临时资产，并保持已完成结果与终态不可复活。
4. THE SYSTEM SHALL 仅复用经过进程映像、创建时间、受控组件及绑定核对的运行实例；不得仅凭端口或 HTTP banner 复用既有服务，不终止非本次拥有的进程。
5. THE SYSTEM SHALL 让客户端退出不停止独立中心；模型子进程由中心跟踪所有权和空闲回收，中心异常退出后的本次所属子进程不得继续无界运行。
6. WHILE 模型输出包含未知字段、非法时间戳、越界区间或损坏 JSON THEN THE SYSTEM SHALL 拒绝该结果并报告具体能力错误，不用伪造分数、空字幕或测试 fixture 补成功。

### Requirement 6: 高光完整执行链

**User Story:** As a 使用者, I want 高光任务真的使用中心模型分析并出片, so that 不再停留在资产到齐或管理页可打开。

#### Acceptance Criteria

1. THE SYSTEM SHALL 为新任务引入明确的中心执行路由及快照版本，在创建前检查所需绑定、组件和服务器时长策略；旧自定义 URL 路由继续禁止新启用，不将 center 路由等同任意 local URL。
2. THE SYSTEM SHALL 将转写、流式稳定提交、评分和启用时的视觉适配器接入既有高光管线，保持候选完整性、去重、进度、动态源队列、授权与恢复机制。
3. WHEN 高光启用可选视觉 THEN THE SYSTEM SHALL 通过中心实际执行视觉请求，并在失败时报告失败而非静默忽略；关闭视觉时不要求加载视觉模型。
4. THE SYSTEM SHALL 为中心路由任务保留可追踪的 episode、转写结果、候选/选中区间、模型绑定与作业引用，日志不记录完整素材文本或秘密，任务证据仅在本机保存。
5. THE SYSTEM SHALL 保持既有普通切片、云端高光、旧本地任务及历史执行快照的恢复语义，不改写旧快照或隐式切云、切模型和切分析链。
6. WHEN 素材、字幕、评分或完整性条件不足 THEN THE SYSTEM SHALL 明确报告耗尽、缺口或失败，不以随机截取、放宽阈值、重复填充或人工指定首分钟冒充高光分析。

### Requirement 7: 全部其他本地模型功能接入

**User Story:** As a 使用者, I want 音色和人脸功能也能使用中心, so that 全部本地模型用途共享同一受控执行入口。

#### Acceptance Criteria

1. THE SYSTEM SHALL 接通 Demucs htdemucs 与 htdemucs_ft 的中心音源分离请求，返回并验证人声及伴奏 WAV，保持声道、采样率与输入时长契约。
2. THE SYSTEM SHALL 接通 Seed-VC 中心音色转换请求，并真实使用其 Whisper Small、CAMPPlus、BigVGAN 配套绑定和用户选择的参考声音；参考声音不是新增模型权重。
3. THE SYSTEM SHALL 将分离和转换适配至用户版既有音色流水线，保留 DSP 处理、设备策略、音高变换和音画时长守恒，不复用 sys.executable 启动冻结 EXE。
4. THE SYSTEM SHALL 提供 Haar 正脸、侧脸与无人脸的结构化判断，并将用户版相关本地预检经引擎接入中心；已有服务器强制的人脸验证与授权不被本地预检绕过或替代。
5. THE SYSTEM SHALL 对每种能力分别执行真实小输入验证，Seed-VC 内部三项按套件证据记录，不伪造独立直接调用通过；未验证项保持未验证。

### Requirement 8: 服务器最终导出时长策略

**User Story:** As a 管理者, I want 在服务器规定高光成片最小、最大和默认时长, so that 用户可以在统一边界内选择实际导出长度。

#### Acceptance Criteria

1. THE SYSTEM SHALL 提供独立版本化的高光导出策略 export_duration_seconds，含 min、max、default 三个整数秒字段，并校验 1 <= min <= default <= max <= 600；新策略初始值为 20/120/60。
2. THE SYSTEM SHALL 将该策略置于管理版既有高光处理配置区域，支持读取、修改、版本冲突、历史只读与明确保存反馈，不修改文案/视频生成的 duration_seconds 配置。
3. THE SYSTEM SHALL 通过既有鉴权的服务器公开投影/resolve 链向用户版和适用开发版提供策略与独立 version/SHA256 引用，不要求取得云 Provider 秘密才能读取本地中心任务的时长策略。
4. THE SYSTEM SHALL 将导出策略与 candidate_min_sec/candidate_target_sec/candidate_max_sec 分开序列化与校验；修改导出策略不得改变候选窗口、旧高光 canonical JSON 或历史哈希。
5. WHEN 策略不可用、无有效版本、签发/传输失败或不一致 THEN THE SYSTEM SHALL 在新任务创建前返回明确错误，不用客户端硬编码默认值绕过服务器边界；已冻结策略的续跑按原快照执行。

### Requirement 9: 用户时长选择和任务冻结

**User Story:** As a 使用者, I want 修改本次高光实际导出时长, so that 服务器默认值不会强制所有成片等长。

#### Acceptance Criteria

1. THE SYSTEM SHALL 在用户版高光任务向导展示只读最小/最大时长、默认值和可编辑本次每条成片时长；初次加载选默认值，刷新不得覆盖用户仍合法的已编辑值。
2. WHEN 请求创建或复制为新高光任务 THEN THE SYSTEM SHALL 使用服务端当前策略校验 export_duration_sec，未填写采用该策略默认，越界、非整数、零、非法 JSON 类型 SHALL 在写任务和执行快照前拒绝，不静默截断到边界。
3. WHEN 页面持有的策略引用与创建时服务器版本不一致 THEN THE SYSTEM SHALL 返回冲突并展示新范围，保留原输入等待明确重试，不自动重新提交任务。
4. THE SYSTEM SHALL 在任务创建成功时冻结用户时长、完整策略、策略引用与中心执行身份，并在摘要、任务详情和完成证据中展示请求时长与实测时长；重试、续跑及当前配置变化不得修改它们。
5. THE SYSTEM SHALL 对适用开发版入口和共享 Dart/Go DTO 同步契约，不给普通切片或视频生成新增无关必填项；所有历史无该字段任务仍走旧语义。

### Requirement 10: 时长执行与音画安全

**User Story:** As a 使用者, I want 导出的文件确实符合本次时长并可正常播放, so that 界面设置不会被第二阶段随机时长覆盖。

#### Acceptance Criteria

1. THE SYSTEM SHALL 在候选选择、源区间确定和传统第二阶段合成中传递被冻结的导出时长，禁止后续随机 TargetDuration 覆盖；最终文件须匹配请求值，误差只允许一个输出视频帧与一个音频编码帧量级的明确容差。
2. WHEN 选中事件长度不同于目标 THEN THE SYSTEM SHALL 只在同集有效素材内按合法上下文扩展或调整并重新确认高光与完整性条件；不能满足时跳过候选，不跨集随意拼接、重复补帧、静音填充或变速凑时长。
3. THE SYSTEM SHALL 在输出前确认可实现区间及帧量化，并在输出后用真实 FFprobe 校验音轨/视频轨、首尾时间、实测时长和音画时长差；失败文件不得计入已完成成片。
4. THE SYSTEM SHALL 保持既有音画同步、音频采样率/时间基、片段音频裁剪和验证约束，不以取消字幕边界检查、放宽评分或跳过授权换取一次出片成功。
5. WHEN 已扫描素材无法形成满足策略的成片 THEN THE SYSTEM SHALL 返回明确不足或耗尽状态与原因，不保存零字节 MP4 或报告目标已达到。

### Requirement 11: 界面、依赖与报告一致性

**User Story:** As a 使用者, I want 在现有页面区分中心在线、模型可运行和任务进度, so that 不被“十二项存在”误导。

#### Acceptance Criteria

1. THE SYSTEM SHALL 在既有用户版任务、依赖和管理版模型区域显示运行能力、绑定缺口、活动请求与错误，不新增三端侧栏或将浏览器中转页当功能实现。
2. THE SYSTEM SHALL 保持依赖检查逐行反馈、并发和完成事件，按当前功能判断必需与可选；只读检查不启动中心或模型，已存在不升级为推理通过。
3. THE SYSTEM SHALL 由中心 HTTP 汇总提供资产存在与独立运行证据，客户端不得自行扫描或从文件存在推断 ready；中心过期/断连/组件改变时正确失效。
4. THE SYSTEM SHALL 覆盖窄屏、键盘操作、取消、错误保留、长路径文本与迟到响应保护；按钮绑定真实接口，GUI 迭代遵守仅热重载的约束。

### Requirement 12: 真实素材闭环及验证纪律

**User Story:** As a 维护者, I want 使用指定真实素材验证完整链路, so that 完成结论有真实推理和成片证据。

#### Acceptance Criteria

1. THE SYSTEM SHALL 在隔离本地测试环境中，经有效授权的用户版本地引擎任务创建与运行接口，使用 `D:\一院烟火，岁岁向南` 按集数顺序发现的真实素材，按服务器默认 60 秒目标实际生成一条高光成片；有效策略被明确修改时以任务冻结策略为准并记录差异。
2. THE SYSTEM SHALL 将本次输出置于素材目录中新建且唯一的高光验收子目录，不覆盖已有“切片”或其他输出；原素材、权重与历史配置的内容不得修改。
3. THE SYSTEM SHALL 保存任务 ID、源集/区间、执行绑定、真实模型作业、转写/评分证据、策略引用、成片 SHA-256、FFprobe 及播放检查结果，未执行的步骤不得标为通过。
4. THE SYSTEM SHALL 提供不依赖真实权重的默认单元/契约测试，以及显式启用的各模型真实推理、原生流式与媒体端到端测试；mock 成功不得替代实际模型调用或该真实成片。
5. THE SYSTEM SHALL 实际运行受影响的 Go test/vet/build、Dart 定向测试/分析，并验证真实界面正常与错误流程；若 GUI 目标不可热重载、授权或构建输入缺失，保持对应项未完成并说明阻断，不擅自重启或绕过。
6. THE SYSTEM SHALL 不自动提交、推送、生产部署、发布、升级应用版本，不删除/搬迁/清理模型，不安装大量工具冒充输入齐备；本轮只对目标功能所需的本机模型执行作显式扩展。

## Out of Scope

- 重新开放任意模型 URL、任意 EXE/脚本执行、远程中心或 LAN 服务。
- 将云文案与云视频生成服务改成未经规格定义的本地生成模型。
- 修改授权、计费、设备绑定或服务器强制验证以绕过本机验收阻断。
- 修改已有任务快照、自动迁移消费者路径和秘密、模型缓存清理或重复文件删除。
- 以文件完整、元数据入口测试或十二项资产存在代替真实推理和最终成片。
- 自动发版、生产参数修改、安装包发布、GUI 重启/重打包或新的提交推送。
