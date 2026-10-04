package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestXUICloneRollsBackWhenBindFails(t *testing.T) {
	var added, deleted bool
	var created map[string]any

	writeEnvelope := func(w http.ResponseWriter, success bool, msg string, obj any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": success,
			"msg":     msg,
			"obj":     obj,
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/panel/api/inbounds/list":
			list := []map[string]any{{
				"id": 1, "port": 10001, "protocol": "vless", "remark": "template",
				"enable": true, "tag": "in-10001-tcp", "listen": "",
				"settings":       map[string]any{"clients": []any{}},
				"streamSettings": map[string]any{"network": "tcp"},
				"sniffing":       map[string]any{"enabled": true},
			}}
			if added {
				list = append(list, created)
			}
			writeEnvelope(w, true, "", list)
		case r.Method == http.MethodPost && r.URL.Path == "/panel/api/inbounds/add":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode add payload: %v", err)
				writeEnvelope(w, false, err.Error(), nil)
				return
			}
			created["id"] = float64(2)
			created["tag"] = fmt.Sprintf("in-%d-tcp", int(toFloat(created["port"])))
			created["settings"] = map[string]any{"clients": []any{}}
			created["streamSettings"] = map[string]any{"network": "tcp"}
			created["sniffing"] = map[string]any{"enabled": true}
			added = true
			writeEnvelope(w, true, "", map[string]any{"id": 2})
		case r.Method == http.MethodPost && r.URL.Path == "/panel/api/inbounds/del/2":
			deleted = true
			added = false
			writeEnvelope(w, true, "", nil)
		case r.Method == http.MethodPost && r.URL.Path == "/panel/api/xray/":
			if added {
				writeEnvelope(w, false, "forced bind failure", nil)
				return
			}
			setting, _ := json.Marshal(map[string]any{
				"outbounds": []any{},
				"routing":   map[string]any{"rules": []any{}},
			})
			inner, _ := json.Marshal(map[string]any{
				"outboundTestUrl": "",
				"xraySetting":     json.RawMessage(setting),
			})
			writeEnvelope(w, true, "", string(inner))
		case r.Method == http.MethodPost && (r.URL.Path == "/panel/api/xray/update" || r.URL.Path == "/panel/api/server/restartXrayService"):
			writeEnvelope(w, true, "", nil)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	x := &XUI{Host: u.Hostname(), Port: port, Scheme: u.Scheme, client: server.Client()}
	tunnel := &Tunnel{Status: "up", Node: Node{HostName: "jp1", CountryCode: "JP"}}
	if _, err := x.CloneToTunnels(1, []string{"jp1"}, []*Tunnel{tunnel}); err == nil || !strings.Contains(err.Error(), "forced bind failure") {
		t.Fatalf("expected bind failure, got %v", err)
	}
	if !deleted || added {
		t.Fatalf("partial clone was not rolled back: added=%v deleted=%v", added, deleted)
	}
}

// 换节点改名：只动 fanout 自己起的名字。
//
// 旧实现按 "-" 切段替换末两段，既会切坏用户自己的备注，
// 又会在"拿复制品当模板再复制"时叠成 fanout-JP-243-VN-165 这种。
func TestRenameExitLabelOnlyTouchesGenerated(t *testing.T) {
	cases := []struct{ remark, label, want string }{
		// fanout 起的名字：整体换掉
		{"🇰🇷 韩国 248", "🇯🇵 日本 132", "🇯🇵 日本 132"},
		{"🇰🇷 韩国 248 2", "🇯🇵 日本 132", "🇯🇵 日本 132"},
		// 用户自己起的：一律不碰，包括旧版本留下来的那些
		{"线路A", "🇯🇵 日本 132", "线路A"},
		{"线路A-KR-248", "🇯🇵 日本 132", "线路A-KR-248"},
		{"fanout-JP-243-VN-165", "🇯🇵 日本 132", "fanout-JP-243-VN-165"},
		{"无格式", "🇯🇵 日本 132", "无格式"},
		{"", "🇯🇵 日本 132", ""},
	}
	for _, c := range cases {
		got := renameExitLabel(c.remark, c.label)
		if got != c.want {
			t.Errorf("renameExitLabel(%q) = %q, want %q", c.remark, got, c.want)
		}
	}
}

func TestResolvedInboundTagPrefersAPITag(t *testing.T) {
	stream := json.RawMessage(`{"network":"ws"}`)
	got := resolvedInboundTag("in-12080-tcp", 12080, stream)
	if got != "in-12080-tcp" {
		t.Fatalf("resolvedInboundTag() = %q, want API tag %q", got, "in-12080-tcp")
	}
}

func TestResolvedInboundTagFallsBackForLegacyAPI(t *testing.T) {
	stream := json.RawMessage(`{"network":"ws"}`)
	got := resolvedInboundTag("", 12080, stream)
	if got != "in-12080-ws" {
		t.Fatalf("resolvedInboundTag() = %q, want reconstructed tag %q", got, "in-12080-ws")
	}
}

// 面板没开 SSL 时会打印 "Warning: Panel is not secure with SSL"，
// 它包含 "Panel is secure with SSL" 这个子串，曾被误判成 https（issue #8）。
func TestXUISSLFromSettings(t *testing.T) {
	const off = `current panel settings as follows:
Warning: Panel is not secure with SSL
hasDefaultCredential: false
port: 37285
webBasePath: /abc123/
`
	const on = `current panel settings as follows:
Panel is secure with SSL
port: 2053
webBasePath: /xyz/
`
	const silent = `current panel settings as follows:
port: 2053
webBasePath: /xyz/
`
	cases := []struct {
		name       string
		text       string
		wantOn     bool
		wantStated bool
	}{
		{"未启用 SSL", off, false, true},
		{"已启用 SSL", on, true, true},
		{"没提 SSL", silent, false, false},
	}
	for _, c := range cases {
		on, stated := xuiSSLFromSettings(c.text)
		if on != c.wantOn || stated != c.wantStated {
			t.Errorf("%s: xuiSSLFromSettings() = (%v,%v), want (%v,%v)",
				c.name, on, stated, c.wantOn, c.wantStated)
		}
	}
}

// cert 为空时正则里的 \s* 会跨行，把下一行的 "key:" 当成证书路径（issue #8）。
func TestXUICertConfigured(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"证书为空", "cert:\nkey:\n", false},
		{"证书为空带空格", "cert: \nkey: \n", false},
		{"证书已配置", "cert: /root/cert.crt\nkey: /root/private.key\n", true},
	}
	for _, c := range cases {
		if got := xuiCertConfigured(c.text); got != c.want {
			t.Errorf("%s: xuiCertConfigured() = %v, want %v", c.name, got, c.want)
		}
	}
}

// 端口/路径的取值同样不能跨行。
func TestXUISettingFieldsStayOnOwnLine(t *testing.T) {
	const text = `current panel settings as follows:
Warning: Panel is not secure with SSL
hasDefaultCredential: false
port: 37285
webBasePath: /abc123/
`
	pm := reXUIPort.FindStringSubmatch(text)
	if pm == nil || pm[1] != "37285" {
		t.Fatalf("reXUIPort 解析失败: %v", pm)
	}
	bm := reXUIBase.FindStringSubmatch(text)
	if bm == nil || bm[1] != "/abc123/" {
		t.Fatalf("reXUIBase 解析失败: %v", bm)
	}
	// 值为空时宁可解析不出（调用方会报错），也不能把下一行当成路径
	if m := reXUIBase.FindStringSubmatch("webBasePath:\nport: 1\n"); m != nil {
		t.Fatalf("webBasePath 为空时不应吃掉下一行: %v", m)
	}
}

// vmess 的分享链接是 base64 编码的 JSON，按 ":端口?" 匹配一条也筛不出来。
func TestLinkForPortHandlesVMess(t *testing.T) {
	conf := map[string]any{
		"v": "2", "ps": "t-ws", "add": "localhost", "port": 40978,
		"id": "06a30cf8-2c06-4aeb-82fe-b4c7f1ab0159", "net": "ws", "path": "/abc",
	}
	blob, _ := json.Marshal(conf)
	link := "vmess://" + base64.StdEncoding.EncodeToString(blob)

	fixed, ok := linkForPort(link, 40978, "1.2.3.4")
	if !ok {
		t.Fatal("端口对得上却被筛掉了")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fixed, "vmess://"))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got["add"] != "1.2.3.4" {
		t.Errorf("add = %v, want 1.2.3.4", got["add"])
	}
	if int(toFloat(got["port"])) != 40978 {
		t.Errorf("端口被改坏了: %v", got["port"])
	}

	if _, ok := linkForPort(link, 12345, "1.2.3.4"); ok {
		t.Error("端口对不上时不该返回")
	}
}

func TestLinkForPortHandlesURIStyle(t *testing.T) {
	link := "vless://uuid@localhost:26387?security=none&type=tcp#t-tcp"
	fixed, ok := linkForPort(link, 26387, "1.2.3.4")
	if !ok {
		t.Fatal("端口对得上却被筛掉了")
	}
	if !strings.Contains(fixed, "@1.2.3.4:26387") {
		t.Errorf("地址没换: %s", fixed)
	}
	if _, ok := linkForPort(link, 99999, "1.2.3.4"); ok {
		t.Error("端口对不上时不该返回")
	}
}
