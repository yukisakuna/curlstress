# curlstress

`curlstress` は、`curl.txt` に書いた cURL を読み込んで、同じ HTTP リクエストを並列で投げ続ける Go 製の CLI です。

普段 API の確認に使っている cURL を、そのまま負荷試験に持ち込みたいときに向いています。設定ファイルを増やすより、まず 1 本の cURL を置いて回したい、という前提のツールです。

## すぐ試す

Go `1.26` 以上で動きます。配布バイナリを使うなら `go build` は不要です。

`curl.txt` の例:

```bash
curl 'https://example.com/api/orders' \
  -X POST \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer replace-me' \
  --data-binary '{"sku":"abc-123","qty":1}'
```

実行:

```bash
go build -o curlstress .
./curlstress -curl-file curl.txt -duration 30s -workers 128 -rps 1000
```


## フラグ

| フラグ | 説明 |
| --- | --- |
| `-curl-file` | cURL コマンドを書いたテキストファイル。1 ファイル 1 コマンド前提 |
| `-duration` | 負荷をかける時間 |
| `-workers` | 同時実行ワーカー数 |
| `-rps` | 秒間リクエスト数の上限。`0` なら無制限 |
| `-queue` | 互換性維持用。いまの direct-worker 実装では無視 |
| `-req-timeout` | リクエスト単位のタイムアウト。`0` で無効 |
| `-http-timeout` | 共有 `http.Client` のタイムアウト。`0` で無効 |
| `-progress` | 進捗表示の間隔。`0` で無効 |

## 読み取れる cURL オプション

普段使う範囲では、次のオプションを解釈します。

- `-X`, `--request`
- `-H`, `--header`
- `-d`, `--data`, `--data-raw`, `--data-ascii`
- `--data-binary`
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

`@payload.json` や `@headers.txt` のような相対パスは、`curl.txt` が置いてあるディレクトリ基準で解決します。

次のフラグは受け付けますが、リクエスト内容には使いません。

- `-s`, `--silent`
- `-S`, `--show-error`
- `-v`, `--verbose`
- `-i`, `--include`
- `-o`, `--output`
- `-m`, `--max-time`
- `--connect-timeout`
- `-w`, `--write-out`
- `--http1.1`
- `--http2`
- `--path-as-is`
- `--globoff`
- `--fail`
- `--fail-with-body`

`--compressed` は例外で、`Accept-Encoding: gzip, deflate, br` を付ける形で扱います。

## 実行の中身

- リクエストは各ワーカーが直接送ります。中央キューで詰めて捨てる作りではありません。
- 条件が合う単一リクエスト先では、HTTP/1.1 keep-alive の raw backend を自動で選びます。無理な場合は `net/http` にフォールバックします。
- `-rps 0` では、ワーカー数、接続再利用、相手側の処理性能が主な上限になります。
- `-rps > 0` では、全体の RPS をワーカーへ分配して、中央ボトルネックを作らないようにしています。

## 割り切り

- 1 つのファイルに書ける cURL は 1 件だけです。
- URL は `http` と `https` だけを対象にしています。
- `-F`, `--form` による multipart upload にはまだ対応していません。
- フォーム系の data と raw/binary/json 系の data は混ぜられません。
- `-G`, `--get` は form-style の `-d` 系オプションと組み合わせた場合だけ扱えます。
- Cookie jar ファイルを `-b @file` で読む使い方には対応していません。
- `-d @file` は未対応です。ファイルの内容をそのまま送りたいときは `--data-binary @file` か `--json @file` を使ってください。
- バイト列をそのまま送りたいなら `--data-binary @file` を使うのが安全です。

## 確認

```bash
go test ./...
go build -o curlstress .
./curlstress -curl-file curl.txt -duration 5s -workers 8 -rps 50
go test -run ^$ -bench . -benchmem
```

## リリース

通常の `commit` や `push` だけでは GitHub Release は作られません。`v*` 形式のタグを push したときだけ [`.github/workflows/release.yml`](.github/workflows/release.yml) が動きます。

```bash
git tag v0.1.0
git push origin v0.1.0
```

タグが GitHub に届くと、Linux / macOS / Windows 向けの `curlstress` バイナリをビルドして、GitHub Releases にアーカイブを添付します。


## AIだと思いましたか？

正解です。おめでとう、そしてGPT-5.4ありがとう笑
