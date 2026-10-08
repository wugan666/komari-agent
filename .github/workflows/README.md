# 自主管理的构建和发布

只允许 `wugan666/komari-agent` 的 `owned` 分支发布。本人仓库的 `main` 保留为
不触发发布的安全副本；原上游只通过独立 `upstream` 远程参考，不把其工作流直接
同步回本人分支。所有平台、远程终端、任务执行、文件管理和自更新均保留。

## 发布流程

- `build.yml`：`owned` 的 push / pull_request 只运行测试和跨平台构建，不发布。
- `release.yml`：手动运行，输入新的稳定版本，例如 `v1.5.12`。
- `snapshot.yml`：仅手动运行，生成 `Snapshot-yymmddhhMMSS-runID-attempt`。
- `publish.yml`：可复用发布实现，验证当前 `owned` commit，先测试，再并行构建，
  收齐产物、校验后创建草稿 release，最后一次性公开。
- `release-docker.yml`：仅由完成测试和 release 发布的工作流调用，直接使用同一轮
  已校验的 Linux 二进制，不另外编译。
- `generate-release-notes.yml`：手动更新已验证属于 `owned` 历史的 release 说明。

可在 Actions 中选择上述稳定或快照工作流，分支选择 `owned`。使用 CLI：

```sh
gh workflow run release.yml --repo wugan666/komari-agent --ref owned -f version=v1.5.12
gh workflow run snapshot.yml --repo wugan666/komari-agent --ref owned
```

运行开始、发布 release 和发布镜像前会核对 `owned` 分支头。期间若出现新提交，
旧任务拒绝继续发布；应对新的已审查 commit 再运行。已有 tag/release 从不删除、
覆盖或重用；上传中断可能留下草稿，需要检查后另选新版本。

## 版本、产物和更新来源

稳定版只使用普通 SemVer，不把 `+build` 元数据当作升级序号；同分支修订递增 patch。
快照始终标记 prerelease，不设为 latest。历史快照保留，便于回溯和手动恢复。

产物名称保留为 `komari-agent-${GOOS}-${GOARCH}`，Windows 附加 `.exe`。
每个二进制附带同名 `.sha256` 文件，内容为标准 `sha256sum` 格式：
64 位十六进制摘要、两个空格、文件名。发布前验证 14 个平台、28 个文件全部到齐。

每次构建都显式注入：

```sh
-X github.com/komari-monitor/komari-agent/update.CurrentVersion="$VERSION"
-X github.com/komari-monitor/komari-agent/update.Repo="$GITHUB_REPOSITORY"
```

原 Go 模块路径保留用于内部包引用，不代表运行时访问上游。
构建使用固定 Go 1.26.8、`GOTOOLCHAIN=local`、`-mod=readonly` 和 `go mod verify`。
全部 Actions 固定完整 commit SHA，Docker 基础镜像及 BuildKit 固定 digest。

## 测试和权限

测试是独立 job，下载依赖并用 `go test -c -o <临时目录>/ ./...` 预编译，随后执行
`go test -count=1 -timeout 60s ./...`，外层 `timeout 60s` 限制测试执行总时长。
POSIX 与 PowerShell 安装器另运行离线回归测试，各自限制 60 秒。
发布 job 必须依赖测试和全部构建成功。测试、构建默认为 `contents: read`；
仅 release 发布 job 有 `contents: write`，仅镜像发布 job 有 `packages: write`。
可复用工作流入口声明的写权限只是上限，内部只给相应发布 job。
不用 `pull_request_target`，checkout 不持久保存凭据，也不启用构建缓存。

输入通过环境变量传入 shell，先验证版本格式，不将用户输入直接拼进脚本。
发布机器不存放更新用 GitHub 写权限令牌。

## Docker

发布到 `ghcr.io/wugan666/komari-agent`：

- 稳定版：不可重用的版本 tag，以及可移动 `latest`。
- 快照版：不可重用的快照版本 tag，以及可移动 `snapshot`。

镜像支持 `linux/amd64`、`linux/arm64`，保留容器标记。基础镜像 marker 阶段
使用构建机架构，最终阶段只 COPY，不需要运行外部 QEMU/binfmt 安装器。
容器更新应拉取本人仓库的新镜像；容器内替换二进制不能持久改变镜像。

## 信任边界和维护

人工审查后才合并上游修改，再由本人触发发布；不要添加自动拉取上游并发布的任务。
SHA-256 防止下载损坏和不一致，不是独立数字签名，不能抵抗本人 GitHub 发布账号
或具有写权限的构建任务同时被攻破。仓库权限、账号 MFA、依赖及固定 Actions 更新
仍需维护。上述改动不是对整个 Agent 的完整安全审计。
