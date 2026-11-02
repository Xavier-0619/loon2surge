# loon2surge

把 Loon 插件（`.lpx`）转换成 Surge 模块（`.sgmodule`）。
**单二进制、零第三方依赖**，用 Go 重写；顺带每天自动同步[可莉的插件中心](https://hub.kelee.one/)。

设计原则只有一条：**宁可少转一行并告诉你，也不生成能导入但行为不对的模块。**

## 目录速览

```
main.go                命令行入口（convert / sync）
internal/lpx/          转换引擎：规则 / 改写 / 脚本 / 头部 / 组装
internal/kelee/        同步：拉列表、比哈希、下载、转换、写索引
internal/cliargs/      让标志可以写在路径后面
loon/  surge/          Loon 原始插件 / 转换后的 Surge 模块（自动生成，勿手改）
index.json             每个插件的哈希、状态与诊断（自动生成）
INDEX.md               给人看的插件索引（自动生成）
.github/workflows/     定时同步流水线
```

## 快速开始

```bash
go build -o loon2surge .          # 需要 Go 1.22+

# 单个文件，输出到同目录
./loon2surge loon/知乎去广告.lpx

# 整个目录 → 指定输出目录 + JSON 报告
./loon2surge convert Loon -o Surge --report build/report.json

# 只看诊断，不落盘
./loon2surge convert Loon --check
```

## 命令参考

| 命令 | 作用 |
| --- | --- |
| `loon2surge convert <文件\|目录\|-> [-o 输出] [--check] [--report x.json]` | 转换插件；`-` 表示读标准输入 |
| `loon2surge sync [--root .] [--jobs 8] [--probe] [--dry-run]` | 同步可莉插件中心并转换成模块 |
| `loon2surge sync --index-file list.json` | 用本地列表离线调试 |
| `loon2surge sync --only A,B --limit 5` | 只处理指定/前 N 个（调试用，不会误删其它插件） |
| `loon2surge version` | 版本号 |

转换时并行读取与解析（`runtime.NumCPU()` 个 worker），进度按文件顺序输出，日志不会交错。

## 自动同步流水线

每天**北京时间 02:00**（`cron: 0 18 * * *`）跑一次，有变化才提交：

```
hub.kelee.one/list.json ──► loon/*.lpx ──► surge/*.sgmodule
                        ├──► index.json   哈希 / 状态 / 诊断
                        └──► INDEX.md     插件索引表格
```

| 步骤 | 做什么 |
| --- | --- |
| 📥 检出仓库 | 拉全量历史，方便判断改动 |
| 🐹 准备 Go 环境 | 按 `go.mod` 装 Go，走官方缓存 |
| 🧪 转换器自检 | `go vet` + `go test`，转换逻辑坏了就别碰数据 |
| 🔨 编译转换器 | 只编译一次，后面全部复用二进制 |
| 📡 上游连通性自检 | 手动触发时可选，先看是不是被 403 |
| 🔄 拉取插件并转换 | 下载 → 哈希比对 → 转换，8 并发 |
| 📊 汇总仓库现状 | 把插件/模块数量写进 Job Summary |
| 🚀 提交并推送 | 提交信息带日期：`🔄 自动更新 2026-11-02` |
| 🩺 汇报失败 | 同步出错时把整轮标红，但已成功的改动照样落盘 |

提交信息长这样，日期在标题里，统计在正文：

```
🔄 自动更新 2026-11-02

统计：新增 1 · 更新 3 · 删除 0 · 失败 0 · 合计 275
```

**对比方式**：上游的 `date` 字段是站点重建时间（275 条里 273 条完全相同），当不了版本号。
所以每轮都下载并算 SHA-256，哈希没变就不落盘、不重转，`index.json` 也不会重写——
Git 里不会出现"只改时间戳"的空提交。

**首次启用**：

1. 推上仓库，确认 `Settings → Actions → General → Workflow permissions` 是 **Read and write**
2. `Actions → 可莉插件自动同步 → Run workflow`，模式选 **只做连通性自检**
3. 通了再选 **同步并提交** 跑一次，`loon/` 和 `surge/` 就有内容了

**转换失败时**：旧模块会被删除（避免留下和 lpx 版本对不上的旧模块），`index.json` 里标
`convert-failed` 并附原因。下载失败时不动旧文件，只在报告里记一笔。

### 被 Cloudflare 拦住怎么办

`kelee.one` 挂着 Cloudflare 的机器人防护，**机房 IP 是直接 403 拒绝**（不是人机验证）。
GitHub 托管 runner 正是机房 IP，所以第一次跑很可能就撞上。先自检：

```bash
./loon2surge sync --probe
```

三条出路，按推荐顺序：

| 方案 | 做法 |
| --- | --- |
| 自建镜像 | 找能放静态文件的地方挂一份 `Tool/Loon/Lpx/*.lpx`，在仓库 Variables 加 `KLEE_MIRROR=https://你的前缀`。程序先试官方，失败自动退到镜像 |
| 自托管 runner | 家里那台机器装 runner，出口是家宽，直接能下 |
| 本地跑 | 同一份命令：`./loon2surge sync` 然后 `git push`；`--dry-run` 先看差异 |

对 403 有记忆：一轮里撞过墙的域名不反复重试，**不删已有文件、不写空索引**，
在 Job Summary 写警告并以退出码 3 结束。

## 转换规则

| Loon | Surge | 说明 |
| --- | --- | --- |
| `#!name/desc/author/icon/tag/...` | 同名 `#!` | 不认识的字段丢弃并提示 |
| `#!system=iOS/iPadOS` | `#!system=ios` | 两端都给时省略 |
| `[Argument]` | `#!arguments=名:默认值` | 未被引用的参数自动删除 |
| `{Name}` / `${Name}` | `{{{Name}}}` | 只替换已声明的参数，不碰 `${url}` |
| `real-ip` | `always-real-ip = %APPEND%` | 多条合并去重 |
| `include-tun` | `tun-included-routes = %APPEND%` | 规范化成网络地址 |
| `ipv6-vif = off` | `ipv6-vif = disabled` | `auto`/`always` 不生成覆盖项 |
| `[Rule]` 裸域名 | `DOMAIN,x,REJECT,extended-matching,pre-matching` | 广告拦截语义 |
| 域名类规则 | 补 `extended-matching` | 拒绝策略再补 `pre-matching` |
| IP 类规则 | 补 `no-resolve` | 拒绝策略再补 `pre-matching` |
| `AND/OR/NOT` 逻辑规则 | 递归补齐子 matcher 选项 | 保留原括号结构 |
| `URL-REGEX,...,REJECT-DICT/IMG` | `[Map Local]` | 空 JSON / 1px gif |
| `request if ${url} ~= /re/i then ...` | 各 HTTP section | `/i` 转成 `(?i:re)` |
| `response.json.jq(...)` | `http-response-jq` | 单引号包裹，内部 `'` 转 `\u0027` |
| `response.json.delete(...)` | `delpaths([[...]])` | 支持 `a.b`、`a[0].b` |
| `reject_dict(200)` | `[Map Local]` 空 JSON | 状态码限定 200–999 |
| `request.header.set` | `header-del` + `header-add` | Surge 没有 set |
| `redirect(302, url)` | `[URL Rewrite] pattern url 302` | 只支持 302/307 |
| `http-request <pattern> script-path=...` | `type=http-request, pattern=...` | 补 Loon 的默认 timeout |
| `cron {Cron} ...` | `type=cron, cronexp="{{{Cron}}}"` | |
| `enable={Arg}` | 行前缀 `{{{Arg}}}` + 默认值 `#` | 参数默认值决定默认开关 |
| `[MITM] hostname=` | `hostname = %APPEND%` | 多条合并，`%APPEND%` 只补一次 |

输出 section 顺序固定：`General → IP Rewrite → Rule → URL Rewrite → Header Rewrite → Body Rewrite → Map Local → Script → MITM`。

## 无法转换时怎么办

分三档，都不会「悄悄丢掉」：

1. **单行注释 + 报告** — 例如 `url.replace`、非拒绝策略下的 `pre-matching`。
   该行以 `#` 注释保留在文件末尾，模块其余部分照常可用。
2. **策略级注释** — Surge 模块 `[Rule]` 只允许 `DIRECT/REJECT/REJECT-TINYGIF`，
   出现 `PROXY` 等策略时只注释这一条规则，不让整个模块导入失败。
3. **整模块失败** — 只有结构性错误（例如 `[IP Rewrite]` 写了非法地址）才拒绝产出，
   此时不会覆盖上一版文件。

诊断都带错误码（`rule-policy`、`rewrite-action`、`script-enable`…），`--report` 可输出 JSON 给 CI 判断。

## 性能

同一份 5.6 KB 的真实插件（知乎去广告），本机实测：

| 指标 | Python 版 | Go 版 | 提升 |
| --- | --- | --- | --- |
| 单次转换（进程内） | 1.090 ms | 0.167 ms | **6.5×** |
| 命令行端到端（含启动） | 668 ms | 97 ms | **6.9×** |
| 批量转换 | 串行 | 按 CPU 数并行 | 满载时再快数倍 |

再加上同步阶段 8 并发下载、下载完就地转换（不再走第二遍遍历），275 个插件的整轮耗时主要花在网络等待上。

## 代码结构

```
main.go             命令行：参数、批处理、控制台与 JSON 报告
internal/lpx/
  model.go          Plugin / Ctx / Note / Result、section 顺序、版本门槛
  syntax.go         顶层切分、正则字面量、引号、JQ 单引号、.lpx 解析
  rule.go           [Rule] → [Rule]/[Map Local]，含逻辑规则括号解析器
  rewrite.go        [Rewrite] → URL / Header / Body / Map Local（V2 + 旧语法）
  script.go         [Script] → [Script]，含 enable 行前缀方案
  convert.go        [Argument]/[General]/[IP Rewrite]/[MITM] + 组装输出
  convert_test.go   23 个用例 + 整篇金标准比对 + 基准测试
internal/kelee/
  http.go           多来源抓取、403 记忆、退避重试
  store.go          索引读写、INDEX.md、内容不变就不写盘
  sync.go           哈希比对、并发下载、删除保护、报告
internal/cliargs/   标志与位置参数分离
```

依赖单向：`model ← syntax ← rule/rewrite/script ← convert ← main`，没有循环引用。
每个 section 的处理器签名统一成 `func(ctx *Ctx, 行号 int, 行 string) ([]Emit, error)`，
认不出来就返回 `Unsupported{Code, Message}`，上层统一注释并写报告。

### 想加一种新语法

1. 在 `rewrite.go` 的动作分发里加一条，或新写一个函数返回 `[]Emit`；
2. 认不出来时返回 `unsupported("code", "原因")`，上层会自动注释并写报告；
3. 需要更高客户端版本就调用 `ctx.Need(ReqModern)`；
4. 去 `convert_test.go` 补一个用例（改动影响面大时顺手更新 `testdata/zhihu.sgmodule`）。

`[Rule]` 的新类型加进 `rule.go` 的 `domainTypes` / `ipTypes` / `plainTypes` 之一即可，选项补齐是自动的。

## 版本门槛

按用到的特性自动取最大值，写进 `#!requirement`：

| 触发条件 | `CORE_VERSION>=` |
| --- | --- |
| 模块参数、`extended-matching`、`[Map Local]`、`[Body Rewrite]` | `20` |
| JQ、`pre-matching`、`URL-REGEX` 的 `extended-matching` | `6008000` |
| `[IP Rewrite]`、`tun-included-routes`、`DOMAIN-WILDCARD`、`PROCESS-NAME` | `6010000` |

## 已知边界

- `url.replace(a, b)` 不转换：Loon 是「URL 子串替换」，Surge 的 `[URL Rewrite]` 是「整条 URL 替换」，两者不等价，硬转会改坏跳转。
- `[Script]` 的 `generic` 会照转但给出警告：Loon 有被选节点上下文，Surge 没有。
- `network-changed` 触发器、`response.body.mock`、Loon 专属的 `img-url` 等暂无对应。
- MITM 不会自动推断：源插件没写 `hostname` 但存在改写行时，只提示手动补。
- 静态检查通过 ≠ 真机可用。远程脚本、正则引擎差异仍需在 Surge 里实测一遍。

## 测试

```bash
go test ./...                     # 单元 + 端到端（本地假站点，不联网）
go test ./internal/lpx -bench .   # 转换性能
```

## 关于内容归属

`loon/` 里是**可莉的原创插件**，不是本仓库的东西。可莉在
[ProxyResource](https://github.com/luestr/ProxyResource) 写明：仅供学习研究，
禁止在中国境内平台公开传播或售卖，转载需保留署名并遵守 CC BY-NC-SA 4.0。

所以这个镜像**建议保持私有仓库**。另外官方明确说过"所有插件均为 Loon 专用，
不建议其他工具转换使用"——`surge/` 里的东西是给自己试验的，别拿去问可莉。
