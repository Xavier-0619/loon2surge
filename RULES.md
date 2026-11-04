# 转换规则

这份文档记录 `internal/lpx` 实际做的事情，以及"为什么这件事不能做"。
规则以代码为准；写法与含义对不上时，先改代码再改这里。

参考依据（冲突时以 Surge Manual 为准）：

- [Loon 插件](https://nsloon.app/docs/Plugin/)、[Rewrite V2](https://nsloon.app/docs/Rewrite/rewrite_v2/)、[Script V2](https://nsloon.app/docs/Script/script_v2/)
- [Surge Manual](https://manual.nssurge.com/)：模块结构、`#!arguments`、`#!requirement`，以及
  [Body Rewrite](https://manual.nssurge.com/http/body-rewrite.html) /
  [Header Rewrite](https://manual.nssurge.com/http/header-rewrite.html) /
  [Map Local](https://manual.nssurge.com/http/map-local.html) /
  [URL Rewrite](https://manual.nssurge.com/http/url-rewrite.html)
- [deanxizian/loon_to_surge](https://github.com/deanxizian/loon_to_surge)：同类项目的取舍，用来对照
- [Surge Core Version](https://manual.nssurge.com/profile/requirement.html)：`CORE_VERSION` 编码方式

## 总原则

> 宁可少转一行并明确报出来，也不生成能导入但行为不对的模块。

具体表现为三条：

1. **不确定就不转。** 没有查过两端语义的语法，一律注释保留在文件末尾，并在提交信息里标「需人工确认」。
2. **注释不吞内容。** 转不了的行原样留在文件里，用户能看见、能自己补，不会凭空消失。
3. **产物要能被独立检查。** 转换结束后把自己写出的文本当成品重读一遍（见「产物自检」）。

## 输出形态

- 输入：`.lpx` / `.plugin` / `.txt`
- 输出：`.sgmodule`
- 文件名取 `#!name`，过滤 `\ / : * ? " < > |` 与控制字符
- 多个插件写同一个 `#!name` 时合成一个模块（见下节）；`-2`、`-3` 只用于
  `loon/` 里的原始文件，避免上游的两份快照互相覆盖

section 顺序固定（只输出非空的）：

```ini
[General]
[IP Rewrite]
[Rule]
[URL Rewrite]
[Header Rewrite]
[Body Rewrite]
[Map Local]
[Script]
[MITM]
```

`Panel` 不在这个序列里，转换器不生成它；若手工加进来，`validate` 会按 Script 的位置接受。

## 同名插件融合

上游会把同一个 App 的规则拆成几份，但都写同一个 `#!name`。可莉的
「挖财记账去广告」就是两份：`WaCaiJiZhang_remove_ads` 管弹窗和悬浮广告，
`Wacai_remove_ads` 管开屏和活动页，各补一半接口。

按名字各写一个文件，后转的会覆盖前一份；按 `-2` 分开写，用户要装两个才完整，
两个模块里还各有一份重复规则。所以转换前先合成一版：

- section 按行并集，**完全相同的行只留一次**
- 头部取主源（文件名最贴近 `#!name` 的那份，也就是同步时先收录的）；
  `desc` / `author` / `tag` 拼接去重，`date` 取最新，`icon` 跟主源
- 合并后行号是重排的，所以诊断带来源文件名：`[code] 文件名 L12: …`
- 产物级去重（原文不同但转出同一行）只在融合时开；单源不动——那是上游自己
  重复，逐行对应才对得上

索引里主源记 `merged: [...]`，其余标 `status: merged` + `merged_into: 主源`，
不再单独产出文件。`convert` 按 `#!name` 分组，`sync` 按正文里的 `#!name` 分组，
两边都走 `lpx.ConvertSources`，同一个目录跑出来的结果一致。

## 元数据

| Loon | 处理 |
| --- | --- |
| `name`、`desc`、`author`、`icon` | 保留 |
| `category` | 固定写成 `#!category=<--category，默认「插件转换」>` |
| `system` | iOS/iPadOS → `ios`，macOS → `mac`；两端都有则省略；其他 → 丢弃并记 `unsupported-system` |
| `openUrl`、`open`、`tag`、`homepage`、`date` | 保留 |
| 其他 `#!` 字段（`loon_version`、`system_version` 等） | 丢弃，记 `meta-dropped` |

## Argument

- 每行 `=` 左边是参数名，逗号分隔后的第二项是默认值。
- 参数名只保留 ASCII 字母、数字、下划线，其余转 `_`；数字开头加 `ARG_`；归一后重名 → 致命。
- 正文里的 `{Name}` / `${Name}` 统一换成 `{{{Name}}}`。
- **未被任何产出行引用的参数会删除**，记 `argument-unused`。
- 默认值含逗号或（去空白后）以双引号开头 → 用双引号包住并转义；
  出现引号包住的值时 `requirement` 抬到 `6008000`，否则抬到 `5005000`（Mac 5.5.0 引入参数表）。

为什么不对含空格的值加引号：Surge 参数表里引号会进到值本身，`Cron:0 0 * * *` 加上引号后
脚本收到的是 `"0 0 * * *"`，cron 直接失效。

## General

| Loon | Surge | 说明 |
| --- | --- | --- |
| `real-ip` / `always-real-ip` | `always-real-ip = %APPEND% …` | 多次声明合并成一条，按首次出现去重 |
| `include-tun` | `tun-included-routes = %APPEND% …` | 只接受 IPv4 CIDR（裸 IP 补 `/32`），规范化后去重 |
| `ipv6-vif = off` | `ipv6-vif = disabled` | 抬高到 `6010000` |
| `ipv6-vif = auto/always` | 不输出 | 与 Surge 默认一致，记 `general-value` |
| 其他键 | 不输出 | 记 `general-unsupported`，标「需人工确认」 |

选择"不输出 + 提示"而不是"整份失败"：General 项没转只是少个覆盖项，模块本身还能用；
但绝不能因为不认识就静默丢弃，所以它一定出现在报告的「需人工确认」里。

## IP Rewrite

- 只接受 `地址 = reflect|reject|drop`，地址必须是单个 IPv4 字面量。
- CIDR、IPv6、参数绑定、未知动作 → 致命（`ip-rewrite`）。
- 重复地址保留第一条，记 `ip-rewrite-duplicate`。
- 门槛抬到 `6010000`。

## Rule

### 类型与选项

允许的类型：`DOMAIN`、`DOMAIN-SUFFIX`、`DOMAIN-KEYWORD`、`DOMAIN-WILDCARD`、
`IP-CIDR`、`IP-CIDR6`、`GEOIP`、`IP-ASN`（`IPASN` 归一）、`AND`/`OR`/`NOT`、
`DEST-PORT`、`PROTOCOL`、`SRC-PORT`、`URL-REGEX`、`USER-AGENT`、`PROCESS-NAME`。

选项只认 `extended-matching`、`no-resolve`、`pre-matching`，其余 → 致命。
源文件里写了的三个选项不照搬，按类型重建：

- 域名类（含 `URL-REGEX`）→ `extended-matching`
- IP 类 → `no-resolve`
- 拒绝策略（`REJECT` / `REJECT-TINYGIF`）→ 追加 `pre-matching`，门槛 `6008000`

### 策略

Surge 模块的 `[Rule]` 只接受 `DIRECT`、`REJECT`、`REJECT-TINYGIF`。
出现 `PROXY` 等策略时，**这一行注释保留**，整份模块照常产出——少一条规则能手动补，
把整份模块排除掉则等于什么都拿不到。这与上游"整份排除"的取舍相反。

### 裸域名

`example.com` → `DOMAIN,example.com,REJECT,extended-matching,pre-matching`，
门槛抬到 `6008000`（`pre-matching`，与普通规则同一套标准）。

### URL-REGEX 特例

`URL-REGEX,^https://…,REJECT-DICT|REJECT-ARRAY|REJECT-IMG` 转成 `[Map Local]`：

```ini
^https://… data-type=text data="{}" status-code=200 header="Content-Type:application/json"
^https://… data-type=tiny-gif status-code=200
```

### 逻辑规则

递归重建括号结构，内部域名类补 `extended-matching`、IP 类补 `no-resolve`；
顶层只在"拒绝策略且所有子项都支持 `pre-matching`"时才补，逻辑节点本身不加 `extended-matching`。
内部每一项都按自己的类型抬门槛（`URL-REGEX` → `6008000`，`DOMAIN-WILDCARD` / `PROCESS-NAME` →
`6010000`），和写成单条规则时同一套标准——否则模块声明的版本比实际用到的低，
低版本 Surge 上那一行会静默不生效。

### PROCESS-NAME

只在插件 `#!system` 明确含 macOS 时转换，且：

- 按可执行文件名匹配（不含 `/`）→ 拒绝：Loon 忽略大小写，Surge 所有模式都区分。
- 路径含字符集合或反斜杠 → 拒绝：Surge 文档没写清匹配细节。
- `NOT` 的子树含进程条件 → 拒绝：两端对未知进程信息的处理不同。

## Rewrite

### 旧语法

`<pattern> <动作>`，动作分流到 `[URL Rewrite]`、`[Header Rewrite]`、`[Body Rewrite]`、`[Map Local]`。

### V2 语法

`request|response if <条件> then <动作>`，条件目前只支持：

- 单个 `${url} ~= /正则/flags`，flags 只能是空或 `i`（`i` 转成作用域内联 `(?i:…)`，保留捕获组编号）。
- 单个 `${url} == "常量"`，转成完整 URL 正则。
- 尾部 `as name` 捕获：`${name.1}` 在动作里转成 `$1`。

URL 的 `m`/`s` flag 没有确认过等价写法 → 整行注释。

### V2 动作

| 动作 | 输出 | 备注 |
| --- | --- | --- |
| `redirect(302/307, 目标)` | `[URL Rewrite] pattern 目标 状态码` | 只允许 302/307 |
| `url.replace(目标)` | `[URL Rewrite] pattern 目标 header` | `header` = 只改 URL，不动请求体 |
| `reject*` | `[Map Local]` | 状态码 200–999 |
| `*.header.add/set/del/replace` | `[Header Rewrite]` | `set` 生成 `header-del` + `header-add` |
| `*.body.replace(/re/flags, 替换)` | `http-<phase> <url> <正则> <替换>` | flags 全显式，见下 |
| `*.json.jq/delete/add/replace` | `http-<phase>-jq <url> '<表达式>'` | 表达式压成一行 |
| `response.body.mock(正文)` | `[Map Local]` | 含引号走 base64 |
| `response.body.mock_file(url)` | `[Map Local] data-type=file` | 兼容 `("text", url, 200)` 签名 |

**Body Rewrite 的两个坑：**

1. 格式是 `http-request|http-response <url> <正则> <替换>`，**没有** `http-response-replace-regex`
   这种写法。写成后者能导入，但整行不生效。
2. Surge 的 Body Rewrite 默认把 `^`/`$` 当行锚点，Loon 不是。所以 flags 必须写全：
   无 flags → `(?-ims)`，只有 `/m` → `(?m-is)`，`/is` → `(?is-m)`。

Header Rewrite 改 `Content-Length` / `Transfer-Encoding` 会被 Surge 拒绝这次改写，直接不转。

### JSON 路径

- 支持 `a.b`、`data[0].ad`、`data["a.b"]`，允许一个前导点。
- 空路径、尾部点、连续点、未闭合下标、通配符/切片 → 致命，不自动缩短成父路径。

### JQ 表达式

按词法压成一行，`#` 注释整段丢弃（字符串和 `\(…)` 插值里的 `#` 不算注释）。
字符串里的控制字符转成 JSON 转义，单引号转成 `\u0027`（JQ 没有 `\'`，Surge 又用单引号包表达式）。
`$__loc__` 依赖源文件名和行号，内联后必然是错的，直接拒绝。

不做这一步而只是把换行换成空格，会让 `# 注释` 吞掉它后面的所有代码。

## Script

### 触发器

`http-request` / `http-response` / `cron` / `generic` 保留；`request`/`response` 归一为 `http-*`；
`network-changed` 没有对应，注释保留。

### 属性

保留 `script-path`、`requires-body`、`binary-body-mode`、`max-size`、`engine`、`timeout`、
`ability`、`script-update-interval`、`debug`、`wake-system`、`argument`。

- `requires_body` 这类下划线写法归一成 `requires-body`（不归一就会当未知属性丢掉，
  而掉了它脚本读不到响应体——模块能导入，去广告静默失效）。
- 未知属性 → 丢弃并标「需人工确认」。
- 同一属性赋两个不同值 → 整行拒绝，不猜哪个生效。
- `img-url` / `icon` 是 Loon 界面用的显示字段，静默丢弃。

### 默认超时

Loon 旧语法没写 `timeout` 时取 10 秒，Surge 默认 5 秒，所以显式补 `timeout=10`。
`cron` / `generic` 补 300。V2 语法同样按 HTTP 10、Cron/Generic 300 处理。

### cron

静态表达式必须是 5 或 6 段、每段只含数字和 `* , - /`，否则整行拒绝。
含 `{Param}` 的表达式跳过静态检查，交给 Surge 在运行期判断。

### 参数清单

`script("url", {${A}, ${B}})` → `argument="{"A":{{{A}}},"B":{{{B}}}}"`。
未在 `[Argument]` 声明过的名字写 `null`，并标「需人工确认」——Loon 那边它们本来就是 undefined。

### enable 开关

| Loon | Surge |
| --- | --- |
| `enable=false` 等假值 | 行首加 `#` |
| `enable=true` | 原样 |
| `enable={P}`，P 只当开关 | 参数默认值改 `#`，行首加 `{{{P}}}` |
| `enable={P}`，P 同时是入参 | 不用行前缀，按默认值静态注释或保留 |

最后一行为什么不强行加前缀：脚本拿到的参数会变成 `#` 这种开关值，而不是原本的入参。
这种取舍会记 `script-enable-shared`。

### generic

照转，但一定标「需人工确认」：Loon 有被选节点上下文（`$environment.params.node`），Surge 没有。
需要 `$argument.policy` 的脚本得手工补模块参数，转换器不替用户猜脚本内部逻辑。

## MITM

只转 `hostname = …`，并去掉源里的 `%APPEND%` / `%INSERT%`，输出时统一补一次 `%APPEND%`。
其他键 → 不进产物，记 `mitm-unsupported`。

源插件有改写行但没有 `[MITM]` 时，提示手动补——不能替用户推断要解密哪些域名。

## 版本门槛

`CORE_VERSION` 按 Mac 版本编码（`major*1000000 + minor*1000 + patch`），
iOS 版本按同批发布对应到 Mac 版本上，见 [requirement](https://manual.nssurge.com/profile/requirement.html)。
`5005000` = Mac 5.5.0（模块参数表），`5006000` = iOS 5.10.0 / Mac 5.6.0
（Map Local 的 `data-type`/`status-code` 与 Body Rewrite）。

| 触发条件 | requirement |
| --- | --- |
| 普通规则、Script、URL/Header Rewrite 等旧版语法 | `CORE_VERSION>=20` |
| 模块参数表 `#!arguments` | `CORE_VERSION>=5005000` |
| Map Local 的 `data-type`/`status-code`、普通 Body Rewrite | `CORE_VERSION>=5006000` |
| JQ、`pre-matching`、URL-REGEX `extended-matching`、引号包住的参数默认值 | `CORE_VERSION>=6008000` |
| IP Rewrite、`tun-included-routes`、`ipv6-vif`、`DOMAIN-WILDCARD`、`PROCESS-NAME` | `CORE_VERSION>=6010000` |

取所有命中项的最大值。门槛宁可保守：向不支持的版本宣称可用，用户只会看到"模块装上了但不生效"。

## 诊断分级

每条诊断带 `Action`：

- `adapted`：转换器做了等价改写（属性名归一、重名脚本加后缀、enable 转行前缀、
  `merged-source` 同名插件融合…），模块直接能用。
- `review`：需要人看一眼（原文注释在末尾、运行时语义差异、通用项被丢掉…）。

融合后行号是重排的，带行号的诊断会补上来源文件名（`[code] 文件名 L12: …`），
免得照着合并后的行号去翻原始插件。

`Skipped` 标记"这一行没能转换"，在提交信息里压成一句「N 行无法自动转换」，不逐条刷屏。

转换结果分三种：正常产出、`module-excluded`（Surge 表达不了，属正常结论）、真失败。

## 产物自检

`Validate(text)` 把生成的模块当别人写的文件重读，检查：

- 头部有 `#!name`，`#!arguments` 名字唯一，`#!requirement` 格式正确且不低于内容
  实际用到的语法（低了就是"低版本上这些行静默不生效"，记 `requirement-low`）
- section 都属于已知集合、不重复、顺序符合上面的固定序列
- 没有 `enable=` / `enabled?=` / `%Arg%` / 裸 `{Name}` 残留（`%APPEND%` 除外）
- `[Rule]` 至少三个字段，策略只允许 `DIRECT` / `REJECT` / `REJECT-TINYGIF`
- `[Map Local]` 的 `status-code` 在 200–999，`data-type` 属于四种
- `[Body Rewrite]` 行首是 `http-request` / `http-response`（含 `-jq`）
- `[Script]` 行有合法的 `type=`，`[Header Rewrite]` 有方向前缀，`[MITM]` 只有 `hostname`

转换结束时会自动跑一次，有问题记为 `product-invalid`，该模块不落盘。
`loon2surge validate` 用同一套规则检查仓库里已有的文件。

## 已知边界

- `jq_file` / `jq-path` 的远程 JQ 不抓取内联（转换器不联网）。
- V2 的 `m` / `s` URL flag、复合条件、`reject_video`、`network-changed` 不转换。
- Object 参数直接序列化成 JSON 字符串，前提是脚本会用 `JSON.parse($argument)`；
  不会解析的脚本需要人工核对。
- 静态检查通过 ≠ 真机可用：正则引擎、MITM、脚本行为仍需在 Surge 里实测。
