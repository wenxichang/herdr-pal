# Herdr Bundle @BUNDLE_VERSION@

目标平台：`@BUNDLE_TARGET@`

Herdr 源码提交：`@HERDR_COMMIT@`

本目录同时包含 Herdr 和 Herdr Pal。请在当前目录执行：

```sh
./install.sh
```

安装器会引导你选择安装目录，并输入 HPRP Server URL 与机器 Key。默认安装目录是
`~/.local/bin`，已有二进制和配置会先备份。安装器只生成配置，不安装系统用户服务。
官方 Herdr 不支持 `[[sidecar]]`，旧安装器标记的 Pal sidecar 块会自动移除；未受管的
sidecar 配置需要自行迁移。

机器 Key 由管理员按用户和机器签发；用户只需在安装时填写 Server URL 与机器 Key。

安装完成后先启动 Herdr，再在另一个终端执行安装器打印的 Pal 命令，并保持该终端运行。
默认安装路径的命令是 `~/.local/bin/herdr-pal -config ~/.config/herdr-pal/config.json`。
`live-handoff` 不会启动 Pal。最后在企业微信中执行 `/ls` 验证连接。

需要连接 DeepSeek Harness 时，下载同一 Release 中的 `dsh-herdr-pal-*.tgz`，按
[dsh 插件说明](https://github.com/wenxichang/herdr-pal/blob/main/plugins/dsh-herdr-pal/README.md)
安装和配置。插件支持内网离线导入；可同时接入 Herdr 与 dsh，也可在 Pal 配置中设置
`herdr.disabled=true` 只连接 dsh。
