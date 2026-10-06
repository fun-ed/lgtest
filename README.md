# lgtest

使用 Psychz 與 Vultr 公開 Looking Glass 檔案的 Go 下載測速 CLI。支援互動選擇地區與檔案，以及 direct、HTTP proxy、SOCKS5 路由。下載內容寫入 `io.Discard`，結果可保存為 JSON。

本工具不是 proxy server，不會修改 GOST、VPS、防火牆或 MTU。

## 編譯與測試

需要 Go 1.27.1。`mise.toml` 固定同一版本；使用 mise 時依其信任流程啟用專案配置。

```bash
git clone https://github.com/fun-ed/lgtest.git
cd lgtest
go test ./...
go test -race ./...
go vet ./...
go build -o lgtest ./cmd/lgtest
./lgtest help
```

uTLS v1.8.2 提供 Firefox ClientHello，x/net v0.59.0 提供 HTTP/2 與 HTML 解析。HTTP CONNECT 與無認證 SOCKS5 negotiation 由 transport 實作。依賴版本與校驗保存在 `go.mod`、`go.sum`。

## 地區與互動選擇

```bash
./lgtest list
./lgtest list --provider psychz
./lgtest list --provider vultr
./lgtest run --interactive
```

在終端機執行 `./lgtest run` 也會進入互動模式，依序選 region、檔案與路由。只有一個檔案時自動選取。非互動模式必須提供 `--region`；多檔地區另需指定 `--file`。

預設目錄內嵌於執行檔，來源在 `internal/catalog/snapshot.json`。2026-10-06 的 Camofox 快照列出 Psychz 17 個 location 與 Vultr 33 個 region。Psychz 的 16 個 location 與 Vultr 全部 33 個 region 已取得公開 IPv4、IPv6 與檔案連結；Psychz Singapore 連線逾時，保留 error，未推測 IP 或檔案。

## 更新目錄

```bash
mkdir -p catalog
./lgtest refresh --output catalog/locations-new.json
./lgtest list --catalog catalog/locations-new.json

# 若直連來源頁面受阻，可經既有 HTTP proxy 更新
./lgtest refresh --proxy http://127.0.0.1:8080 \
  --output catalog/locations-via-proxy.json
```

`refresh` 從兩個 seed 的地區選單發現 locations，再讀取各地區頁面，不下載測速檔；最多四個頁面並行，每頁有 timeout 與大小限制。執行期不需要 Camofox 常駐。

輸出不覆寫已有檔案。部分頁面失敗時保存 partial catalog 並回傳非零結束碼；seed 失敗則不能宣稱完整發現。以 `--catalog` 指定新目錄，不需重新編譯。

## 下載測速

```bash
mkdir -p results

# Direct 忽略 HTTP_PROXY、HTTPS_PROXY 等環境代理
./lgtest run --region psychz:tw --file 200MB --mode direct \
  --output results/direct-new.json

# HTTP proxy，六次完整下載
./lgtest run --region psychz:tw --file 200MB --mode http \
  --proxy http://127.0.0.1:8080 --runs 6 \
  --output results/http-six-new.json

# SOCKS5h 的 auto family 可交由 proxy 解析目的 hostname
./lgtest run --region vultr:hnd-jp-ping --file 100MB --mode socks5 \
  --proxy socks5h://127.0.0.1:1080 \
  --output results/socks5-new.json

# 短驗證，最多讀 1MiB，結果標示 limited
./lgtest run --region psychz:tw --file 200MB --mode http \
  --max-bytes 1048576 --output results/partial-new.json
```

`--mode http` 預設使用 `http://127.0.0.1:8080`；`--mode socks5` 預設使用 `socks5h://127.0.0.1:1080`。Proxy URL 不接受 userinfo，本工具不支援含帳密的 proxy 入口。不要在命令、URL 或結果檔放入憑證。

預設每次完整下載所選檔案。Psychz 常見為 200MB，Vultr 提供 100MB 與 1GB；label 不當成精確 bytes，完整性依實際 Content-Length 與讀取結果檢查。多次測試可能產生大量流量，請先確認檔案大小與 `--runs`。

## IPv4 與 IPv6

```bash
./lgtest run --region psychz:tw --file 200MB --mode direct --family 4
./lgtest run --region psychz:tw --file 200MB --mode socks5 --family 6
```

Family 指的是目的 test server，不是本機到 proxy 的 socket 家族，也不控制 GOST 到 VPS 的家族。

- `auto` 使用檔案 hostname，實際選擇依 resolver 或 proxy。
- `4`、`6` 使用該地區頁面廣告的 test IP，保留原 URL Host、TLS SNI 與憑證驗證。CONNECT 或 SOCKS5 目的地址也使用此 IP。
- 沒有 advertised IPv6 的 region 會拒絕 family 6，不捏造地址。
- Advertised IP 不保證同時提供檔案網站；失敗不會 fallback 到另一家族並宣稱成功。

初次 live 驗證時，Psychz Taipei 的 IPv6 經 HTTP proxy 收到 CONNECT 503。這只證明該次路徑失敗，不能判定所有 IPv6 不可用，也不能判定是上游路由或目的網站的問題。

## Firefox profile 與限制

預設 `--profile firefox` 使用 Firefox 120 風格的 User-Agent、Accept-Language、Fetch headers，以及 uTLS `HelloFirefox_120` ClientHello。uTLS v1.8.2 的 `HelloFirefox_Auto` 仍對應 Firefox 120，不是 2026 年最新 Firefox。

- 保留 profile 的 ALPN；協商 h2 時以 HTTP/2 傳輸，否則使用 HTTP/1.1。
- 正常驗證憑證與 hostname，不提供 insecure bypass。
- 使用 `Accept-Encoding: identity` 量測檔案 bytes，不宣稱 headers 與真瀏覽器完全一致。
- HTTP/2 SETTINGS、HPACK、header 順序與完整行為不是 Firefox stack，不保證相同 JA4 或不被 DPI 偵測。
- 此 profile 只影響工具到測試網站的 TLS；若經 GOST，不控制 GOST 到 VPS 的外層指紋。
- `--profile go` 使用 native Go TLS，供對照。

每次 request 建立新連線，適合冷連線、單檔測速，不提供跨次連線復用或 HTTP/3。結果不能直接證明 GOST 多路復用效能、抗 DPI 能力或長連線穩定性。

## 結果與驗證範圍

結果包含 HTTP status、目的網站 HTTP version、bytes、Content-Length、收到 headers 的時間、實際 first body byte、總耗時與下載期速度。平均 Mbps 為 bytes × 8 / 1e6 / 總耗時；transfer Mbps 扣除取得 headers 前的等待。摘要只對成功下載計算平均、中位數、最快、最慢與 aggregate Mbps，並列出失敗次數。

非 200、HTML 錯誤頁、異常 Content-Encoding、空檔與截斷 Content-Length 都算失敗。結果 JSON 每次完成後更新，取消時保留已完成數據。Capped 結果標示 `limited`，不應與完整下載做同條件排名。

本機 tests 覆蓋來源解析、HTTP/TLS h1/h2、CONNECT/SOCKS5、IP pin/SNI、憑證拒絕、取消與 timeout、提前關閉 body、下載完整性，以及 CLI auto family 與無效選擇。Live 驗證已確認三種路由、互動操作與 Vultr Tokyo 完整 100MiB 的 HTTP/2 下載可用。私人測試結果檔不包含在此 repo。

Ping、MTU/PMTU、fragmentation、traceroute 尚未實作。不能從下載速度推測 IP fragmentation 或 PMTU。

## 來源

- [Psychz Looking Glass](https://lg.tw.psychz.net/)
- [Vultr Looking Glass](https://hnd-jp-ping.vultr.com/)
- [Go 官方下載](https://go.dev/dl/?mode=json)
- [uTLS v1.8.2](https://github.com/refraction-networking/utls/tree/v1.8.2)
- [x/net HTTP/2](https://pkg.go.dev/golang.org/x/net/http2)
