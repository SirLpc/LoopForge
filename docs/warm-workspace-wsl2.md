# WSL2 的预热工作区

这套部署把专用 Runner 和 Unity 检出放在同一台机器的 Docker Desktop Linux 引擎上。Runner 使用独立登记和状态文件。现有普通 Runner 可以继续处理已有的工作区。

## 存储与启动

1. 创建独占的 Docker 本地卷，使用默认的 `local` 驱动选项。将整个卷挂到 Runner 容器中的固定目录，例如 `/loopforge-pool`。在卷中分别准备 `slots`、`workspace`、`config` 和 `tools`。初始化完成后退出准备数据的容器。
2. 把 Runner 镜像内的可执行文件原样安装到 `/loopforge-pool/tools/agentflow-runner`，例如以 `0555` 权限安装。镜像内的 Runner 可执行文件也供短期进程检查容器使用。更换镜像时同步更新工具副本。
3. 在卷中保存池配置与状态，槽位路径使用 Runner 容器内的绝对路径。两份 Git 检出的 `origin` 应与平台登记的仓库一致。Windows 源目录可保留用于原有工具。
4. Runner 容器使用本机 Docker Unix socket 和可达的平台地址。启动该模式需要以下参数，登记凭据通过现有 Runner 配置方式提供。

```text
-warm-workspace-pool-file /loopforge-pool/config/pool.json
-warm-docker-volume example-unity-warm
-warm-docker-volume-root /loopforge-pool
-workspace /loopforge-pool/workspace
-state /loopforge-pool/config/state.json
```

配置也可通过 `AGENTFLOW_RUNNER_WORKSPACE_POOL_FILE`、`AGENTFLOW_RUNNER_WARM_DOCKER_VOLUME` 和 `AGENTFLOW_RUNNER_WARM_DOCKER_VOLUME_ROOT` 提供。Docker Desktop 模式通过更换镜像升级。详见[通用登记与验收指南](warm-workspace-pool.md)。

## 归还检查

任务容器挂载当前租约对应的卷子路径。Git 对象和辅助工具通过只读子路径挂载。归还前，Runner 检查引擎中运行和停止的容器。未知的重叠挂载、无法读取的进程状态或未交付的文件都会让槽位继续由原所有者保留。

进程检查在同一镜像的临时容器中运行。该容器使用 Docker 宿主 PID 视图、无网络和只读根文件系统，临时增加读取进程句柄所需的能力。Runner 主容器沿用普通 PID 隔离。Docker 管理员仅在槽位退出调度且已空闲时维护文件。

## 试运行与回退

先用临时卷和测试检出执行领取、任务挂载、Git 操作、拒绝错误归还和重新领取。再检查真实槽位的 Git 远端和 Unity 缓存，完成一次项目仓库读写和归还。退出专用模式前等待租约归还，保留卷及登记状态可用于排查已分配的工作区。Windows 与 Linux 运行环境不同的 Unity Editor 可能需要重新导入资产。
