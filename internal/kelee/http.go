package kelee

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 这一层的请求特征不是随手写的，是照着"在 GitHub runner 上确实跑得通"的那份实现抄的：
//
//	HTTP/1.1 · 只带 User-Agent / Accept / Accept-Language · 不发 Referer · 不要 gzip。
//
// kelee.one 挂在 Cloudflare 后面，放行判断同时看来源 IP 和客户端特征。
// Go 默认客户端会走 HTTP/2、自动加 Accept-Encoding: gzip，再叠一个 Referer，
// 这套指纹更容易被判成爬虫：同一个 runner 上，别人朴素请求拿 200，我们拿 403。
// 所以这里刻意用最朴素的请求——少一点"现代化"，换一个能跑通的握手。
//
// 即便如此，机房出口 IP 仍会被随机拒掉一部分，那部分只能靠换 runner 重试，
// 或者用自建镜像（KLEE_MIRROR）绕过去。

// AcceptLanguage 与 Loon 客户端一致，别随手改成浏览器口味。
const AcceptLanguage = "zh-CN,zh-Hans;q=0.9"

// Blocked 表示目标站点直接拒绝（Cloudflare 拦截页）。
type Blocked struct{ Reason string }

func (e *Blocked) Error() string { return e.Reason }

// FetchError 表示这一条没下下来，但换个时间可能就好了。
type FetchError struct{ Reason string }

func (e *FetchError) Error() string { return e.Reason }

// permanentError 标记"重试也没意义"的错误，比如 404。
type permanentError struct{ Reason string }

func (e *permanentError) Error() string { return e.Reason }

const (
	// requestTimeout 单条请求的上限；上游同款实现用的是 60 秒。
	requestTimeout = 60 * time.Second
	// attempts 单条地址最多请求几次，只对"可能自己好"的错误生效。
	attempts = 3
)

// Sources 按顺序尝试多个站点，并记住哪个已经被拒，免得一轮里反复撞墙。
type Sources struct {
	hosts   []string
	mirrors []string
	client  *http.Client

	mu   sync.Mutex
	dead map[string]string
}

// NewSources 组装来源列表：官方站 + 可选镜像。
func NewSources(mirror string) *Sources {
	var mirrors []string
	if trimmed := strings.TrimRight(strings.TrimSpace(mirror), "/"); trimmed != "" {
		mirrors = append(mirrors, trimmed)
	}
	return &Sources{
		hosts:   append([]string{Canonical}, mirrors...),
		mirrors: mirrors,
		client:  newClient(),
		dead:    map[string]string{},
	}
}

// newClient 造一个"朴素客户端"：HTTP/1.1、不发 Accept-Encoding、每站最多 4 条连接。
//
// 限制并发不是为了省资源，而是不要在 Cloudflare 面前形成突发打请求的形态。
func newClient() *http.Client {
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
	return &http.Client{Timeout: requestTimeout, Transport: transport}
}

// hostKey 把地址归一成小写主机名，作为"这轮撞过墙"的记录键。
func hostKey(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return strings.ToLower(parsed.Hostname())
	}
	return strings.ToLower(raw)
}

// alive 表示还有没被拒绝的来源。索引在 hub、文件在 kelee.one，
// 所以要按真实主机名逐一对账，不能拿已拒绝的数量去比配置的数量。
func (s *Sources) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, host := range s.hosts {
		if _, dead := s.dead[hostKey(host)]; !dead {
			return true
		}
	}
	return false
}

func (s *Sources) isDead(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, dead := s.dead[hostKey(host)]
	return dead
}

func (s *Sources) kill(host, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead[hostKey(host)] = reason
}

// candidates 返回尝试顺序：原始地址优先，其次才是各镜像的同路径地址。
//
// 这里刻意不把原始域名改写成 Canonical：索引在 hub.kelee.one、文件在 kelee.one，
// 是两个不同的服务，把 hub 的路径搬到 kelee.one 上去只会得到 404。
func (s *Sources) candidates(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !strings.HasSuffix(parsed.Hostname(), "kelee.one") {
		return []string{raw}
	}
	tail := parsed.Path
	if parsed.RawQuery != "" {
		tail += "?" + parsed.RawQuery
	}
	out := make([]string, 1, 1+len(s.mirrors))
	out[0] = raw
	for _, mirror := range s.mirrors {
		out = append(out, mirror+tail)
	}
	return out
}

// Fetch 依次尝试可用来源，拿到第一份内容就返回。
func (s *Sources) Fetch(raw string) ([]byte, error) {
	var last error
	blocked := false

	for _, candidate := range s.candidates(raw) {
		parsed, err := url.Parse(candidate)
		if err != nil {
			last = &FetchError{Reason: err.Error()}
			continue
		}
		if s.isDead(parsed.Host) {
			continue
		}
		data, err := s.download(candidate)
		if err == nil {
			return data, nil
		}
		var denied *Blocked
		if errors.As(err, &denied) {
			// 403 是这台机器整个 IP 的判决，换路径也没用，直接判死省下后续几百次请求
			s.kill(parsed.Host, denied.Reason)
			blocked = true
		}
		last = err
	}

	if blocked && last != nil {
		var denied *Blocked
		if errors.As(last, &denied) {
			return nil, last
		}
		return nil, &Blocked{Reason: last.Error()}
	}
	if last == nil {
		return nil, &FetchError{Reason: "没有可用来源"}
	}
	return nil, last
}

// download 对同一个地址最多试几次，只重试"可能自己好"的错误。
func (s *Sources) download(raw string) ([]byte, error) {
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		data, err := s.once(raw)
		if err == nil {
			return data, nil
		}
		last = err

		var permanent *permanentError
		if errors.As(err, &permanent) {
			return nil, err
		}
		var denied *Blocked
		if errors.As(err, &denied) {
			return nil, err // 被 Cloudflare 判了就别再敲，原地重试只是浪费 20 秒
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
	}
	return nil, last
}

// once 发一次请求：三个头，就三个。
func (s *Sources) once(raw string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, &permanentError{Reason: err.Error()}
	}
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", AcceptLanguage)

	response, err := s.client.Do(request)
	if err != nil {
		return nil, &FetchError{Reason: err.Error() + " ← " + raw}
	}
	defer response.Body.Close()

	switch code := response.StatusCode; {
	case code == http.StatusOK:
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, &FetchError{Reason: err.Error() + " ← " + raw}
		}
		return data, nil
	case code == http.StatusForbidden:
		return nil, &Blocked{Reason: fmt.Sprintf("%s 拒绝访问（403%s）", request.URL.Host, edgeHint(response))}
	case code == http.StatusNotFound:
		return nil, &permanentError{Reason: "404 " + raw}
	case code == http.StatusTooManyRequests, code >= 500:
		return nil, &FetchError{Reason: fmt.Sprintf("HTTP %d %s", code, raw)}
	default:
		return nil, &permanentError{Reason: fmt.Sprintf("HTTP %d %s", code, raw)}
	}
}

// edgeHint 把 Cloudflare 的定位信息摘出来，失败时能直接看出是哪种拦截。
func edgeHint(response *http.Response) string {
	var parts []string
	if ray := response.Header.Get("cf-ray"); ray != "" {
		parts = append(parts, "cf-ray="+ray)
	}
	if mitigated := response.Header.Get("cf-mitigated"); mitigated != "" {
		parts = append(parts, "cf-mitigated="+mitigated)
	}
	if server := response.Header.Get("server"); server != "" && len(parts) == 0 {
		parts = append(parts, "server="+server)
	}
	if len(parts) == 0 {
		return ""
	}
	return "，" + strings.Join(parts, "，")
}
