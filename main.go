package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port            int
	RuntimeDir      string
	SegmentDuration int
	Bitrate         string
	AudioBitrate    string
	Encoder         string
	Decoder         string
	FFmpegPath      string
	FFprobePath     string
	CacheDir        string
	MaxJobs         int
	PlaylistWait    time.Duration
	SegmentWait     time.Duration
	PreferAudioCopy bool
	ForceKeyFrames  bool
	DisableSceneCut bool
	LogFFmpegError  bool
}

type VideoInfo struct {
	Duration   float64
	HasAudio   bool
	AudioCodec string
	FPS        float64
}

type job struct {
	done chan struct{}
	err  error
}

type playlistMeta struct {
	Duration float64 `json:"duration"`
	Segment  int     `json:"segment"`
}

var (
	jobs   = map[string]*job{}
	jobsMu sync.Mutex
	jobSem chan struct{}

	infoCache   = map[string]cachedInfo{}
	infoCacheMu sync.Mutex
)

type cachedInfo struct {
	info VideoInfo
	ts   time.Time
}

func main() {
	cfg := loadConfig()
	log.Printf("runtime dir: %s", cfg.RuntimeDir)
	log.Printf("segment duration: %ds", cfg.SegmentDuration)

	if cfg.Encoder == "" || cfg.Encoder == "auto" {
		enc, dec := detectHardware(cfg.FFmpegPath)
		if enc != "" {
			cfg.Encoder = enc
		}
		if dec != "" {
			cfg.Decoder = dec
		}
	}
	if cfg.Encoder == "" {
		cfg.Encoder = "libx264"
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		log.Fatalf("failed to create cache dir: %v", err)
	}

	jobSem = make(chan struct{}, cfg.MaxJobs)

	http.HandleFunc("/video/rttPlaylist", func(w http.ResponseWriter, r *http.Request) {
		handlePlaylist(w, r, cfg)
	})
	http.HandleFunc("/video/rttSegment", func(w http.ResponseWriter, r *http.Request) {
		handleSegment(w, r, cfg)
	})
	http.HandleFunc("/video/rttSegment.ts", func(w http.ResponseWriter, r *http.Request) {
		handleSegment(w, r, cfg)
	})
	http.HandleFunc("/video/hls/", func(w http.ResponseWriter, r *http.Request) {
		handleHLSFile(w, r, cfg)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 Not Found."))
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() Config {
	cwd, _ := os.Getwd()
	runtimeDir := cwd

	// compatibility with old args: first arg runtime dir, --st <seconds>
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "--st" {
		runtimeDir = args[0]
		if st, err := os.Stat(runtimeDir); err != nil || !st.IsDir() {
			runtimeDir = cwd
		}
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--st" && i+1 < len(args) {
			if v, err := strconv.Atoi(args[i+1]); err == nil && v > 0 {
				os.Setenv("RTT_SEGMENT", strconv.Itoa(v))
			}
			break
		}
	}

	cfg := Config{
		Port:            envInt("RTT_PORT", 8082),
		RuntimeDir:      envString("RTT_RUNTIME", runtimeDir),
		SegmentDuration: envInt("RTT_SEGMENT", 5),
		Bitrate:         envString("RTT_BITRATE", "4567k"),
		AudioBitrate:    envString("RTT_AUDIO_BITRATE", "256k"),
		Encoder:         envString("RTT_ENCODER", "auto"),
		Decoder:         envString("RTT_DECODER", ""),
		FFmpegPath:      envString("RTT_FFMPEG", "ffmpeg"),
		FFprobePath:     envString("RTT_FFPROBE", "ffprobe"),
		MaxJobs:         envInt("RTT_MAX_JOBS", 2),
		PlaylistWait:    time.Duration(envInt("RTT_PLAYLIST_WAIT", 8)) * time.Second,
		SegmentWait:     time.Duration(envInt("RTT_SEGMENT_WAIT", 30)) * time.Second,
		PreferAudioCopy: envString("RTT_AUDIO_COPY", "0") != "0",
		ForceKeyFrames:  envString("RTT_FORCE_KEYFRAME", "1") != "0",
		DisableSceneCut: envString("RTT_SC_THRESHOLD", "0") == "0",
		LogFFmpegError:  true,
	}

	cacheDir := envString("RTT_CACHE_DIR", "")
	if cacheDir == "" {
		cacheDir = filepath.Join(cfg.RuntimeDir, "tmp", "hls")
	}
	cfg.CacheDir = cacheDir
	return cfg
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func safeFilePath(baseDir, rel string) (string, error) {
	clean := filepath.Clean(rel)
	abs := filepath.Join(baseDir, clean)
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, baseAbs+string(os.PathSeparator)) && abs != baseAbs {
		return "", errors.New("illegal path")
	}
	return abs, nil
}

func handlePlaylist(w http.ResponseWriter, r *http.Request, cfg Config) {
	videoPath := r.URL.Query().Get("path")
	if videoPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	absPath, err := safeFilePath(cfg.RuntimeDir, videoPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(absPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	info, err := getVideoInfo(cfg, absPath)
	if err != nil {
		http.Error(w, "probe failed", http.StatusInternalServerError)
		return
	}
	cacheDir := buildCacheDir(cfg, absPath, info)
	playlistPath := filepath.Join(cacheDir, "index.m3u8")
	ensureHLSStarted(cfg, absPath, cacheDir, playlistPath, info)
	cacheID := filepath.Base(cacheDir)
	playlist := buildVODPlaylist(cacheID, info.Duration, cfg.SegmentDuration)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(playlist))
}

func handleSegment(w http.ResponseWriter, r *http.Request, cfg Config) {
	videoPath := r.URL.Query().Get("path")
	segStr := r.URL.Query().Get("segment")
	if videoPath == "" || segStr == "" {
		http.Error(w, "missing params", http.StatusBadRequest)
		return
	}
	seg, err := strconv.Atoi(segStr)
	if err != nil || seg < 0 {
		http.Error(w, "invalid segment", http.StatusBadRequest)
		return
	}
	absPath, err := safeFilePath(cfg.RuntimeDir, videoPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(absPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	info, err := getVideoInfo(cfg, absPath)
	if err != nil {
		http.Error(w, "probe failed", http.StatusInternalServerError)
		return
	}
	cacheDir := buildCacheDir(cfg, absPath, info)
	playlistPath := filepath.Join(cacheDir, "index.m3u8")
	ensureHLSStarted(cfg, absPath, cacheDir, playlistPath, info)
	segmentPath := filepath.Join(cacheDir, fmt.Sprintf("seg_%05d.ts", seg))
	if err := waitForFile(segmentPath, cfg.SegmentWait); err != nil {
		http.Error(w, "segment not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "video/MP2T")
	serveFile(w, r, segmentPath)
}

func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, _ := f.Stat()
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

func buildVODPlaylist(cacheID string, duration float64, segSeconds int) string {
	if duration <= 0 {
		duration = float64(segSeconds)
	}
	segDur := float64(segSeconds)
	total := int(math.Ceil(duration / segDur))
	if total < 1 {
		total = 1
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(segDur))))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < total; i++ {
		d := segDur
		if i == total-1 {
			last := duration - (segDur * float64(total-1))
			if last > 0 {
				d = last
			}
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", d))
		b.WriteString(fmt.Sprintf("/video/hls/%s/seg_%05d.ts\n", url.PathEscape(cacheID), i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func handleHLSFile(w http.ResponseWriter, r *http.Request, cfg Config) {
	rel := strings.TrimPrefix(r.URL.Path, "/video/hls/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cacheID := parts[0]
	name := parts[1]
	if name == "index.m3u8" {
		if meta, err := readPlaylistMeta(filepath.Join(cfg.CacheDir, cacheID)); err == nil {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			playlist := buildVODPlaylist(cacheID, meta.Duration, meta.Segment)
			_, _ = w.Write([]byte(playlist))
			return
		}
	}
	clean := filepath.Clean(filepath.Join(cacheID, name))
	full := filepath.Join(cfg.CacheDir, clean)
	cacheAbs, err := filepath.Abs(cfg.CacheDir)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil || !strings.HasPrefix(fullAbs, cacheAbs+string(os.PathSeparator)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, err := os.Stat(fullAbs); err != nil {
		if strings.HasSuffix(fullAbs, ".ts") {
			if err := waitForFile(fullAbs, cfg.SegmentWait); err != nil {
				http.Error(w, "segment not ready", http.StatusServiceUnavailable)
				return
			}
		} else {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	if strings.HasSuffix(fullAbs, ".ts") {
		w.Header().Set("Content-Type", "video/MP2T")
	}
	serveFile(w, r, fullAbs)
}

func buildCacheDir(cfg Config, absPath string, info VideoInfo) string {
	stat, _ := os.Stat(absPath)
	size := int64(0)
	mtime := int64(0)
	if stat != nil {
		size = stat.Size()
		mtime = stat.ModTime().UnixNano()
	}
	key := fmt.Sprintf("%s|%d|%d|%d|%s|%s|%t|%s", absPath, size, mtime, cfg.SegmentDuration, cfg.Bitrate, cfg.Encoder, cfg.PreferAudioCopy, info.AudioCodec)
	h := sha1.Sum([]byte(key))
	return filepath.Join(cfg.CacheDir, hex.EncodeToString(h[:]))
}

func ensureHLSStarted(cfg Config, absPath, cacheDir, playlistPath string, info VideoInfo) {
	jobsMu.Lock()
	if _, ok := jobs[cacheDir]; ok {
		jobsMu.Unlock()
		return
	}
	j := &job{done: make(chan struct{})}
	jobs[cacheDir] = j
	jobsMu.Unlock()

	go func() {
		defer close(j.done)
		jobSem <- struct{}{}
		defer func() { <-jobSem }()

		log.Printf("ffmpeg start: %s", absPath)
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			j.err = err
			jobsMu.Lock()
			delete(jobs, cacheDir)
			jobsMu.Unlock()
			log.Printf("ffmpeg start failed: %v", err)
			return
		}
		_ = writePlaylistMeta(cacheDir, playlistMeta{
			Duration: info.Duration,
			Segment:  cfg.SegmentDuration,
		})
		cmd := buildFFmpegCommand(cfg, absPath, cacheDir, playlistPath, info)
		if err := runFFmpeg(cfg, cmd); err != nil {
			if isHardwareEncoder(cfg.Encoder) {
				log.Printf("ffmpeg failed with hardware encoder, retrying with libx264: %v", err)
				fallback := cfg
				fallback.Encoder = "libx264"
				cmd = buildFFmpegCommand(fallback, absPath, cacheDir, playlistPath, info)
				if err2 := runFFmpeg(fallback, cmd); err2 == nil {
					log.Printf("ffmpeg done (fallback): %s", absPath)
					return
				} else {
					err = err2
				}
			}
			j.err = err
			jobsMu.Lock()
			delete(jobs, cacheDir)
			jobsMu.Unlock()
			log.Printf("ffmpeg failed: %v", err)
			return
		}
		_ = markPlaylistDone(cacheDir)
		log.Printf("ffmpeg done: %s", absPath)
	}()
}

func runFFmpeg(cfg Config, args []string) error {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, cfg.FFmpegPath, args...)
	if cfg.LogFFmpegError {
		cmd.Stdout = nil
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

func buildFFmpegCommand(cfg Config, inputPath, cacheDir, playlistPath string, info VideoInfo) []string {
	seg := strconv.Itoa(cfg.SegmentDuration)
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
	}
	if cfg.Decoder != "" {
		args = append(args, "-hwaccel", cfg.Decoder)
	}
	args = append(args, "-i", inputPath)
	args = append(args,
		"-c:v", cfg.Encoder,
		"-b:v", cfg.Bitrate,
	)
	if cfg.Encoder == "h264_videotoolbox" {
		args = append(args, "-allow_sw", "1")
	}
	if cfg.ForceKeyFrames {
		args = append(args, "-flags", "+cgop")
		args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", cfg.SegmentDuration))
	}
	if cfg.DisableSceneCut {
		args = append(args, "-sc_threshold", "0")
	}
	if info.FPS > 0 {
		gop := int(math.Round(info.FPS * float64(cfg.SegmentDuration)))
		if gop > 0 {
			args = append(args, "-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop))
		}
	}

	if info.HasAudio {
		if cfg.PreferAudioCopy && info.AudioCodec == "aac" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", cfg.AudioBitrate, "-af", "aresample=async=1:min_hard_comp=0.1:first_pts=0")
		}
	} else {
		args = append(args, "-an")
	}

	segPattern := filepath.Join(cacheDir, "seg_%05d.ts")
	args = append(args,
		"-f", "hls",
		"-hls_time", seg,
		"-hls_segment_type", "mpegts",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+append_list+temp_file",
		"-hls_segment_filename", segPattern,
		"-start_number", "0",
		playlistPath,
	)
	return args
}

func getVideoInfo(cfg Config, absPath string) (VideoInfo, error) {
	infoCacheMu.Lock()
	if cached, ok := infoCache[absPath]; ok {
		if time.Since(cached.ts) < 5*time.Minute {
			infoCacheMu.Unlock()
			return cached.info, nil
		}
	}
	infoCacheMu.Unlock()

	cmd := exec.Command(cfg.FFprobePath, "-v", "error", "-show_entries", "format=duration", "-show_entries", "stream=codec_type,codec_name,avg_frame_rate", "-of", "json", absPath)
	out, err := cmd.Output()
	if err != nil {
		return VideoInfo{}, err
	}
	var parsed struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			AvgFrameRate string `json:"avg_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return VideoInfo{}, err
	}
	info := VideoInfo{}
	if parsed.Format.Duration != "" {
		if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil {
			info.Duration = d
		}
	}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "audio":
			info.HasAudio = true
			if info.AudioCodec == "" {
				info.AudioCodec = s.CodecName
			}
		case "video":
			if info.FPS == 0 && s.AvgFrameRate != "" {
				info.FPS = parseRational(s.AvgFrameRate)
			}
		}
	}
	infoCacheMu.Lock()
	infoCache[absPath] = cachedInfo{info: info, ts: time.Now()}
	infoCacheMu.Unlock()
	return info, nil
}

func parseRational(v string) float64 {
	parts := strings.Split(v, "/")
	if len(parts) != 2 {
		return 0
	}
	n, err1 := strconv.ParseFloat(parts[0], 64)
	d, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s", path)
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func playlistMetaPath(cacheDir string) string {
	return filepath.Join(cacheDir, "meta.json")
}

func playlistDonePath(cacheDir string) string {
	return filepath.Join(cacheDir, "done.flag")
}

func writePlaylistMeta(cacheDir string, meta playlistMeta) error {
	tmp := playlistMetaPath(cacheDir) + ".tmp"
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, playlistMetaPath(cacheDir))
}

func readPlaylistMeta(cacheDir string) (playlistMeta, error) {
	data, err := os.ReadFile(playlistMetaPath(cacheDir))
	if err != nil {
		return playlistMeta{}, err
	}
	var meta playlistMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return playlistMeta{}, err
	}
	if meta.Segment <= 0 {
		meta.Segment = 5
	}
	return meta, nil
}

func markPlaylistDone(cacheDir string) error {
	return os.WriteFile(playlistDonePath(cacheDir), []byte("ok\n"), 0o644)
}

func detectHardware(ffmpegPath string) (encoder string, decoder string) {
	// decoder
	if out, err := exec.Command(ffmpegPath, "-hide_banner", "-hwaccels").Output(); err == nil {
		text := string(out)
		if strings.Contains(text, "videotoolbox") {
			decoder = "videotoolbox"
		} else if strings.Contains(text, "cuda") {
			decoder = "cuda"
		} else if strings.Contains(text, "qsv") {
			decoder = "qsv"
		} else if strings.Contains(text, "amf") {
			decoder = "amf"
		}
	}
	// encoder
	if out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output(); err == nil {
		text := string(out)
		if strings.Contains(text, "h264_videotoolbox") {
			encoder = "h264_videotoolbox"
		} else if strings.Contains(text, "h264_nvenc") {
			encoder = "h264_nvenc"
		} else if strings.Contains(text, "h264_qsv") {
			encoder = "h264_qsv"
		} else if strings.Contains(text, "h264_amf") {
			encoder = "h264_amf"
		}
	}
	return encoder, decoder
}

func isHardwareEncoder(encoder string) bool {
	switch encoder {
	case "h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_amf":
		return true
	default:
		return false
	}
}

func init() {
	flag.CommandLine.SetOutput(io.Discard)
}
