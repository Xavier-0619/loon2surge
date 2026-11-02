"""借 Python 的 HTTP/TLS 栈下载单个插件文件。

为什么要有这个文件
------------------
kelee.one 挂在 Cloudflare 后面。它判定"是不是机器人"主要看三样：出口 IP、请求头、
以及 **TLS 握手指纹**（JA3/JA4）。前两样已经对齐了，剩下最后一样没法在 Go 里伪装：
Go 的 crypto/tls 发出的 ClientHello 是公开且知名的机器人指纹，Cloudflare 直接回
`403 + cf-mitigated=challenge`（托管质询），连 JS 都不用跑就拒了。

上游同款项目用的是 python urllib，在同样的 GitHub runner 上能稳定通过——因为它的
TLS 栈是 OpenSSL，指纹不一样。所以这里只借它做一件事：把字节拿回来。
插件解析、规则转换、落盘全部仍在 Go 里，这个文件不参与任何业务逻辑。

用法：python3 pyfetch.py <url> <输出文件>
  标准输出打印 HTTP 状态码（403 也算"拿到了答复"，由 Go 侧分类）
  网络层失败才以非零退出码结束，让 Go 侧按可重试处理
"""

import sys
import urllib.error
import urllib.request

# 与 Go 侧 UserAgent / AcceptLanguage 保持完全一致
USER_AGENT = "Loon/860 CFNetwork/3826.500.111.2.2 Darwin/24.4.0"
ACCEPT_LANGUAGE = "zh-CN,zh-Hans;q=0.9"
TIMEOUT = 60


def main() -> int:
    if len(sys.argv) < 3:
        print("usage: pyfetch.py <url> <out>", file=sys.stderr)
        return 2

    url, out = sys.argv[1], sys.argv[2]
    request = urllib.request.Request(
        url,
        headers={
            "User-Agent": USER_AGENT,
            "Accept": "*/*",
            "Accept-Language": ACCEPT_LANGUAGE,
        },
    )

    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT) as response:
            with open(out, "wb") as handle:
                handle.write(response.read())
            print(response.status)
            return 0
    except urllib.error.HTTPError as error:
        # 403/404 也是有效答复，交给 Go 分类，不当成网络故障。
        # cf-ray / cf-mitigated 打到 stderr，Go 侧据此区分"托管质询"和"IP 封锁"。
        hint = "，".join(
            f"{name}={error.headers.get(name)}"
            for name in ("cf-ray", "cf-mitigated")
            if error.headers.get(name)
        )
        if hint:
            print(hint, file=sys.stderr)
        print(error.code)
        return 0
    except Exception as error:  # noqa: BLE001 - 超时、DNS、TLS 失败都算可重试
        print(error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
