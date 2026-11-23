# WSL2 原生预热工作区

原生 Runner 直接使用 Windows 项目的原始 Git 检出。任务经 bwrap 执行，领取和归还只建立或撤销工作区的私有挂载。源码、Git 检出和 Unity Library 始终留在原目录。Windows Unity Hub 可以从同一项目目录打开任务修改后的文件。每个目录对应一个独占租约。

## 配置

1. 在 WSL2 安装 Git、bwrap、SOP 使用的 Agent CLI，以及仓库需要的 Git LFS。关闭 Unity 后检查检出和索引干净，并确认已有 LFS 资产已下载到工作树。Runner 使用这份检出的 Git 元数据并保留本地分支。项目所在盘需允许 WSL 读写。
2. 在 WSL 的 Linux 文件系统中为 Runner 创建独立的状态目录和工作区目录。状态目录和工作区目录须分开，沙箱会隔离整份状态目录。为原生 Runner 使用独立绑定和状态，保留旧 Runner 的状态以便处理存量租约。Runner 的工作区根目录应在 WSL 主目录下。将各槽位的原始绝对路径登记到池配置里，例如：

```json
{
  "slots": [
    {
      "id": "example-desktop",
      "projectId": "example-project",
      "repositoryId": "example-client",
      "path": "/mnt/d/UnityProjects/example",
      "unityProjectPath": "client"
    }
  ]
}
```

3. 使用绑定当前用户的 Runner 配置，在 WSL 的私有挂载命名空间启动：

```bash
unshare --user --map-root-user --mount --propagation private -- \
  agentflow-runner -warm-native \
  -warm-workspace-pool-file ~/.config/loopforge-native/pool.json \
  -workspace ~/.local/share/loopforge-native/workspace \
  -state ~/.config/loopforge-native/state.json
```

平台地址和绑定凭据沿用 Runner 现有配置方式。也可使用 `AGENTFLOW_RUNNER_WARM_NATIVE=1`。新建 SOP 或对话选择 native 沙箱，并绑定有对应登记的个人执行机器。项目使用「预热工作区池」。运行环境在创建工作区时冻结。

## Windows Unity 验收

准备验收时可以在「执行机器 → 我的机器 → 预热工作区」暂停该槽位调度。已开始的任务可以结束，新的租约等待。关闭原工作区的终端并等槽位归还后，再在 Windows Unity Hub 打开原项目，例如 `D:\UnityProjects\example\client`。关闭 Unity 后恢复该槽位调度。Runner 在领取、切换分支、启动写操作和归还时也会检查 Unity 的项目锁文件。

归还需要提交并推送应保留的代码、归档未被 Git 忽略的运行产物并关闭相关终端。Git 忽略的未跟踪文件随槽位保留，包括按 Git 规则忽略的 `Library`。当前检出的分支和文件在归还后仍可用于本地验收。

新领取时，Runner 按任务配置的分支和最新远端提交增量检出。提交相同时复用已有文件。有变化时只扫描并下载变更所需的 LFS 对象。目标分支的跟踪路径与本地忽略文件冲突时，Git 会停止切换并保留本地文件。重开已归还的工作区时，Runner 按已记录的分支与精确提交恢复。切换现有部署前，先在旧 Runner 完成存量交付和归还。历史 Docker 卷上的代码可以通过已交付的 Git 分支继续使用。

归还先确认远端提交、索引、归档文件和 LFS 缓存，收到归还回执后再于卸载前完整核对原目录。确认阶段复用同一回执已经验证的 LFS 清单及未发生变化的缓存对象。Runner 重启后会重新验证清单和缓存。若本地源码或索引发生变化，原租约继续持有槽位。处理改动后，可以重新发起归还。暂时的编辑器占用会等待下一次确认。

升级时先更新 API 和 Worker，再更新原生 Runner。升级期间暂停该槽位调度并结束已有工作区用量，待相关进程使用同版协议后恢复。新版 Runner 可以读取先前的租约日志。需要回退时，先用新版 Runner 处理存量租约，再按独立 Runner 状态流程切换。

通用池协议见[Runner 预热工作区池](warm-workspace-pool.md)。
