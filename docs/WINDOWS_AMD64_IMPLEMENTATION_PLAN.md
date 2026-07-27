# Windows AMD64 支持实施计划

**目标：** 让 `herdr-pal-windows-amd64.exe` 能通过 Named Pipe 连接 Windows Herdr Beta。

**架构：** 使用构建标签提供 Unix/Windows 本地拨号实现，Herdr 协议层只依赖统一 endpoint
拨号接口。平台路径探测和 Unix 长路径别名同样隔离，避免平台判断扩散到 Bridge 业务层。

## 任务

1. 为平台 endpoint 映射和拨号接口编写失败测试，验证 Windows marker 路径映射及现有测试
   拨号器兼容性。
2. 引入 Windows Named Pipe 拨号实现，并让普通请求与事件订阅共享该入口。
3. 为平台默认 Socket 推导编写失败测试，拆分 Unix 与 Windows HOME/APPDATA 探测。
4. 为 Windows 直通稳定拨号路径编写失败测试，隔离 Unix `/tmp` 短别名逻辑。
5. 扩展构建脚本测试和 `build.sh`，生成 Windows AMD64 客户端与校验和。
6. 更新 README 和交接文档中的平台、产物及 Windows 启动说明。
7. 运行 `./unittest.sh`、`go test -race ./...`、`./build.sh` 和 Windows 交叉编译检查。
