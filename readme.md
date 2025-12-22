# Monte Carlo IP Searcher（mcis）

一个 **Cloudflare IP 优选**工具：用**递进式蒙特卡罗 + 多头分散搜索（multi-head + beam）**在更少探测次数下，从 IPv4/IPv6 网段里找到更快/更稳定的 IP。

示例优选域名：`hao.haohaohao.xyz`

IPv4 和 IPv6 的推荐命令分别为
```bash
go run ./cmd/mcis --budget 500 --concurrency 50 --heads 8 --beam 32 -v --out text --sni example.com --host-header example.com --cidr-file .\ipv4cidr.txt
go run ./cmd/mcis --budget 2000 --concurrency 100 --heads 8 --beam 32 -v --out text --sni example.com --host-header example.com --cidr-file .\ipv6-cidr.txt
```

注意，本项目使用的是 https 真返回测速，所以显示延迟会是其它工具的结果加上一个固定值，使用起来是一样的。使用你的网站作为 sni 和 host，可以保证优选出来的 ip 当前在你的区域一定对你的网站生效，如有特殊需求还可自定义 path。

推荐在晚高峰时段运行测试，因为本项目采用不同 IP 之间的延迟差异来缩小查找范围，差异越小，收敛越困难。

## 特色

- **递进式搜索**：不是全段扫描，而是对表现更好的子网逐步“下钻拆分”，把预算集中到更有潜力的区域。
- **多头分散探索**：同时并行探索多个次优子网，降低“先入为主”陷入局部最优的概率。
- **IPv4 / IPv6 同时支持**：CIDR 解析、拆分、采样、探测全流程支持 v4/v6 混合输入。
- **强制直连探测**：即使系统/环境变量配置了代理，本工具也会**忽略 `HTTP_PROXY/HTTPS_PROXY/NO_PROXY`**，确保测速不被代理污染。
- **探测方式**：默认对 `https://<ip>/cdn-cgi/trace` 发起请求（可自定义 `--sni` / `--host-header` / `--path`）。
- **输出格式**：支持 `jsonl` / `csv` / `text`。

## 环境要求

- Go 1.20+（推荐使用较新版本）
- 可访问 Cloudflare（探测需要能建立到目标 IP 的 TLS 连接）

## 安装 / 构建

在仓库根目录：

```bash
go test ./...
go build -o mcis ./cmd/mcis
```

Windows PowerShell 也可以直接：

```powershell
go build -o mcis.exe .\cmd\mcis
```

## 快速开始

### 1）用单个 CIDR（IPv4）

```bash
./mcis --cidr 1.1.1.0/24 --budget 200 --concurrency 50 --heads 4 --beam 32 -v --out text
```

### 2）用单个 CIDR（IPv6）

```bash
./mcis --cidr 2606:4700::/32 --budget 500 --concurrency 100 --heads 4 --beam 32 -v --out jsonl
```

### 3）从文件读取多个 CIDR（IPv4/IPv6 混合）

```bash
./mcis --cidr-file cidrs.txt --budget 2000 --concurrency 200 --heads 4 --beam 32 -v --out csv --out-file result.csv
```

## 项目自带网段（bgp.he.net 高可见度）

仓库内自带一份 **Cloudflare 实际在用（BGP 可见度高）**的网段列表：

- `ipv6-cidr.txt`：**2025-12-21** 从 `bgp.he.net` 抓取整理，筛选条件为 **visibility > 90%** 的 Cloudflare IPv6 前缀（每行一个 CIDR）。

说明：

- 该列表用于提供一个“更贴近实际在用”的候选搜索空间，减少在冷门/未广播段上的无效探测。
- BGP 可见度与实际可用性会随时间变化；建议你按需定期更新该文件。

## CIDR 文件格式（`--cidr-file`）

- 每行一个 CIDR
- 支持空行
- 支持 `#` 注释（行首或行尾）

示例 `cidrs.txt`：

```text
# v4
1.1.0.0/16
1.0.0.0/16

# v6
2606:4700::/32
```

## 输出说明

### `--out text`

每行：

- `rank`
- `ip`
- `score_ms`（越小越好；失败会被惩罚）
- `ok/status`
- `prefix`
- `colo`（若 trace 返回包含该字段）
- `dl_*`（可选）：若启用下载测速（见下方 `--download-top`），会追加 `dl_ok/dl_mbps/dl_ms` 等字段

### `--out jsonl`

一行一个 JSON，对应 `TopResult` 结构，包含：`ip/prefix/ok/status/connect_ms/tls_ms/ttfb_ms/total_ms/score_ms/trace/...`

### `--out csv`

包含常用字段列，适合直接导入表格分析。

## 参数详解（常用）

- `--cidr`：输入 CIDR（可重复）
- `--cidr-file`：从文件读取 CIDR
- `--budget`：总探测次数（越大越稳，但更耗时）
- `--concurrency`：并发探测数量
- `--top`：输出 Top N IP
- `--timeout`：单次探测超时（如 `2s` / `3s`）
- `--heads`：多头数量（分散探索）
- `--beam`：每个 head 保留的候选前缀数量（越大越“发散”）
- `--min-samples-split`：前缀至少采样多少次才允许下钻拆分
- `--split-step-v4`：IPv4 下钻时前缀长度增加步长（例如 `/16 -> /18` 用 `2`）
- `--split-step-v6`：IPv6 下钻时前缀长度增加步长（例如 `/32 -> /36` 用 `4`）
- `--max-bits-v4` / `--max-bits-v6`：限制下钻到的最细前缀
- `--sni`：TLS SNI（默认 `example.com`）
- `--host-header`：HTTP Host（默认 `example.com`）
- `--path`：请求路径（默认 `/cdn-cgi/trace`）
- `--out`：输出格式 `jsonl|csv|text`
- `--out-file`：输出到文件（默认 stdout）
- `--seed`：随机种子（0 表示使用时间种子）
- `-v`：输出进度到 stderr

## 下载速度测试（search 后对 Top IP）

搜索结束后，可对排名靠前的 IP 进行**下载速度测试**（默认 URL：`https://speed.cloudflare.com/__down?bytes=50000000`）。

- `--download-top`：对 Top N 进行测速（默认 5，设为 0 关闭）
- `--download-bytes`：下载大小（默认 50000000）
- `--download-timeout`：单个 IP 下载测速超时（默认 45s）

提示：

- 下载测速会消耗明显流量与时间（50MB/个 IP），建议先用小 N 验证。
- 本项目同样会**强制直连**并忽略代理环境变量，避免测速被代理扭曲。

## 代理/直连说明（重要）

本工具探测时**强制直连**：即使你设置了环境变量（如 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`），也不会生效。

如果你希望“走代理”测速，请不要用本项目的探测器逻辑，或自行修改代码（`internal/probe/trace.go` 中 `Transport.Proxy` 被显式设为 `nil`）。

（这样设计是为了避免在系统代理环境下得到被代理扭曲的延迟/可用性结果。）

## 常见问题

### 为什么全部 `ok=false`？

常见原因：

- 网络无法直连到目标 IP 的 443
- 本地网络/防火墙拦截
- 目标 IP 不支持当前 `--sni/--host-header/--path` 组合

建议先把 `--timeout` 调大一点，并尝试使用默认参数（`example.com` + `/cdn-cgi/trace`）。

## License

待补充。