# 安装 OpenAI 上游图片优化与 Zstd 压缩

## 从 v1.1.0 升级

1. 在 Sub2API 后台的插件管理上传 `openai-upstream-zstd-1.2.1-linux-amd64.s2plugin`。两版共享插件 ID `cn.vipox.openai-upstream-zstd` 和发行方 `vipox-zstd-20261104`。
2. 读取当前配置。图片优化默认关闭，现有 zstd、代理、请求体上限和超时配置可直接读取。
3. 开启图片优化并保存。新建 HTTP 对话，发送一张几 MiB 的 PNG 截图，再在同一会话续轮。观察图片统计和首字延迟。

## 首次安装

从构建产物 `trusted-publisher.yaml` 取出对应公钥，合并到宿主 `config.yaml` 的 `plugins.trusted_publishers`。宿主读取配置后即可上传并启用 Linux amd64 签名包。0.2.23 处于清单允许范围，启用时宿主可能要求接受未实测的宿主版本。

## 日常使用

- 首选 JPEG 92、长边 0。等比缩边可试 2560 px，适合普通大截图。
- 图片处理针对最终 Codex OAuth 的 HTTP 请求。zstd 对纯文本同样生效。5 Mbps 出口可保持 zstd 并发 2，最大压缩体默认 512 MiB，填 0 可解除该项上限。
- 图片异常或超限时使用原图继续请求。临时目录需要可写空间。大请求会使用匿名文件准备出站包体。
- 想暂停新功能时在配置页关闭图片优化，继续使用本版的 zstd。

参见[本地验证和实测范围](../validation.md)。
