# loon2surge

把 Loon 插件（`.lpx`）转换成 Surge 模块（`.sgmodule`）。
**单二进制、零第三方依赖**，用 Go 重写；顺带每天自动同步[可莉的插件中心](https://hub.kelee.one/)。

设计原则只有一条：**宁可少转一行并告诉你，也不生成能导入但行为不对的模块。**

## 模块网站

打开 [**Loon2Surge 模块库**](https://xavier-0619.github.io/loon2surge/)，即可搜索、
按分类浏览并一键导入 Surge；也可以查看每个模块的转换诊断、复制模块链接。
网站是纯静态页面，无需后端，模块文件直接从本仓库的 Raw 地址加载。

网页只上架 `status = ok` 且**没有待人工确认项**的模块；带 `review` 诊断的模块
（含无法自动确认的内容）不在页面上展示，但其产物仍保留在 `surge/` 与索引中
供人工核查。过滤在前端完成，因此每日同步刷新 `index.json` 后无需额外处理。

首次部署时，在仓库 **Settings → Pages → Build and deployment** 中将 Source 选为
**GitHub Actions**。推送到 `main` 后，`🌐 发布模块网站` 工作流会自动发布 `docs/`，
并在每次 `index.json` 更新后同步网站数据。本地预览：

```bash
python -m http.server 8080 --directory docs
# 打开 http://127.0.0.1:8080
```

## 外部模块（ios151/Rewrite）

可莉那边只覆盖 Loon 插件。有些功能上游没人写成 Loon 插件，但别的仓库已经有现成的
Surge 模块，所以每天转换完之后会再跑一次 `loon2surge external`：

- **快照**：把 [ios151/Rewrite](https://github.com/ios151/Rewrite) 里的 `.sgmodule`
  同步到 [`external/ios151/`](external/ios151)，保留上游目录结构，外加一份 `manifest.json`
  记下每个文件的下场（并入 / 重复 / 不兼容）
- **只收上游独有的**：按 `#!name` 比而不是文件名。比对前先摘掉「解锁 / 去广告 / VIP」
  这类只说明"怎么处理"的词，所以「酷狗音乐会员解锁」认得和「酷狗音乐去广告」是同一件事；
  中文名允许前缀重合与高相似度，纯英文名只认完全一致（否则 `linearitcurve` 会被判成 `Line去广告`）
- **撞名保留可莉的**：Loon 插件是转换器逐行核对出来的，比手写模块可靠
- **按 Surge 语法整理**：去 BOM、换行与行尾空白归一、`#!key = value` 收成 `#!key=value`、
  头部按 `MetaOrder` 排序、补齐缺失的 `#!author` / `#!category`、section 按惯例排序并用空行分开、
  删掉空 section 与段内完全重复的行；`[Script]` 去掉行尾多余逗号、`requires-body` 的 `true/false`
  统一成 `1/0`、`pattern` 里主机名没转义的点补成 `\.`；`[MITM]` 合并多行 `hostname`、去重、
  去掉 Surge 不认的端口。**规则内容、正则分支、脚本参数的值一律不碰**，动了什么逐条记进清单的
  `tidy` 字段
- **不收**：缺 `#!name`、小火箭专用、混着 Loon 语法（`script-type` 报错）。这些在清单里写明原因
- **中文参数名**：Surge 认 `日志级别:info` 这种写法，转换器的校验规则只认 ASCII。
  这类问题只在清单里登记豁免，`validate` 按清单逐文件放行这一条，其它问题照旧报错
- **跟着上游下线**：上游删掉的模块会从 `surge/` 移除，但只删"我们还是当初写进去那份"的文件，
  人工改过的不动

手动跑：

```bash
./loon2surge external --dry-run      # 只看会加什么
./loon2surge external                # 落盘
```

## 目录速览

```
cmd/loon2surge/        进程入口（只有 main，参数交给 internal/cli）
internal/cli/          命令行：参数解析、退出码、各子命令的编排
internal/lpx/          转换引擎：规则 / 改写 / 脚本 / 头部 / 组装 / 产物自检
internal/upstream/     同步：拉列表、比哈希、下载、转换、写索引、远程脚本审计
internal/external/     并入第三方仓库现成的 Surge 模块（按 #!name 去重）
internal/fsutil/       统一落盘：内容不变不写、临时文件 + rename
loon/  surge/          Loon 原始插件 / 转换后的 Surge 模块（按插件里的 #!name 命名）
external/ios151/       第三方模块快照与清单（自动生成，不要手改）
index.json             每个插件的哈希、状态与诊断（自动生成）
INDEX.md               给人看的插件索引（自动生成）
scripts/               上游监听脚本（只在 CI 里跑）
docs/                  GitHub Pages 模块网站（静态 HTML / CSS / JS）
.github/workflows/     每日同步 / PR 校验 / 上游监听 / Pages 发布
AGENTS.md              开发约定与踩过的坑（改代码前先看）
RULES.md               转换规则与"为什么这一行不转"
```

## 快速开始

```bash
go build -o loon2surge ./cmd/loon2surge     # 需要 Go 1.27+

# 单个文件，输出到同目录
./loon2surge loon/知乎去广告.lpx

# 整个目录 → 指定输出目录 + JSON 报告
./loon2surge convert loon -o surge --report build/report.json

# 只看诊断，不落盘
./loon2surge convert loon --check
```

## 命令参考

| 命令 | 作用 |
| --- | --- |
| `loon2surge convert <文件\|目录\|-> [-o 输出] [--check] [--report x.json]` | 转换插件；`-` 表示读标准输入 |
| `loon2surge sync [--root .] [--jobs 4] [--probe] [--dry-run]` | 同步可莉插件中心并转换成模块 |
| `loon2surge sync --fetcher go` | 强制用 Go 原生通道下载（默认 auto：有 python3 就借它的 TLS 栈） |
| `loon2surge sync --index-file list.json` | 用本地列表离线调试 |
| `loon2surge sync --only A,B --limit 5` | 只处理指定/前 N 个（调试用，不会误删其它插件） |
| `loon2surge validate [--dir surge]` | 把已发布的模块当成品再校验一遍（语法残留、策略、状态码、section 顺序） |
| `loon2surge audit [--strict]` | 抓一遍模块引用的远程脚本，记录状态码与 SHA-256 |
| `loon2surge external [--dir external/ios151] [--dry-run]` | 把第三方仓库的模块同步到快照目录，并把 `surge/` 里没有的并进去 |
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
| 🔄 拉取插件并转换 | 下载 → 定名 → 哈希比对 → 转换，4 并发 |
| 📊 汇总仓库现状 | 把插件/模块数量、实际下载通道写进 Job Summary |
| 🚀 提交并推送 | 提交信息由程序生成：标题是北京日期，正文列出这次动过的模块 |
| 🩺 汇报失败 | 同步出错时把整轮标红，但已成功的改动照样落盘 |

另有三个配套流水线：

- **✅ 代码与产物校验**（`validate.yml`）：PR 和 push 到 main 时跑格式、`go vet`、测试，
  以及 `loon2surge validate` 把仓库里已有的模块当成品再读一遍，并核对 `index.json` 与 `surge/` 是否一一对应。
- **🛰️ 上游更新监听**（`upstream-watch.yml`）：每周一北京时间 09:00 看一眼
  [deanxizian/loon_to_surge](https://github.com/deanxizian/loon_to_surge) 的新提交
  （跳过 `chore: update Kelee Surge modules`），有值得借鉴的内容就让模型读 diff 生成改动，
  跑通测试后开一个 draft PR 等人审核。模型判断不需要移植或调用失败时只开 issue 提醒，不动代码。
- **🧑‍⚖️ 上游 PR 自动审核**（`upstream-review.yml`）：只认带 `upstream-sync` 标签的 PR。
  先跑构建、测试、产物自检，再让模型读 diff 逐条对照 `RULES.md` 与 `AGENTS.md`。
  三关全过才批准（draft 转正 + approve），**从不自动合并**。

### 上游监听与审核怎么配

**开箱即用，不用配任何密钥。** 模型走 GitHub 自带的 Models 网关
（`https://models.github.ai/inference`，模型 `openai/gpt-4o-mini`），
凭证是 workflow 里的 `github.token`，成本记在 GitHub 头上。对应的权限是
`permissions: models: read`——**删了这行就会降级成 issue 提醒**。

想换更强的模型，再加下面的配置（有自己的 key 就优先用，没有才回落到 GitHub Models）：

| 类型 | 名称 | 说明 |
| --- | --- | --- |
| secret | `AI_API_KEY` | 兼容 OpenAI 接口的密钥，配了就不再用 GitHub Models |
| variable | `AI_BASE_URL` | 默认 `https://api.deepseek.com/v1`（仅配了 `AI_API_KEY` 时生效） |
| variable | `AI_MODEL` | 默认 `deepseek-chat`（同上）；用 GitHub Models 时默认 `openai/gpt-4o-mini` |
| secret | `REVIEW_TOKEN` | 细粒度 PAT，勾 `Pull requests: Read and write`；不配也能审，只是批不了（bot 批不了自己开的 PR） |

审核通过会自动 approve，但**不会自动合并**——点 merge 那一步永远是人。进度记在 `.upstream-sync.json` 里，
一轮的最大处理量是 12 个提交，超出的下一轮继续。

### 文件叫什么名字

`loon/` 与 `surge/` 里的文件名取插件自己的 `#!name`，不是下载地址里的文件名：

```
loon/51Job_remove_ads.lpx  ──►  #!name=前程无忧去广告  ──►  loon/前程无忧去广告.lpx
                                                             surge/前程无忧去广告.sgmodule
```

上游只改名字也会让文件跟着改名（提交信息里记一条"重命名"），不会变成"删一个 + 加一个"。
定名时挡掉了文件名不该有的东西：`/ \ : * ? " < > |`、控制字符、首尾空白与句点、
长度超过 200 字节的截断（按 UTF-8 边界切，不会切碎汉字）、Windows 设备名（`CON` 之类）。
真出现同名（可莉那边有两个"挖财记账去广告"）就按插件地址排序，后来的加 `-2`——
排序基准是地址而不是下载顺序，所以同一份列表每轮算出的名字一定相同，不会天天改名。

**改名靠地址认人**：`index.json` 用插件地址当键，`stem` 只是当时的文件名。
这样改一次名不会被误判成"旧插件下架 + 新插件上线"。

### 提交信息

由 `loon2surge` 直接生成（`--commit-file`），标题只放北京日期，正文每条明细都是同一个形状：
**时间在前、模块在后**。

```
🔄 自动更新 2026-11-03

新增 1 · 更新 3 · 重命名 1 · 删除 1 · 失败 0 · 需人工确认 1 · 合计 275

新增模块 1 个：
- 2026-10-02 13:57:49 · 前程无忧去广告.sgmodule

更新模块 3 个：
- 2026-10-02 13:57:49 · 财新去广告.sgmodule · 已适配
- 2026-10-02 13:57:49 · 流媒体解锁检测.sgmodule
- 2026-10-19 23:24:57 · 代理链路检测.sgmodule · ⚠️ 需人工确认 · 已适配

重命名 1 个（文件名跟随 #!name）：
- 2026-10-02 13:57:49 · Stream_Check → 流媒体解锁检测

删除模块 1 个（上游已下架）：
- 2026-10-02 13:57:49 · Old_Plugin.sgmodule

⚠️ 需人工确认 1 个（转换器没能自动处理，请看一眼）：
- 2026-10-19 23:24:57 · 代理链路检测.sgmodule · generic 脚本没有节点上下文

已自动适配 2 个（转换器做了等价改写，功能不受影响）：
- 2026-10-02 13:57:49 · 财新去广告.sgmodule · 属性名归一（下划线转连字符） ×6；重名脚本自动加后缀
- 2026-10-19 23:24:57 · 代理链路检测.sgmodule · 属性名归一（下划线转连字符）

同步时间：2026-11-03 02:31:05（北京时间）
```

时间一律取可莉 `list.json` 里的 `date`，不是本机同步时刻；上游没给日期时写"未知时间"，
也占住时间位，保证每行能竖着对齐扫。单个分组超过 60 条会折叠成 `… 其余 N 个略`。

**角标说明**：模块名后面挂 `已适配` 表示转换器替它做了等价改写；挂 `⚠️ 需人工确认`
表示有原始行没能自动处理，或者运行时行为和 Loon 不完全一样。正文末尾两节会把原因摊开写。
只看本轮改动过的模块——历史欠账不会每轮刷一遍；完整的存量清单在 Job Summary 里。

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

`kelee.one` 挂着 Cloudflare 的机器人防护，它看三样东西：**出口 IP**、**请求头**、
**TLS 握手指纹**（JA3/JA4）。前两样能对齐，第三样在 Go 里根本改不了：

> Go 的 `crypto/tls` 发出的 ClientHello 是公开指纹，Cloudflare 直接判成自动化流量，
> 回 `403 + cf-mitigated=challenge`（托管质询）。这不是 IP 封锁——换 IP 也没用，
> 同一个 runner 上换成 Python 的 OpenSSL 栈就能过，上游同款项目正是这么做的。

所以下载这一步改成"借外部 TLS 栈"：`python3` 里的 `urllib` 负责取字节（嵌入式脚本
`internal/upstream/pyfetch.py`，25 行，不含任何业务逻辑），解析、转换、落盘全部仍在 Go 里。
找不到 python3 就退回 Go 原生通道，功能不缺，只是更容易被质询——日志里会写明用的是哪条。

| 项 | 取值 | 为什么 |
| --- | --- | --- |
| 下载通道 | `python3`（auto 优先） | TLS 指纹与上游同款；`--fetcher go` 可强制原生 |
| UA | `Loon/860 … Darwin/24.4.0` | 站点按 UA 放行，浏览器 UA 反而 403 |
| 协议 | 只走 HTTP/1.1 | Go 默认的 h2 指纹更容易被判成爬虫 |
| 请求头 | 只有 UA / Accept / Accept-Language | 不发 Referer，也不发 Accept-Encoding |
| 并发 | 4 | 不要在 Cloudflare 面前形成突发 |
| 重试 | 网络类错误退避重试 3 次，403 立刻停 | 403 是整台机器的判决，原地重试只是浪费 |

排错先看报错里的 `cf-mitigated` 字段：

| 现象 | 含义 | 处理 |
| --- | --- | --- |
| `403，cf-mitigated=challenge` | TLS 指纹被质询 | 确认通道是 `python3`（`--probe` 第一行会打印） |
| `403`，没有 `challenge` | 出口 IP 被拒 | 换 runner 重试即可，workflow 会自动换三台 |

确实是 IP 抽签时，workflow 失败会自动换一台 runner 重来，最多三台。先自检：

```bash
./loon2surge sync --probe
```

三条出路，按推荐顺序：

| 方案 | 做法 |
| --- | --- |
| 自建镜像 | 找能放静态文件的地方挂一份 `Tool/Loon/Lpx/*.lpx`，在仓库 Variables 加 `KLEE_MIRROR=https://你的前缀`。程序先试官方，失败自动退到镜像 |
| 自托管 runner | 家里那台机器装 runner，出口是家宽，直接能下 |
| 本地跑 | 同一份命令：`./loon2surge sync` 然后 `git push`；`--dry-run` 先看差异 |

万一连 `list.json` 都被拒：程序会退回仓库里上一轮的 `index.json`，继续对齐已有插件，
**不删任何文件、不写空索引**，并在 Job Summary 里注明本轮用的是旧列表；连旧索引都没有
才以退出码 3 结束。对 403 也有记忆——一轮里撞过墙的域名不反复重试。

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
| V2：`request if ${url} ~= /re/i then script("url") with …` | `type=http-request, pattern=...` | `request`/`response` → `http-request`/`http-response`，路径取自 `script(…)` |
| V2：`generic then script("url") with tag=…` | `type=generic, script-path=…` | `img_url` 等显示元数据丢弃 |
| V2：`script("url", {${A}, ${B}}) with …` | `argument="{…}"` + `#!arguments` | Surge 没有位置参数，拼成 JSON 交给 `$argument` |
| `requires_body=true` / `binary_body_mode` | `requires-body` / `binary-body-mode` | 下划线写法归一成连字符，`requires-body` 丢了脚本就读不到响应体 |
| 同名 `tag` 出现多次 | 自动加 `-2`、`-3` 后缀 | Surge 里同名脚本只会留下一条生效 |
| `cron {Cron} ...` | `type=cron, cronexp="{{{Cron}}}"` | |
| `enable={Arg}` | 行前缀 `{{{Arg}}}` + 默认值 `#` | 参数默认值决定默认开关 |
| `[MITM] hostname=` | `hostname = %APPEND%` | 多条合并，`%APPEND%` 只补一次 |

输出 section 顺序固定：`General → IP Rewrite → Rule → URL Rewrite → Header Rewrite → Body Rewrite → Map Local → Script → MITM`。

## 无法转换时怎么办

先把两件事分开：**转换器替你改好了**，和**转换器没能处理、要你看一眼**。
两者都会写进 `index.json`（`adapted` / `review` 两个字段）、提交信息、Job Summary，
不靠翻日志。

| 档位 | 触发 | 结果 | 记录在哪 |
| --- | --- | --- | --- |
| 已自动适配 | `requires_body` 归一、重名脚本加后缀、V2 语法改写、参数清单转 `argument` | 模块照常产出，功能不受影响 | 提交信息「已自动适配」+ `adapted` 字段 |
| **需人工确认** | 有行只能注释保留、`generic` 脚本运行时差异、缺 `[MITM]`、未声明的参数 | **模块照常产出**，原文注释在文件末尾 | 提交信息「⚠️ 需人工确认」+ `review` 字段 |
| 整份排除（Loon 专用） | 去掉无法转换的部分后什么都不剩 | 不生成模块，保留 `.lpx` | 报告「未生成」，不算失败 |
| 转换失败 | 源文件本身有毛病（`[Argument]` 缺默认值、非法地址） | 不覆盖上一版，单独列出等人处理 | 报告「转换失败」，退出码非 0 |

前三档的共同点是**不假装成功**：能改的改完并写清改了什么，改不了的留原文注释、把文件位置
和人该看的东西一并标出来。

### 那一行到底怎么处理

遇到转不了的行，按这个顺序想，不要停在"注释掉"就完事：

1. **能等价改写就改写** — `requires_body`→`requires-body`、V2 的 `script("url")`→`script-path=`、
   `{${A}}`→`argument` JSON。这类改写写进 `adapted`，并加一条回归用例钉住。
2. **整条规则装不进 Surge 就只注释这条** — `PROXY` 策略的规则注释掉，模块其余部分照常可用。
3. **只剩注释就没意义就整份排除** — 见下一节。
4. **运行时行为对不上就标「需人工确认」** — 例如 `generic` 脚本拿不到 Loon 的节点上下文。
   模块是能导入的，但别让人以为它一定在工作。

### 对照 deanxizian/loon_to_surge

它把每处特殊处理都记进 `Surge/convert-report.json`，用 `kind` 分类，与"转换失败"严格分开：

| 它的 kind | 是什么 | 这里的对应 |
| --- | --- | --- |
| `script-attribute-normalized`、`script-property-corrected` | 属性名/取值规整 | 已自动适配 |
| `script-object-adapted` | `script("url", {${A}})` 参数清单改写 | 已自动适配 |
| `generic-script-adapted` | `generic` 脚本改写 | 已自动适配 + ⚠️ 需人工确认（运行时差异） |
| `source-repair-applied` | 源文件里 `response.json.replace` 之类补成可执行动作 | 直接转换（这里按语法映射，不需要"修"） |
| `script-http-name-shared` | 脚本重名 | 已自动适配（自动加 `-2`；它只报不改，第二条会被第一条覆盖） |
| `module-excluded` | 整份表达不出来 | 未生成（Loon 专用） |

本轮又对齐了它几处更关键的实现（详见 [RULES.md](RULES.md)）：

| 它的行为 | 为什么重要 |
| --- | --- |
| Body Rewrite 写成 `http-response <url> <正则> <替换>`，并显式写出 `(?ims)` / `(?-ims)` | Surge 的 Body Rewrite 默认按行锚定 `^`/`$`，Loon 不是。之前这里输出 `http-response-replace-regex`——那不是合法写法，模块能导入但整行不生效 |
| JQ 表达式按词法去注释、再压成一行 | 直接把换行换成空格，`#` 注释会吞掉后面的代码 |
| `? as name` 捕获 + `${name.1}` 引用 | 19 条 Loon V2 改写依赖它，之前整行注释 |
| `enable` 参数同时是脚本入参时不用行前缀 | 否则脚本拿到的是开关值而不是参数 |
| 产物独立校验器 | 转换器的诊断看不见自己的 bug |

差别只有两处：
- 它对「自动加入TestFlight」这类**部分可转**的插件整份排除；这里会把能转的脚本转出来，
  只把 `PROXY` 规则留在文件末尾注释里——规则少一条能手动补，脚本没了就真没了。
- 它检测到脚本重名但没改名，生成的模块里同名脚本只有一条生效；这里自动加后缀并记录。

### 为什么有的插件天生没有 Surge 模块

Surge 模块的 `[Rule]` 段只接受 `DIRECT`、`REJECT`、`REJECT-TINYGIF`——不能用策略组。
而可莉有些插件的整份内容就是一串"这些域名走代理"：

```
[Rule]
DOMAIN-SUFFIX, dnsleaktest.com, PROXY
DOMAIN-SUFFIX, whoer.net, PROXY
```

这种插件在 Loon 里靠 `PROXY` 指代"当前选中的节点"，搬到模块里没有对等物。
硬转只会得到一个空模块，看着装上其实什么也没拦——所以结论是**不生成**，
但原始 `.lpx` 照常同步下来，在 Loon 里仍然可用。

对照 [deanxizian/loon_to_surge](https://github.com/deanxizian/loon_to_surge)：
它把这类记为 `module-excluded`（275 个里 18 个），与"转换失败"严格分开，
并且明确不为此占住文件名。这里采用同一套语义，中文写作「未生成（Loon 专用）」。
差别只有一处：若插件**除了**这些规则还有可用脚本（例如「自动加入TestFlight」），
这里会把脚本转出来、把无法转换的规则留在文件末尾注释里，而不是整份丢掉。

### 报告长什么样

Job Summary 里每类分开列，一眼能看出哪个要管、哪个不用管：

```
- 新增 275 · 更新 0 · 重命名 0 · 删除 0 · 转换失败 0 · 未生成 1
- 需人工确认 14 · 已自动适配 68

### ⚠️ 需人工确认 14
> 这些模块已经生成并能导入，但里面有转换器没能自动处理的原始行（注释在文件末尾），
> 或者运行时行为和 Loon 不完全一样。改完源插件后下一轮会重新判定。

- `代理链路检测`：generic 脚本没有节点上下文
- `BoxJs`：1 行无法自动转换，原文注释在文件末尾

### 已自动适配 68（不需要处理）
> 转换器做了等价改写，功能不受影响，只是生成的模块和源文件长得不完全一样。

- `财新去广告`：属性名归一（下划线转连字符） ×6；重名脚本自动加后缀

### 未生成模块 1（Loon 专用，不是故障）
> 这些插件的内容 Surge 模块表达不出来（通常是规则里写了 PROXY…）。
> 原始 .lpx 已照常保存，可以在 Loon 里使用；这里不算失败，也不会让流水线变红。

- `DNS防泄露`：模块规则只能用 DIRECT/REJECT，原文用的是 PROXY
```

（实测 273 个插件：需人工确认 14 个、已自动适配 68 个、未生成 1 个、转换失败 0 个。）

诊断都带错误码（`rule-policy`、`rewrite-action`、`script-attribute-alias`…），`--report` 可输出 JSON 给 CI 判断。

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
cmd/loon2surge/main.go  只有一行：把 os.Args 交给 cli.Run
internal/cli/
  cli.go            版本号、用法文本、子命令分发、退出码
  args.go           标志与位置参数分离（可以混着写）
  convert.go        convert：并行批处理、控制台进度与 JSON 报告
  sync.go           sync → upstream.Config
  validate.go       validate：把成品再校验一遍，按清单放行豁免项
  audit.go          audit → upstream.AuditOptions
  external.go       external → external.Options
internal/lpx/
  model.go          section / meta 顺序、Note / Result、诊断过滤
  args.go           [Argument] 定义与值表
  ctx.go            转换上下文：告警、适配、复查、失败
  syntax.go         顶层切分、正则字面量、引号、注释、分词
  parse.go          #! 头部与 .lpx 解析
  jq.go             JQ 单引号 → 合法 JSON 字面量
  rule.go           [Rule] → [Rule]/[Map Local]，含逻辑规则括号解析器
  rewrite.go        [Rewrite] → URL / Header / Body / Map Local（V2 + 旧语法）
  script.go         [Script] → [Script]，含 enable 行前缀方案
  convert.go        [General]/[IP Rewrite]/[MITM] + 组装输出
  validate.go       产物自检：语法残留、策略、状态码、section 顺序
  convert_test.go   用例 + 整篇金标准比对 + 基准测试
internal/upstream/
  run.go            同步入口：读取配置、探测可用性、汇总结果
  sync.go           哈希比对、并发下载、删除保护
  state.go          index.json / INDEX.md 读写
  source.go         来源列表、命名、条目键
  fetch.go          多来源、403 记忆、退避重试、把 403 分成"质询"与"IP 拒绝"
  transport.go      下载通道：python3（默认，借 OpenSSL TLS 栈）/ Go 原生（兜底）
  pyfetch.py        嵌入的 25 行下载脚本，只负责取字节，不含业务逻辑
  summary.go        控制台摘要、重命名清单
  commit.go         提交信息
  audit.go          远程脚本审计
  names.go          文件名清洗与 Windows 保留名
internal/external/
  external.go       并入流程：比对、判重、写回
  source.go         本地目录 / tarball 读取与分类
  tidy.go           按 Surge 语法整理（去 BOM、排序、去重、转义）
  names.go          #!name 归一、相似度、撞名处理
  manifest.go       manifest.json 读写、快照清理
internal/fsutil/    统一落盘：内容不变不写、临时文件 + rename
```

依赖单向：`model ← syntax ← rule/rewrite/script ← convert ← cli`，没有循环引用。
`lpx` 不认命令行，`upstream` / `external` 不认 `flag`——参数一律由 `cli` 填进结构体再传下去。
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

- `url.replace` 只在 request 阶段转换，输出 `pattern 目标 header`（`header` 表示只改 URL、不动请求体里的地址）；
  response 阶段的同名动作没有等价写法，会注释保留。
- `response.body.mock` 内联正文含引号时走 `data-type=base64`；含换行则拒绝——单行 Map Local 装不下。
- `response.body.mock_file` 支持 `mock_file(url)` 和 `mock_file("text", url, 200)` 两种签名。
- `jq_file` / `jq-path` 指向的远程 JQ 暂不抓取内联：转换器不联网，这一行会注释保留并标「需人工确认」。
- `[Script]` 的 `generic` 会照转但给出警告：Loon 有被选节点上下文，Surge 没有。
  个别脚本还要 `$argument.policy` 之类的模块参数（如「代理链路检测」），那属于脚本级知识，
  转换器不会替你猜，需要自己补 `#!arguments=Policy:PROXY` 和 `argument="policy={{{Policy}}}"`。
- `network-changed` 触发器、Loon 专属的 `img-url` 等暂无对应。
- MITM 不会自动推断：源插件没写 `hostname` 但存在改写行时，只提示手动补。
- 静态检查通过 ≠ 真机可用。远程脚本、正则引擎差异仍需在 Surge 里实测一遍。

## 测试

```bash
go test ./...                     # 单元 + 端到端（本地假站点，不联网）
go test ./internal/lpx -bench .   # 转换性能
loon2surge validate --dir surge   # 已发布产物的独立校验
loon2surge audit                  # 远程脚本可达性（会联网）
```

产物有两道闸：转换结束时会对自己刚写出的文本跑一遍 `Validate`（有问题记为
`product-invalid`，该模块不落盘），`validate` 命令则对仓库里已有的文件再跑一遍。
两处用的是同一套规则，所以"转换器自检通过"和"仓库里的文件合规"是同一件事。

## 关于内容归属

`loon/` 里是**可莉的原创插件**，不是本仓库的东西。可莉在
[ProxyResource](https://github.com/luestr/ProxyResource) 写明：仅供学习研究，
禁止在中国境内平台公开传播或售卖，转载需保留署名并遵守 CC BY-NC-SA 4.0。

所以这个镜像**建议保持私有仓库**。另外官方明确说过"所有插件均为 Loon 专用，
不建议其他工具转换使用"——`surge/` 里的东西是给自己试验的，别拿去问可莉。
