#!/usr/bin/env python3
import argparse
import json
import math
import os
import shutil
import subprocess
import sys
from pathlib import Path


def run(cmd, check=True):
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if check and proc.returncode != 0:
        raise RuntimeError(f"command failed: {' '.join(cmd)}\n{proc.stderr}")
    return proc


def parse_rational(v: str) -> float:
    if not v or v == "0/0":
        return 0.0
    if "/" not in v:
        try:
            return float(v)
        except ValueError:
            return 0.0
    n, d = v.split("/", 1)
    try:
        n = float(n)
        d = float(d)
        if d == 0:
            return 0.0
        return n / d
    except ValueError:
        return 0.0


def detect_encoder(ffmpeg_path: str) -> str:
    out = run([ffmpeg_path, "-hide_banner", "-encoders"], check=True).stdout
    if "h264_videotoolbox" in out:
        return "h264_videotoolbox"
    if "h264_nvenc" in out:
        return "h264_nvenc"
    if "h264_qsv" in out:
        return "h264_qsv"
    if "h264_amf" in out:
        return "h264_amf"
    return "libx264"


def get_info(ffprobe_path: str, input_path: str):
    cmd = [
        ffprobe_path,
        "-v", "error",
        "-show_entries", "format=duration",
        "-show_entries", "stream=codec_type,codec_name,avg_frame_rate",
        "-of", "json",
        input_path,
    ]
    data = json.loads(run(cmd, check=True).stdout)
    info = {"duration": 0.0, "has_audio": False, "audio_codec": "", "fps": 0.0}
    try:
        info["duration"] = float(data.get("format", {}).get("duration", 0.0))
    except ValueError:
        info["duration"] = 0.0
    for s in data.get("streams", []):
        if s.get("codec_type") == "audio":
            info["has_audio"] = True
            if not info["audio_codec"]:
                info["audio_codec"] = s.get("codec_name", "")
        if s.get("codec_type") == "video" and info["fps"] == 0.0:
            info["fps"] = parse_rational(s.get("avg_frame_rate", ""))
    return info


def build_ffmpeg_cmd(cfg, info, input_path, out_dir):
    seg = str(cfg.segment)
    encoder = cfg.encoder
    gop = 0
    if info["fps"] > 0:
        gop = int(round(info["fps"] * cfg.segment))
    cmd = [cfg.ffmpeg, "-hide_banner", "-loglevel", "error", "-i", input_path]
    if cfg.hwaccel:
        cmd = [cfg.ffmpeg, "-hide_banner", "-loglevel", "error", "-hwaccel", cfg.hwaccel, "-i", input_path]
    cmd += ["-c:v", encoder, "-b:v", cfg.bitrate]
    if encoder == "h264_videotoolbox":
        cmd += ["-allow_sw", "1"]
    if cfg.force_keyframes:
        cmd += ["-flags", "+cgop", "-force_key_frames", f"expr:gte(t,n_forced*{cfg.segment})"]
    if cfg.disable_scenecut:
        cmd += ["-sc_threshold", "0"]
    if gop > 0:
        cmd += ["-g", str(gop), "-keyint_min", str(gop)]

    if info["has_audio"]:
        if cfg.audio_copy and info["audio_codec"] == "aac":
            cmd += ["-c:a", "copy"]
        else:
            cmd += ["-c:a", "aac", "-b:a", cfg.audio_bitrate]
    else:
        cmd += ["-an"]

    seg_pattern = str(Path(out_dir) / "seg_%05d.ts")
    cmd += [
        "-f", "hls",
        "-hls_time", seg,
        "-hls_segment_type", "mpegts",
        "-hls_list_size", "0",
        "-hls_flags", "independent_segments+append_list+temp_file",
        "-hls_segment_filename", seg_pattern,
        "-start_number", "0",
        str(Path(out_dir) / "index.m3u8"),
    ]
    if cfg.duration > 0:
        cmd.insert(cmd.index("-i"), "-t")
        cmd.insert(cmd.index("-i"), str(cfg.duration))
    return cmd


def verify_segments(cfg, out_dir):
    out_dir = Path(out_dir)
    segments = sorted(out_dir.glob("seg_*.ts"))
    if not segments:
        raise RuntimeError("no segments generated")

    last_idx = max(int(s.stem.split("_")[1]) for s in segments)
    bad_keyframes = []
    bad_durations = []

    for seg in segments:
        idx = int(seg.stem.split("_")[1])
        # first video frame should be keyframe
        cmd = [
            cfg.ffprobe,
            "-v", "error",
            "-select_streams", "v",
            "-show_frames",
            "-show_entries", "frame=key_frame",
            "-of", "csv=p=0",
            "-read_intervals", "0%+1",
            str(seg),
        ]
        out = run(cmd, check=True).stdout.strip().splitlines()
        first_val = None
        for line in out:
            line = line.strip()
            if line in ("0", "1"):
                first_val = line
                break
        if first_val != "1":
            bad_keyframes.append(seg.name)

        # duration check (allow last segment to be shorter)
        dur_cmd = [cfg.ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", str(seg)]
        dur_out = run(dur_cmd, check=True).stdout.strip()
        try:
            dur = float(dur_out)
        except ValueError:
            dur = 0.0
        if idx != last_idx and abs(dur - cfg.segment) > cfg.segment_tolerance:
            bad_durations.append((seg.name, dur))

    # decode check
    decode_cmd = [cfg.ffmpeg, "-v", "error", "-i", str(out_dir / "index.m3u8"), "-f", "null", "-"]
    decode = run(decode_cmd, check=False)

    return {
        "segments": len(segments),
        "bad_keyframes": bad_keyframes,
        "bad_durations": bad_durations,
        "decode_ok": decode.returncode == 0,
        "decode_errors": decode.stderr.strip(),
    }


def main():
    parser = argparse.ArgumentParser(description="Generate HLS and verify segment continuity")
    parser.add_argument("input", help="input video path")
    parser.add_argument("--out", default="tmp/hls_verify", help="output dir")
    parser.add_argument("--segment", type=int, default=5, help="segment duration seconds")
    parser.add_argument("--duration", type=int, default=0, help="limit transcode duration seconds")
    parser.add_argument("--bitrate", default="4567k")
    parser.add_argument("--audio-bitrate", default="256k")
    parser.add_argument("--ffmpeg", default="ffmpeg")
    parser.add_argument("--ffprobe", default="ffprobe")
    parser.add_argument("--encoder", default="auto")
    parser.add_argument("--hwaccel", default="")
    parser.add_argument("--no-audio-copy", action="store_true")
    parser.add_argument("--no-force-keyframes", action="store_true")
    parser.add_argument("--scenecut", action="store_true", help="allow scenecut")
    parser.add_argument("--segment-tolerance", type=float, default=0.3)
    args = parser.parse_args()

    input_path = str(Path(args.input).resolve())
    out_dir = Path(args.out)
    if out_dir.exists():
        shutil.rmtree(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    encoder = args.encoder
    if encoder == "auto":
        encoder = detect_encoder(args.ffmpeg)

    info = get_info(args.ffprobe, input_path)

    class Cfg:
        pass

    cfg = Cfg()
    cfg.segment = args.segment
    cfg.duration = args.duration
    cfg.bitrate = args.bitrate
    cfg.audio_bitrate = args.audio_bitrate
    cfg.ffmpeg = args.ffmpeg
    cfg.ffprobe = args.ffprobe
    cfg.encoder = encoder
    cfg.hwaccel = args.hwaccel
    cfg.audio_copy = not args.no_audio_copy
    cfg.force_keyframes = not args.no_force_keyframes
    cfg.disable_scenecut = not args.scenecut
    cfg.segment_tolerance = args.segment_tolerance

    cmd = build_ffmpeg_cmd(cfg, info, input_path, str(out_dir))
    print("ffmpeg:", " ".join(cmd))
    try:
        run(cmd, check=True)
    except RuntimeError as err:
        if cfg.encoder in ("h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_amf"):
            print("hardware encoder failed, retry with libx264")
            cfg.encoder = "libx264"
            cmd = build_ffmpeg_cmd(cfg, info, input_path, str(out_dir))
            print("ffmpeg:", " ".join(cmd))
            run(cmd, check=True)
        else:
            raise

    result = verify_segments(cfg, str(out_dir))
    print("segments:", result["segments"])
    print("decode ok:", result["decode_ok"])
    if result["decode_errors"]:
        print("decode errors:\n", result["decode_errors"])
    print("bad keyframe segments:", len(result["bad_keyframes"]))
    if result["bad_keyframes"]:
        print("first bad keyframes:", result["bad_keyframes"][:5])
    print("bad duration segments:", len(result["bad_durations"]))
    if result["bad_durations"]:
        print("first bad durations:", result["bad_durations"][:5])


if __name__ == "__main__":
    main()
