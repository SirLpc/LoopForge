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
      "unityProjectPath": "client",
      "unityEditorPath": "C:\\UnityVersions\\2022.3.62f3\\Editor\\Unity.exe"
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

## SOP 节点运行 Unity 测试

`unityEditorPath` 是可选的槽位配置。配置后重启 Runner，使用该槽位的 Agent、脚本和验证命令可以在各自的任务中调用：

```bash
"$AGENTFLOW_UNITY_TEST" --mode EditMode --test-filter InteractionSequenceSceneTests
"$AGENTFLOW_UNITY_TEST" --mode PlayMode
```

租约中有多个已配置的 Unity 仓库时，用 `--repository` 指定仓库 ID。Runner 使用原始 Windows 项目路径和已安装的对应版本。Unity 在 Windows 用户会话下运行并使用该用户的许可证。只对信任的项目槽位启用此配置，因为编辑器会在该会话中执行项目代码。

命令等待运行结束并输出 JSON，包含本次状态、Git 提交、可取得的工作树状态，以及 `xmlPath` 和 `logPath`。这些路径相对于运行工作区根目录。节点可以将报告复制到现有制品契约声明的路径。通过需要有效的 NUnit XML、至少一个执行通过的用例和正常的编辑器退出。一个项目同时运行一项测试，编辑器占用时会报告 `busy`。每次调用最多 30 分钟，任务取消时 Runner 会结束本次测试进程树。

NUnit XML 上限为 8 MiB，超过时返回基础设施错误。超过 4 MiB 的编辑器日志保留末尾，并在结果中标记 `logTruncated`。

## Windows Unity 验收

准备验收时可以在「执行机器 → 我的机器 → 预热工作区」暂停该槽位调度。已开始的任务可以结束，新的租约等待。关闭原工作区的终端并等槽位归还后，再在 Windows Unity Hub 打开原项目，例如 `D:\UnityProjects\example\client`。关闭 Unity 后恢复该槽位调度。Runner 在领取、切换分支、启动写操作和归还时也会检查 Unity 的项目锁文件。

归还需要提交并推送应保留的代码、归档未被 Git 忽略的运行产物并关闭相关终端。Git 忽略的未跟踪文件随槽位保留，包括按 Git 规则忽略的 `Library`。当前检出的分支和文件在归还后仍可用于本地验收。

新领取时，Runner 按任务配置的分支和最新远端提交增量检出。提交相同时复用已有文件。有变化时只扫描并下载变更所需的 LFS 对象。目标分支的跟踪路径与本地忽略文件冲突时，Git 会停止切换并保留本地文件。重开已归还的工作区时，Runner 按已记录的分支与精确提交恢复。切换现有部署前，先在旧 Runner 完成存量交付和归还。历史 Docker 卷上的代码可以通过已交付的 Git 分支继续使用。

跟踪路径达到 512 个的 Windows 工作区可以使用 Windows Git 做检出前后的状态检查，并由 Windows PowerShell 批量读取重开、恢复和归还时需要校验的文件。Runner 通过 WSL 互操作自动使用 Windows PATH 上的 `git.exe`，LFS 临时数据保存在 Windows 临时目录。较小的工作区、带有 `metadata` 选项的 WSL 挂载和 Windows 工具不可用的环境继续使用 WSL 检查。特殊文件和需要 Git 内容转换的文件也会交由 WSL 校验。

符合条件的 Windows 槽位用 Windows Git 切换提交、更新任务分支和挂接 HEAD。Runner 在执行前确认 Windows 路径与租约挂载指向同一个检出，并在命令执行期间锁定路径。Windows Git 在临时索引上验证工作区和刷新文件状态，确认暂存条目及真实索引都符合预期后发布。LFS 仍按变更验证并落盘，索引刷新只处理本次替换的资产和中断恢复涉及的变更。文本换行沿用工作区原有规则。

远端认证、Git 对象缓存和 LFS 下载仍由 WSL Runner 执行。

符合条件的租约也会在 Windows 侧执行 SOP 产物交接的工作树 diff、页面状态和版本查询，以及交付时的本地暂存和提交。Windows 暂存先写入临时索引和 LFS 对象目录，Runner 发布对象后才替换正式索引。可验证为本地且无需转换 LFS 资产的分支切换和合并也使用 Windows Git。需要下载或重新物化 LFS 资产的操作、LFS 仓库的变基、WSL 私有缓存与临时工作树，以及带凭据的远端操作沿用现有隔离执行路径。预热 Runner 日志的 `backend=windows` 可以确认实际执行端。

Runner 日志以运行 ID 记录拉取、本地对象导入、切换前检查、提交切换、LFS 落盘、索引刷新、就绪和归还耗时。若 Windows helper 无法启动，且本地检出写入尚未开始，Runner 可以使用 WSL 路径。写入受系统中断时，可按现有检出意图和内容证明检查槽位，再决定是否恢复。

归还先确认远端提交、索引、归档文件和 LFS 缓存，收到归还回执后再于卸载前完整核对原目录。确认阶段复用同一回执已经验证的 LFS 清单及未发生变化的缓存对象。Runner 重启后会重新验证清单和缓存。若本地源码或索引发生变化，原租约继续持有槽位。处理改动后，可以重新发起归还。暂时的编辑器占用会等待下一次确认。

升级时先更新 API 和 Worker，再更新原生 Runner。升级期间暂停该槽位调度并结束已有工作区用量，待相关进程使用同版协议后恢复。新版 Runner 可以读取先前的租约日志。需要回退时，先用新版 Runner 处理存量租约，再按独立 Runner 状态流程切换。

## 同机一键更新

平台通过 Compose 源码部署且原生 Runner 由同一个 WSL 用户的 systemd 服务管理时，可在 Windows 双击仓库根目录的 `redeploy-wsl.bat`。执行前暂停相关槽位并完成现有工作区的归还。脚本从安装目录当前检出的源码构建平台，等待平台健康检查，再从新容器取出对应的 Linux Runner，备份旧二进制为 `agentflow-runner.previous` 并重启 Runner 服务。源码更新和本次发布要求的手工 SQL 由部署人员处理。

可先在 Windows CMD 执行 `redeploy-wsl.bat --check` 检查部署目标。批处理使用 Ubuntu。在其他 WSL 发行版中可直接执行 `bash scripts/redeploy-wsl.sh`。脚本默认重启 `loopforge-native-runner.service` 并更新 `~/.local/share/loopforge-native/agentflow-runner`，可在 WSL 中分别设置 `LOOPFORGE_RUNNER_UNIT` 和 `LOOPFORGE_RUNNER_BINARY` 指定已有安装的服务与二进制。更新完成后，在平台确认 Runner 在线并验证一个槽位的交付与归还，再恢复其余槽位。

通用池协议见[Runner 预热工作区池](warm-workspace-pool.md)。
