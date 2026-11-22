package signup

// Read-only sources the agent uses to learn about an applicant: a public web page and a GitHub user.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProfileSources is what the agent's read tools can fetch. Tests replace it; the default reaches the public internet.
type ProfileSources interface {
	// FetchProfilePage returns the visible text of a public HTTPS page.
	FetchProfilePage(ctx context.Context, url string) (ProfilePage, error)
	// FetchGitHubUser returns a GitHub user's public profile and their most starred recent repositories.
	FetchGitHubUser(ctx context.Context, login string) (GitHubUserSummary, error)
}

// ProfilePage is the text of one fetched page.
type ProfilePage struct {
	URL         string `json:"url"`
	Status      int    `json:"status"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	IsTruncated bool   `json:"isTruncated"`
}

// GitHubRepository is one of a user's repositories.
type GitHubRepository struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Language    *string `json:"language"`
	Stars       float64 `json:"stars"`
	PushedAt    *string `json:"pushedAt"`
	IsFork      bool    `json:"isFork"`
}

// GitHubUserSummary is the public facts of one GitHub user.
type GitHubUserSummary struct {
	Login           string             `json:"login"`
	Name            *string            `json:"name"`
	Bio             *string            `json:"bio"`
	Company         *string            `json:"company"`
	Blog            *string            `json:"blog"`
	Location        *string            `json:"location"`
	Followers       float64            `json:"followers"`
	PublicRepos     float64            `json:"publicRepos"`
	CreatedAt       string             `json:"createdAt"`
	HTMLURL         string             `json:"htmlUrl"`
	TopRepositories []GitHubRepository `json:"topRepositories"`
}

// ProfileSourceError is a failure the agent should read, such as a blocked address or a 404.
type ProfileSourceError struct{ Message string }

func (err *ProfileSourceError) Error() string { return err.Message }

func profileError(format string, arguments ...any) error {
	return &ProfileSourceError{Message: fmt.Sprintf(format, arguments...)}
}

// Limits of FetchPublicPage.
const (
	profileMaxBytes       = 1024 * 1024
	profileTimeout        = 10 * time.Second
	profileMaxRedirects   = 3
	profileMaxText        = 20_000
	profileMaxParsedChars = 200_000
)

var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// blockedPrefixes are the addresses a fetch must never reach: loopback, private, link-local, carrier-grade NAT,
// multicast and reserved. An IPv4-mapped IPv6 address is checked against the IPv4 prefixes.
var blockedPrefixes = func() []netip.Prefix {
	var prefixes []netip.Prefix
	for _, prefix := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
		"192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "::/96", "2001::/32", "2001:db8::/32", "2002::/16", "fec0::/10",
		"fc00::/7", "fe80::/10", "ff00::/8",
	} {
		prefixes = append(prefixes, netip.MustParsePrefix(prefix))
	}
	return prefixes
}()

// IsPublicAddress reports whether an IP address is one a public fetch may connect to.
func IsPublicAddress(address string) bool {
	parsed, err := netip.ParseAddr(address)
	if err != nil || parsed.Zone() != "" {
		return false
	}
	return isPublicAddr(parsed)
}

func isPublicAddr(address netip.Addr) bool {
	address = address.WithZone("")
	if address.Is4In6() {
		address = address.Unmap()
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Addr().Is4() == address.Is4() && prefix.Contains(address) {
			return false
		}
	}
	return true
}

// CheckProfileURL checks a URL before any connection: HTTPS only, no credentials, the default port, and no literal
// non-public address.
func CheckProfileURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || (parsed.Host == "" && parsed.Opaque == "") {
		// Quoted as JSON: the URL comes from the model, and a raw line break in it could pass for another turn-note entry.
		return nil, profileError("not a valid URL: %s", jsJSONString(cutUTF16(rawURL, 200)))
	}
	if parsed.Scheme != "https" {
		return nil, profileError("only https URLs can be fetched")
	}
	if parsed.User != nil {
		return nil, profileError("URLs with credentials are not fetched")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return nil, profileError("only the default HTTPS port is fetched")
	}
	host := strings.ToLower(parsed.Hostname())
	if address, err := netip.ParseAddr(host); err == nil && !isPublicAddr(address) {
		return nil, profileError("the URL points to a private or local address")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, profileError("the URL points to a private or local host")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed, nil
}

// Lookup resolves a host name to its addresses; tests pass their own.
type Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// RawResponse is one HTTP response, read up to a byte limit.
type RawResponse struct {
	Status      int
	Location    string
	ContentType string
	Body        []byte
	IsTruncated bool
}

// PageTransport sends one GET through a dialer that only connects to addresses lookup checked; tests pass their own.
type PageTransport func(ctx context.Context, target *url.URL, lookup Lookup, maxBytes int) (RawResponse, error)

// publicDialer connects only to addresses that are all public: every address lookup returns is checked, and the
// connection goes to a checked address, so no later resolution can point it elsewhere.
func publicDialer(lookup Lookup) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		var addresses []netip.Addr
		if literal, err := netip.ParseAddr(host); err == nil {
			addresses = []netip.Addr{literal}
		} else if addresses, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, profileError("the host resolves to a private or local address")
		}
		for _, resolved := range addresses {
			if !isPublicAddr(resolved) {
				return nil, profileError("the host resolves to a private or local address")
			}
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].WithZone("").String(), port))
	}
}

// httpsGet is one GET through its own Transport: no environment proxy, which would resolve the host instead of the
// checked dialer, and no redirect following.
func httpsGet(ctx context.Context, target *url.URL, lookup Lookup, maxBytes int) (RawResponse, error) {
	transport := &http.Transport{Proxy: nil, DialContext: publicDialer(lookup), DisableKeepAlives: true, ForceAttemptHTTP2: true, TLSHandshakeTimeout: profileTimeout}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return RawResponse{}, err
	}
	request.Header.Set("user-agent", "dex-signup-agent-demo/0.1")
	request.Header.Set("accept", "text/html,text/plain;q=0.9,*/*;q=0.5")
	response, err := client.Do(request)
	if err != nil {
		return RawResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)))
	if err != nil && len(body) < maxBytes {
		return RawResponse{}, err
	}
	return RawResponse{
		Status: response.StatusCode, Location: response.Header.Get("location"), ContentType: response.Header.Get("content-type"),
		Body: body, IsTruncated: len(body) >= maxBytes,
	}, nil
}

// FetchPublicPage fetches a public HTTPS page and returns its visible text: at most 1 MB read, 10 seconds, three
// redirects, each redirect target checked like the first URL, and every resolved address checked before the
// connection. It returns a *ProfileSourceError when the URL or an address is not allowed, or the fetch fails.
func FetchPublicPage(ctx context.Context, rawURL string, lookup Lookup, transport PageTransport) (ProfilePage, error) {
	if lookup == nil {
		lookup = defaultLookup
	}
	if transport == nil {
		transport = httpsGet
	}
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	target, err := CheckProfileURL(rawURL)
	if err != nil {
		return ProfilePage{}, err
	}
	for redirects := 0; ; redirects++ {
		response, err := transport(ctx, target, lookup, profileMaxBytes)
		if err != nil {
			var sourceError *ProfileSourceError
			if errors.As(err, &sourceError) {
				return ProfilePage{}, sourceError
			}
			return ProfilePage{}, profileError("fetching %s failed: %s", target.Hostname(), cutUTF16(collapseWhitespace(unwrapURLError(err).Error()), 200))
		}
		if response.Status >= 300 && response.Status < 400 && response.Location != "" {
			if redirects >= profileMaxRedirects {
				return ProfilePage{}, profileError("too many redirects")
			}
			location, err := target.Parse(response.Location)
			if err != nil {
				return ProfilePage{}, profileError("not a valid URL: %s", jsJSONString(cutUTF16(response.Location, 200)))
			}
			if target, err = CheckProfileURL(location.String()); err != nil {
				return ProfilePage{}, err
			}
			continue
		}
		html := cutUTF16(strings.ToValidUTF8(string(response.Body), "�"), profileMaxParsedChars)
		isHTML := strings.Contains(response.ContentType, "html") || htmlStart.MatchString(cutUTF16(html, 100))
		text := ""
		title := ""
		if isHTML {
			text = HTMLToText(html)
			title = titleOf(html)
		} else {
			text = jsTrim(collapseWhitespace(html))
		}
		text = cutUTF16(text, profileMaxText)
		return ProfilePage{URL: target.String(), Status: response.Status, Title: title, Text: text, IsTruncated: response.IsTruncated || utf16Length(text) >= profileMaxText}, nil
	}
}

var (
	htmlStart           = regexp.MustCompile(`(?i)^` + jsWhitespaceClass + `*<(!doctype|html)`)
	horizontalSpaceRun  = regexp.MustCompile(`[ \t\f\v\r]+`)
	lineBreakRun        = regexp.MustCompile(` ?\n[ \n]*`)
	tooManyLineBreaks   = regexp.MustCompile(`\n{3,}`)
	htmlEntity          = regexp.MustCompile(`(?i)&(#x[0-9a-f]{1,6}|#\d{1,7}|[a-z]{1,8});`)
	skippedElements     = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "template": true}
	lineBreakingElement = map[string]bool{"br": true, "p": true, "div": true, "li": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "tr": true}
)

// asciiLower lowercases only ASCII letters, so every byte index of the result is the same as in text.
func asciiLower(text string) string {
	bytes := []byte(text)
	for index, character := range bytes {
		if character >= 'A' && character <= 'Z' {
			bytes[index] = character + ('a' - 'A')
		}
	}
	return string(bytes)
}

// HTMLToText is the visible text of HTML, in one linear pass with index searches: no backtracking patterns on page
// content.
func HTMLToText(html string) string {
	lower := asciiLower(html)
	var text strings.Builder
	index := 0
	for index < len(html) {
		open := indexFrom(html, "<", index)
		if open < 0 {
			text.WriteString(html[index:])
			break
		}
		text.WriteString(html[index:open])
		if strings.HasPrefix(html[open:], "<!--") {
			end := indexFrom(html, "-->", open+4)
			if end < 0 {
				index = len(html)
			} else {
				index = end + 3
			}
			text.WriteByte(' ')
			continue
		}
		closeAt := indexFrom(html, ">", open)
		if closeAt < 0 {
			break
		}
		isEnd := open+1 < len(html) && html[open+1] == '/'
		nameStart := open + 1
		if isEnd {
			nameStart = open + 2
		}
		nameEnd := nameStart
		for nameEnd < closeAt && ((lower[nameEnd] >= 'a' && lower[nameEnd] <= 'z') || (lower[nameEnd] >= '0' && lower[nameEnd] <= '9')) {
			nameEnd++
		}
		name := ""
		if nameStart <= nameEnd {
			name = lower[nameStart:nameEnd]
		}
		if !isEnd && skippedElements[name] {
			end := indexFrom(lower, "</"+name, closeAt)
			endClose := -1
			if end >= 0 {
				endClose = indexFrom(html, ">", end)
			}
			if endClose < 0 {
				index = len(html)
			} else {
				index = endClose + 1
			}
			text.WriteByte(' ')
			continue
		}
		if lineBreakingElement[name] {
			text.WriteByte('\n')
		} else {
			text.WriteByte(' ')
		}
		index = closeAt + 1
	}
	decoded := decodeEntities(text.String())
	decoded = horizontalSpaceRun.ReplaceAllString(decoded, " ")
	decoded = lineBreakRun.ReplaceAllString(decoded, "\n")
	decoded = tooManyLineBreaks.ReplaceAllString(decoded, "\n\n")
	return jsTrim(decoded)
}

func titleOf(html string) string {
	lower := asciiLower(html)
	open := strings.Index(lower, "<title")
	start, end := -1, -1
	if open >= 0 {
		start = indexFrom(lower, ">", open)
	}
	if start >= 0 {
		end = indexFrom(lower, "</title", start)
	}
	if end < 0 {
		return ""
	}
	return cutUTF16(jsTrim(collapseWhitespace(decodeEntities(html[start+1:end]))), 300)
}

var namedEntities = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": " "}

func decodeEntities(text string) string {
	return htmlEntity.ReplaceAllStringFunc(text, func(entity string) string {
		code := entity[1 : len(entity)-1]
		value := int64(-1)
		switch {
		case strings.HasPrefix(code, "#x") || strings.HasPrefix(code, "#X"):
			value, _ = strconv.ParseInt(code[2:], 16, 64)
		case strings.HasPrefix(code, "#"):
			value, _ = strconv.ParseInt(code[1:], 10, 64)
		}
		if value >= 0 {
			if value > 0 && value <= 0x10ffff {
				return string(rune(value))
			}
			return entity
		}
		if named, ok := namedEntities[strings.ToLower(code)]; ok {
			return named
		}
		return entity
	})
}

// PublicProfileSources are the real sources: public web pages, and the GitHub public REST API with an optional token
// for its rate limit.
type PublicProfileSources struct {
	GitHubToken string
	// GitHubAPI is the GitHub API base URL; tests point it elsewhere.
	GitHubAPI string
}

// FetchProfilePage fetches one public HTTPS page.
func (sources PublicProfileSources) FetchProfilePage(ctx context.Context, rawURL string) (ProfilePage, error) {
	return FetchPublicPage(ctx, rawURL, nil, nil)
}

// FetchGitHubUser reads a GitHub user and their repositories, the 8 most starred of the 30 pushed last.
func (sources PublicProfileSources) FetchGitHubUser(ctx context.Context, login string) (GitHubUserSummary, error) {
	if !githubLoginPattern.MatchString(login) {
		return GitHubUserSummary{}, profileError("%s is not a GitHub login", jsJSONString(cutUTF16(login, 60)))
	}
	var user map[string]json.RawMessage
	if err := sources.getJSON(ctx, "/users/"+login, &user); err != nil {
		return GitHubUserSummary{}, err
	}
	var repositories []map[string]json.RawMessage
	var rawRepositories json.RawMessage
	if err := sources.getJSON(ctx, "/users/"+login+"/repos?sort=pushed&per_page=30&type=owner", &rawRepositories); err != nil {
		return GitHubUserSummary{}, err
	}
	_ = json.Unmarshal(rawRepositories, &repositories)
	summary := GitHubUserSummary{
		Login: firstString(user["login"], login), Name: shortText(user["name"]), Bio: shortText(user["bio"]), Company: shortText(user["company"]),
		Blog: shortText(user["blog"]), Location: shortText(user["location"]), Followers: numberOf(user["followers"]), PublicRepos: numberOf(user["public_repos"]),
		CreatedAt: firstString(user["created_at"], ""), HTMLURL: firstString(user["html_url"], "https://github.com/"+login),
		TopRepositories: []GitHubRepository{},
	}
	for _, repository := range repositories {
		var isFork bool
		_ = json.Unmarshal(repository["fork"], &isFork)
		summary.TopRepositories = append(summary.TopRepositories, GitHubRepository{
			Name: firstString(repository["name"], ""), Description: shortText(repository["description"]), Language: shortText(repository["language"]),
			Stars: numberOf(repository["stargazers_count"]), PushedAt: shortText(repository["pushed_at"]), IsFork: isFork,
		})
	}
	sort.SliceStable(summary.TopRepositories, func(left, right int) bool {
		return summary.TopRepositories[left].Stars > summary.TopRepositories[right].Stars
	})
	if len(summary.TopRepositories) > 8 {
		summary.TopRepositories = summary.TopRepositories[:8]
	}
	return summary, nil
}

// getJSON makes one GitHub API call, its body read up to 1 MB.
func (sources PublicProfileSources) getJSON(ctx context.Context, path string, target any) error {
	base := sources.GitHubAPI
	if base == "" {
		base = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "dex-signup-agent-demo/0.1")
	if sources.GitHubToken != "" {
		request.Header.Set("authorization", "Bearer "+sources.GitHubToken)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("GitHub answered with a redirect") }}
	response, err := client.Do(request)
	if err != nil {
		return unwrapURLError(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return profileError("GitHub has no such user")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return profileError("GitHub answered HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, profileMaxBytes+1))
	if err != nil {
		return err
	}
	if len(body) > profileMaxBytes {
		return profileError("GitHub response is too large")
	}
	return json.Unmarshal(body, target)
}

// shortText is a non-empty string cut to 500 characters, or nil.
func shortText(raw json.RawMessage) *string {
	var text string
	if json.Unmarshal(raw, &text) != nil || text == "" {
		return nil
	}
	text = cutUTF16(text, 500)
	return &text
}

// firstString is String(value ?? fallback) for a string value.
func firstString(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	return stringOf(raw)
}

// numberOf is Number(value ?? 0) for a number value.
func numberOf(raw json.RawMessage) float64 {
	var number float64
	_ = json.Unmarshal(raw, &number)
	return number
}
