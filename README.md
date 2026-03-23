# lRTTServer
一个基于 Go + FFmpeg 的实时转码/HLS 服务.

## 启动

```bash
go run main.go
```

默认监听 `8082`，默认分片时长 5 秒。

## 访问

```
/video/rttPlaylist?path=1.mp4
/video/rttSegment?path=1.mp4&segment=0
```

## 常用环境变量

- `RTT_SEGMENT`：分片时长（秒），默认 5
- `RTT_BITRATE`：视频码率，默认 `4567k`
- `RTT_AUDIO_BITRATE`：音频码率，默认 `256k`
- `RTT_ENCODER` / `RTT_DECODER`：指定编码器/解码器（默认自动探测硬件）
- `RTT_MAX_JOBS`：并发转码任务数，默认 2
- `RTT_CACHE_DIR`：HLS 缓存目录，默认 `./tmp/hls`
- `RTT_AUDIO_COPY`：是否音频直拷，默认 `0`（建议重编码保证时间戳稳定）

## 验证（ffmpeg 自动化检查）

```bash
python3 scripts/verify_hls.py 1.mp4 --duration 180
```

会生成 HLS 分片并检查：
- 分片是否从关键帧开始
- 分片时长是否稳定
- 解码是否有错误
