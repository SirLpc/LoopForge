package gitwork

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"agentflow/internal/gitegress"
)

const PlainHTTPPrefix = "http://"

// HTTPHostScope retains the existing HTTPS key and includes the scheme in a
// plain HTTP key. This keeps the two credentials separate on the same port.
func HTTPHostScope(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(raw, PlainHTTPPrefix) {
		return HTTPSHostScope(raw)
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	invalid := errors.New("HTTP Git 地址须使用内网 IP，可附带端口，例如 http://192.168.50.10:8443")
	if err != nil || u.User != nil || u.Path != "" || strings.ContainsAny(raw, "?#%") {
		return "", invalid
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip.Is4In6() || !ip.IsPrivate() || ip.Is6() != strings.HasPrefix(u.Host, "[") {
		return "", invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", invalid
		}
		if n != 80 {
			return PlainHTTPPrefix + net.JoinHostPort(ip.String(), port), nil
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if ip.Is6() {
		return PlainHTTPPrefix + "[" + ip.String() + "]", nil
	}
	return PlainHTTPPrefix + ip.String(), nil
}

// CheckHTTPAccess enforces the deployment's opt-in for plain HTTP endpoints.
// Callers pass a scope that has already been validated by HTTPHostScope.
func CheckHTTPAccess(scope string) error {
	if !strings.HasPrefix(scope, PlainHTTPPrefix) {
		return nil
	}
	u, err := url.Parse(scope)
	if err != nil {
		return errHTTPSHost
	}
	ip, _ := netip.ParseAddr(u.Hostname())
	if !gitegress.PrivateHTTPAllowed(ip) {
		return fmt.Errorf("HTTP Git 地址 %s 需要管理员在 %s 中允许该内网 IP", scope, gitegress.AllowCIDRsEnv)
	}
	return nil
}

// HTTPRemoteScope returns the credential key of an HTTP or HTTPS remote.
func HTTPRemoteScope(remote string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(remote))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	scope, err := HTTPHostScope(u.Scheme + "://" + u.Host)
	return scope, err == nil
}

// HTTPRepositoryURL accepts HTTPS or a private literal HTTP endpoint.
// Private GitLab clone addresses use .git so that Git does not need a redirect.
func HTTPRepositoryURL(raw string) (remote, name string, err error) {
	address := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "git clone "))
	if !strings.HasPrefix(strings.ToLower(address), PlainHTTPPrefix) {
		return HTTPSRepositoryURL(raw)
	}
	u, err := url.Parse(address)
	if err != nil || u.User != nil || strings.ContainsAny(address, "?#\\\t\r\n ") {
		return "", "", fmt.Errorf("请提供不含凭据、查询参数或片段的内网 HTTP Git 克隆地址")
	}
	scope, err := HTTPHostScope(PlainHTTPPrefix + u.Host)
	if err != nil {
		return "", "", err
	}
	return normalizeGitHTTPRepository(u, strings.TrimPrefix(scope, PlainHTTPPrefix))
}
