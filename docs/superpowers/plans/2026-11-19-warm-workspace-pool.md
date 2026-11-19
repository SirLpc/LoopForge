# 预热工作区池 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让选择预热模式的项目在 Runner 上独占租用运维准备的 Git 检出和 Unity `Library`，容量不足时让 SOP 和对话排队。

**Architecture:** `ProjectsRepo` 保存模式，`RunnersRepo` 保存 Runner 广告和可取消的等待请求，`RunsRepo` 保存工作区归属和恢复引用。Runner 持有持久的本机分配记录，按租约代次把同一份真实检出迁入既有运行布局。引擎复用现有 SOP、对话、IDE 和维护入口，将预热回收接到原有生命周期。

**Tech Stack:** Go 1.26、GORM／MySQL、Git／Git LFS、Runner 控制连接、现有 Docker 和 native 沙箱、Studio 与聊天工作台。

**Spec:** `docs/superpowers/specs/2026-11-19-warm-workspace-pool-design.md`

## 全局约束

- 配置默认「普通检出」，只有新建的仓库型工作区读取项目新设置。子 SOP 和借用工作区以实际工作区所有者为准。
- 预热请求等待兼容槽位，可停止或取消。队列等待分离于 Git、CLI 和 Runner 命令执行时长。
- 预热工作区禁止创建、上传或恢复整目录快照。保留尚未交付的用户数据，已归还的工作区用精确 Git 引用和归档产物恢复。
- 保留每个登记目录自身的 `Library` 和未变化的已检出文件。跨项目、跨仓库、跨信任范围和代次不符的请求必须被拒绝。路径和授权材料留在现有隔离边界内。
- 遵守根目录 `AGENTS.md` 的 Repo、handler 和迁移归属。使用增量 DDL，测试数据仅进入测试入口。前端改动遵循 `design.md`。
- 为减少 fork 同步冲突，在现有领域包内用独立文件承载预热池专属逻辑和测试。`daemon.go`、`node_execute.go`、`assistant_workspace_open.go` 等上游入口仅放必要的装配、模式分派或安全检查。表单和状态组件沿用现有接入点，避免重排上游代码。
- 每个任务独立完成红绿测试和局部验证。整套路径接入并完成拒绝条件测试后再发布 Runner 预热能力。

## 审阅重点

- Runner 在认领或归还中途重启，并收到旧代次的重复确认。任务 2、4 和 6 验证归属与可恢复性。
- 第二个项目登记指向同一目录的路径，或工作区进程替换 Git 配置、路径链接。任务 2 和 3 验证本地边界。
- 单槽位对话切换同一仓库，旧目录仍被原生 CLI、终端或未交付工作占用。任务 7 验证交接和取消。
- 已跟踪的 `Library`、本地引用只保留名字、同名产物字节不同或授权被撤销。任务 3 和 6 验证归还依据。
- 两个控制实例同时分配并且会话取消恰好遇上认领成功。任务 4、5 和 7 验证队列落库与陈旧确认的处理。

---

### 任务 1 · 项目设置和持久类型

**文件** `internal/domain/project.go`、新建 `internal/domain/warm_workspace.go`、`internal/domain/observability.go`、`internal/repo/models.go`、`internal/repo/projects.go`、`internal/httpapi/projects.go`。测试新增 `internal/repo/project_workspace_mode_test.go` 和 `internal/httpapi/project_workspace_mode_test.go`。

**接口** `domain.WorkspaceMode` 使用 `checkout` 和 `warm_pool`，`domain.Project.WorkspaceMode` 和 `domain.TelemetryContext.WorkspaceMode` 共享类型。`domain.WarmWorkspaceDemand` 包含 `OwnerRunID`、`RunnerID`、`ProjectID` 和 `RepositoryIDs`。`domain.WarmWorkspaceLease` 包含 `OwnerRunID`、`RunnerID`、`Generation uint64` 和 `Slots map[string]string`，映射仓库 ID 到槽位 ID。

- [ ] 在 `TestProjectWorkspaceModeRoundTrip` 和 `TestProjectWorkspaceModeAPI` 固定断言 `legacy.WorkspaceMode == domain.WorkspaceModeCheckout`、配置保存后为 `domain.WorkspaceModeWarmPool`、旧 PUT 省略字段保留已有值、非法值返回 HTTP 400，项目无写权限返回现有的权限错误。
- [ ] 运行 `go test ./internal/repo ./internal/httpapi -run 'TestProjectWorkspaceMode' -count=1`，确认失败来自缺少模式契约。
- [ ] 将模式映射到 `projectRow` 的增量字段。在 `ProjectsRepo.PutProject` 保留省略值，在请求入口校验枚举和已有项目写权限。仅定义运行上下文字段，具体冻结在任务 5 和 7 接入。
- [ ] 重跑模式专项测试和 `go test ./internal/domain ./internal/repo ./internal/httpapi -run 'Test.*Project' -count=1`。
- [ ] 用 `feat: persist project workspace mode` 提交本任务文件。

### 任务 2 · Runner 目录登记和本机分配记录

**文件** 新建 `internal/runner/warm_workspace_pool.go`、`internal/runner/warm_workspace_state.go`、`internal/runner/warm_workspace_pool_test.go`，修改 `internal/runner/daemon.go` 和 `cmd/agentflow-runner/main.go`。

**接口** `runner.Config.WarmWorkspacePoolFile string` 由 `AGENTFLOW_RUNNER_WORKSPACE_POOL_FILE` 或 `-warm-workspace-pool-file` 提供。`openWarmWorkspacePool(configPath, workspaceRoot, statePath string) (*warmWorkspacePool, error)` 读取 `slots` 对象列表，每条含 `id`、`projectId`、`repositoryId`、`path`、`unityProjectPath`、可选的 `discardGenerated`。`(*warmWorkspacePool).reserve(lease domain.WarmWorkspaceLease) error` 持久化本机意图，`(*warmWorkspacePool).place(ctx context.Context, lease domain.WarmWorkspaceLease, repoDirs map[string]string) error` 移入工作区，`(*warmWorkspacePool).returnSlots(ctx context.Context, lease domain.WarmWorkspaceLease) error` 完成由任务 6 授权的归还。

- [ ] 在 `TestWarmWorkspacePoolPaths` 断言重复路径、父子重叠、软链接和不同文件系统拒绝。`TestWarmWorkspacePoolMoveAndRestart` 用 `os.SameFile(before, after)` 检查迁移为同一 inode，模拟仅部分仓库移动、磁盘与持久记录之间中断，断言重新打开后旧代次无法迁移或归还，新代次只在已安全归还后可使用。
- [ ] 运行 `go test ./internal/runner -run '^TestWarmWorkspacePool' -count=1`，确认测试在功能缺失处失败。
- [ ] 实现配置解析、真实路径检查和状态文件独占锁。每个多仓库转换先写入持久意图，再校验目录 inode、归属、代次和目标位置，按档案原子改名，最后提交状态。单步失败保留可重试状态，无法确定归属的槽位标记待检查。启动时先核对操作记录和实际路径，再允许声明空闲。任务 6 负责证明归还前进程已退出。
- [ ] 重跑专项测试并运行 `go test -race ./internal/runner -run '^TestWarmWorkspacePool' -count=1`。
- [ ] 用 `feat: journal runner warm workspace directory leases` 提交本任务文件。

### 任务 3 · 暖检出和 Git 元数据隔离

**文件** 新建 `internal/runner/warm_workspace_git.go`、`internal/runner/warm_workspace_git_test.go`。修改 `internal/domain/warm_workspace.go`、`internal/runner/repository_checkout.go`、`internal/runner/run_sandbox.go`、`internal/runtime/sandbox_docker.go` 和 `internal/runtime/sandbox_process.go` 中的可信缓存挂载路径。

**接口** `prepareWarmCheckout(ctx context.Context, checkout, cache string, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef) error`。`domain.WarmWorkspaceRef` 持久化 `RepositoryID`、`Branch` 与 `Commit`，空分支表示 detached HEAD。Runner 本机信任的对象缓存与活动 `.git` 各自独立，沙箱的可信缓存只读可达。

- [ ] 在 `TestWarmCheckoutPreservesLibraryAndUsesLatestRef` 用两个基线提交断言 `HEAD` 等于远端最新提交，`Library` 哨兵和未变化源文件保留 inode。`TestWarmCheckoutGitIsolation` 断言前次本地对象、配置和 hook 无法进入新租约，伪造远端、跟踪文件在 `Library` 下及软链接缓存均拒绝。`TestWarmCheckoutBranchModes` 包含新建、重名拒绝、续接、只读及按精确旧提交恢复。使用真实 Git LFS 夹具验证目标对象落盘并保留未变化资产。
- [ ] 运行 `go test ./internal/runner -run '^TestWarmCheckout' -count=1`，确认测试体现缺失的准备路径。
- [ ] 将运维准备的 Git 对象接入 Runner 私有的可信远端缓存。之后只凭匹配的冻结远端和现有授权传输更新缓存。为每个租约生成独立可写 Git 元数据，使用 Git alternates 读取固定的只读本机对象路径。Runner 按已验证的原 checkout 基线构造索引，再借现有分支语义增量 checkout。应用认证前校验仓库 ID 与远端，避开可写仓库内的 hook、配置和 credential helper。Git LFS 物化只读取目标引用需要的资产，工作区释放时隔离租约的未验证对象。Docker 与 native 分别验证该缓存路径的只读约束。
- [ ] 重跑 `go test ./internal/runner ./internal/runtime -run 'TestWarmCheckout|Test.*Sandbox' -count=1`，验证支持的沙箱中 Git 原生命令可读仓库且无法写入对象缓存。
- [ ] 用 `feat: reuse verified git checkouts for warm leases` 提交本任务文件。

### 任务 4 · 持久排队和 Runner 协议

**文件** 新建 `internal/repo/runner_warm_workspaces.go`、`internal/repo/runner_warm_workspaces_test.go`、`internal/runner/warm_workspace_control.go`。修改 `internal/repo/models.go`、`internal/domain/runner.go`、`internal/domain/runner_workspace.go`、`internal/runner/client.go`、`internal/runner/daemon.go`、`internal/httpapi/runners.go` 和 `internal/engine/workspace_checkpoint_transport.go`。

**接口** `RunnersRepo.EnqueueWarmWorkspace(demand domain.WarmWorkspaceDemand, now time.Time) error`、`ReserveNextWarmWorkspace(runnerID string, now time.Time) (domain.WarmWorkspaceLease, bool, error)`、`WarmWorkspaceAllocation(ownerRunID string) (domain.WarmWorkspaceLease, domain.WarmWorkspaceState, bool, error)`、`CancelWarmWorkspace(ownerRunID string, now time.Time) error` 和 `SettleWarmWorkspace(lease domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) error`。`WarmWorkspaceState` 使用 `queued`、`reserved`、`ready`、`releasing`、`released`、`canceled` 和 `blocked`。`domain.WarmWorkspaceSlot` 由 `ID`、`ProjectID`、`RepositoryID`、`State` 组成，通过心跳报告本机状态。`domain.RunnerWorkspaceRequest.WarmWorkspaceLease` 传递已验证的 `*domain.WarmWorkspaceLease`，操作值使用 `warm_prepare`、`warm_status`、`warm_release_check` 和 `warm_release_commit`。

- [ ] `TestWarmWorkspaceQueueOldestEligible` 验证任务和对话同队列，多个仓库全量分配、后续兼容请求可绕过无法满足的组合，已绑定所有者去重。`TestWarmWorkspaceQueueMySQLConcurrentCancel` 用两个 repo 连接并发认领同一槽位，断言仅一份 reservation 生效。取消后迟到的 ready 不可交给用户，状态仍可完成受控归还。`TestWarmWorkspaceControlUsesTrustedOwner` 验证旧版本拒绝和伪造所有者／机器／代次拒绝。
- [ ] 运行 `go test ./internal/repo ./internal/httpapi ./internal/runner -run 'TestWarmWorkspaceQueue|TestWarmWorkspaceControl' -count=1`，确认缺少队列与协议入口。
- [ ] 按 Runner 事务门闩串行挑选最早可满足需求，分配唯一槽位并递增 Runner 本机范围的持久代次。心跳记录可用性，活跃 reservation 单独落库，使用短控制请求和幂等状态查询协调本机结果。Git 准备保留独立的执行超时和进度。对还未确认的超时继续查询同一代次，控制面取消后撤销排队或安排安全归还。旧 Runner 或历史工作区从能力校验走原有路径。
- [ ] 运行专项命令，配置一次性测试库时再运行 `go test ./internal/repo -run '^TestWarmWorkspaceQueueMySQLConcurrentCancel$' -count=1 -v`，确认它实际执行。用控制连接测试模拟 Runner 离线和上线继续同一 reservation。
- [ ] 用 `feat: persist the shared warm workspace queue` 提交本任务文件。

### 任务 5 · SOP、工作区访问及执行门闩

**文件** 新建 `internal/engine/warm_workspace.go`、`internal/engine/warm_workspace_test.go`。修改 `internal/engine/run_start.go`、`internal/engine/run_recovery.go`、`internal/engine/cloud_pool.go`、`internal/engine/node_execute.go`、`internal/engine/workspace_preparation.go`、`internal/engine/runner_dispatch.go`、`internal/runner/workspace.go` 和 `internal/runner/daemon.go`。

**接口** `(*engine.Engine).EnsureWarmWorkspace(ctx context.Context, run domain.Run, actorID int64) (domain.WarmWorkspaceLease, error)` 为工作区所有者安排和等待分配。`(*engine.Engine).WarmWorkspaceStatus(runID string) (domain.WarmWorkspaceState, error)` 给持久状态。所有存在真实预热检出的 Runner 任务和请求以可信 `WarmWorkspaceLease` 定位，通过 `workspaceRunID(run)` 对齐共享工作区。

- [ ] `TestWarmWorkspaceSOPWaitsBeforeDispatch` 用三份运行、两个槽位断言前两份执行，第三份有 `workspace.pool_waiting` 且没有已认领 Runner 任务。取消排队后留下零绑定。`TestWarmWorkspaceSharedOwnerAndNoFallback` 断言子 SOP、脚本、终端和工作区读取用父工作区代次，配置无效及占满状态与普通检出分离，已有任务不会被切换到别的 Runner。
- [ ] 运行 `go test ./internal/engine -run '^TestWarmWorkspaceSOP|^TestWarmWorkspaceShared' -count=1`，确认失败在等待、分配或续用契约。
- [ ] 在新建仓库型根运行冻结模式和需求集合。个人执行沿用已选机器，云端 SOP 从支持完整组合的 Runner 选择。使用可恢复调度循环驱动任务 4 的预约，本机确认后才启动 CLI，恢复时按已有预约继续。把准备、终端、文件浏览与编辑、Git 交付、脚本、预览和浏览器入口接到已分配的目录，借用沿用已有权限入口。保持相同工作区所有者的访问门闩，代次不符时立即拒绝 Runner 文件操作和执行。
- [ ] 重跑专项测试与 `go test ./internal/engine ./internal/runner -run 'Test.*Workspace|Test.*RunnerDispatch' -count=1`。
- [ ] 用 `feat: admit SOP work through warm workspace leases` 提交本任务文件。

### 任务 6 · 无整目录快照的归还与精确恢复

**文件** 新建 `internal/runner/warm_workspace_release.go`、`internal/runner/warm_workspace_release_test.go`、`internal/repo/warm_workspace_refs.go`。修改 `internal/engine/workspace_cleanup.go`、`internal/engine/conversation_workspace.go`、`internal/engine/conversation_workspace_save.go`、`internal/engine/workspace_artifacts.go`、`internal/engine/workspace_checkpoint_stream.go`、`internal/engine/workspace_restore.go`、`internal/runner/run_sandbox.go`、`internal/runner/workspace.go`、`internal/httpapi/assistant_session_cleanup.go`。

**接口** `(*warmWorkspacePool).proveReturn(ctx context.Context, lease domain.WarmWorkspaceLease, repos []domain.RunnerRepository) ([]domain.WarmWorkspaceRef, error)` 在进程围栏内只读检查。`(*RunsRepo).RecordWarmWorkspaceRefs(runID string, lease domain.WarmWorkspaceLease, refs []domain.WarmWorkspaceRef) error`、`WarmWorkspaceRefs(runID string) ([]domain.WarmWorkspaceRef, error)` 持久化精确恢复信息。`(*engine.Engine).ReleaseWarmWorkspace(ctx context.Context, run domain.Run, actorID int64) error` 编排两阶段确认。

- [ ] `TestWarmWorkspaceReleaseProtectsLocalData` 包含工作树、暂存、未跟踪、忽略文件、子模块、stash、脱离 HEAD、指向远端提交的本地标签／分支、签入的 `Library`、同名不同字节产物和撤销 Git 权限，每种都断言目录仍归原所有者。`TestWarmWorkspaceReleaseKeepsLibraryAndRestoresExactCommit` 验证归还时录入引用、目录可重领、恢复旧 SHA、`Library` 保留，且创建、导出、上传和恢复整目录快照的调用次数均为 0。`TestWarmWorkspaceReclaimDuringRestart` 在引用提交前后及目录迁移后模拟进程重启，断言未完成归还不会释放给下一个所有者。
- [ ] 运行 `go test ./internal/runner ./internal/engine -run '^TestWarmWorkspaceRelease|^TestWarmWorkspaceReclaim' -count=1`，观察红灯。
- [ ] 在现有工作区所有者的独占门闩下，先停并验证无关联容器、原生进程、IDE/终端或任务。自动回收以 SOP 负责人或最近的授权对话仓库操作者进行只读远端验证。有排队需求时优先检查空闲对话。仅确认为干净且可恢复时保留引用，检查归档路径与字节后再清理租约控制文件及运维明确允许的生成目录，移动并释放槽位。远端或文件无法证明时返回原因并保留全部用户文件与入口。仍归属租约的工作区先安全归还，再删除。以相同代次完成分步确认，在已有自动清理、快照保存、快照恢复和远端传输入口检查冻结模式。
- [ ] 重跑专项命令及 `go test ./internal/repo ./internal/engine ./internal/runner -run 'Test.*Workspace|Test.*Checkpoint' -count=1`，覆盖终端仍运行及旧代次回收重试。归还后执行入口用任务 3 和已记录的旧 SHA 恢复。
- [ ] 用 `feat: release warm workspaces without full checkpoints` 提交本任务文件。

### 任务 7 · 项目对话、切换与停止

**文件** 新增 `internal/httpapi/assistant_warm_workspace.go`、`internal/httpapi/assistant_warm_workspace_test.go` 和 `internal/repo/assistant_warm_workspace.go`。在 `internal/engine/chat_workspace.go`、`internal/httpapi/assistant_execution.go`、`internal/httpapi/assistant_workspace_open.go`、`internal/httpapi/assistant_workspace_turn.go`、`internal/httpapi/assistant_generation.go`、`internal/httpapi/assistant_resume_recovery.go` 和 `internal/repo/assistant_generations.go` 添加必要接入点。

**接口** `workspaceOpenChoice.RepositoryID string` 只由登记项目仓库的预热入口使用。`(*Engine).StartRepositoryConversationWorkspace(input string, actor domain.TelemetryContext, sandboxID string, repository domain.RepositoryContext) (domain.Run, error)` 冻结一个仓库。用现有生成记录保存待处理的 `workspaceOwnerRunID` 与 `generationID` 关联，按任务 4 的队列状态并经代次校验恢复。

- [ ] `TestWarmWorkspaceConversationQueuesAndResumes` 断言已有 SOP 占满目录时返回可见排队状态，断开浏览器或重启控制面后，归还会唤醒同一轮次。`TestWarmWorkspaceConversationSwitchOneSlot` 分别验证旧目录干净时切换可完成、旧目录脏或终端未关闭时保留绑定并提示、停止时队列和转租取消。`TestWarmWorkspaceConversationAuthorization` 覆盖外项目 ID、切换发言人 Git 权限、已绑定但缺登记的机器、普通空白工作区、借用 SOP 和旧客户端。
- [ ] 运行 `go test ./internal/httpapi -run '^TestWarmWorkspaceConversation' -count=1`，确认缺少入口、等待或交接逻辑。
- [ ] 在工具说明提供项目仓库标识，按运行环境和仓库 ID 找对应会话工作区。首次云端选机优先选择可租用项目默认仓库的机器，现有会话按已选机器校验。把等待记在生成记录，进入等待时停止当前有写权限的原生 CLI 阶段，先对原目录结清本轮使用。干净原目录可参与分配，唯一匹配但有未交付改动的原目录保留原绑定。新目录就绪后经现有模式交接继续生成，停止以现有 generation 取消信号撤销预约，重连对账后可以继续原请求。轮次结束走任务 6 的归还策略。
- [ ] 重跑专项命令及 `go test ./internal/httpapi ./internal/engine -run 'Test.*ConversationWorkspace|Test.*WarmWorkspace|Test.*GenerationReap' -count=1`。
- [ ] 用 `feat: queue repository workspaces for project conversations` 提交本任务文件。

### 任务 8 · 配置界面、排队提示与交付验证

**文件** 修改 `web/app/projects.js`、`web/app/runs.js`、`chatbot/src/useChat.js`、`chatbot/src/activity.js`、`chatbot/src/ActivityTrace.jsx`、`docs/conversation-workspaces.md` 和 `internal/runner/daemon.go`。新增 `docs/warm-workspace-pool.md`、`web/project_workspace_mode_test.js`、`web/project_workspace_mode_test.go`、`chatbot/tests/warm-workspace-activity.test.jsx`。

**接口** Studio 项目表单提交 `workspaceMode`。SOP 与聊天沿用进度事件的 `workspace.pool_waiting`，展示「等待预热工作区」和已有的取消入口。预热支持能力随 Runner 版本和实际可用驱动一起声明。

- [ ] 在 `web/project_workspace_mode_test.js` 和 `chatbot/tests/warm-workspace-activity.test.jsx` 断言配置双向保存、旧值默认普通检出、排队状态和取消操作可见、进度事件中没有宿主路径。用 `web/project_workspace_mode_test.go` 接入仓库现有 Node 测试运行方式。
- [ ] 运行 `go test ./web -run '^TestProjectWorkspaceMode' -count=1` 与 `npm --prefix chatbot test -- warm-workspace-activity`，确认缺少界面契约导致失败。
- [ ] 使用项目既有 Selector 和任务／会话状态组件。文档给出两份虚构预热目录的登记示例、同卷限制、已有分支／LFS 前提、驱动资格、取消与归还时机、被占用原因和重新领取的行为。版本能力启用时验证 API、Worker、Runner 同步升级，旧模式兼容。
- [ ] 执行 `go test ./internal/domain ./internal/repo ./internal/runner ./internal/runtime ./internal/engine ./internal/httpapi ./cmd/agentflow-runner ./web`、`go test -race ./internal/runner ./internal/engine -run 'Test.*WarmWorkspace|TestWarmCheckout' -count=1`、`npm --prefix chatbot test`、`git diff --check`。具备隔离 MySQL 的环境运行相应两个实例队列测试。
- [ ] 在有 Unity 的 Runner 做验收，从两份热目录和一个真实项目发起三份仓库请求，记录第三份排队到继续的过程。比较首次检出、第二次复用和 Unity 测试启动用时，确认槽位 `Library` 复用与整目录快照为零。若没有现场项目或编辑器，在交付中说明待执行的现场验收项。
- [ ] 用 `feat: expose warm workspace mode and queue status` 提交本任务文件。按所选执行方式进行独立审阅并执行完成前验证。逐处检查现有文件的 diff，将新增业务逻辑移入预热池专属文件，保留必要接入点。
