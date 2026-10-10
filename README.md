# eapaka-ossbss-sample

外部 OSS/BSS（運用・業務支援システム）のサンプル実装としての統合BFF（コマンド名 `eapaka-ossbss`）です。[eapaka-node-provisioner](https://github.com/oyaguma3/eapaka-node-provisioner) の API を使って、[EAP-AKA RADIUS PoC](https://github.com/oyaguma3/eapaka-radius-server-poc) と [aka-only-server](https://github.com/oyaguma3/aka-only-server) の加入者・RADIUSクライアント・認可ポリシーを、テナントと役割に基づく権限で操作します。

```
[ブラウザ] ─> Cloudflare Access（メール認証）─ Tunnel ─> 統合BFF ─> eapaka-node-provisioner ─> RADIUS PoC / aka-only-server
                                                          │
                                                     PostgreSQL
```

- 加入者と RADIUSクライアントをテナントに属させ、テナントの利用者は自分のテナントのものだけを扱います（テナントに属さないものも扱えます）。
- 認証は Cloudflare Access / Tunnel（メールのワンタイム PIN）で行い、統合BFF は Access の JWT を検証して利用者を識別します。
- 加入者を登録したまま一時的に使えなくする（停止・再開）ことができます。
- 画面は Go + htmx + daisyUI です。

個人利用・検証用途のサンプルです。

## 現在の状態

設計中です。設計は [docs/design-overview.md](docs/design-overview.md) にあります。実装はこれからです。

## ライセンス

[LICENSE](LICENSE) を参照してください。
