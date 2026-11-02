package kelee

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Blocked 表示目标站点直接拒绝（Cloudflare 拦截页）：换来源或下次再说，重试没意义。
type Blocked struct{ Reason string }

func (e *Blocked) Error() string { return e.Reason }

// FetchError 表示这一条没下下来，但换个时间可能就好了。
type FetchError struct{ Reason string }

func (e *FetchError) Error() string { return e.Reason }

// Sources 按顺序尝试多个站点，并记住哪个已经拒绝过，免得一轮里反复撞墙。
type Sources struct {
	hosts  []string
	client *http.Client
	mu     sync.Mutex
	dead   map[string]string
}

// NewSources 组装来源列表：官方站 + 可选镜像。
func NewSources(mirror string) *Sources {
	hosts := []string{Canonical}
	if strings.TrimSpace(mirror) != "" {
		hosts = append(hosts, strings.TrimRight(strings.TrimSpace(mirror), "/"))
	}
	return &Sources{
		hosts:  hosts,
		client: &http.Client{Timeout: 40 * time.Second},
		dead:   map[string]string{},
	}
}

func (s *Sources) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.dead) < len(s.hosts)
}

func (s *Sources) isDead(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, dead := s.dead[host]
	return dead
}

func (s *Sources) kill(host, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead[host] = reason
}

// candidates 把 kelee.one 的地址改写成各来源的同路径地址；外链（如 GitHub）不动。
func (s *Sources) candidates(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !strings.HasSuffix(parsed.Hostname(), "kelee.one") {
		return []string{raw}
	}
	tail := parsed.Path
	if parsed.RawQuery != "" {
		tail += "?" + parsed.RawQuery
	}
	out := make([]string, 0, len(s.hosts))
	for _, host := range s.hosts {
		out = append(out, host+tail)
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
		data, err := s.get(candidate)
		if err == nil {
			return data, nil
		}
		var denied *Blocked
		if errors.As(err, &denied) {
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

func (s *Sources) get(raw string) ([]byte, error) {
	const attempts = 3

	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			return nil, &FetchError{Reason: err.Error()}
		}
		request.Header.Set("User-Agent", UserAgent)
		request.Header.Set("Accept", "*/*")
		request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		request.Header.Set("Referer", "https://hub.kelee.one/")

		response, err := s.client.Do(request)
		if err != nil {
			last = &FetchError{Reason: err.Error() + " ← " + raw}
			sleepBackoff(attempt)
			continue
		}

		host := request.URL.Host
		switch {
		case response.StatusCode == http.StatusForbidden:
			response.Body.Close()
			return nil, &Blocked{Reason: host + " 拒绝访问（403）"}
		case response.StatusCode == http.StatusNotFound:
			response.Body.Close()
			return nil, &FetchError{Reason: "404 " + raw}
		case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= 500:
			response.Body.Close()
			last = &FetchError{Reason: fmt.Sprintf("HTTP %d %s", response.StatusCode, raw)}
			sleepBackoff(attempt)
			continue
		case response.StatusCode != http.StatusOK:
			response.Body.Close()
			return nil, &FetchError{Reason: fmt.Sprintf("HTTP %d %s", response.StatusCode, raw)}
		}

		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			last = &FetchError{Reason: err.Error() + " ← " + raw}
			sleepBackoff(attempt)
			continue
		}
		return data, nil
	}
	return nil, last
}

func sleepBackoff(attempt int) {
	delay := 1.5*float64(attempt+1) + rand.Float64()
	time.Sleep(time.Duration(delay * float64(time.Second)))
}
