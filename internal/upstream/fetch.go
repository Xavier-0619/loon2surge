package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// AcceptLanguage 与 Loon 客户端一致，别随手改成浏览器口味。
const AcceptLanguage = "zh-CN,zh-Hans;q=0.9"

// Blocked 表示目标站点直接拒绝（Cloudflare 拦截或质询）。
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
	sender  sender

	mu   sync.Mutex
	dead map[string]string
}

// NewSourcesWith 指定下载通道：auto / python / go。
func NewSourcesWith(mirror, fetcher string) (*Sources, error) {
	channel, err := pickSender(fetcher)
	if err != nil {
		return nil, err
	}
	// auto 想用 python 却没找到：要说出来，否则后面 403 会让人以为是 IP 问题
	if _, fellBack := channel.(*goSender); fellBack && (fetcher == "" || fetcher == "auto") {
		fmt.Fprintln(os.Stderr, "! 没找到 python3，退回 Go 原生通道：Cloudflare 可能直接返回托管质询（403）")
	}
	var mirrors []string
	if trimmed := strings.TrimRight(strings.TrimSpace(mirror), "/"); trimmed != "" {
		mirrors = append(mirrors, trimmed)
	}
	return &Sources{
		hosts:   append([]string{Canonical}, mirrors...),
		mirrors: mirrors,
		sender:  channel,
		dead:    map[string]string{},
	}, nil
}

// channel 说明这一轮用哪条通道下载，出问题先看它。
func (s *Sources) channel() string { return s.sender.name() }

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
// 是两个不同的服务，把 hub 的路径搬到 kelee.one 上只会得到 404。
func (s *Sources) candidates(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil || !isKeleeHost(parsed.Hostname()) {
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

// isKeleeHost 只认真正的 kelee.one 及其子域。
//
// 光用 HasSuffix 会把 notkelee.one 也算进来，那样的话一个陌生主机的地址会被
// 改写成镜像站上的同路径地址，拿回来的东西根本不是它声称的那份。
func isKeleeHost(host string) bool {
	host = strings.ToLower(host)
	return host == "kelee.one" || strings.HasSuffix(host, ".kelee.one")
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
			return nil, err // 被判了就別再敲，原地重试只是浪费 20 秒
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
	}
	return nil, last
}

// once 发一次请求，并把状态码翻译成"该重试 / 该放弃 / 被拦了"。
func (s *Sources) once(raw string) ([]byte, error) {
	result, err := s.sender.get(raw)
	if err != nil {
		return nil, &FetchError{Reason: err.Error() + " ← " + raw}
	}

	switch code := result.status; {
	case code == http.StatusOK:
		return result.body, nil
	case code == http.StatusForbidden:
		return nil, &Blocked{Reason: describe403(hostKey(raw), result.hint)}
	case code == http.StatusNotFound:
		return nil, &permanentError{Reason: "404 " + raw}
	case code == http.StatusTooManyRequests, code >= 500:
		return nil, &FetchError{Reason: fmt.Sprintf("HTTP %d %s", code, raw)}
	default:
		return nil, &permanentError{Reason: fmt.Sprintf("HTTP %d %s", code, raw)}
	}
}

// describe403 把 Cloudflare 的两种拒绝区分开，不然排查时会一直猜 IP。
func describe403(host, hint string) string {
	if strings.Contains(hint, "cf-mitigated=challenge") {
		return fmt.Sprintf("%s 被 Cloudflare 托管质询（JS challenge%s）——这是 TLS 指纹被判成机器人，不是 IP 封锁", host, hint)
	}
	return fmt.Sprintf("%s 拒绝访问（403%s）", host, hint)
}

// headerHint 从响应头里摘出 Cloudflare 的定位信息。
func headerHint(header http.Header) string {
	var parts []string
	if ray := header.Get("cf-ray"); ray != "" {
		parts = append(parts, "cf-ray="+ray)
	}
	if mitigated := header.Get("cf-mitigated"); mitigated != "" {
		parts = append(parts, "cf-mitigated="+mitigated)
	}
	if server := header.Get("server"); server != "" && len(parts) == 0 {
		parts = append(parts, "server="+server)
	}
	if len(parts) == 0 {
		return ""
	}
	return "，" + strings.Join(parts, "，")
}
