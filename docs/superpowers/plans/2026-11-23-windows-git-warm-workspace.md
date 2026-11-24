# 预热工作区 Windows Git 实施计划

**目标** 让 Windows 盘上的 WSL2 原生预热槽位优先使用 Windows Git，减少从分配槽位到首个节点，以及归还、重开的等待时间。

**设计依据** 此次运行从分配到首节点耗时 24 分 34 秒，远端 fetch 约 6 秒。同一目录直接运行 Windows Git status 耗时 0.28 秒，Runner 复制当前索引后耗时 1.48 秒。使用从同一提交重建的索引时，10 秒内仍在刷新文件状态并已启动 LFS 过滤器。WSL Git 直接检查时仅预读文件状态就耗时 17.8 秒。历史运行缺少逐命令计时，因此这些数据用来选择测量点，实际提速以改造前后的对照为准。

## 操作分配

| 操作 | 执行方式 |
| --- | --- |
| 领取、重开、恢复时读取真实工作树的 HEAD、refs、索引、状态、差异和忽略规则 | 已登记且满足兼容条件的 Windows 槽位优先走 Windows Git。每个操作先归属到真实检出或隔离验证目录，再决定调用位置 |
| 从受信缓存导入 Git 对象、切换提交、更新任务分支、挂接 HEAD、写 Git 身份 | 同一 Windows Git 写入链。缓存到检出的对象导入只允许选定的本地传输，并验证检出对象库仍可独立读取 |
| Git LFS 本地处理 | Windows Git LFS 处理符合条件的本地过滤；沿用验证后增量落盘和可恢复的落盘记录。文件落盘后由 Windows Git 刷新真实索引，保留可复用的文件状态 |
| 归还前证明、归还提交前复核、精确恢复证明 | Windows 端批量枚举和读取符合条件的文件，复用现有批量验证。保留实际字节、索引、忽略规则及归档校验。特殊路径和内容转换由现有等价校验处理 |
| 远端 fetch、ls-remote、授权证明、LFS 下载和 WSL 私有缓存上的对象操作 | 走现有 Runner 认证和缓存路径。后续优化以阶段耗时为依据，先覆盖实际访问 Windows 工作树和真实索引的调用 |
| 页面及 SOP 触发的 Runner 托管 Git 操作 | 工作区 diff、历史、提交详情和基线文件读取优先走 Windows Git。任务分支同步的本地检查、暂存和提交复用同一执行层，带凭据传输沿用现有入口 |

执行边界设在预热工作区的 Runner 托管操作。Agent 沙箱内的命令由其现有隔离执行环境处理。Linux 文件系统、Docker 槽位及无法通过 Windows 能力检查的槽位走各自现有路径。

## 分步实施

### 1. 建立分阶段基线与 Windows Git 调用边界

**文件** `internal/runner/warm_native_windows_host.go`、`warm_native_windows.ps1`、`warm_native_git.go`、`warm_workspace_access.go`、`warm_native_windows_linux_test.go`。

- [ ] 记录按 run、操作名及执行端关联的耗时。覆盖远端 fetch、本地对象导入、检出前检查、分支切换、LFS 获取与落盘、就绪前检查、归还证明及复核。记录能力检查失败和回退原因，过滤凭据、命令参数中的敏感数据及宿主绝对路径。
- [ ] 扩展现有 Windows helper，通过明确的操作及结构化参数区分只读查询、真实索引刷新和检出写入。复用登记路径与租约挂载的同一性检查。验证 Windows Git、所需 Git LFS、文件语义及目标路径。将 Windows Git 取消与退出纳入 Runner 的工作区操作生命周期。
- [ ] 新增测试覆盖能力探测失败、路径含空格和非 ASCII 字符、错误保留及进程超时回收。在写操作开始前选定执行端。写入后如遇取消、超时或错误，等待子进程退出并使用现有持久化意图判定接续操作。

### 2. 将领取和恢复的本地检出切到 Windows Git

**文件** `internal/runner/warm_native_git.go`、`warm_native_checkout.go`、`warm_native_lfs.go`、`warm_workspace_access.go`、`warm_native_incremental_linux_test.go`、`warm_native_windows_linux_test.go`。

- [ ] 在现有独占租约和工作区操作锁内，用同一执行端完成检出前检查、对象导入、分支和 HEAD 更新、身份写入及就绪前检查。保留远端已验证的目标提交、分支 compare-and-swap、忽略文件冲突保护和 Unity 占用检查。
- [ ] LFS 落盘并验证后，按变更路径刷新真实索引，再执行覆盖整个工作树的就绪检查。允许 Git 更新索引中的文件状态缓存。测试索引中的对象 ID、模式、暂存区内容和保护标记与预期一致，检查失败时保留操作者的文件和本地提交。
- [ ] 扩展现有增量检出测试，覆盖同提交同分支、同提交换分支、不同提交含 LFS、已修改文件、暂停后重开，以及对象导入、分支更新和 LFS 替换间的中断恢复。验证工作区就绪后能被 Windows Git 与 Unity 从原路径打开。

### 3. 覆盖归还链路和托管 Git 入口

**文件** `internal/runner/warm_native_proof.go`、`warm_native_return.go`、`warm_workspace_release.go`、`warm_workspace_ignores.go`、`workspace.go`、`work_branch.go`。

- [ ] 复用 Windows 端批量读取完成归还证明和最终复核。减少热路径逐文件跨 WSL 边界的调用。保持 LFS 已验证对象复用、归档摘要校验、归还回执和最后一次复核。通过现有归还测试锁定编辑后拒绝、旧回执、重启重试和多槽位部分归还。
- [ ] 从 `runnerReadWorkspaceDiff`、`runnerCollectWorkspaceLog`、`runnerCollectWorkspaceCommitFiles`、`runnerReadWorkspaceGitBlob` 接入只读操作。沿 `syncTaskWorkBranches` 拆出有明确边界的本地检查、暂存、提交和拉取后的本地更新。保持原有权限和响应格式。提交和其他本地写操作只有在同一租约校验、路径校验和并发控制可复用时才切换到 Windows Git。
- [ ] 对剩余 WSL Git 调用记录具体原因并验证。优先完成真实工作树和索引的调用。对私有仓库对象计算，只有阶段基线显示值得迁移，并能通过路径、对象格式和认证等价测试时才切换。

### 4. 验收与上线

**文件** `docs/warm-workspace-wsl2.md`，以及上述 Windows 原生槽位集成测试。

- [ ] 在真实 Windows 盘上的独立临时槽位跑端到端测试。使用同一远端、同一对提交、同一文件和 LFS 规模对比升级前后。分别测量冷索引和热索引下的无变更领取、增量检出、精确重开与归还。每种场景至少重复三次，报告中位数、最慢值及各阶段耗时。
- [ ] 建议验收线为有可复用索引的领取和常见增量切换，本地准备中位数至少缩短 50%。冷索引、归还和精确重开单列结果，按相同条件比较。每次测试确认工作区使用的执行端、实际命令退出原因、Git 状态和 LFS 内容。
- [ ] 运行 `go test -count=1 ./internal/runner ./internal/gitwork ./internal/engine`，同时在 WSL 私有挂载命名空间下运行 Windows 槽位集成测试。核对 Windows Git 不可用、DrvFS 开启 metadata、纯 Linux 和 Docker 的回归结果。
- [ ] 先在独立 Windows 槽位灰度，更新运行手册。部署和回退均先暂停该槽位并等待现有租约归还。沿用已有租约和日志格式，验证新版可以读取存量工作区状态。

## 实施与验证记录

- Windows 槽位的分支和 HEAD 写入、提交者身份更新、受保护的索引刷新以及忽略规则查询已接入 Windows Git。原生归还与精确恢复继续使用已有 Windows 批量字节校验和完整证明。
- 受信对象缓存的本地导入继续通过 WSL Git 完成。它依赖 WSL 路径和对象库布局，且这一步与索引刷新分开计时。页面读取和 SOP 交付仍在现有隔离 Git helper 内执行，Windows 互操作需要不同的隔离实现才能接入这两个入口。
- 在临时 NTFS 槽位用同一对提交验证。夹具有约 1,800 个跟踪文件和 24 MiB LFS 数据，每次增量检出变更一个 4 MiB 资产。优化后的一轮复测与同序号基线样本相比，热索引增量检出从 17.11 秒降至 12.76 秒，冷索引从 25.86 秒降至 14.04 秒，无变更领取从 2.94 秒降至 2.77 秒。归还和精确重开未观察到稳定提速，增量检出未达到建议的 50% 中位数验收线。
