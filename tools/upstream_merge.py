#!/usr/bin/env python3
"""把上游 loon_to_surge 的新提交翻译成本仓库（Go 重写版）的改动。

为什么不是"照抄补丁"：两边实现语言和结构都不一样（上游是一个 3000 行的 Python
脚本，这里是分包的 Go）。能搬的是**语义**——它新支持的语法、它修掉的边界 bug、
它写进 RULES.md 的判断依据。所以流程是：

  1. collect  拉上游，挑出上次之后的新提交（跳过纯同步产物的提交）
  2. prompt   把提交说明 + 相关文件的 diff 拼成一份给模型看的材料
  3. apply    模型返回 JSON（按 find/replace 改文件），这里落盘
  4. advance  记下处理到哪个提交，下次从这里继续

设计取向：
  * 只允许模型改源码和文档，不能碰 loon/、surge/、.github/workflows/ 以外的东西；
  * 每处替换必须唯一命中，命中不了就整体失败，绝不猜；
  * 模型不可用时不算失败——退化成"开个 issue 提醒"，让人自己看。

用法：
  python3 tools/upstream_merge.py collect
  python3 tools/upstream_merge.py prompt
  python3 tools/upstream_merge.py apply
  python3 tools/upstream_merge.py advance --sha <sha>
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import textwrap
import urllib.error
import urllib.request
from pathlib import Path

UPSTREAM_URL = "https://github.com/deanxizian/loon_to_surge.git"
STATE_FILE = ".upstream-sync.json"
WORK_DIR = ".upstream-work"
UPSTREAM_DIR = Path(WORK_DIR) / "upstream"

# 这些提交只是把同步结果写进仓库，对我们的实现没有任何信息量。
SKIP_SUBJECTS = (
    "chore: update kelee surge modules",
    "chore: update kelee loon plugins",
)

# 值得看的路径：转换逻辑、规则文档、回归测试。
INTERESTING_PREFIXES = ("scripts/", "tests/")
INTERESTING_FILES = ("RULES.md", "AGENTS.md", "README.md")

# 允许模型改动的路径。产物目录和 CI 配置一律不给改。
EDITABLE_PREFIXES = ("internal/", "tools/", "tests/")
EDITABLE_FILES = ("main.go", "README.md", "RULES.md", "AGENTS.md", "go.mod")

MAX_DIFF_PER_FILE = 3500
MAX_PROMPT_CHARS = 55000
MAX_COMMITS = 12
MAX_REVIEW_DIFF = 40000


# ---------------------------------------------------------------------------
# 小工具
# ---------------------------------------------------------------------------


def run(cmd: list[str], cwd: Path | None = None, check: bool = True) -> str:
    result = subprocess.run(
        cmd, cwd=cwd, capture_output=True, text=True, encoding="utf-8", errors="replace"
    )
    if check and result.returncode != 0:
        raise RuntimeError(f"命令失败：{' '.join(cmd)}\n{result.stderr[:2000]}")
    return result.stdout


def load_state() -> dict:
    path = Path(STATE_FILE)
    if not path.exists():
        return {"last_sha": "", "history": []}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {"last_sha": "", "history": []}


def save_state(state: dict) -> None:
    Path(STATE_FILE).write_text(
        json.dumps(state, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )


def work_file(name: str) -> Path:
    Path(WORK_DIR).mkdir(parents=True, exist_ok=True)
    return Path(WORK_DIR) / name


def is_skipped(subject: str) -> bool:
    lowered = subject.strip().lower()
    return any(lowered.startswith(skip) for skip in SKIP_SUBJECTS)


def is_interesting(path: str) -> bool:
    if path.startswith(INTERESTING_PREFIXES):
        return True
    return path in INTERESTING_FILES


def is_editable(path: str) -> bool:
    if path.startswith(EDITABLE_PREFIXES):
        return True
    return path in EDITABLE_FILES


# ---------------------------------------------------------------------------
# collect
# ---------------------------------------------------------------------------


def ensure_upstream() -> Path:
    """浅克隆上游；已有就 fetch。只需要最近的历史，不下载产物。"""
    if (UPSTREAM_DIR / ".git").exists():
        run(["git", "fetch", "--quiet", "--depth", "80", "origin", "HEAD"], cwd=UPSTREAM_DIR)
        run(["git", "reset", "--quiet", "--hard", "FETCH_HEAD"], cwd=UPSTREAM_DIR)
    else:
        UPSTREAM_DIR.parent.mkdir(parents=True, exist_ok=True)
        if UPSTREAM_DIR.exists():
            shutil.rmtree(UPSTREAM_DIR)
        run(
            [
                "git", "clone", "--quiet", "--depth", "80", "--single-branch",
                "--filter=blob:none", "--no-checkout", UPSTREAM_URL, str(UPSTREAM_DIR),
            ]
        )
        run(["git", "checkout", "--quiet", "HEAD"], cwd=UPSTREAM_DIR)
    return UPSTREAM_DIR


def commit_files(upstream: Path, sha: str) -> list[str]:
    output = run(
        ["git", "show", "--name-only", "--format=", sha], cwd=upstream, check=False
    )
    return [line.strip() for line in output.splitlines() if line.strip()]


def commit_diff(upstream: Path, sha: str) -> str:
    """只取值得看的文件的 diff，产物和图片不参与。"""
    paths = [path for path in commit_files(upstream, sha) if is_interesting(path)]
    if not paths:
        return ""
    chunks = []
    for path in paths[:12]:
        diff = run(
            ["git", "show", "--format=", "--unified=3", sha, "--", path],
            cwd=upstream,
            check=False,
        )
        if not diff.strip():
            continue
        if len(diff) > MAX_DIFF_PER_FILE:
            diff = diff[:MAX_DIFF_PER_FILE] + f"\n…（{path} 的 diff 已截断）\n"
        chunks.append(diff)
    return "".join(chunks)


def collect(args: argparse.Namespace) -> int:
    upstream = ensure_upstream()
    state = load_state()
    last_sha = state.get("last_sha", "")

    fmt = "%H%x1f%s%x1f%b%x1e"
    rev = f"{last_sha}..HEAD" if last_sha else "HEAD~20..HEAD"
    raw = run(["git", "log", f"--format={fmt}", rev], cwd=upstream, check=False)
    if not raw.strip():
        summary = {"has_new": False, "count": 0, "commits": []}
        work_file("commits.json").write_text(
            json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8"
        )
        print("上游没有新提交")
        return 0

    commits = []
    for record in raw.split("\x1e"):
        record = record.strip("\n")
        if not record:
            continue
        sha, subject, body = (record.split("\x1f") + ["", ""])[:3]
        if is_skipped(subject):
            continue
        files = [path for path in commit_files(upstream, sha) if is_interesting(path)]
        commits.append(
            {
                "sha": sha,
                "subject": subject.strip(),
                "body": body.strip()[:1500],
                "files": files,
                "diff": commit_diff(upstream, sha) if files else "",
            }
        )

    # 从旧到新：模型按时间顺序理解"先加了什么、后来又修了什么"。
    commits.reverse()
    total = len(commits)
    commits = commits[:MAX_COMMITS]

    head_sha = run(["git", "rev-parse", "HEAD"], cwd=upstream).strip()
    summary = {
        "has_new": total > 0,
        "count": total,
        "used": len(commits),
        "head_sha": head_sha,
        "commits": [{"sha": c["sha"], "subject": c["subject"]} for c in commits],
        "detail": commits,
    }
    work_file("commits.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8"
    )

    if total == 0:
        print("上游只有同步产物的提交，跳过")
    else:
        print(f"发现 {total} 个值得看的提交（本轮处理 {len(commits)} 个）")
        for item in commits:
            print(f"  - {item['sha'][:8]} {item['subject']}")

    github_output("has_new", "true" if total > 0 else "false")
    github_output("count", str(total))
    github_output("head_sha", head_sha)
    return 0


# ---------------------------------------------------------------------------
# prompt
# ---------------------------------------------------------------------------


def read_repo_doc(path: str) -> str:
    file = Path(path)
    if not file.exists():
        return f"（没有 {path}）"
    return file.read_text(encoding="utf-8")


def source_tree() -> str:
    """给模型一份当前实现的文件清单，避免它凭空猜路径。"""
    lines = []
    for path in sorted(Path("internal").rglob("*.go")):
        lines.append(f"- {path.as_posix()} ({len(path.read_text(encoding='utf-8').splitlines())} 行)")
    for name in ("main.go",):
        if Path(name).exists():
            lines.append(f"- {name} ({len(Path(name).read_text(encoding='utf-8').splitlines())} 行)")
    return "\n".join(lines)


PROMPT_HEAD = """你是一个 Go 项目的维护者。这个项目把 Loon 插件转换成 Surge 模块，
它的设计参考了另一个同名的 Python 项目（我称它为"上游"）。现在上游有了一些新提交，
请你判断其中哪些内容值得移植过来，并直接给出改动。

## 铁律（违反任何一条都会被拒绝）

1. **不确定就不转。** 上游新支持的语法如果语义没写清楚，宁可继续"注释保留 + 标需人工确认"，
   也不要猜。
2. **只改源码和文档。** 允许改：<<EDITABLE>>。
   绝对不要碰 loon/、surge/、index.json、INDEX.md、.github/ —— 那些是流水线产物和 CI 配置。
3. **改动的每一处都必须能定位。** 用 find/replace 的形式给出，find 必须在该文件里
   唯一出现；做不到就说明原因、不要给这一条。
4. **改了转换结果就递增版本号。** internal/lpx/convert.go 里的 `const Version` 控制
   同步流程是否重转产物。输出会变化时必须递增（例如 2.1.0 → 2.2.0），否则老模块不会更新。
5. **补测试。** 每处语义改动都应配一条 *_test.go 里的用例。

## 当前实现

```
<<TREE>>
```

## 项目约定（AGENTS.md）

<<DOC>>

## 已知的实现细节（RULES.md，节选）

<<RULES>>

## 上游的新提交

<<COMMITS>>

## 你要输出什么

一段 JSON（不要用 Markdown 代码块包裹），结构：

```json
{
  "summary": "一句话说明这批提交里有什么值得移植",
  "decisions": [
    {"sha": "提交前 8 位", "verdict": "adopt|skip|review", "why": "为什么"}
  ],
  "files": [
    {"path": "internal/lpx/rewrite.go",
     "edits": [{"find": "唯一匹配的原文片段", "replace": "替换后的内容"}]}
  ],
  "notes": "需要人工确认的地方，写清楚是哪一条、为什么"
}
```

要求：
- decisions 要覆盖上面每一个提交，`skip` 也要写原因（例如"只是上游产物同步""与 Go 实现无关"）。
- edits 里的 find 片段要足够长（包含上下文），保证唯一；不要包含行号。
- 如果这批提交没有值得移植的内容，files 返回空数组，summary 说明原因。
- 不要输出解释性文字，只输出 JSON。
"""


def build_prompt(args: argparse.Namespace) -> int:
    data = json.loads(work_file("commits.json").read_text(encoding="utf-8"))
    commits = data.get("detail", [])
    if not commits:
        print("没有提交可写进 prompt")
        return 0

    blocks = []
    for item in commits:
        block = [f"### {item['sha'][:8]} {item['subject']}"]
        if item["body"]:
            block.append(textwrap.indent(item["body"], "> "))
        if item["files"]:
            block.append("涉及文件：" + "、".join(item["files"]))
        if item["diff"]:
            block.append("```diff\n" + item["diff"] + "```")
        else:
            block.append("（这个提交没有改动转换脚本或文档）")
        blocks.append("\n".join(block))

    prompt = (
        PROMPT_HEAD.replace("<<EDITABLE>>", "、".join(EDITABLE_PREFIXES + EDITABLE_FILES))
        .replace("<<TREE>>", source_tree())
        .replace("<<DOC>>", read_repo_doc("AGENTS.md")[:9000])
        .replace("<<RULES>>", read_repo_doc("RULES.md")[:18000])
        .replace("<<COMMITS>>", "\n\n".join(blocks))
    )
    if len(prompt) > MAX_PROMPT_CHARS:
        prompt = prompt[:MAX_PROMPT_CHARS] + "\n\n（材料已截断，请只基于能看见的内容判断）\n"

    work_file("prompt.txt").write_text(prompt, encoding="utf-8")
    print(f"prompt 已生成：{len(prompt)} 字符")
    return 0


# ---------------------------------------------------------------------------
# apply
# ---------------------------------------------------------------------------


# 没配 AI_API_KEY 时的后备方案：GitHub 自己托管的模型网关，用仓库自带的 token 就能调
# （workflow 里要开 permissions: models: read）。成本为 0，缺点是模型视野小一点。
GITHUB_MODELS_BASE = "https://models.github.ai/inference"
GITHUB_MODELS_MODEL = "openai/gpt-4o-mini"


def llm_endpoint() -> tuple[str, str, str]:
    """决定用哪个模型。自己的 key 优先，其次仓库自带的 GITHUB_TOKEN。"""
    base_url = os.environ.get("AI_BASE_URL", "").strip()
    model = os.environ.get("AI_MODEL", "").strip()

    api_key = os.environ.get("AI_API_KEY", "").strip()
    if api_key:
        return base_url or "https://api.deepseek.com/v1", api_key, model or "deepseek-chat"

    token = os.environ.get("GITHUB_TOKEN", "").strip()
    if token:
        return base_url or GITHUB_MODELS_BASE, token, model or GITHUB_MODELS_MODEL

    raise RuntimeError(
        "既没有 AI_API_KEY，也没有 GITHUB_TOKEN，跳过自动融合"
        "（多半是 workflow 少了 permissions: models: read）"
    )


def call_llm(prompt: str) -> str:
    base_url, api_key, model = llm_endpoint()
    # 有的网关按这个头做鉴权，少一个就被 401
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0.2,
        "max_tokens": 8000,
    }
    request = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions",
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=300) as response:
            body = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", "replace")[:1000]
        raise RuntimeError(f"模型接口返回 {error.code}：{detail}") from error
    except urllib.error.URLError as error:
        raise RuntimeError(f"连不上模型接口：{error.reason}") from error

    choices = body.get("choices") or []
    if not choices:
        raise RuntimeError(f"模型没有返回内容：{json.dumps(body, ensure_ascii=False)[:500]}")
    return choices[0]["message"]["content"]


def strip_fence(text: str) -> str:
    text = text.strip()
    if text.startswith("```"):
        text = re.sub(r"^```[a-zA-Z]*\n", "", text)
        text = re.sub(r"\n```$", "", text)
    return text.strip()


def apply_payload(payload: dict) -> list[str]:
    changed: list[str] = []
    for item in payload.get("files", []):
        path = item.get("path", "")
        if not is_editable(path):
            raise RuntimeError(f"模型想改一个不允许改的文件：{path}")
        target = Path(path)
        if "content" in item:  # 新建文件
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(item["content"], encoding="utf-8")
            changed.append(f"新建 {path}")
            continue
        if not target.exists():
            raise RuntimeError(f"要改的文件不存在：{path}")
        text = target.read_text(encoding="utf-8")
        for edit in item.get("edits", []):
            find = edit.get("find", "")
            replace = edit.get("replace", "")
            if not find:
                raise RuntimeError(f"{path} 的某一处修改缺少 find")
            count = text.count(find)
            if count != 1:
                raise RuntimeError(
                    f"{path} 里 find 片段命中 {count} 次（必须唯一）：{find[:120]!r}"
                )
            text = text.replace(find, replace, 1)
        target.write_text(text, encoding="utf-8")
        changed.append(f"修改 {path}")
    return changed


def apply_changes(args: argparse.Namespace) -> int:
    prompt = work_file("prompt.txt").read_text(encoding="utf-8")
    try:
        raw = call_llm(prompt)
    except RuntimeError as error:
        print(f"! {error}", file=sys.stderr)
        work_file("result.json").write_text(
            json.dumps({"ok": False, "error": str(error)}, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        return 2

    work_file("response.txt").write_text(raw, encoding="utf-8")
    try:
        payload = json.loads(strip_fence(raw))
    except json.JSONDecodeError as error:
        print(f"! 模型返回的不是 JSON：{error}", file=sys.stderr)
        return 3

    try:
        changed = apply_payload(payload)
    except RuntimeError as error:
        print(f"! {error}", file=sys.stderr)
        work_file("result.json").write_text(
            json.dumps({"ok": False, "error": str(error), "payload": payload},
                       ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        return 4

    result = {
        "ok": bool(changed),
        "summary": payload.get("summary", ""),
        "notes": payload.get("notes", ""),
        "decisions": payload.get("decisions", []),
        "changed": changed,
    }
    work_file("result.json").write_text(
        json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8"
    )

    print(f"应用了 {len(changed)} 处改动")
    for line in changed:
        print(f"  - {line}")
    if not changed:
        print("模型认为这批提交不需要移植")
    return 0


# ---------------------------------------------------------------------------
# advance
# ---------------------------------------------------------------------------


def advance(args: argparse.Namespace) -> int:
    state = load_state()
    state["last_sha"] = args.sha
    state.setdefault("history", []).append(
        {"sha": args.sha, "at": args.at, "result": args.result}
    )
    state["history"] = state["history"][-40:]
    save_state(state)
    print(f"已记录处理到 {args.sha[:8]}（{args.result}）")
    return 0


# ---------------------------------------------------------------------------
# review
# ---------------------------------------------------------------------------

# 这几条是「机器先看一遍」的硬性检查，不依赖模型判断。
# 模型可能被说服，代码不会——放在模型前面当护栏。
REVIEW_CHECKS = (
    "有没有放宽「拿不准就不转」的边界（AGENTS.md 的五条铁律）",
    "改了转换结果，但 internal/lpx/convert.go 里的 const Version 没有递增",
    "改了行为却没有配套测试",
    "引入网络请求、新的外部依赖或硬编码密钥",
    "动了 .github/ 下的文件（流水线不该由自动融合修改）",
)

REVIEW_PROMPT = """你在审核一个自动生成的 PR。

这个仓库把 Loon 插件（.lpx）转换成 Surge 模块（.sgmodule）。这个 PR 是把上游仓库
github.com/deanxizian/loon_to_surge 的改动融合进来之后生成的，现在需要你判断能不能合。

## 仓库规矩（RULES.md）
<<RULES>>

## 协作约定（AGENTS.md）
<<AGENTS>>

## 这次改了哪些文件
<<FILES>>

## 完整 diff
<<DIFF>>

## 逐条判断这些
<<CHECKS>>

要求：
- 只针对 diff 里能看见的东西下结论，别猜。
- 拿不准就判 request_changes，并说明缺什么信息。
- 注释、文档、测试的同步更新算合格；光改文档不改代码也合格。
- 只是把上游写得更好的地方抄过来、行为不变，算合格。

只输出 JSON，不要任何解释文字：
{"verdict": "approve 或 request_changes",
 "summary": "一句话结论",
 "checks": [{"item": "检查项原文", "pass": true, "note": "依据，引用具体文件或行"}],
 "concerns": ["必须改的问题；不通过时这里不能为空，通过时给空数组"]}
"""

SECRET_PATTERNS = (
    re.compile(r"sk-[A-Za-z0-9]{20,}"),
    re.compile(r"ghp_[A-Za-z0-9]{20,}"),
    re.compile(r"github_pat_[A-Za-z0-9_]{20,}"),
)


def pr_diff(base: str, head: str) -> tuple[str, list[str]]:
    names = run(["git", "diff", "--name-only", f"{base}...{head}"]).splitlines()
    diff = run(["git", "diff", "--find-renames", f"{base}...{head}"])
    return diff, [name.strip() for name in names if name.strip()]


def added_lines(diff: str) -> list[str]:
    return [
        line[1:]
        for line in diff.splitlines()
        if line.startswith("+") and not line.startswith("+++")
    ]


def mechanical_checks(files: list[str], diff: str) -> list[dict]:
    problems: list[dict] = []

    touched_ci = [name for name in files if name.startswith(".github/")]
    if touched_ci:
        problems.append({
            "item": "动了 .github/",
            "note": "流水线文件不该由自动融合改：" + "、".join(touched_ci[:5]),
        })

    touched_conv = [name for name in files if name.startswith("internal/lpx/")]
    if touched_conv and not any("const Version" in line for line in added_lines(diff)):
        problems.append({
            "item": "改了转换逻辑没动 const Version",
            "note": "产物不会重新生成，这份改动等于白发",
        })

    for pattern in SECRET_PATTERNS:
        if pattern.search(diff):
            problems.append({
                "item": "疑似硬编码密钥",
                "note": "diff 里出现了像密钥的字符串，需要人工确认",
            })
            break

    return problems


def review_markdown(result: dict) -> str:
    approve = result["approve"]
    lines = [
        f"## {'✅ AI 审核通过' if approve else '❌ AI 审核未通过'}",
        "",
        (result.get("summary") or "（模型没有给出结论）").strip(),
        "",
    ]

    checks = result.get("checks") or []
    if checks:
        lines += ["### 逐条判断", "", "| 检查项 | 结果 | 依据 |", "| --- | --- | --- |"]
        for item in checks:
            mark = "✅" if item.get("pass") else "❌"
            title = str(item.get("item", "")).replace("|", "/").replace("\n", " ")
            note = str(item.get("note", "")).replace("|", "/").replace("\n", " ")
            lines.append(f"| {title} | {mark} | {note[:220]} |")
        lines.append("")

    concerns = [str(c).strip() for c in (result.get("concerns") or []) if str(c).strip()]
    if concerns:
        lines += ["### 要处理的问题", ""] + [f"- {c}" for c in concerns] + [""]

    mechanical = result.get("mechanical") or []
    if mechanical:
        lines += ["### 机器检查没过", ""] + [f"- ❌ {m['note']}" for m in mechanical] + [""]

    files = result.get("files") or []
    if files:
        lines += ["### 改了哪些文件", "", "```"] + files[:40] + ["```", ""]

    lines += [
        "---",
        "",
        "批准不等于合并，merge 永远由人点。" if approve
        else "改好之后推一次，会自动再审。",
    ]
    return "\n".join(lines) + "\n"


def review(args: argparse.Namespace) -> int:
    try:
        diff, files = pr_diff(args.base, args.head)
    except RuntimeError as error:
        print(f"! 拿不到 diff：{error}", file=sys.stderr)
        return 2

    if not diff.strip():
        print("! 这个 PR 没有改动任何文件", file=sys.stderr)
        return 2

    problems = mechanical_checks(files, diff)

    prompt = REVIEW_PROMPT
    prompt = prompt.replace("<<RULES>>", read_repo_doc("RULES.md"))
    prompt = prompt.replace("<<AGENTS>>", read_repo_doc("AGENTS.md"))
    prompt = prompt.replace("<<FILES>>", "\n".join(f"- {name}" for name in files))
    prompt = prompt.replace(
        "<<CHECKS>>",
        "\n".join(f"{index}. {item}" for index, item in enumerate(REVIEW_CHECKS, 1)),
    )

    body = diff
    if len(body) > MAX_REVIEW_DIFF:
        body = body[:MAX_REVIEW_DIFF] + "\n\n（diff 太长，后面截掉了，看到这里为止）"
    prompt = prompt.replace("<<DIFF>>", body)
    if len(prompt) > MAX_PROMPT_CHARS:
        prompt = prompt[:MAX_PROMPT_CHARS] + "\n\n（材料已截断）\n"

    try:
        raw = call_llm(prompt)
    except RuntimeError as error:
        print(f"! {error}", file=sys.stderr)
        return 2

    work_file("review-response.txt").write_text(raw, encoding="utf-8")
    try:
        payload = json.loads(strip_fence(raw))
    except json.JSONDecodeError as error:
        print(f"! 模型返回的不是 JSON：{error}", file=sys.stderr)
        return 3

    verdict = str(payload.get("verdict", "")).strip().lower()
    concerns = [str(c).strip() for c in (payload.get("concerns") or []) if str(c).strip()]

    # 三个条件全满足才批准：代码检查干净、模型说 approve、模型没留任何问题
    approve = verdict == "approve" and not concerns and not problems

    result = {
        "approve": approve,
        "verdict": verdict,
        "summary": str(payload.get("summary", "")).strip(),
        "checks": payload.get("checks") or [],
        "concerns": concerns,
        "mechanical": problems,
        "files": files,
    }
    work_file("review.json").write_text(
        json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    work_file("review.md").write_text(review_markdown(result), encoding="utf-8")

    print(f"模型结论：{verdict or '（空）'}，机器检查 {len(problems)} 项不过")
    print("最终：" + ("批准" if approve else "不批准"))
    return 0 if approve else 1


# ---------------------------------------------------------------------------
# report
# ---------------------------------------------------------------------------

VERDICT_MARK = {"adopt": "✅ 移植", "skip": "⏭️ 跳过", "review": "👀 待确认"}


def load_json(path: Path, default: dict) -> dict:
    if not path.exists():
        return default
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return default


def commit_list() -> list[str]:
    data = load_json(work_file("commits.json"), {})
    return [f"- `{item['sha'][:8]}` {item['subject']}" for item in data.get("commits", [])]


def build_report(args: argparse.Namespace) -> int:
    result = load_json(work_file("result.json"), {})
    lines: list[str] = []

    if result.get("summary"):
        lines += ["### 🧠 模型的判断", "", result["summary"], ""]

    for item in result.get("decisions", []):
        mark = VERDICT_MARK.get(item.get("verdict", ""), item.get("verdict", "?"))
        lines.append(f"- `{item.get('sha', '')}` {mark}：{item.get('why', '')}")

    if result.get("changed"):
        lines += ["", "**改动文件**", ""] + [f"- {name}" for name in result["changed"]]

    if result.get("notes"):
        lines += ["", "**需要人工确认**", "", result["notes"]]

    if result.get("error"):
        lines += ["", "**这次没有生成改动**", "", "```", result["error"], "```"]

    body: list[str] = []
    if args.kind == "pr":
        body += [
            "这个 PR 由每周的上游监听自动生成，**不会自动合并**。",
            "",
        ]
        body += lines or ["模型没有给出结论。"]
        body += ["", "### 📌 上游这批提交", ""] + (commit_list() or ["（没有记录）"])
        body += [
            "",
            f"### 🧪 测试：{args.status or '未运行'}",
            "",
            "### ✅ 审核时看什么",
            "",
            "1. 有没有悄悄放宽「不确定就不转」的边界（见 `AGENTS.md` 的五条铁律）",
            "2. 有没有同步更新 `RULES.md` 和对应的测试",
            "3. 转换结果变了的话，`internal/lpx/convert.go` 的 `const Version` 有没有递增",
            "4. 金标准 `internal/lpx/testdata/zhihu.sgmodule` 的变化是否与说明一致",
            "",
            "---",
            "合进来之后，下一次同步就会用新规则重新生成全部模块。",
        ]
    else:
        body += [
            "这次**没有产生代码改动**。上游的新提交还没有被处理：看完了请手动运行"
            "「上游更新监听」或直接改 `.upstream-sync.json`。",
            "",
        ]
        body += lines
        body += ["", "### 📌 上游这批提交", ""] + (commit_list() or ["（没有记录）"])
        body += [
            "",
            "### 🔧 换更强的模型（可选）",
            "",
            "默认用仓库自带的 `GITHUB_TOKEN` 调 GitHub Models，不需要配任何密钥；",
            "想换更强的模型，就在 Settings → Secrets and variables → Actions 里加"
            " `AI_API_KEY`（兼容 OpenAI 接口即可），可选 `AI_BASE_URL` / `AI_MODEL`，",
            "配了之后优先用自己的。",
            "",
            f"上游 HEAD：`{args.sha or '未知'}`",
        ]

    text = "\n".join(body) + "\n"
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
    else:
        print(text)
    return 0


def github_output(key: str, value: str) -> None:
    path = os.environ.get("GITHUB_OUTPUT")
    if not path:
        return
    with open(path, "a", encoding="utf-8") as handle:
        handle.write(f"{key}={value}\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("collect", help="拉上游并挑出新提交")
    sub.add_parser("prompt", help="生成给模型的材料")
    sub.add_parser("apply", help="调用模型并应用改动")
    adv = sub.add_parser("advance", help="记录处理进度")
    adv.add_argument("--sha", required=True)
    adv.add_argument("--at", default="")
    adv.add_argument("--result", default="ok", choices=["ok", "no-change", "failed", "skipped"])
    rev = sub.add_parser("review", help="审核当前 PR 的改动")
    rev.add_argument("--base", default="origin/main")
    rev.add_argument("--head", default="HEAD")
    rep = sub.add_parser("report", help="生成 PR 或 issue 的正文")
    rep.add_argument("--kind", default="pr", choices=["pr", "issue"])
    rep.add_argument("--status", default="")
    rep.add_argument("--sha", default="")
    rep.add_argument("--out", default="")

    args = parser.parse_args()
    handlers = {
        "collect": collect,
        "prompt": build_prompt,
        "apply": apply_changes,
        "advance": advance,
        "review": review,
        "report": build_report,
    }
    return handlers[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
