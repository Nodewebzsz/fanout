# fanout

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把 VPN Gate 的公共节点变成本地 SOCKS5 端口：一个端口一个出口 IP。
再给每个出口挂一个节点链接，客户端连哪个端口就从哪个国家出去。

节点链接有三种管法：同机装了 3x-ui 或 xray-cf-lite 就接管它们的入站，
都没装则 fanout 自己跑 Xray，建站、改站、发链接都在同一个界面里完成。

![主界面](https://images.joeyblog.net/2026/7/27/fanout-dashboard.png)

四条隧道跑在一台机器上，四个端口对应四个国家的出口，母机自己的 IP 不受影响：

![出口验证](https://images.joeyblog.net/2026/7/26/fanout-6-exit-ip.png)

## 原理

每个节点跑在独立的 network namespace 里，netns 内启动官方 openvpn 客户端。
SOCKS5 监听在母机，出站连接用 `setns` 切进对应 netns 建立。

这样做的好处：VPN 的路由劫持只影响自己的 netns，不会切断母机的网络；
多个节点互不干扰，各自一个出口 IP。

```
客户端 ──> 母机 SOCKS5 :随机端口 ──> netns foN ──> openvpn ──> VPN Gate 节点
```

## 安装

需要 root，Linux（依赖 netns）。

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Nodewebzsz/fanout/main/install.sh)
```

会自动下载对应架构的预编译二进制。也可以 clone 仓库后在源码目录运行同一个脚本，
那样会从源码编译（需要 Go 1.24+）。

依赖（openvpn / curl / openssl / iproute / iptables）会按发行版自动装，
apt、dnf、yum、pacman、apk、zypper 都认。没装 3x-ui 时还会顺带下载一份
Xray 到 `/var/lib/fanout/bin/`，装了则跳过，入站交给面板管。

服务用 systemd 或 OpenRC 都能装，装完自动开机自启。

**Alpine** 默认不带 bash，先装一下：

```bash
apk add bash curl
bash <(curl -fsSL https://raw.githubusercontent.com/Nodewebzsz/fanout/main/install.sh)
```

另外 fanout 要在 netns 里跑 openvpn，**宿主必须放开 `/dev/net/tun`**。
不少 LXC 小鸡没给这个权限，`ls /dev/net/tun` 不存在且 `mknod` 报
Operation not permitted 的话，这台机器用不了，跟发行版无关。

装完敲 `f` 打开管理菜单：

![管理菜单](https://images.joeyblog.net/2026/7/26/fanout-7-menu.png)

装完会打印管理界面地址、访问路径和口令：

```
管理界面  http://<你的IP>:8899/gwPuWHvaNr/
访问口令  f81120ac328d11c11b
```

路径和口令都是随机生成的，分别存在 `/var/lib/fanout/basepath` 和
`/var/lib/fanout/password`。路径不对一律返回 404，扫端口的看不到这里跑着什么。

### 启动参数

直接运行二进制时可使用以下参数。安装脚本生成的服务默认只指定 `-dir`，端口和监听
地址优先从 `/var/lib/fanout/settings.json` 读取；Web 界面或 `f` 菜单修改后，重启仍会
沿用保存的设置。

```bash
fanout -dir /var/lib/fanout \
  -web 8899 \
  -max 20 \
  -ip 203.0.113.10 \
  -panel native
```

- `-dir`：运行数据目录，默认 `/var/lib/fanout`；不同目录可运行彼此隔离的实例。
- `-web`：首次启动时的管理端口，默认 `8899`；显式传入时覆盖已保存端口。
- `-max`：允许的最大隧道槽位，默认 `20`。
- `-ip`：分享节点链接时使用的公网 IPv4；不传则自动探测，也可用
  `FANOUT_PUBLIC_IP` 环境变量指定。
- `-panel`：固定使用 `3x-ui`、`native` 或 `xray-cf-lite`；不传则按设置或自动探测。
- `-version`：打印版本并退出。

## 使用

界面以**出口**为单位：一行就是一条隧道加上挂在它上面的节点链接。

点「新建出口」，先选一个已有节点作模板，再一次勾选多个国家并分别填写目标数量。
例如日本 3 个、韩国 2 个可以一单提交。国家列表按当前空闲节点数量排序，数量只是
参考：目标可以大于当前空闲数，fanout 会先创建实际验证可用的部分，剩余缺口留到
后续自动检测继续填补，不会在这次任务里一直等待。

提交的是持续生效的“出口保有目标”，不是只执行一次的批量命令。主界面的
「出口保有目标」区域会显示每个国家的健康、连接中、等待填补、缺口、超额数量，
以及最近和下次检查时间。点「立即检测并自动填补」可以随时手动执行完整检测。

![新建出口](https://images.joeyblog.net/2026/7/27/fanout-wizard.png)

每行右侧两个按钮：换一个节点（出口 IP 变、端口不变，已分发的客户端配置不用改），
或者停掉这个出口。换节点会避开这条出口之前用过的，连点几次每次都是新 IP。

自动保有出口故障时只会换成同国家节点，并优先在原槽位修复，SOCKS5 端口、用户名
密码、Xray 入站和客户端链接都不变。同国家暂时没有可用节点时显示“等待填补”，
本轮立即结束，等下次自动或手动检测再试，不会拿别的国家顶替。

把目标数量调小不会自动删除已经健康的多余出口，界面会显示“超出目标”，由你决定
停哪一条。直接停止一个仍在目标内的出口时界面会提示：目标数量不变，系统还会补齐。

出口和节点的名字自动起成 `🇯🇵 日本 243`（国旗 + 国家 + 出口 IP 末段）。
自己改过的名字不会被覆盖——换节点时只重写 fanout 自己起的那些。

点节点名进详情，可以改端口、备注、启停，管理客户端，以及改绑到别的出口：

![节点详情](https://images.joeyblog.net/2026/7/27/fanout-detail.png)

一个入站可以挂多套客户端凭据，分发给不同的人；每套都能单独重置，
重置后旧链接立即失效。

「导出链接」一次性拿到所有节点链接：

![导出链接](https://images.joeyblog.net/2026/7/27/fanout-export.png)

### 订阅

节点多了就别一条条复制了。点「订阅」拿一条地址，填进客户端的订阅里，
以后加出口、删出口都会自己跟上。换节点不影响它——端口不变，链接也不变。

地址形如 `http://<服务器IP>:<端口>/<访问路径>/sub?token=<口令>`。
默认出 base64，各家客户端通吃；想看明文在后面加 `&target=links`。
默认只放绑了出口的节点，走直连的不往里混；想全都要就加 `&bound=0`。

订阅要给客户端直接拉，所以这条地址不要登录，**那串 token 就等于密码**，别发群里。
泄露了点「换一串口令」，旧地址立刻失效。

### 只用家宽

VPN Gate 的清单里混着一批它自己的机房服务器（`public-vpn-*` 和
`219.100.37.0/24`），出口一眼看得出是数据中心，还更容易满员。
设置里「只用家宽节点」默认开着，挑节点、地区可用数、自动重连的备选都只看
志愿者家宽。想连机房的一起用就把它关掉。

开关只管新挑的节点，已经跑着的出口不会因为改设置被换掉。

### 节点链接从哪来

同机装了 3x-ui 就直接接管面板里的入站，面板端口、路径、API token 全自动探测，
开了 SSL 也能识别。没装 3x-ui 时 fanout 自己跑一个 Xray，界面上多一个「新建节点」
按钮，可以选协议（VLESS / VMess / Trojan）、传输（TCP / WebSocket / gRPC /
HTTPUpgrade / XHTTP）和安全层（无 / TLS / REALITY）。

![新建节点](https://images.joeyblog.net/2026/7/27/fanout-newnode.png)

REALITY 的密钥对和 shortId 自动生成；TLS 不填证书就生成自签的，分享链接会带上
证书指纹让客户端固定信任。也可以填自己的证书路径。

接管 3x-ui 和自建这两种模式下，改端口、启停、加删客户端、绑定出口的操作完全一致，
用起来没有区别。

装了 [xray-cf-lite](https://github.com/byJoey/xray-cf-lite) 的机器会自动接管它生成的
三个节点。这个模式下节点归 xray-cf-lite 管，fanout 只负责给每个节点指定走哪条出口，
所以界面上不提供新建、删除和改节点的入口——想改端口或 UUID 去 xray-cf-lite 那边改。
两边共用同一份 Xray 配置，fanout 只往里加自己前缀的出站和分流规则，互不覆盖。
由于无法克隆和维护节点，xray-cf-lite 模式也不提供自动保有目标。

后端在设置面板里可以随时切换，本机没装的会置灰并说明原因；也可以用
`-panel 3x-ui` / `-panel native` / `-panel xray-cf-lite` 启动参数固定。
界面里选过之后会记住，重启仍然生效。

## 运维

装完后敲 `f` 打开管理菜单：启停、看日志、查隧道、改端口/口令/访问路径、更新、卸载。

```
  状态      运行中
  版本      fanout <当前版本>
  开机自启  enabled

  管理地址  http://1.2.3.4:8899/gwPuWHvaNr/
  访问口令  f81120ac328d11c11b

   1) 启动          2) 停止
   3) 重启          4) 查看日志
   5) 隧道列表      6) 连接信息
   7) 改端口        8) 改口令
   9) 改访问路径   10) 开机自启开关
  11) 更新         12) 卸载
```

也可以直接带参数用：

```bash
f info       # 连接信息
f list       # 隧道列表
f restart    # 重启
f log        # 跟踪日志
f update     # 更新到最新版
f uninstall  # 卸载
```

隧道状态存在 `/var/lib/fanout/state.json`，国家保有目标存在
`/var/lib/fanout/country_targets.json`，都采用原子写入；重启后目标和出口归属不会丢。
历史版本创建、没有 `target_id` 的出口继续按原方式运行，不会被自动目标接管或计数。

健康检查每 60 秒跑一次。检测请求会从真实的本地 SOCKS5 端口出发，带该出口自己的
用户名密码，完整经过 netns、OpenVPN，再访问公网 IP 检测服务；主检测地址失败时会
自动换备用地址。返回 IP 必须是合法 IPv4 且与建隧道时记录的一致。连续两次失败才
确认掉线，避免一次网络抖动触发换节点。

托管出口确认掉线后立即请求同国家原槽位修复。完整缺口协调会在服务启动时、每
15 分钟以及点击「立即检测并自动填补」时执行。失败候选冷却 15 分钟；一个国家候选
耗尽后本轮直接结束，其他国家继续处理。未托管的历史出口仍使用原来的自动重连逻辑。

### 国家目标文件与 API

国家目标保存在 `${WORK_DIR}/country_targets.json`，默认路径是
`/var/lib/fanout/country_targets.json`。文件由 fanout 原子写入，不建议手工编辑；迁移
或备份时可以和 `state.json` 一起保存。目标键由面板类型、模板入站 ID 和大写国家代码
组成，例如 `native:12:JP`。

管理界面使用以下接口：

- `GET /<访问路径>/api/targets`：读取所有目标及健康、连接中、等待填补、缺口和超额状态。
- `POST /<访问路径>/api/targets`：保存多个国家目标。请求体示例：
  `{"template_id":12,"targets":[{"country_code":"JP","target_count":3}]}`。
- `POST /<访问路径>/api/targets/reconcile`：立即触发一次全量检测和自动填补。

这些接口和其他管理接口一样需要登录会话。旧版本创建的出口没有 `target_id`，会继续
按未托管出口处理，不会被国家目标自动接管、计数或删除。

## 已知限制

- SOCKS5 支持 CONNECT 和 UDP ASSOCIATE，DNS/QUIC 这类 UDP 也走隧道
  （感谢 [@zsawi](https://github.com/zsawi) 的 [#22](https://github.com/byJoey/fanout/pull/22)）。
  域名仍在本机解析。
- VPN Gate 是志愿者节点，有相当比例已下线或满员（`AUTH_FAILED`）。
  未托管出口启动时连不上会自动顺着同地区候选往下试，最多 6 个；托管目标每轮只
  尝试当前可选候选，耗尽即等待下一轮，不会无限阻塞。
- 管理界面只有随机路径 + 口令登录，没有 HTTPS。放公网建议前面套一层反代。
  订阅地址同理，token 是明文走的。
- 换节点会避开这条出口之前用过的节点（最多记 16 个），同地区都换过一轮后
  从头开始。一个地区只有一个可用节点时换不动，界面会直接说明原因。
- 多国家目标只对能够克隆和维护入站的 `native`、`3x-ui` 后端生效；
  `xray-cf-lite` 只能绑定已有节点，不能创建或自动补齐目标。

## 许可

[MIT](LICENSE)。

节点来自 [VPN Gate](https://www.vpngate.net/)（筑波大学的学术实验项目），
本工具只是调用其公开的节点列表并用官方 openvpn 客户端连接，不修改也不代理其服务。
使用时请遵守 VPN Gate 的条款和你所在地的法律。

## 交流

- GitHub：<https://github.com/Nodewebzsz/fanout>
- 交流群：<https://t.me/+ft-zI76oovgwNmRh>
- 视频教程：<https://youtube.com/@joeyblog>
- 博客：<https://joeyblog.net>

用着有问题、或者想要什么功能，去群里说或提 issue。
