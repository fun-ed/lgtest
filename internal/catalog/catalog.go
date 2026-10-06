package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

var Seeds = map[string]string{"psychz": "https://lg.tw.psychz.net/", "vultr": "https://hnd-jp-ping.vultr.com/"}

type File struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}
type Region struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	PageURL  string `json:"page_url"`
	IPv4     string `json:"ipv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
	Files    []File `json:"files"`
	Error    string `json:"error,omitempty"`
}
type Catalog struct {
	SchemaVersion int       `json:"schema_version"`
	FetchedAt     time.Time `json:"fetched_at"`
	Method        string    `json:"method"`
	Regions       []Region  `json:"regions"`
}

func Allowed(provider string, u *url.URL) bool {
	if u == nil || u.User != nil || u.Scheme != "https" || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch provider {
	case "psychz":
		return strings.HasPrefix(host, "lg.") && strings.HasSuffix(host, ".psychz.net")
	case "vultr":
		return strings.HasSuffix(host, "-ping.vultr.com")
	}
	return false
}
func ID(provider, host string) string {
	return provider + ":" + strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(host, "lg."), ".psychz.net"), ".vultr.com")
}
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func text(n *html.Node) string {
	var b strings.Builder
	var visit func(*html.Node)
	visit = func(p *html.Node) {
		if p.Type == html.TextNode {
			b.WriteString(p.Data)
			b.WriteByte(' ')
		}
		for c := p.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
func each(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		each(c, fn)
	}
}
func testIP(s string, want4 bool) string {
	for _, token := range strings.Fields(s) {
		token = strings.Trim(token, "[](),")
		ip := net.ParseIP(token)
		if ip != nil && (ip.To4() != nil) == want4 {
			return ip.String()
		}
	}
	return ""
}

// Parse extracts only public test targets, never the visitor-IP field.
func Parse(provider, page string, body io.Reader) (Region, []Region, error) {
	base, err := url.Parse(page)
	if err != nil || !Allowed(provider, base) {
		return Region{}, nil, fmt.Errorf("unsupported provider page")
	}
	root, err := html.Parse(body)
	if err != nil {
		return Region{}, nil, err
	}
	current := Region{ID: ID(provider, base.Hostname()), Provider: provider, Name: base.Hostname(), PageURL: base.String(), Files: []File{}}
	discovered := map[string]Region{}
	seenFiles := map[string]bool{}
	each(root, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		label := text(n)
		if n.Data == "li" {
			if strings.HasPrefix(label, "Server Location:") {
				current.Name = strings.TrimSpace(strings.TrimPrefix(label, "Server Location:"))
			}
			if strings.HasPrefix(label, "Test IPv4:") {
				current.IPv4 = testIP(strings.TrimPrefix(label, "Test IPv4:"), true)
			}
			if strings.HasPrefix(label, "Test IPv6:") {
				current.IPv6 = testIP(strings.TrimPrefix(label, "Test IPv6:"), false)
			}
			if strings.HasPrefix(label, "Test files:") {
				each(n, func(a *html.Node) {
					if a.Data != "a" {
						return
					}
					u, e := base.Parse(attr(a, "href"))
					if e != nil || !Allowed(provider, u) || u.Fragment != "" || u.RawQuery != "" {
						return
					}
					if !strings.HasSuffix(u.Path, ".test") && !strings.HasSuffix(u.Path, ".bin") {
						return
					}
					if !seenFiles[u.String()] {
						current.Files = append(current.Files, File{Label: text(a), URL: u.String()})
						seenFiles[u.String()] = true
					}
				})
			}
		}
		raw := ""
		if provider == "psychz" && n.Data == "a" {
			raw = attr(n, "href")
		}
		if provider == "vultr" && n.Data == "option" {
			raw = attr(n, "data-url")
		}
		if raw == "" {
			return
		}
		u, e := base.Parse(raw)
		if e != nil || !Allowed(provider, u) || (u.Path != "" && u.Path != "/") || u.Fragment != "" || u.RawQuery != "" {
			return
		}
		u.Path = "/"
		candidate := Region{ID: ID(provider, u.Hostname()), Provider: provider, Name: label, PageURL: u.String(), Files: []File{}}
		old, exists := discovered[candidate.ID]
		if !exists || old.Name == "" {
			discovered[candidate.ID] = candidate
		}
		if provider == "vultr" && attr(n, "selected") != "" {
			current.Name = label
		}
	})
	if len(current.Files) == 0 {
		return current, nil, fmt.Errorf("no test-file links found")
	}
	discovered[current.ID] = current
	list := make([]Region, 0, len(discovered))
	for _, r := range discovered {
		if r.Name == "" {
			r.Name = r.ID
		}
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return current, list, nil
}

func fetch(ctx context.Context, client *http.Client, provider, page string) (Region, []Region, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return Region{}, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:120.0) Gecko/20100101 Firefox/120.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return Region{}, nil, fmt.Errorf("page request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Region{}, nil, fmt.Errorf("page HTTP %d", resp.StatusCode)
	}
	final := resp.Request.URL
	if !Allowed(provider, final) {
		return Region{}, nil, fmt.Errorf("page redirected outside provider")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil {
		return Region{}, nil, fmt.Errorf("page read failed")
	}
	if len(data) > 4*1024*1024 {
		return Region{}, nil, fmt.Errorf("page exceeds 4 MiB")
	}
	return Parse(provider, page, strings.NewReader(string(data)))
}

// Refresh discovers public locations from seeds, then reads their advertised data.
// A failed page remains visible with an error; stale IPs are not invented.
func Refresh(ctx context.Context, client *http.Client, provider string) (Catalog, error) {
	result := Catalog{SchemaVersion: 1, FetchedAt: time.Now().UTC(), Method: "live-html", Regions: []Region{}}
	providers := []string{"psychz", "vultr"}
	if provider != "all" {
		if _, ok := Seeds[provider]; !ok {
			return result, fmt.Errorf("unknown provider")
		}
		providers = []string{provider}
	}
	targets := []Region{}
	for _, p := range providers {
		current, found, err := fetch(ctx, client, p, Seeds[p])
		if err != nil {
			return result, fmt.Errorf("%s seed: %w", p, err)
		}
		result.Regions = append(result.Regions, current)
		for _, r := range found {
			if r.ID != current.ID {
				targets = append(targets, r)
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, target := range targets {
		wg.Add(1)
		go func(r Region) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				r.Error = "refresh cancelled"
				mu.Lock()
				result.Regions = append(result.Regions, r)
				mu.Unlock()
				return
			}
			defer func() { <-slots }()
			data, _, err := fetch(ctx, client, r.Provider, r.PageURL)
			if err != nil {
				r.Error = err.Error()
			} else {
				if data.Name == r.PageURL || data.Name == strings.TrimSuffix(strings.TrimPrefix(r.PageURL, "https://"), "/") {
					data.Name = r.Name
				}
				r = data
			}
			mu.Lock()
			result.Regions = append(result.Regions, r)
			mu.Unlock()
		}(target)
	}
	wg.Wait()
	sort.Slice(result.Regions, func(i, j int) bool { return result.Regions[i].ID < result.Regions[j].ID })
	return result, nil
}
func Load(path string) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, err
	}
	return decode(data)
}
func decode(data []byte) (Catalog, error) {
	var result Catalog
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if result.SchemaVersion != 1 || len(result.Regions) == 0 {
		return result, fmt.Errorf("invalid or empty catalog")
	}
	for _, r := range result.Regions {
		if r.ID == "" || r.Provider == "" {
			return result, fmt.Errorf("invalid region")
		}
		if r.IPv4 != "" && (net.ParseIP(r.IPv4) == nil || net.ParseIP(r.IPv4).To4() == nil) {
			return result, fmt.Errorf("invalid IPv4")
		}
		if r.IPv6 != "" && (net.ParseIP(r.IPv6) == nil || net.ParseIP(r.IPv6).To4() != nil) {
			return result, fmt.Errorf("invalid IPv6")
		}
		page, e := url.Parse(r.PageURL)
		if e != nil || !Allowed(r.Provider, page) {
			return result, fmt.Errorf("invalid region page")
		}
		for _, f := range r.Files {
			u, e := url.Parse(f.URL)
			if e != nil || !Allowed(r.Provider, u) {
				return result, fmt.Errorf("invalid file URL")
			}
		}
	}
	return result, nil
}
func SaveNew(path string, value any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
