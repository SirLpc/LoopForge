# Fork 自定义功能 · 内网 HTTP GitLab

`gitlab-http.patch` 保存这项功能的源码和回归测试。`run.sh` 从当前提交导出临时源码，在副本上应用补丁。工作目录中的上游文件可直接用于同步和日常开发。

## 同步后检查

同步上游代码后，从仓库根目录运行检查。检查使用当前 `HEAD` 中的已提交内容。

```bash
bash deploy/fork/gitlab-http/run.sh --check
```

如果上游改到了同一段代码，检查会报出需要更新补丁的文件。可以把补丁应用到独立的开发分支，解决冲突并重跑回归测试，再把该分支的功能差异更新到本目录的补丁文件。

## 部署和启用

在现有部署的 `.env` 配置允许访问的 GitLab 地址。以下示例需要换成实际 IP。

```dotenv
AGENTFLOW_GIT_EGRESS_ALLOW_CIDRS=192.168.50.10/32
```

部署沿用现有 `.env`、Compose 项目和数据目录。`LOOPFORGE_DATA_DIR` 使用绝对路径。平台和原生 Runner 都需要更新时执行 WSL 命令。只部署 Compose 平台时执行 `--deploy`。

```bash
bash deploy/fork/gitlab-http/run.sh --deploy-wsl
```

在个人配置添加 GitLab 令牌时，主机填完整的 `http://IP:端口`。项目仓库使用同一地址，例如 `http://192.168.50.10:8443/group/repo.git`。HTTP 会明文传输令牌，适用于可信内网。

后续的源码部署也通过本目录的入口完成，以继续应用这个补丁。`--exec` 可以在打好补丁的临时源码中运行测试或构建命令。
