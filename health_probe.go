package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
)

type probeCommand func(name string, args ...string) ([]byte, error)

var probeURLs = []string{
	"https://api.ipify.org",
	"https://ipv4.icanhazip.com",
}

func defaultProbeCommand(name string, args ...string) ([]byte, error) {
	return cmdOutput(exec.Command(name, args...))
}

// probeExitThroughSOCKS validates the same public SOCKS5 path used by clients.
// It tries two independent IP endpoints so one provider outage does not evict a tunnel.
func probeExitThroughSOCKS(t *Tunnel, run probeCommand) (string, error) {
	if t == nil {
		return "", fmt.Errorf("出口隧道不能为空")
	}
	if t.Port < 1 || t.Port > 65535 {
		return "", fmt.Errorf("SOCKS5 端口无效: %d", t.Port)
	}
	if run == nil {
		run = defaultProbeCommand
	}
	cred := t.credential()
	proxy := fmt.Sprintf("127.0.0.1:%d", t.Port)
	var failures []string
	for _, endpoint := range probeURLs {
		out, err := run("curl",
			"-fsS", "--max-time", "8",
			"--socks5-hostname", proxy,
			"--proxy-user", cred.User+":"+cred.Pass,
			endpoint,
		)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", endpoint, err))
			continue
		}
		value := strings.TrimSpace(string(out))
		parsed := net.ParseIP(value)
		if parsed == nil || parsed.To4() == nil {
			failures = append(failures, fmt.Sprintf("%s: 返回的不是 IPv4", endpoint))
			continue
		}
		return value, nil
	}
	return "", fmt.Errorf("SOCKS5 出口探测失败: %s", strings.Join(failures, "; "))
}
