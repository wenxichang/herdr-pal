# Herdr Pal Bundle @BUNDLE_VERSION@

目标平台：`@BUNDLE_TARGET@`

本安装包只携带 Herdr Pal。请在当前目录执行：

```sh
./install.sh
```

安装器会引导你选择安装目录，并输入 HPRP Server URL 与机器 Key。默认安装目录是
`~/.local/bin`，已有 Pal 二进制和配置会先备份。

安装器会检查本机 Herdr：兼容版本直接复用；缺失时自动下载已验证的 Herdr
`@HERDR_VERSION@`；发现不兼容版本时先询问是否安装兼容版本。Homebrew、mise、Nix 或
其他目录中的 Herdr 不会被覆盖。

安装完成后，Herdr 的 Startup 插件会调用 `herdr-pal start`；Pal 自己守护业务进程，并在
Herdr 公共服务持续停止后退出。

机器 Key 由管理员按用户和机器签发；用户只需在安装时填写 Server URL 与机器 Key。

安装完成后启动 Herdr，并在企业微信中执行 `/ls` 验证连接。
