# 自建版本维护说明

## 保留的功能

本 fork 保留上游的监控、远程终端、命令执行、文件管理、
自动发现、稳定版/快照版自动更新及跨平台支持。
以 root 运行时，你配置的 Komari 面板仍有 root 级远程控制权限。
Linux 不写入 MOTD 警告，升级会清理旧版受管提示；远控权限与功能仍按管理员配置保留。

## 上游同步基线

`owned` 通过 `git merge upstream/main` 跟随上游，当前已合入至 `efe34d5`
（切换版本协议、启动参数查询、终端保活、LXC uptime、NVIDIA 温度修复）。
更新模块直接采用上游实现，自有改动只有：更新仓库固定为 `wugan666/komari-agent`、
GitHub 下载始终校验证书、优先从面板获取更新（`update/controller.go`）。

## 更新来源

源码与发布仓库：<https://github.com/wugan666/komari-agent>。
维护分支为 `owned`，`main` 同步安全构建配置且不用于发布，
避免 fork 初始分支遗留的上游工作流仍可触发发布。
上游代码通过本地 `upstream` 远程查阅，不自动同步到任何发布分支。
安装器与 Agent 默认更新仓库均为 `wugan666/komari-agent`，没有官方仓库回退。
正式构建同时显式嵌入本仓库名称，可通过 `komari-agent --version` 核对。
更新仓库不能通过面板或运行参数修改。

Go module 与内部 import 继续使用 `github.com/komari-monitor/komari-agent`，
以减少无关代码变动；构建使用本 checkout 的源码，并不因此下载上游 Agent 二进制。

Agent 启动时及之后每 6 小时检查更新，只在当前 `主.次` 版本线内升级到更高版本。
更新优先来自所连接面板的 `/download/agent`（`VERSION`、二进制及其 `.sha256`）；
面板没有托管本平台二进制时才查询 GitHub Release。管理员也可以在面板里对节点
下发“切换版本”。原有 `--disable-auto-update` 参数继续可用。

版本号以仓库根目录的 `VERSION` 文件为准，主控发布脚本据此构建并内置安装包。

## 同机多个 Agent

`install.sh` 只管理与 `--install-service-name` 同名的服务：
- 同名服务已指向另一个面板时拒绝覆盖，提示改用独立的服务名和目录（或 `--install-force`）。
- 其他服务名若使用相同的面板和 token，会被移除，避免两个 Agent 互相挤掉连接。

## 安装

生产服务器建议使用自己发布的固定稳定版本。首版标签为 `1.5.12`，
这是本 fork 的版本号，不表示已经合并上游未来的同名版本。

```bash
curl --fail --location --proto '=https' --tlsv1.2 \
  -o komari-install.sh \
  https://raw.githubusercontent.com/wugan666/komari-agent/1.5.12/install.sh
# 阅读脚本后，再使用你自己的面板地址和 Agent token 安装。
bash komari-install.sh --install-version 1.5.12 \
  --endpoint https://your-panel.example --token YOUR_AGENT_TOKEN
```

安装器先解析固定版本、下载并验证产物，再替换现有 Agent。
默认不经过第三方 GitHub 下载镜像。显式指定 `--install-ghproxy` 时，
使用者仍需信任该代理；经同一代理取得的 SHA-256 不构成独立签名验证。
新装系统若缺少工具，系统包管理器及 Windows 服务包装器仍属于安装信任链。
不要把含真实 token 的安装命令写进公开 issue、脚本或日志。

现有机器必须主动替换一次 Agent，才能从上游更新渠道切换到本仓库。
仅更换网页上的安装命令不会改变已运行程序的内置更新地址。

## 审查与发布

1. 在 `owned` 上审查并合入需要的修改；不要自动同步上游后自动发布。
2. 运行测试与安装器检查，查看变更涉及的权限和网络请求。
3. 使用本仓库的稳定发布工作流，输入递增的 SemVer 版本号。
4. 工作流测试、构建所有平台并收齐校验文件后，才公开 Release。
5. 已部署 Agent 会从你的 Release 自动更新；紧急修复发布新的更高版本。

不要只递增 `+build` 元数据，SemVer 不会把它视为更高版本。
已经发布的标签与产物不应覆盖重建；需要回退代码时发布一个更高的修复版本。
快照需要手动发布，不会因普通源码 push 自动下发。
具体工作流入口和命令见 [.github/workflows/README.md](.github/workflows/README.md)。

## 信任边界

本 fork 将上游发布者直接下发更新的能力移交给你自己的仓库与发布流程，
并不等于已经完成对上游全部源码和依赖的安全审计。
需要持续保护 GitHub 账号、可写令牌、仓库协作者、工作流、构建工具与依赖。
建议开启强身份验证，只给实际需要发布的人写权限。

SHA-256 可发现损坏或产物与校验文件不一致，但不能阻止有权同时替换二者的人。
抵抗本仓库发布账号被盗，需要另外引入独立保管签名私钥和客户端内置公钥。
自建更新源不会让恶意 root 程序失去 root 权限，面板本身也需要安全维护。

MIT 许可证及上游版权声明保持不变。

## 1.5.16 深度优化

- 写入 WebSocket 有 10 秒期限，关闭可打断阻塞写；控制帧和解压后的 HTTP 响应受 16 MiB 上限保护。
- 每路命令输出保留前 1 MiB 并注明截断，继续排空进程输出并保留退出码；日志不再记录完整远程命令。
- 更新检查和基础信息上传不阻塞实时上报启动。IPv4/IPv6 查询并行，每个地址族总预算 12 秒，单个来源最多 4 秒，校验真实 IP 并限制响应大小。
- 更新器仅接受精确平台的原始二进制及配套 SHA-256，移除未使用的归档和 OpenPGP 依赖；保留自有仓库验证、防降级及原有原子替换/失败回滚实现，保留文件权限。
- Go 工具链为 1.26.8，更新已确认受影响的网络/Unicode 依赖。完整测试、关键路径 race 测试和 govulncheck 通过。

## 1.5.17 采样性能

实时报告只读取 CPU 使用率，型号与核心拓扑继续由基础信息上传独立收集。Linux 的 RAM/Swap
使用同一份 meminfo 快照，保留原来的内存统计选项；进程计数分块读取名称，避免创建目录元数据、
排序和无效整数解析。采样频率、上报字段和远控能力保持原配置。

新增完整报告 BenchmarkGenerateReport；同机基准每次分配从约 524 KB 降至 273 KB，
约 4090 次分配降至 1900 次。内存记账边界、分块进程计数及采样竞争检测通过。

## 1.5.18 容器内存来源兼容

`HOST_PROC` 与 `--memory-include-cache` 同时使用时，保留原 gopsutil 的宿主机内存读取路径，
避免合并 meminfo 读取改变统计来源。该组合沿用原 RAM/Swap 读取；其余采样性能优化保持生效。
新增隔离 HOST_PROC 固定 meminfo 回归测试，检查总内存、已用内存和报告模式，monitoring race 及全量测试通过。
