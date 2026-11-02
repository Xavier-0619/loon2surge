# 开发与维护

给修改这个仓库的人（以及 AI 助手）看的。转换规则本身见 [RULES.md](RULES.md)，
面向使用者的说明见 [README.md](README.md)。

## 这个仓库做什么

把可莉插件中心（[hub.kelee.one](https://hub.kelee.one/)）的 Loon 插件下载到 `loon/`，
转成 Surge 模块写到 `surge/`，每天自动跑一次。转换器是 Go 写的，不依赖 Python 或 jq。

## 目录

| 路径 | 职责 |
| --- | --- |
| `internal/lpx/` | 转换引擎：`convert.go` 调度，`rule.go`/`rewrite.go`/`script.go` 各管一段，`syntax.go` 是共用的顶层切分与引号处理，`validate.go` 是产物自检 |
| `internal/kelee/` | 同步流水线：抓取（`http.go`/`transport.go`）、落盘（`store.go`）、提交信息（`commit.go`）、文件命名（`name.go`）、远程脚本审计（`audit.go`） |
| `internal/cliargs/` | 让 flag 和位置参数可以混着写 |
| `docs/` | GitHub Pages 模块网站；`data/index.json` 是发布用索引副本，由 `pages.yml` 在部署时更新 |
| `internal/external/` | 并入第三方仓库（ios151/Rewrite）现成的 Surge 模块：抓取、按 Surge 语法整理（tidy）、按 `#!name` 去重、写清单 |
| `ios151 Rewrite/` | 第三方模块快照 + `manifest.json`，由 `loon2surge external` 生成，**不要手改** |
| `loon/`、`surge/`、`index.json`、`INDEX.md` | 生成物，**不要手改** |
| `.github/workflows/` | 每日同步、PR 校验、上游监听、上游 PR 审核、Pages 发布 |
| `tools/` | 上游监听用的脚本（Python，只在 CI 里跑） |

## 五条铁律

1. **不确定就不转。** 新增一种语法前，先去 Surge Manual 和 Loon 文档确认两端语义；
   确认不了就注释保留 + 标 `review`，不要"看起来差不多"地硬转。
2. **注释不能吞内容。** 转不了的行原样留在文件末尾，用户要能看见。
3. **诊断要分级。** 转换器替用户改的（`Adapt`）和要用户自己看的（`Review`）分开记，
   两者都会进提交信息，混在一起就等于没说。
4. **产物要能自检。** 改动 `internal/lpx` 后跑 `go test ./...`，再用
   `loon2surge validate --dir surge` 检查真实产物——转换器的诊断看不见自己的 bug。
5. **不要动 `loon/` 和 `surge/` 里的内容。** 它们是流水线的输出，手改会在下次同步被覆盖，
   而且会让提交信息里的"新增/更新/删除"对不上。

## 改一处转换逻辑的流程

```bash
# 1. 改代码，加一条能复现问题的测试
go test ./internal/lpx -run TestXxx -v

# 2. 全量跑一遍，看诊断分布有没有异常变化
go run . loon -o /tmp/out --report /tmp/report.json

# 3. 产物自检
go run . validate --dir /tmp/out

# 4. 全部测试
go test ./...
```

判断"行为变化是否合理"的方法：对比改动前后的诊断计数。正常的变化是某类 `review`
减少（说明原先不支持的语法现在支持了）；如果 `product-invalid` 或致命诊断突然变多，
大概率是新规则太严或误判，先修再合。

`internal/lpx/testdata/zhihu.sgmodule` 是金标准。改了输出形态要重新生成：

```bash
go run . internal/lpx/testdata/zhihu.lpx --category 可莉插件 -o internal/lpx/testdata/zhihu.sgmodule
```

生成后**先看 diff 再提交**——金标准变了就是全网 270 多个模块都会变。

## 提交信息

`internal/kelee/commit.go` 负责。结构：

```
🔄 自动更新 2026-11-03

273 个插件 · 新增 2 · 更新 15 · 重命名 1 · 下架 0 · 未生成 1 · 需人工确认 14

新增
- 11-02 14:20 · 某某去广告.sgmodule

更新
- 11-02 14:20 · 知乎去广告.sgmodule · 已自动适配 2

⚠️ 需人工确认
- 财新去广告.sgmodule
  - 6 行无法自动转换，原文注释在文件末尾

已自动适配
- 某模块.sgmodule
  - 属性名归一 requires_body → requires-body
```

时间取插件在可莉的更新时间，不是同步时间。明细超过 60 条会折叠。

## 上游监听

`.github/workflows/upstream-watch.yml` 每周检查
[deanxizian/loon_to_surge](https://github.com/deanxizian/loon_to_surge) 的新提交
（跳过 `chore: update Kelee Surge modules`），有新东西就让模型读 diff、往这个仓库里
生成补丁，跑通测试后开 PR。细节见该文件顶部的注释和 `tools/upstream_merge.py`。

它每周最多开一个 PR，而且**不合代码**——合并永远是人来点。审查时重点看：

- 新逻辑有没有更新 `RULES.md` 和测试；
- 有没有悄悄放宽"不确定就不转"的边界；
- 金标准 `zhihu.sgmodule` 的变化是否与说明一致。

### 自动审核

`upstream-review.yml` 认 `upstream-sync` 标签，PR 一开就审（推新提交会重审）：
先跑构建、测试、产物自检，再让模型读 diff 逐条对照 `RULES.md`、`AGENTS.md`。

批准要求三个条件同时成立：

1. 构建、测试、产物自检全过；
2. 机器检查干净——没动 `.github/`、改了 `internal/lpx/` 就一定递增了 `const Version`、没有硬编码密钥；
3. 模型说 approve，且 `concerns` 为空。

机器检查在 `tools/upstream_merge.py` 的 `mechanical_checks()`，**先于模型**跑：
模型可能被说服，代码不会。批准 = draft 转正 + approve，**merge 仍由人点**。

## 踩过的坑

- **Body Rewrite 不是 `http-response-replace-regex`。** Surge 的写法是
  `http-response <url> <正则> <替换>`。写错能导入、但整行不生效，而且很难看出来。
- **Surge 的 Body Rewrite 默认按行锚定 `^`/`$`。** Loon 不是。所以 flags 必须写全
  （无 flags 也要写 `(?-ims)`）。
- **JQ 表达式不能只把换行换成空格。** `#` 注释靠换行结束，压平后会把后面的代码一起吃掉。
- **Loon 参数的两层转义要分清。** `jq("...\"...")` 里的 `\"` 是 Loon 层，要还原；
  `json.delete(["data.a", "data[\"3D\"]"])` 里的 `\"` 是 JSON 层，不能还原。
  还原错了 JSON 就废了。
- **`%APPEND%` 不是占位符。** 自检找 `%Arg%` 时要放过它。
- **`requires_body` 归一不是美化。** 不归一会被当未知属性丢掉，脚本就读不到响应体。
- **可莉按 UA 放行。** 下载必须带 Loon 的 UA，改浏览器 UA 会整个 403。
- **Cloudflare 看 TLS 指纹。** Go 原生栈会拿到 `cf-mitigated=challenge`，所以要能借
  Python 的 OpenSSL 栈（`internal/kelee/pyfetch.py`）。
