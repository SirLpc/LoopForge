# 结束 Chat 会话实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal** 让用户主动结束普通 Chat 会话并归还预热槽位，保留可查看的会话历史。

**Architecture** 用助手会话行串行化结束请求、生成准入和内容变更。后台等待当前轮次收尾，复用现有的预热归还与代次校验，Chat 通过会话详情和列表显示进度与结果。

**Tech Stack** Go 1.26.11、Gin、GORM、React、Vitest。

**Spec** [会话结束设计](../specs/2026-11-23-end-chat-session-design.md)

## 全局约定

- 普通个人和项目 Chat 会话沿用会话菜单及项目访问权限。SOP 和定时任务会话沿用各自的生命周期。
- Runner 继续校验进程、Git 内容和产物。会话借用的 SOP 工作区仍由原 SOP 管理，普通检出的回收沿用原有机制。
- 历史记录、附件、产物、需求和历史工作区 ID 保留。置顶、重命名、归档和删除仍可用。
- 现有会话 JSON 存储新增结束状态。继续遵守 `AGENTS.md` 的 `AssistantRepo` 和 `assistantHandlers` 领域边界。

## 审查重点

- 旧客户端的 `PUT` 携带会话全量字段时，服务端保留自己的结束状态和原历史（任务 1）。
- 遗留的同步 Chat 请求与结束并发时，和网页生成一样有可取消的持久记录（任务 1）。
- 第一个工作区已归还、第二个工作区被 Runner 拒绝时，重试沿用已生效的归还（任务 2）。
- 服务进程中断或迟到的归还回包不能覆盖之后的结束请求（任务 2）。
- 切到其他会话或刷新页面时，旧请求和浏览器排队消息不能写到错误的会话（任务 3）。

---

### Task 1 · 会话状态和写入准入

**文件** `internal/domain/assistant.go`、新建 `internal/repo/assistant_session_end.go` 和 `assistant_session_end_test.go`。在 `assistant.go`、`assistant_generations.go`、`assistant_generation_reply.go`、`assistant_warm_workspace.go`、`assistant_shared.go`、`assistant_proposals.go`、`assistant_execution.go`、`assistant_context.go` 和 `assistant_sandbox.go` 的现有会话写入事务接入校验。同步 Chat 接在 `internal/httpapi/assistant.go` 和 `assistant_direct_generation.go`。

**接口** `domain.AssistantSession.End *domain.AssistantSessionEnd`（`end`），结构包含 `State string`（`ending`、`ended` 或 `failed`）、`AttemptID string`、`Deadline *time.Time`、`EndedAt *time.Time` 和 `Error string`，JSON 使用 `state`、`attemptId`、`deadline`、`endedAt`、`error`。`(*AssistantRepo).BeginAssistantSessionEnd(userID int64, id, attemptID string) (domain.AssistantSession, string, error)` 返回会话和本次请求要求停止的生成 ID。同一会话已有有效请求或已结束时返回原记录及空生成 ID。空 `End` 的历史记录可写。

- [ ] 写 Repo 回归用例。`TestAssistantSessionEndAdmission` 验证 `require.ErrorIs(t, store.BeginAssistantGeneration(g), repo.ErrAssistantSessionEnding)`、旧请求的全文写入被拒绝、仅改标题可用、伪造结束状态无效。`TestAssistantSessionEndCancel` 验证 `stored.CancelRequested == true`、该轮审批及新转向失效、已接受的本轮仍能保存中断历史。HTTP `TestDirectChatClaimsGenerationForSessionEnd` 验证同步普通 Chat 与网页生成共用数据库生成记录，结束其记录后同步请求收到取消。
- [ ] 运行 `go test ./internal/repo ./internal/httpapi -run 'Test(AssistantSessionEndAdmission|AssistantSessionEndCancel|DirectChatClaimsGenerationForSessionEnd)$' -count=1`。确认用例先因缺少结束状态或同步准入失败。
- [ ] 实现 `BeginAssistantSessionEnd`。复用 `inferredAssistantSessionSource` 及已有会话身份校验。事务锁顺序为会话、生成、预热排队，使用同一事务保存 `ending` 并按现有逻辑标记取消该轮排队和审批。用数据库时间判断有效期限。归还预算为每个自有工作区最多 31 分钟，另留 2 分钟给停止与汇总；后台持有相同 `AttemptID`。
- [ ] 在 `BeginAssistantGeneration`、编辑、提议保存和自动续聊、沙箱、模式、需求绑定、工作区新绑定的事务内校验 `ending`/`ended`。已接受生成的收尾沿用生成 ID 核验，保留 `End` 并继续允许原绑定的失败回滚。普通 `PUT` 的只读分支从已锁定的原记录保存标题，拒绝变更消息或执行上下文。现有同步 Chat 复用 `claimDirectSharedGeneration` 的持久准入和取消轮询，按实际共享属性保存，用 `PutAssistantGenerationReply` 核验生成 ID。标记已接受但尚未执行的新轮次请求前也走此准入。调整涉及两把锁的写入顺序以避免与结束互锁。
- [ ] 重新运行本任务的三项用例及 `go test ./internal/repo ./internal/httpapi -run 'Test.*(Direct.*Generation|Shared.*Reply|Assistant.*Steer|Proposal)' -count=1`，均通过后提交本任务。

### Task 2 · 后台归还与会话端点

**文件** 新建 `internal/httpapi/assistant_session_end.go`、`internal/httpapi/assistant_session_end_test.go`，修改 `internal/httpapi/server.go`、`assistant_session.go`、`assistant_conversation_cleanup.go`，扩展 `internal/repo/assistant_session_end.go` 和相关测试。

**接口** `POST /api/assistant/sessions/:id/end`。新建或已有有效操作返回 `202` 和会话状态，已结束返回 `200`，未登录返回 `401`，缺少权限或专用生命周期返回 `403`，会话缺失返回 `404`。`(*AssistantRepo).AssistantSessionEndReady(userID int64, id, attemptID string) (domain.AssistantSession, bool, error)` 读取同一次操作及持久生成状况。`(*AssistantRepo).FinishAssistantSessionEnd(expected domain.AssistantSession, leases map[string]*domain.WarmWorkspaceLease, endError string) error` 按 `expected.End.AttemptID` 写入结果。

- [ ] 在现有预热测试 fixture 上写 `TestAssistantSessionEndReturnsWarmWorkspaces`，断言权限、`202 → ended`、Runner 检查及确认操作顺序、消息和附件仍在、第二次调用 `200`、SOP 借用未触发归还。写 `TestAssistantSessionEndDrainsGeneration` 覆盖运行中、预热排队和仍在退出的本地生成。写 `TestAssistantSessionEndRetainsOnReturnFailure`，用两个工作区验证第一个已归还而第二个失败后 `state == "failed"`、错误可读、重试仅需归还剩余工作区。Repo `TestAssistantSessionEndRejectsStaleAttempt` 验证过期尝试可重试，旧完成调用和变更后的租约代次均不能写入 `ended`。
- [ ] 运行 `go test ./internal/repo ./internal/httpapi -run 'TestAssistantSessionEnd' -count=1`，确认本任务新增用例先失败。
- [ ] 实现端点和后台流程。使用 `conversationScope` 解析真实所有者，全程按数据库记录判定来源。返回的 `End.AttemptID` 与本请求生成的标识相同才启动后台工作，使用独立于 HTTP 连接的上下文、`inflightTasks` 和已登记期限。调用已有生成取消入口共用的本地取消逻辑，等待持久生成状态及进程内执行退出，沿用 stale generation 恢复和保存中断回复逻辑。数据库读取失败时结束这次尝试并保存错误。归还前复核尝试标识，已停止的生成还在收尾时在停止预算内等其使用租约释放。
- [ ] 从现有删除逻辑提取按自有工作区归还并收集原代次的 helper，原删除流程也使用它。按 `Run.IsWorkspace()` 和预热类型过滤，逐项调用 `ReleaseWarmWorkspace`，按原语义为已排队请求取消代次。归还中断或终端忙碌时保持原数据，用已有脱敏逻辑提供可处理原因。成功提交复用删除路径中的会话绑定检查及 `lockReturnedWorkspace`，把旧记录中隐式持有的工作区补入历史，再清空当前绑定和更新模式。结束处理中拦截并发删除，结束后走原删除流程。
- [ ] 列表和详情返回 `end`，把已过期的 `ending` 投影为可重试的 `failed`，实际重试在事务内用新标识认领。加入运行期可读错误并校验请求重复提交。运行 `go test ./internal/repo ./internal/httpapi ./internal/engine -run 'Test.*(AssistantSessionEnd|WarmWorkspaceRelease|WarmWorkspaceConversation)' -count=1`，全部通过后提交本任务。

### Task 3 · Chat 会话菜单、状态和消息队列

**文件** `chatbot/src/App.jsx`、`chatbot/src/useChat.js`、`chatbot/src/message-queue.js`、`chatbot/tests/app-render.test.jsx`、`chatbot/tests/message-queue-hook.test.jsx`。沿用 `design.md` 的会话菜单、Dialog、状态提示和 Harness 图标。

**接口** `useChat().endSession(id): Promise<boolean>` 调用任务 2 的路径，合并返回状态并刷新。`useChat().readOnly` 表示权限只读或会话处于 `ending`/`ended`，会话操作菜单仍区分权限及会话结束状态。

- [ ] 写界面用例 `can end a Chat session from the context menu`，验证确认文案、一次点击只发一个请求、SOP/定时会话菜单无此项。写 `shows ended chat history with no composer`，验证历史可见、显示「已结束」、输入框和工作区操作隐藏、归档和重命名仍可操作。写 hook 用例 `holds queued messages while ending and after a failed return`，验证现有队列保持暂停、状态刷新加载详情、失败原因出现、其他会话不被旧响应覆盖；补测成功和重新打开时无排队发送。
- [ ] 在 `chatbot/` 运行 `npm test -- tests/app-render.test.jsx tests/message-queue-hook.test.jsx`，确认新增用例先失败。
- [ ] 接入 `endSession`，确认时立即暂停该会话队列。`ending` 时定期读取详情，已有列表轮询检测其他标签页的状态变更，发现变化时刷新当前详情。用会话 ID 及请求当前性检查合并结果。结束失败后保留待发送内容并延用手动恢复队列，结束成功后停止该会话的自动发送入口。继续屏蔽 `send`、重发、转向和消息编辑的旧 UI 回调。
- [ ] 在现有会话菜单与 Dialog 加「结束会话」，说明「停止当前回复并尝试归还预热工作区。聊天记录会保留。」在列表和原会话显示「正在结束」或「已结束」，使用现有只读提示样式。会话结束中及已结束时关闭/隐藏会话工作区和编辑入口，保留附件和产物浏览。失败原因沿用 Chat 错误提示。
- [ ] 在 `chatbot/` 运行 `npm test` 与 `npm run build`。从仓库根目录运行 `go test ./internal/repo ./internal/httpapi ./internal/engine ./web` 及 `git diff --check`。审查端点权限、失败后的重试、旧记录和并发竞态，记录环境导致的验证限制，提交本任务。
