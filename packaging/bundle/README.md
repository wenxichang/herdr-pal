# Herdr Bundle @BUNDLE_VERSION@

目标平台：`@BUNDLE_TARGET@`

Herdr 源码提交：`@HERDR_COMMIT@`

本目录同时包含 Herdr 和 Herdr Pal。请在当前目录执行：

```sh
./install.sh
```

安装器会引导你选择安装目录，并输入 HPRP Server URL 与机器 Key。默认安装目录是
`~/.local/bin`，已有二进制和配置会先备份。安装完成后，Herdr 的 Startup 插件会调用
`herdr-pal start`；Pal 自己守护业务进程，并在 Herdr 公共服务持续停止后退出。

机器 Key 由管理员按用户和机器签发；用户只需在安装时填写 Server URL 与机器 Key。

安装完成后启动 Herdr，并在企业微信中执行 `/ls` 验证连接。
