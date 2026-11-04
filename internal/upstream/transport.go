package upstream

import (
	"crypto/tls"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 下载通道。为什么做成可替换的接口：
//
// Cloudflare 判定机器人看三样——出口 IP、请求头、**TLS 握手指纹**。前两样好办，
// 第三样在 Go 里改不了：crypto/tls 的 ClientHello 是公开指纹，直接被判成自动化流量，
// 回 403 + cf-mitigated=challenge。上游同款项目用 python urllib 却能在同一个 runner 上
// 稳定通过，差别只在 TLS 栈（OpenSSL）。
//
// 所以下载这一步允许借外部进程（python3）拿字节，Go 只管分类、重试、转换。
// 找不到 python3 时自动退回纯 Go，功能不缺，只是在 Cloudflare 面前更容易被质询。

// pyfetch 是嵌入的下载脚本，见文件内注释。
//
//go:embed pyfetch.py
var pyfetch string

// reply 是一次请求的结果。status 为 0 表示没拿到 HTTP 答复（网络层失败）。
type reply struct {
	body   []byte
	status int
	hint   string // 失败时说明是哪一层拦的，例如 cf-ray / cf-mitigated
}

type sender interface {
	// name 会打印到日志与 Job Summary，出问题先看它选的是哪条通道。
	name() string
	get(raw string) (reply, error)
}

// pickSender 按模式挑一条下载通道。
//
//	auto  有 python3 就用它（与上游同款 TLS 栈），否则退回 Go
//	python 强制用 python3，找不到就报错
//	go    强制用 Go 原生客户端
func pickSender(mode string) (sender, error) {
	switch mode {
	case "go":
		return newGoSender(), nil
	case "python":
		return newPySender()
	case "", "auto":
		if found, err := newPySender(); err == nil {
			return found, nil
		}
		return newGoSender(), nil
	default:
		return nil, fmt.Errorf("未知的下载通道 %q，可选 auto / python / go", mode)
	}
}

// ---------------------------------------------------------------------------
// Go 原生通道（兜底）
// ---------------------------------------------------------------------------

type goSender struct{ client *http.Client }

func newGoSender() *goSender {
	// 只走 HTTP/1.1、不发 Accept-Encoding：Go 默认的 h2 + gzip 组合更容易被判成爬虫。
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 20 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:  true,
		MaxIdleConnsPerHost: 4,
		MaxConnsPerHost:     4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &goSender{client: &http.Client{Timeout: requestTimeout, Transport: transport}}
}

func (g *goSender) name() string { return "Go 原生（TLS 指纹可能被 Cloudflare 质询）" }

func (g *goSender) get(raw string) (reply, error) {
	request, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return reply{}, err
	}
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", AcceptLanguage)

	response, err := g.client.Do(request)
	if err != nil {
		return reply{}, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{body: body, status: response.StatusCode, hint: headerHint(response.Header)}, nil
}

// ---------------------------------------------------------------------------
// Python 通道（默认）
// ---------------------------------------------------------------------------

type pySender struct {
	python string
	script string
	dir    string
	seq    atomic.Int64
}

func newPySender() (*pySender, error) {
	python, err := exec.LookPath("python3")
	if err != nil {
		if python, err = exec.LookPath("python"); err != nil {
			return nil, fmt.Errorf("找不到 python3")
		}
	}
	dir, err := os.MkdirTemp("", "loon2surge-")
	if err != nil {
		return nil, err
	}
	script := filepath.Join(dir, "pyfetch.py")
	if err := os.WriteFile(script, []byte(pyfetch), 0o644); err != nil {
		return nil, err
	}
	return &pySender{python: python, script: script, dir: dir}, nil
}

func (p *pySender) name() string { return "python3（OpenSSL TLS 栈，与上游同款）" }

func (p *pySender) get(raw string) (reply, error) {
	out := filepath.Join(p.dir, strconv.FormatInt(p.seq.Add(1), 10)+".body")

	command := exec.Command(p.python, p.script, raw, out)
	var stderr strings.Builder
	command.Stderr = &stderr

	stdout, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return reply{}, fmt.Errorf("%s", message)
	}

	status, err := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if err != nil {
		return reply{}, fmt.Errorf("下载脚本没有返回状态码：%q", strings.TrimSpace(string(stdout)))
	}
	body, readErr := os.ReadFile(out)
	_ = os.Remove(out)
	// 403/404 时脚本本来就不写正文，缺文件是正常答复；但 2xx 报了成功却没拿到字节
	// （或只拿到半份）不能算下载成功——截断的插件长得像完整插件，
	// 它的 SHA 会被记成"这一版内容"，下一轮就再也不会重下了。
	if readErr != nil && status >= http.StatusOK && status < http.StatusMultipleChoices {
		return reply{}, fmt.Errorf("状态 %d 但下载脚本没有留下正文：%w", status, readErr)
	}

	// 脚本在被拒时会把 cf-ray / cf-mitigated 写到 stderr，用来区分"质询"和"IP 封锁"
	hint := strings.TrimSpace(stderr.String())
	if status < 400 || hint == "" {
		hint = ""
	} else {
		hint = "，" + hint
	}

	return reply{body: body, status: status, hint: hint}, nil
}
