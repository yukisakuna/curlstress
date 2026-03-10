# curlstress

`curlstress` は `curl.txt` に書いた 1 件の cURL コマンドを読み取り、同じ HTTP リクエストを並列で継続送信する Go 製の負荷テスト用 CLI です。

## 前提

- Go `1.26` 以上

## ビルド

ローカルで `curlstress` という名前のバイナリを作る場合は次を実行します。

```bash
go build -o curlstress .
```

## 使い方

1. `curl.txt` に cURL コマンドを 1 件だけ書きます。
2. `go test ./...` を実行します。
3. `go build -o curlstress .` でローカルビルドするか、GitHub Releases から配布バイナリを取得します。
4. `./curlstress -curl-file curl.txt -duration 30s -workers 128 -rps 1000` を実行します。

同梱している `curl.txt` は公開用の安全なサンプルです。
実運用のトークン、Cookie、本番向けペイロードはコミットしないでください。

`curl.txt` の例:

```bash
curl 'https://example.com/api/orders' \
  -X POST \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer replace-me' \
  --data-binary '{"sku":"abc-123","qty":1}'
```

## オプション

- `-curl-file`: cURL コマンドを 1 件だけ書いたテキストファイルのパス
- `-duration`: 負荷をかけ続ける時間
- `-workers`: 同時実行ワーカー数
- `-rps`: 秒間リクエスト数の上限。`0` は無制限
- `-queue`: 互換性維持用フラグ。現在の direct-worker 実装では無視
- `-req-timeout`: リクエスト単位のタイムアウト。`0` で無効
- `-http-timeout`: 共有 `http.Client` のタイムアウト。`0` で無効
- `-progress`: 進捗表示の間隔。`0` で無効

## 対応している cURL オプション

- `-X`, `--request`
- `-H`, `--header`
- `-d`, `--data`, `--data-raw`, `--data-binary`, `--data-ascii`
- `--json`
- `-u`, `--user`
- `-I`, `--head`
- `-k`, `--insecure`
- `-L`, `--location`
- `-A`, `--user-agent`
- `-e`, `--referer`
- `-b`, `--cookie`
- `-G`, `--get`
- `--url`

`-s`, `-S`, `-v`, `-o`, `--compressed`, `--http1.1`, `--http2` のような出力寄りのフラグは無視します。

## 実行モデル

- リクエストは各ワーカーが直接送信します。中央キューでドロップする構成ではありません。
- 条件を満たす単一リクエスト先では、HTTP/1.1 keep-alive の raw backend を自動選択します。使えない場合は `net/http` にフォールバックします。
- `-rps 0` のときはワーカー数、接続再利用、対象サーバー性能が主な上限になります。
- `-rps > 0` のときは全体の RPS をワーカーへ分配して、中央ボトルネックを避けます。

## 現在の制限

- 1 ファイルにつき cURL コマンドは 1 件だけ
- 対応 URL は `http` と `https` のみ
- `-F`, `--form` による multipart upload は未対応
- `@payload.json` や `@headers.txt` の相対パスは `curl.txt` 基準で解決
- バイト列を厳密に送りたい場合は `--data-binary @file` を推奨

## 確認手順

```bash
go test ./...
go build -o curlstress .
./curlstress -curl-file curl.txt -duration 5s -workers 8 -rps 50
go test -run ^$ -bench . -benchmem
```

## GitHub Release 運用

通常の `commit` や `push` だけでは GitHub Release は作られません。
`v*` 形式のタグを push したときだけ、[`.github/workflows/release.yml`](.github/workflows/release.yml) が動きます。

```bash
git tag v0.1.0
git push origin v0.1.0
```

タグが GitHub に届くと、Linux、macOS、Windows 向けの `curlstress` バイナリをビルドし、GitHub Releases にアーカイブを添付します。
