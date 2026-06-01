# Golide

芝浦工業大学のデジタル創作サークル「デジクリ」が開発をしている **bolide** の互換クライアント、「Golide」です。
軽量化・安定性の向上を図っています。

コメントの投稿画面・インフラ部を含めて、全てbolideの資産を利用させていただいております。
大変感謝申し上げます。

# フォントのライセンス

同封しているフォント『M PLUS 1p』は、『SIL OPEN FONT LICENSE Version 1.1』に基づき利用しています。

『SIL OPEN FONT LICENSE Version 1.1』および『M PLUS 1p』のライセンス表記は、本リポジトリ内の[FontsLicense.md](FontsLicense.md)に記載しています。

## How to Run

### ビルド済みのバイナリを使用する場合

``` sh
# amd64の場合
.\golide_windows_amd64.exe

# arm64の場合
.\golide_windows_arm64.exe

```

### Goを自分でビルドする場合

``` sh
go build -o main.exe
```

### Goを実行するだけの場合

``` sh
go run main.go embeds.go
```

## 実行時のオプション

``` sh
.\golide_windows_amd64.exe -h
```

または

``` sh
go run main.go embeds.go -h
```

から閲覧できます。