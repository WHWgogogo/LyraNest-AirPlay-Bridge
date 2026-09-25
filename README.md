# LyraNest AirPlay Bridge（AirPlay 2 无线投送桥接服务）

<p align="center">
  <img src="https://raw.githubusercontent.com/WHWgogogo/LyraNest/main/docs/images/lyranest-logo.png" alt="LyraNest AirPlay Bridge" width="150" />
</p>

<p align="center">为 LyraNest 音乐服务提供 Apple AirPlay 2 无线音频推流、HomePod / Apple TV 投送与全端播控同步的官方插件。</p>

<p align="center">
  <a href="https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest"><img src="https://img.shields.io/github/v/release/WHWgogogo/LyraNest-AirPlay-Bridge?display_name=tag&label=Release" alt="Latest Release" /></a>
  <a href="https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest"><img src="https://img.shields.io/badge/Platform-fnOS%20%7C%20Synology%20%7C%20QNAP%20%7C%20TerraMaster%20%7C%20UGNAS%20%7C%20CWNAS%20%7C%20Docker-4f46e5" alt="Platforms" /></a>
  <a href="https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest/download/docker-compose.yml"><img src="https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker&logoColor=white" alt="Docker Compose" /></a>
</p>

<p align="center">
  <a href="https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest">下载最新版</a> ·
  <a href="https://lyranest.cc.cd/">官网</a> ·
  <a href="#docker-compose-部署">Docker 部署</a> ·
  <a href="https://github.com/WHWgogogo/LyraNest">LyraNest 主项目</a>
</p>

> **插件说明**：本插件为 LyraNest 官方投播生态组件，运行于 NAS 或家庭服务器中，负责监听局域网 mDNS 组播自动发现 AirPlay 设备，并执行 RAOP / RTSP / RTP 实时推流。使用前请先部署 [LyraNest 主服务](https://github.com/WHWgogogo/LyraNest)。

当前稳定版本：`0.1.0`

交流 QQ 群：`700454910`

---

## 0.1.0 首发特性

- **完整 AirPlay 2 / RAOP 协议栈**：原生支持将 LyraNest 私有曲库音频无线投送至苹果 HomePod、Apple TV、Sonos 及各类第三方 AirPlay 兼容音箱。
- **CD 级高保真原音传输**：基于 16-bit / 44.1kHz PCM 无损音频管道与 FFmpeg 实时转码，保留真实细节。
- **局域网 mDNS 自动嗅探与保活**：基于零配置网络协议自动捕获局域网在线 AirPlay 音响，毫秒级探测上下线状态。
- **全端投送中心无缝聚合**：与 LyraNest 网页端、桌面端、移动端及 TV 端统一联动，支持全端独立音量调节与进度寻道（Seek Offset）。
- **全平台 NAS 原生安装支持**：全面覆盖飞牛 fnOS、群晖 DSM、铁威马 TOS 7、威联通 QNAP、绿联 UGnas 及畅网 CWNAS。

---

## 功能简介

- **免配即用**：启动服务后，同一局域网下的 Apple HomePod 与 AirPlay 音箱会自动出现在 LyraNest 客户端的「投送中心」设备列表中。
- **精准时钟同步**：采用基于 NTP 的纳秒级时钟对齐机制，彻底避免无线推流过程中的音频断续与丢帧。
- **状态全端双向同步**：在音箱本体或 iOS 控制中心调节音量，LyraNest 客户端同步联动；在客户端拖动进度条，音箱秒级响应。
- **超轻量运行**：Go 语言底层原生编写，内存开销仅数十兆，长期后台运行平稳可靠。

---

## 获取安装包与部署文件

请前往 [GitHub 最新发行版](https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest) 下载对应平台的文件：

| 文件 | 适用平台 / 架构 | 说明 |
| :--- | :--- | :--- |
| `LyraNest-AirPlay-Bridge-0.1.0-fnos-x86.fpk` | 飞牛 fnOS (x86_64) | 飞牛 NAS x86 原生安装包（推荐） |
| `LyraNest-AirPlay-Bridge-0.1.0-fnos-arm.fpk` | 飞牛 fnOS (ARM64) | 飞牛 NAS ARM 原生安装包 |
| `LyraNest-AirPlay-Bridge-0.1.0-cwnas.cpk` | 畅网 NAS (CWNAS / AINAS) | 畅网私有云原生应用安装包 |
| `LyraNest-AirPlay-Bridge-0.1.0-synology-x86_64.spk` | 群晖 DSM (x86_64) | 群晖 DSM 7.x 原生套件 |
| `LyraNest-AirPlay-Bridge-0.1.0-synology-armv8.spk` | 群晖 DSM (ARM64) | 群晖 DSM 7.x 原生套件 |
| `LyraNest-AirPlay-Bridge-0.1.0-terramaster-x86_64.deb` | 铁威马 TOS 7 (x86_64) | 铁威马应用中心原生安装包 |
| `LyraNest-AirPlay-Bridge-0.1.0-terramaster-aarch64.deb`| 铁威马 TOS 7 (ARM64) | 铁威马应用中心原生安装包 |
| `LyraNest-AirPlay-Bridge-0.1.0-qnap-x86_64.qpkg` | 威联通 QNAP (x86_64) | 威联通 App Center 原生套件 |
| `LyraNest-AirPlay-Bridge-0.1.0-qnap-arm_64.qpkg` | 威联通 QNAP (ARM64) | 威联通 App Center 原生套件 |
| `LyraNest-AirPlay-Bridge-0.1.0-ugnas-amd64.upk` | 绿联 NAS (AMD64) | 绿联私有云原生应用包 |
| `LyraNest-AirPlay-Bridge-0.1.0-ugnas-arm64.upk` | 绿联 NAS (ARM64) | 绿联私有云原生应用包 |
| `docker-compose.yml` | 通用 Docker 环境 | Docker Compose 一键部署配置 |

---

## 飞牛 fnOS 原生 FPK 安装（推荐）

1. 从 [GitHub 最新发行版](https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest) 下载与飞牛设备架构一致的 FPK 安装包（x86 选 `fnos-x86.fpk`，ARM 选 `fnos-arm.fpk`）。
2. 在飞牛应用中心选择“手动安装 / 上传应用”，上传 FPK 完成安装。
3. 安装启动后，服务将在后台常驻并监听 `8092` 端口；在 LyraNest 主程序后台「插件设置」或「投送中心」中开启 AirPlay 通道即可。

---

## 畅网 NAS (CWNAS / AINAS) 原生 CPK 安装

畅网 NAS 用户可下载 `LyraNest-AirPlay-Bridge-0.1.0-cwnas.cpk`。在畅网 NAS 系统应用管理器中点击“手动安装 / 本地安装”，选择下载的 `.cpk` 文件即可一键部署并自动注册后台服务。

---

## 其他 NAS 原生安装包

QNAP、Synology DSM、绿联 NAS 与铁威马 TOS 7 用户可从 [GitHub 最新发行版](https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest) 下载对应架构的原生包，在各自系统的应用中心或套件中心选择手动安装：

- **威联通 QNAP**：在 App Center 中启用“允许安装非 QNAP 签名程序”，手动上传 `.qpkg`。
- **群晖 DSM**：打开套件中心点击“手动安装”，上传 `.spk` 安装。
- **绿联 NAS**：在应用中心选择“本地安装”，上传对应架构的 `.upk`。
- **铁威马 TOS 7**：在应用中心选择“手动安装”，上传对应架构的 `.deb`。

---

## Docker 镜像

服务端镜像统一发布至 GitHub Container Registry：

```text
ghcr.io/whwgogogo/lyranest-airplay-bridge:0.1.0
```

支持自动多架构自适应（`linux/amd64` 与 `linux/arm64`），默认提供 `0.1.0` 与 `latest` 标签。

---

## Docker Compose 部署

根目录提供了标准的 [`docker-compose.yml`](docker-compose.yml)：

> [!IMPORTANT]
> **网络模式说明**：本容器**必须使用 `network_mode: host`**！
> AirPlay 协议不是纯 TCP 通信，需要接收局域网组播（`224.0.0.251:5353`）发现音箱，且 HomePod 音箱推流期间会随机回拨主机的 UDP 端口对齐时钟。Docker 默认的 Bridge 网络会丢弃这些数据报导致设备发现失败或播放卡顿。

```yaml
services:
  airplay-bridge:
    image: ghcr.io/whwgogogo/lyranest-airplay-bridge:0.1.0
    container_name: lyranest-airplay-bridge
    restart: unless-stopped
    network_mode: host
    environment:
      BRIDGE_PORT: "8092"
      BRIDGE_BIND: "0.0.0.0"
      BRIDGE_TOKEN: "${AIRPLAY_BRIDGE_TOKEN:-}"
      BRIDGE_ENGINE: "auto"
      CLIAIRPLAY_PATH: "cliairplay"
      FFMPEG_PATH: "ffmpeg"
      BRIDGE_SAMPLE_RATE: "44100"
      BRIDGE_CHANNELS: "2"
      BRIDGE_BIT_DEPTH: "16"
      BRIDGE_LATENCY_MS: "600"
      BRIDGE_DEVICE_TTL_SECONDS: "60"
      BRIDGE_DISCOVERY: "1"
      LYRANEST_SERVER_URL: "${LYRANEST_SERVER_URL:-http://127.0.0.1:8080}"
      BRIDGE_LOG_LEVEL: "info"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "-", "http://127.0.0.1:8092/healthz"]
      interval: 30s
      timeout: 5s
      start_period: 10s
      retries: 3
```

### 部署与启动命令

```bash
mkdir -p lyranest-airplay-bridge && cd lyranest-airplay-bridge
curl -fLO https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge/releases/latest/download/docker-compose.yml
docker compose pull
docker compose up -d
curl http://127.0.0.1:8092/healthz
```

---

## 联动与使用说明

1. 启动 AirPlay Bridge 服务后，确认当前服务器与音箱处于同一局域网（同一二层网络，未开启 AP 隔离）。
2. 打开 LyraNest Web 端或移动客户端，进入播放界面点击「投送中心」图标。
3. 在设备列表中即可看到局域网内的全部 AirPlay 2 设备（例如：客厅 HomePod、书房 Apple TV 等）。
4. 点击设备名称即可秒级连接投送，全端支持拖拽进度条与音量滑块调节。

---

## 官方生态与项目友链

- [LyraNest 主项目](https://github.com/WHWgogogo/LyraNest)：LyraNest 官方全平台自托管音乐服务。
- [LyraNest Local Output](https://github.com/WHWgogogo/LyraNest-Local-Output)：NAS 3.5mm 耳机孔与 USB DAC 声卡直出插件。
- [LyraNest Xiaomi Bridge](https://github.com/WHWgogogo/LyraNest-Xiaomi-Bridge)：小爱音箱语音联动与投送桥接插件。
- [LyraNest Community](https://github.com/WHWgogogo/LyraNest-Community)：开源社区版。
