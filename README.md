# vartio-plugin-chrome-devtools

Vartio 插件：发现、启动并经探针隧道接管远端机器上的 Chrome / Edge / Chromium / 比特浏览器窗口。

插件名是 `cdp`。Hub 从本仓库下载源码，在本机编译成 `vartio-plugin` 后启动。不要把它编进 Hub。

```bash
go test ./...
go build -o vartio-plugin ./cmd/vartio-plugin
```
