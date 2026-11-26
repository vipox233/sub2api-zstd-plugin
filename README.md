# OpenAI 上游图片优化与 Zstd 压缩

[![CI](https://img.shields.io/github/actions/workflow/status/vipox233/sub2api-zstd-plugin/ci.yml?branch=main&label=CI)](https://github.com/vipox233/sub2api-zstd-plugin/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/vipox233/sub2api-zstd-plugin?label=Release)](https://github.com/vipox233/sub2api-zstd-plugin/releases/latest)
[![License](https://img.shields.io/github/license/vipox233/sub2api-zstd-plugin?label=License)](LICENSE)

Sub2API 的 OpenAI OAuth HTTP 出站插件。它优化 Codex Responses 中的内联图片，再用 Zstd 压缩请求体。

## 适用范围与运行条件

- **宿主兼容**：Sub2API `>= 0.2.19 < 0.3.0`（实现 `openai.oauth.outbound_transport.v1`、插件协议 `v1` 与 UI Bridge `v1`）。
- **运行平台**：发行包提供 `linux/amd64` 静态二进制，要求 Sub2API 宿主运行在 Linux x86_64 环境（含常规 Linux 容器部署）。
- **处理范围**：仅作用于 OpenAI `oauth` 账号发往 `https://chatgpt.com/backend-api/codex/responses` 的 `POST` JSON 请求；API Key 账号、WebSocket 连接及其他路径请求原样透传。
- **签名信任**：首次安装前需在宿主 `config.yaml` 的 `plugins.trusted_publishers` 中登记发行方公钥（`vipox-zstd-20261104`）。

## 安装与升级

1. 从 [Releases](https://github.com/vipox233/sub2api-zstd-plugin/releases/latest) 下载最新的 `openai-upstream-zstd-<version>-linux-amd64.s2plugin`，在 Sub2API 插件管理上传。首次安装时按[安装说明](docs/guidebook/install.md)合并 `trusted-publisher.yaml` 中的公钥。
2. 读取已保存配置。新装时图片优化默认关闭。升级保留已保存的图片开关、大小上限和 JPEG 画质等设置。
3. 若旧配置仍保留 64 MiB 的换图上限，填 `0` 即可对更大的请求换图。开启图片优化后，发送带截图的 HTTP 对话，在面板「最近请求」里核对原始、换图后和最终上传体积。

## 图片设置

| 设置 | 默认值 |
|---|---|
| 普通图 | 1080p，等比缩入 1920 × 1080，竖图对调宽高 |
| 工具图 | 1440p，可改成 1080p、原尺寸或跟随普通图 |
| JPEG 画质 | 78，可设置 1–100 |
| 透明 PNG 底色 | 白色，可改成黑色 |
| 替换门槛 | JPEG 相对原图至少节省 8 KiB，填 0 时只要求更小 |

长短边比达到 3:1 的图片按短边上限等比缩小。显式 `detail: original` 使用原始像素尺寸并尝试重编码。静态 PNG 支持透明、16 位和 eXIf；带 MPF 或尾部数据的 JPEG 使用主图。动画 PNG、坏图、超限图和收益不足的图片使用原图。

同一原图在相同配置和插件版本下生成相同的 JPEG。缓存按图片内容和转换参数索引，历史、续轮和工具图共用。升级改变图片表示时，上游可能重新计算已有前缀。

## 上传与统计

默认 Zstd level 5、8 MiB 匹配窗口、512 MiB 换图后请求体上限。换图上限按原始请求体计算。较大的请求默认共享 4 个准备名额，上传前归还。原始和压缩后的请求体各自超过 8 MiB 时使用匿名临时文件，替换内容共享缓存中的字符串。

HTTP 415 用同一份换图后 JSON 明文重试一次，随后暂停 Zstd 2 分钟。面板按传输读取的请求体字节计量，并列出跳过原因、换图耗时及最近 30 条请求。其中两次上传计入对应的一条请求。

## 文档与开发

- 日常部署与参数细节见[安装与配置说明](docs/guidebook/install.md)，性能与回归数据见[基准与验证记录](docs/validation.md)，版本演进见[更新历史](docs/history.md)。
- 本地构建、运行测试、复用 CI 检查或参与贡献时，请阅读[开发与发布说明](docs/development.md)。反馈问题时可结合面板「最近请求」与跳过原因按 Issue 模板提供版本和复现条件。

## 许可与声明

本仓库新增代码按 [GNU LGPL v3.0](LICENSE)（补充条款关联 [GPL v3.0](LICENSE-GPL-3.0.txt)）发布；第三方依赖及 Go 标准库许可与来源说明见 [NOTICE.md](NOTICE.md) 和 [LICENSE-Go.txt](LICENSE-Go.txt)。本项目为个人维护的第三方传输插件，非 Sub2API 或 OpenAI 官方发行组件。
