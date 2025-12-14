# SwitchBot環境監視システム

SwitchBotデバイスから環境データ（温度、湿度、CO2濃度）を取得し、Mastodonに投稿するGoアプリケーションです。AWS Lambdaでの実行とローカル実行の両方に対応しています。

## 機能

- SwitchBot Meter/MeterPro(CO2)デバイスからのデータ取得
- 環境データのMastodon投稿
- AWS CloudWatch Logsへの構造化ログ出力（Metric Filters用）
- バッテリー状態の監視と警告
- 重複投稿の防止機能

## セットアップ

### 1. 設定ファイルの準備

```bash
cp config.json.example config.json
```

`config.json`を編集して、以下の情報を設定してください：

- `SwitchBotToken`: SwitchBot APIトークン
- `SwitchBotSecret`: SwitchBot APIシークレット
- `MastodonURL`: MastodonインスタンスのAPIエンドポイント
- `MastodonToken`: Mastodonアクセストークン
- `BatteryCheckPostCount`: バッテリー状態チェック用の過去投稿数（オプション、デフォルト: 7）

### 2. 依存関係のインストール

```bash
go mod tidy
```

### 3. ローカル実行

```bash
go run main.go
```

## AWS Lambda デプロイ

環境変数として以下を設定：

- `SWITCHBOT_API_TOKEN`
- `SWITCHBOT_API_SECRET`
- `MASTODON_API_URL`
- `MASTODON_ACCESS_TOKEN`
- `BATTERY_CHECK_POST_COUNT` (オプション、デフォルト: 7)

## 出力例

```
# リビング温湿度計 (🔋85%)
温度: 23.5度
湿度: 45.2%

# 書斎CO2計 (⚠️78%)
温度: 24.1度
湿度: 42.8%
CO2: 1250ppm 💨
```

---

# SwitchBot Environmental Monitoring System

A Go application that retrieves environmental data (temperature, humidity, CO2 concentration) from SwitchBot devices and posts to Mastodon. Supports both AWS Lambda execution and local execution.

## Features

- Data retrieval from SwitchBot Meter/MeterPro(CO2) devices
- Environmental data posting to Mastodon
- Structured log output for AWS CloudWatch Logs (for Metric Filters)
- Battery status monitoring and alerts
- Duplicate post prevention

## Setup

### 1. Configuration File Setup

```bash
cp config.json.example config.json
```

Edit `config.json` and configure the following information:

- `SwitchBotToken`: SwitchBot API token
- `SwitchBotSecret`: SwitchBot API secret
- `MastodonURL`: Mastodon instance API endpoint
- `MastodonToken`: Mastodon access token
- `BatteryCheckPostCount`: Number of recent posts to check for battery status (optional, default: 7)

### 2. Install Dependencies

```bash
go mod tidy
```

### 3. Local Execution

```bash
go run main.go
```

## AWS Lambda Deployment

Set the following environment variables:

- `SWITCHBOT_API_TOKEN`
- `SWITCHBOT_API_SECRET`
- `MASTODON_API_URL`
- `MASTODON_ACCESS_TOKEN`
- `BATTERY_CHECK_POST_COUNT` (optional, default: 7)

## Output Example

```
# Living Room Thermometer (🔋85%)
Temperature: 23.5°C
Humidity: 45.2%

# Study CO2 Meter (⚠️78%)
Temperature: 24.1°C
Humidity: 42.8%
CO2: 1250ppm 💨
```

## License

MIT License