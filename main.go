package main

import (
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port            int
	RuntimeDir      string
	VideoDirs       []string
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
	LogSimple       bool
	LogDetail       bool
	BasicAuthUser   string
	BasicAuthPass   string
	BasicAuthOn     bool
	SegmentByKey    bool
	SegmentOnDemand bool
	SegmentSeekAcc  bool
	MaxKeyframeGap  int
	ForceEncoder    bool
}

type ConfigFile struct {
	Port            *int    `json:"port"`
	RuntimeDir      *string `json:"runtime_dir"`
	VideoDirPath    *string `json:"videoDirPath"`
	SegmentDuration *int    `json:"segment"`
	Bitrate         *string `json:"bitrate"`
	AudioBitrate    *string `json:"audio_bitrate"`
	Encoder         *string `json:"encoder"`
	Decoder         *string `json:"decoder"`
	FFmpegPath      *string `json:"ffmpeg"`
	FFprobePath     *string `json:"ffprobe"`
	CacheDir        *string `json:"cache_dir"`
	MaxJobs         *int    `json:"max_jobs"`
	PlaylistWait    *int    `json:"playlist_wait"`
	SegmentWait     *int    `json:"segment_wait"`
	PreferAudioCopy *bool   `json:"audio_copy"`
	ForceKeyFrames  *bool   `json:"force_keyframe"`
	DisableSceneCut *bool   `json:"disable_scene_cut"`
	LogFFmpegError  *bool   `json:"log_ffmpeg_error"`
	LogSimple       *bool   `json:"log_simple"`
	LogDetail       *bool   `json:"log_detail"`
	BasicAuthUser   *string `json:"basic_auth_user"`
	BasicAuthPass   *string `json:"basic_auth_pass"`
	BasicAuthOn     *bool   `json:"basic_auth_enabled"`
	SegmentByKey    *bool   `json:"segment_by_keyframe"`
	SegmentOnDemand *bool   `json:"segment_on_demand"`
	SegmentSeekAcc  *bool   `json:"segment_seek_accurate"`
	MaxKeyframeGap  *int    `json:"max_keyframe_interval"`
	ForceEncoder    *bool   `json:"force_encoder"`
}

type VideoInfo struct {
	Duration    float64
	HasAudio    bool
	AudioCodec  string
	AudioCh     int
	AudioLayout string
	VideoCodec  string
	MaxKFGap    float64
	FPS         float64
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

	segJobs   = map[string]*job{}
	segJobsMu sync.Mutex

	segStats   = map[string]*segmentStat{}
	segStatsMu sync.Mutex

	hwDisableMu sync.Mutex
	hwDisabled  = map[string]bool{}
)

type cachedInfo struct {
	info VideoInfo
	ts   time.Time
}

type segmentStat struct {
	count int64
	sumMs int64
	maxMs int64
	base  int64
	last  int64
}

func main() {
	cfg := loadConfig()
	logSimple(cfg, "MAIN", "runtime dir: %s", cfg.RuntimeDir)
	logSimple(cfg, "MAIN", "video dirs: %s", strings.Join(cfg.VideoDirs, ", "))
	logSimple(cfg, "MAIN", "segment duration: %ds", cfg.SegmentDuration)

	if cfg.Decoder == "" {
		if dec := detectDecoder(cfg.FFmpegPath); dec != "" {
			cfg.Decoder = dec
		}
	}
	cfg.Encoder = pickEncoder(cfg.Encoder, cfg.FFmpegPath)
	validateEncoder(&cfg)
	logSimple(cfg, "MAIN", "encoder: %s decoder: %s", cfg.Encoder, cfg.Decoder)
	if cfg.BasicAuthOn {
		logSimple(cfg, "MAIN", "basic auth: enabled user=%s", cfg.BasicAuthUser)
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		fatalf("MAIN", "failed to create cache dir: %v", err)
	}

	jobSem = make(chan struct{}, cfg.MaxJobs)

	http.HandleFunc("/video/rttPlaylist", withAuth(cfg, func(w http.ResponseWriter, r *http.Request) {
		handlePlaylist(w, r, cfg)
	}))
	http.HandleFunc("/video/rttSegment", withAuth(cfg, func(w http.ResponseWriter, r *http.Request) {
		handleSegment(w, r, cfg)
	}))
	http.HandleFunc("/video/rttSegment.ts", withAuth(cfg, func(w http.ResponseWriter, r *http.Request) {
		handleSegment(w, r, cfg)
	}))
	http.HandleFunc("/browse", withAuth(cfg, func(w http.ResponseWriter, r *http.Request) {
		handleBrowse(w, r, cfg)
	}))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(cfg, w, r) {
			return
		}
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/browse", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 Not Found."))
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	logSimple(cfg, "MAIN", "listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fatalf("MAIN", "server error: %v", err)
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

	configPath := envString("RTT_CONFIG", "")
	if configPath == "" {
		configPath = filepath.Join(runtimeDir, "config.json")
	} else if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(runtimeDir, configPath)
	}
	fileCfg, found, err := readConfigFile(configPath)
	if err != nil {
		fatalf("CONFIG", "invalid config %s: %v", configPath, err)
	}

	cfg := Config{
		Port:            8082,
		RuntimeDir:      runtimeDir,
		VideoDirs:       []string{runtimeDir},
		SegmentDuration: 5,
		Bitrate:         "4567k",
		AudioBitrate:    "256k",
		Encoder:         "h265",
		Decoder:         "",
		FFmpegPath:      "ffmpeg",
		FFprobePath:     "ffprobe",
		MaxJobs:         2,
		PlaylistWait:    8 * time.Second,
		SegmentWait:     30 * time.Second,
		PreferAudioCopy: false,
		ForceKeyFrames:  true,
		DisableSceneCut: true,
		LogFFmpegError:  true,
		LogSimple:       true,
		LogDetail:       true,
		BasicAuthUser:   "",
		BasicAuthPass:   "",
		BasicAuthOn:     false,
		SegmentByKey:    false,
		SegmentOnDemand: true,
		SegmentSeekAcc:  true,
		MaxKeyframeGap:  0,
		ForceEncoder:    false,
	}
	if found {
		applyConfigFile(&cfg, fileCfg)
	}

	cfg.Port = envInt("RTT_PORT", cfg.Port)
	cfg.RuntimeDir = envString("RTT_RUNTIME", cfg.RuntimeDir)
	if v := envString("RTT_VIDEO_DIR", ""); v != "" {
		cfg.VideoDirs = parseVideoDirs(v)
	}
	cfg.SegmentDuration = envInt("RTT_SEGMENT", cfg.SegmentDuration)
	cfg.Bitrate = envString("RTT_BITRATE", cfg.Bitrate)
	cfg.AudioBitrate = envString("RTT_AUDIO_BITRATE", cfg.AudioBitrate)
	cfg.Encoder = envString("RTT_ENCODER", cfg.Encoder)
	cfg.Decoder = envString("RTT_DECODER", cfg.Decoder)
	cfg.FFmpegPath = envString("RTT_FFMPEG", cfg.FFmpegPath)
	cfg.FFprobePath = envString("RTT_FFPROBE", cfg.FFprobePath)
	cfg.MaxJobs = envInt("RTT_MAX_JOBS", cfg.MaxJobs)
	if v := os.Getenv("RTT_PLAYLIST_WAIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PlaylistWait = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("RTT_SEGMENT_WAIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SegmentWait = time.Duration(n) * time.Second
		}
	}
	cfg.PreferAudioCopy = envBool01("RTT_AUDIO_COPY", cfg.PreferAudioCopy)
	cfg.ForceKeyFrames = envBool01("RTT_FORCE_KEYFRAME", cfg.ForceKeyFrames)
	if v := os.Getenv("RTT_SC_THRESHOLD"); v != "" {
		cfg.DisableSceneCut = v == "0"
	}
	cfg.LogSimple = envBool01("RTT_LOG_SIMPLE", cfg.LogSimple)
	cfg.LogDetail = envBool01("RTT_LOG_DETAIL", cfg.LogDetail)
	if v := envString("RTT_BASIC_AUTH_USER", ""); v != "" {
		cfg.BasicAuthUser = v
	}
	if v := envString("RTT_BASIC_AUTH_PASS", ""); v != "" {
		cfg.BasicAuthPass = v
	}
	if v := os.Getenv("RTT_BASIC_AUTH_ENABLED"); v != "" {
		cfg.BasicAuthOn = envBool01("RTT_BASIC_AUTH_ENABLED", cfg.BasicAuthOn)
	}
	if cfg.BasicAuthOn {
		if cfg.BasicAuthUser == "" || cfg.BasicAuthPass == "" {
			fatalf("CONFIG", "basic auth enabled but user/pass missing")
		}
	}
	cfg.SegmentByKey = envBool01("RTT_SEGMENT_BY_KEYFRAME", cfg.SegmentByKey)
	cfg.SegmentOnDemand = envBool01("RTT_SEGMENT_ON_DEMAND", cfg.SegmentOnDemand)
	cfg.SegmentSeekAcc = envBool01("RTT_SEGMENT_SEEK_ACCURATE", cfg.SegmentSeekAcc)
	cfg.MaxKeyframeGap = envInt("RTT_MAX_KEYFRAME_INTERVAL", cfg.MaxKeyframeGap)
	cfg.ForceEncoder = envBool01("RTT_FORCE_ENCODER", cfg.ForceEncoder)

	cfg.VideoDirs = normalizeVideoDirs(cfg.RuntimeDir, cfg.VideoDirs)

	cacheDir := envString("RTT_CACHE_DIR", cfg.CacheDir)
	if cacheDir == "" {
		cacheDir = filepath.Join(cfg.RuntimeDir, "tmp", "hls")
	} else if !filepath.IsAbs(cacheDir) {
		cacheDir = filepath.Join(cfg.RuntimeDir, cacheDir)
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

func envBool01(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v != "0"
	}
	return def
}

func parseVideoDirs(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func normalizeVideoDirs(runtimeDir string, dirs []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		s := strings.TrimSpace(d)
		if s == "" {
			continue
		}
		if !filepath.IsAbs(s) {
			s = filepath.Join(runtimeDir, s)
		}
		s = filepath.Clean(s)
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		out = []string{filepath.Clean(runtimeDir)}
	}
	return out
}

func readConfigFile(path string) (ConfigFile, bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ConfigFile{}, false, nil
		}
		return ConfigFile{}, false, err
	}
	if st.IsDir() {
		return ConfigFile{}, false, fmt.Errorf("config path is a directory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ConfigFile{}, false, err
	}
	var fileCfg ConfigFile
	if err := json.Unmarshal(data, &fileCfg); err != nil {
		return ConfigFile{}, true, err
	}
	return fileCfg, true, nil
}

func applyConfigFile(cfg *Config, fileCfg ConfigFile) {
	if fileCfg.Port != nil {
		cfg.Port = *fileCfg.Port
	}
	if fileCfg.RuntimeDir != nil {
		cfg.RuntimeDir = *fileCfg.RuntimeDir
	}
	if fileCfg.VideoDirPath != nil {
		cfg.VideoDirs = parseVideoDirs(*fileCfg.VideoDirPath)
	}
	if fileCfg.SegmentDuration != nil {
		cfg.SegmentDuration = *fileCfg.SegmentDuration
	}
	if fileCfg.Bitrate != nil {
		cfg.Bitrate = *fileCfg.Bitrate
	}
	if fileCfg.AudioBitrate != nil {
		cfg.AudioBitrate = *fileCfg.AudioBitrate
	}
	if fileCfg.Encoder != nil {
		cfg.Encoder = *fileCfg.Encoder
	}
	if fileCfg.Decoder != nil {
		cfg.Decoder = *fileCfg.Decoder
	}
	if fileCfg.FFmpegPath != nil {
		cfg.FFmpegPath = *fileCfg.FFmpegPath
	}
	if fileCfg.FFprobePath != nil {
		cfg.FFprobePath = *fileCfg.FFprobePath
	}
	if fileCfg.CacheDir != nil {
		cfg.CacheDir = *fileCfg.CacheDir
	}
	if fileCfg.MaxJobs != nil {
		cfg.MaxJobs = *fileCfg.MaxJobs
	}
	if fileCfg.PlaylistWait != nil {
		cfg.PlaylistWait = time.Duration(*fileCfg.PlaylistWait) * time.Second
	}
	if fileCfg.SegmentWait != nil {
		cfg.SegmentWait = time.Duration(*fileCfg.SegmentWait) * time.Second
	}
	if fileCfg.PreferAudioCopy != nil {
		cfg.PreferAudioCopy = *fileCfg.PreferAudioCopy
	}
	if fileCfg.ForceKeyFrames != nil {
		cfg.ForceKeyFrames = *fileCfg.ForceKeyFrames
	}
	if fileCfg.DisableSceneCut != nil {
		cfg.DisableSceneCut = *fileCfg.DisableSceneCut
	}
	if fileCfg.LogFFmpegError != nil {
		cfg.LogFFmpegError = *fileCfg.LogFFmpegError
	}
	if fileCfg.LogSimple != nil {
		cfg.LogSimple = *fileCfg.LogSimple
	}
	if fileCfg.LogDetail != nil {
		cfg.LogDetail = *fileCfg.LogDetail
	}
	if fileCfg.BasicAuthUser != nil {
		cfg.BasicAuthUser = *fileCfg.BasicAuthUser
	}
	if fileCfg.BasicAuthPass != nil {
		cfg.BasicAuthPass = *fileCfg.BasicAuthPass
	}
	if fileCfg.BasicAuthOn != nil {
		cfg.BasicAuthOn = *fileCfg.BasicAuthOn
	}
	if fileCfg.SegmentByKey != nil {
		cfg.SegmentByKey = *fileCfg.SegmentByKey
	}
	if fileCfg.SegmentOnDemand != nil {
		cfg.SegmentOnDemand = *fileCfg.SegmentOnDemand
	}
	if fileCfg.SegmentSeekAcc != nil {
		cfg.SegmentSeekAcc = *fileCfg.SegmentSeekAcc
	}
	if fileCfg.MaxKeyframeGap != nil {
		cfg.MaxKeyframeGap = *fileCfg.MaxKeyframeGap
	}
	if fileCfg.ForceEncoder != nil {
		cfg.ForceEncoder = *fileCfg.ForceEncoder
	}
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

func safeDirPathBase(baseDir, rel string) (string, string, error) {
	clean := filepath.Clean(rel)
	if clean == "." || clean == string(os.PathSeparator) {
		clean = ""
	}
	abs := baseDir
	if clean != "" {
		abs = filepath.Join(baseDir, clean)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", "", err
	}
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(abs, baseAbs+string(os.PathSeparator)) && abs != baseAbs {
		return "", "", errors.New("illegal path")
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", "", err
	}
	if !st.IsDir() {
		return "", "", errors.New("not a directory")
	}
	relPath, err := filepath.Rel(baseAbs, abs)
	if err != nil {
		return "", "", err
	}
	if relPath == "." {
		relPath = ""
	}
	return abs, filepath.ToSlash(relPath), nil
}

func splitRootPrefix(rel string, roots int) (idx int, sub string, ok bool) {
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) == 0 {
		return 0, "", false
	}
	head := parts[0]
	if len(head) < 2 || head[0] != 'r' {
		return 0, "", false
	}
	n, err := strconv.Atoi(head[1:])
	if err != nil || n < 0 || n >= roots {
		return 0, "", false
	}
	if len(parts) == 2 {
		return n, parts[1], true
	}
	return n, "", true
}

func resolveFilePath(cfg Config, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("empty path")
	}
	roots := cfg.VideoDirs
	if len(roots) == 0 {
		return "", errors.New("no video roots")
	}
	if len(roots) == 1 {
		return safeFilePath(roots[0], rel)
	}
	if idx, sub, ok := splitRootPrefix(rel, len(roots)); ok {
		return safeFilePath(roots[idx], sub)
	}
	var matched string
	matchCount := 0
	for _, root := range roots {
		abs, err := safeFilePath(root, rel)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			matched = abs
			matchCount++
		}
	}
	if matchCount == 1 {
		return matched, nil
	}
	if matchCount == 0 {
		return "", errors.New("not found")
	}
	return "", errors.New("ambiguous path")
}

func resolveDirPath(cfg Config, rel string) (string, string, bool, error) {
	rel = strings.TrimSpace(rel)
	roots := cfg.VideoDirs
	if len(roots) == 0 {
		return "", "", false, errors.New("no video roots")
	}
	if len(roots) > 1 && rel == "" {
		return "", "", true, nil
	}
	if len(roots) == 1 {
		abs, relOut, err := safeDirPathBase(roots[0], rel)
		return abs, relOut, false, err
	}
	if idx, sub, ok := splitRootPrefix(rel, len(roots)); ok {
		abs, subRel, err := safeDirPathBase(roots[idx], sub)
		if err != nil {
			return "", "", false, err
		}
		relOut := fmt.Sprintf("r%d", idx)
		if subRel != "" {
			relOut = relOut + "/" + subRel
		}
		return abs, relOut, false, nil
	}
	var matchedAbs string
	var matchedIdx int
	matchCount := 0
	for i, root := range roots {
		abs, subRel, err := safeDirPathBase(root, rel)
		if err != nil {
			continue
		}
		_ = subRel
		matchedAbs = abs
		matchedIdx = i
		matchCount++
	}
	if matchCount == 1 {
		relOut := fmt.Sprintf("r%d", matchedIdx)
		if rel != "" {
			relOut = relOut + "/" + filepath.ToSlash(filepath.Clean(rel))
		}
		return matchedAbs, relOut, false, nil
	}
	if matchCount == 0 {
		return "", "", false, errors.New("not found")
	}
	return "", "", false, errors.New("ambiguous dir")
}

func joinRel(relDir, name string) string {
	if relDir == "" {
		return name
	}
	return relDir + "/" + name
}

func parentRel(relDir string) string {
	if relDir == "" {
		return ""
	}
	p := path.Dir(relDir)
	if p == "." || p == "/" {
		return ""
	}
	return p
}

func htmlEscape(s string) string {
	return html.EscapeString(s)
}

func isVideoFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".mov", ".webm", ".m4v", ".ts", ".avi", ".mpg", ".mpeg":
		return true
	default:
		return false
	}
}

func handlePlaylist(w http.ResponseWriter, r *http.Request, cfg Config) {
	videoPath := r.URL.Query().Get("path")
	if videoPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	bitrate, err := parseBitrateParam(r, cfg.Bitrate)
	if err != nil {
		http.Error(w, "invalid bitrate", http.StatusBadRequest)
		return
	}
	reqCfg := cfg
	reqCfg.Bitrate = bitrate
	absPath, err := resolveFilePath(cfg, videoPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(absPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	info, err := getVideoInfo(reqCfg, absPath)
	if err != nil {
		http.Error(w, "probe failed", http.StatusInternalServerError)
		return
	}
	applySegmentMode(&reqCfg, info, true)
	audioPart := "audio=none"
	if info.HasAudio {
		audioPart = fmt.Sprintf("audio=codec=%s ch=%d layout=%s", info.AudioCodec, info.AudioCh, info.AudioLayout)
	}
	videoPart := fmt.Sprintf("video=dur=%.3fs fps=%.2f", info.Duration, info.FPS)
	logSimple(reqCfg, "PLAY", "playlist path=%s encoder=%s bitrate=%s %s %s", absPath, reqCfg.Encoder, reqCfg.Bitrate, videoPart, audioPart)
	cacheDir := buildCacheDir(reqCfg, absPath, info)
	playlistPath := filepath.Join(cacheDir, "index.m3u8")
	var playlist string
	if reqCfg.SegmentOnDemand {
		playlist = buildVODPlaylist(videoPath, info.Duration, reqCfg.SegmentDuration)
	} else if reqCfg.SegmentByKey {
		ensureHLSStarted(reqCfg, absPath, cacheDir, playlistPath, info)
		if err := waitForFile(playlistPath, cfg.PlaylistWait); err != nil {
			http.Error(w, "playlist not ready", http.StatusServiceUnavailable)
			return
		}
		var err error
		playlist, err = buildPlaylistFromFile(playlistPath, videoPath)
		if err != nil {
			http.Error(w, "playlist parse failed", http.StatusServiceUnavailable)
			return
		}
	} else {
		ensureHLSStarted(reqCfg, absPath, cacheDir, playlistPath, info)
		playlist = buildVODPlaylist(videoPath, info.Duration, reqCfg.SegmentDuration)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(playlist))
}

func handleSegment(w http.ResponseWriter, r *http.Request, cfg Config) {
	reqStart := time.Now()
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
	bitrate, err := parseBitrateParam(r, cfg.Bitrate)
	if err != nil {
		http.Error(w, "invalid bitrate", http.StatusBadRequest)
		return
	}
	reqCfg := cfg
	reqCfg.Bitrate = bitrate
	absPath, err := resolveFilePath(cfg, videoPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(absPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	info, err := getVideoInfo(reqCfg, absPath)
	if err != nil {
		http.Error(w, "probe failed", http.StatusInternalServerError)
		return
	}
	applySegmentMode(&reqCfg, info, false)
	cacheDir := buildCacheDir(reqCfg, absPath, info)
	playlistPath := filepath.Join(cacheDir, "index.m3u8")
	segmentPath := filepath.Join(cacheDir, fmt.Sprintf("seg_%05d.ts", seg))
	logSimple(reqCfg, "PLAY.SEG", "segment=%d path=%s", seg, absPath)
	if reqCfg.SegmentOnDemand {
		if err := ensureSegmentReady(reqCfg, absPath, cacheDir, segmentPath, seg, info); err != nil {
			http.Error(w, "segment not ready", http.StatusServiceUnavailable)
			return
		}
	} else {
		ensureHLSStarted(reqCfg, absPath, cacheDir, playlistPath, info)
		if err := waitForFile(segmentPath, cfg.SegmentWait); err != nil {
			http.Error(w, "segment not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "video/MP2T")
	tw := &ttfbWriter{
		ResponseWriter: w,
		start:          reqStart,
		onFirstWrite: func(d time.Duration) {
			base, avg, max, count := updateSegmentStats(reqCfg, absPath, d)
			delta := d.Milliseconds() - base
			deltaPct := 0.0
			if base > 0 {
				deltaPct = (float64(d.Milliseconds())/float64(base) - 1) * 100
			}
			logSimple(reqCfg, "PLAY.SEG", "segment=%d path=%s ttfb_ms=%d base_ms=%d avg_ms=%d max_ms=%d count=%d delta_ms=%d delta_pct=%.1f", seg, absPath, d.Milliseconds(), base, avg, max, count, delta, deltaPct)
		},
	}
	serveFile(tw, r, segmentPath)
}

func handleBrowse(w http.ResponseWriter, r *http.Request, cfg Config) {
	dirParam := strings.TrimSpace(r.URL.Query().Get("dir"))
	absDir, relDir, rootList, err := resolveDirPath(cfg, dirParam)
	if err != nil {
		http.Error(w, "invalid dir", http.StatusForbidden)
		return
	}
	if rootList {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var b strings.Builder
		b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
		b.WriteString("<title>Browse</title>")
		b.WriteString("<style>body{font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Helvetica,Arial,sans-serif;margin:20px} ")
		b.WriteString("a{text-decoration:none} .path{color:#666;margin-bottom:12px} ")
		b.WriteString("ul{list-style:none;padding-left:0} li{margin:6px 0}</style>")
		b.WriteString("</head><body>")
		b.WriteString("<h2>Browse</h2>")
		b.WriteString("<div class=\"path\">视频根目录</div>")
		b.WriteString("<ul>")
		for i, root := range cfg.VideoDirs {
			label := root
			base := filepath.Base(root)
			if base != "" && base != "." && base != string(os.PathSeparator) {
				label = base + " — " + root
			}
			b.WriteString("<li>📁 <a href=\"/browse?dir=")
			b.WriteString(url.QueryEscape(fmt.Sprintf("r%d", i)))
			b.WriteString("\">")
			b.WriteString(htmlEscape(label))
			b.WriteString("</a></li>")
		}
		b.WriteString("</ul></body></html>")
		_, _ = w.Write([]byte(b.String()))
		return
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		http.Error(w, "read dir failed", http.StatusInternalServerError)
		return
	}
	type item struct {
		Name string
		Path string
		Dir  bool
	}
	var dirs []item
	var videos []item
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			relPath := joinRel(relDir, name)
			dirs = append(dirs, item{Name: name, Path: relPath, Dir: true})
			continue
		}
		if isVideoFile(name) {
			relPath := joinRel(relDir, name)
			videos = append(videos, item{Name: name, Path: relPath})
		}
	}
	sortItems := func(items []item) {
		sort.Slice(items, func(i, j int) bool {
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		})
	}
	sortItems(dirs)
	sortItems(videos)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	b.WriteString("<title>Browse</title>")
	b.WriteString("<style>body{font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Helvetica,Arial,sans-serif;margin:20px} ")
	b.WriteString("a{text-decoration:none} .path{color:#666;margin-bottom:12px} ")
	b.WriteString("ul{list-style:none;padding-left:0} li{margin:6px 0}</style>")
	b.WriteString("</head><body>")
	b.WriteString("<h2>Browse</h2>")
	b.WriteString("<div class=\"path\">当前目录: ")
	if relDir == "" {
		b.WriteString("/")
	} else {
		b.WriteString(htmlEscape(relDir))
	}
	b.WriteString("</div>")
	if relDir != "" {
		parent := parentRel(relDir)
		b.WriteString("<div><a href=\"/browse?dir=")
		b.WriteString(url.QueryEscape(parent))
		b.WriteString("\">.. 上级目录</a></div>")
	}
	if len(dirs) > 0 {
		b.WriteString("<h3>文件夹</h3><ul>")
		for _, d := range dirs {
			b.WriteString("<li>📁 <a href=\"/browse?dir=")
			b.WriteString(url.QueryEscape(d.Path))
			b.WriteString("\">")
			b.WriteString(htmlEscape(d.Name))
			b.WriteString("</a></li>")
		}
		b.WriteString("</ul>")
	}
	if len(videos) > 0 {
		b.WriteString("<h3>视频</h3><ul>")
		for _, v := range videos {
			b.WriteString("<li>🎬 <a href=\"/video/rttPlaylist?path=")
			b.WriteString(url.QueryEscape(v.Path))
			b.WriteString("\">")
			b.WriteString(htmlEscape(v.Name))
			b.WriteString("</a></li>")
		}
		b.WriteString("</ul>")
	}
	if len(dirs) == 0 && len(videos) == 0 {
		b.WriteString("<div>没有找到文件夹或视频文件</div>")
	}
	b.WriteString("</body></html>")
	_, _ = w.Write([]byte(b.String()))
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

func updateSegmentStats(cfg Config, absPath string, d time.Duration) (baseMs, avgMs, maxMs, count int64) {
	key := buildSegmentStatsKey(cfg, absPath)
	ms := d.Milliseconds()
	segStatsMu.Lock()
	defer segStatsMu.Unlock()
	stat, ok := segStats[key]
	if !ok {
		stat = &segmentStat{}
		segStats[key] = stat
	}
	stat.count++
	stat.sumMs += ms
	stat.last = ms
	if stat.count == 1 {
		stat.base = ms
	}
	if ms > stat.maxMs {
		stat.maxMs = ms
	}
	baseMs = stat.base
	avgMs = stat.sumMs / stat.count
	maxMs = stat.maxMs
	count = stat.count
	return baseMs, avgMs, maxMs, count
}

func buildSegmentStatsKey(cfg Config, absPath string) string {
	mode := "interval"
	if cfg.SegmentOnDemand {
		mode = "ondemand"
	} else if cfg.SegmentByKey {
		mode = "keyframe"
	}
	return fmt.Sprintf("%s|%s|%s|%d", absPath, cfg.Bitrate, mode, cfg.SegmentDuration)
}

func markHardwareDisabled(encoder string) {
	if !isHardwareEncoder(encoder) {
		return
	}
	hwDisableMu.Lock()
	hwDisabled[encoder] = true
	hwDisableMu.Unlock()
}

func applyHardwareFallback(cfg *Config, context string) {
	if cfg.ForceEncoder {
		return
	}
	if !isHardwareEncoder(cfg.Encoder) {
		return
	}
	hwDisableMu.Lock()
	disabled := hwDisabled[cfg.Encoder]
	hwDisableMu.Unlock()
	if !disabled {
		return
	}
	cfg.Encoder = "libx264"
	cfg.Decoder = ""
	logSimple(*cfg, "FFMPEG", "hardware disabled, using libx264 (%s)", context)
}

func validateEncoder(cfg *Config) {
	if cfg.ForceEncoder {
		return
	}
	if !isHardwareEncoder(cfg.Encoder) {
		return
	}
	if probeEncoder(cfg.FFmpegPath, cfg.Encoder) {
		return
	}
	markHardwareDisabled(cfg.Encoder)
	logSimple(*cfg, "FFMPEG", "hardware encoder unavailable: %s", cfg.Encoder)
	if strings.HasPrefix(cfg.Encoder, "hevc_") {
		alt := pickEncoder("h264", cfg.FFmpegPath)
		if alt != "" && alt != "libx264" {
			cfg.Encoder = alt
			logSimple(*cfg, "FFMPEG", "switching to %s", cfg.Encoder)
			return
		}
	}
	cfg.Encoder = "libx264"
	cfg.Decoder = ""
	logSimple(*cfg, "FFMPEG", "switching to libx264")
}

func probeEncoder(ffmpegPath, encoder string) bool {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "lavfi",
		"-i", "testsrc=size=128x72:rate=30",
		"-t", "0.1",
		"-c:v", encoder,
		"-pix_fmt", "nv12",
		"-f", "null",
		"-",
	}
	cmd := exec.Command(ffmpegPath, args...)
	return cmd.Run() == nil
}

type ttfbWriter struct {
	http.ResponseWriter
	start       time.Time
	wroteFirst  bool
	onFirstWrite func(time.Duration)
}

func (w *ttfbWriter) markFirstWrite() {
	if w.wroteFirst {
		return
	}
	w.wroteFirst = true
	if w.onFirstWrite != nil {
		w.onFirstWrite(time.Since(w.start))
	}
}

func (w *ttfbWriter) WriteHeader(statusCode int) {
	w.markFirstWrite()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *ttfbWriter) Write(p []byte) (int, error) {
	w.markFirstWrite()
	return w.ResponseWriter.Write(p)
}

func (w *ttfbWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func buildVODPlaylist(videoPath string, duration float64, segSeconds int) string {
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
		b.WriteString(fmt.Sprintf("/video/rttSegment?path=%s&segment=%d\n", url.QueryEscape(videoPath), i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func buildPlaylistFromFile(playlistPath, videoPath string) (string, error) {
	data, err := os.ReadFile(playlistPath)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	var b strings.Builder
	segIndex := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			b.WriteString(line)
			b.WriteString("\n")
			continue
		}
		b.WriteString(fmt.Sprintf("/video/rttSegment?path=%s&segment=%d\n", url.QueryEscape(videoPath), segIndex))
		segIndex++
	}
	if segIndex == 0 {
		return "", errors.New("playlist has no segments")
	}
	return b.String(), nil
}

func buildCacheDir(cfg Config, absPath string, info VideoInfo) string {
	stat, _ := os.Stat(absPath)
	size := int64(0)
	mtime := int64(0)
	if stat != nil {
		size = stat.Size()
		mtime = stat.ModTime().UnixNano()
	}
	key := fmt.Sprintf("%s|%d|%d|%d|%s|%s|%t|%t|%t|%s", absPath, size, mtime, cfg.SegmentDuration, cfg.Bitrate, cfg.Encoder, cfg.PreferAudioCopy, cfg.SegmentByKey, cfg.SegmentOnDemand, info.AudioCodec)
	h := sha1.Sum([]byte(key))
	return filepath.Join(cfg.CacheDir, hex.EncodeToString(h[:]))
}

func ensureSegmentReady(cfg Config, absPath, cacheDir, segmentPath string, segIndex int, info VideoInfo) error {
	if st, err := os.Stat(segmentPath); err == nil {
		if st.Size() > 0 {
			return nil
		}
		_ = os.Remove(segmentPath)
	}
	segJobsMu.Lock()
	if existing, ok := segJobs[segmentPath]; ok {
		segJobsMu.Unlock()
		<-existing.done
		return existing.err
	}
	j := &job{done: make(chan struct{})}
	segJobs[segmentPath] = j
	segJobsMu.Unlock()

	go func() {
		defer close(j.done)
		jobSem <- struct{}{}
		defer func() { <-jobSem }()

		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			j.err = err
			segJobsMu.Lock()
			delete(segJobs, segmentPath)
			segJobsMu.Unlock()
			return
		}
		segDur := float64(cfg.SegmentDuration)
		if segDur <= 0 {
			segDur = 5
		}
		if info.Duration > 0 {
			total := int(math.Ceil(info.Duration / segDur))
			if segIndex >= total {
				j.err = fmt.Errorf("segment out of range")
				segJobsMu.Lock()
				delete(segJobs, segmentPath)
				segJobsMu.Unlock()
				return
			}
		}
		start := float64(segIndex) * segDur
		duration := segDur
		if info.Duration > 0 {
			total := int(math.Ceil(info.Duration / segDur))
			if segIndex == total-1 {
				last := info.Duration - (segDur * float64(total-1))
				if last > 0 {
					duration = last
				}
			}
		}

		localCfg := cfg
		applyHardwareFallback(&localCfg, "segment")
		tmpPath := segmentPath + ".tmp"
		args := buildFFmpegSegmentCommand(localCfg, absPath, tmpPath, start, duration, info)
		logDetail(cfg, "FFMPEG", "seg cmd: %s %s", cfg.FFmpegPath, strings.Join(args, " "))
		err := runFFmpeg(localCfg, args)
		if err != nil && !localCfg.SegmentByKey && isHardwareEncoder(localCfg.Encoder) && localCfg.Decoder != "" {
			_ = os.Remove(tmpPath)
			logSimple(cfg, "FFMPEG", "segment hw decode failed, retrying with software decode: %v", err)
			fallback := localCfg
			fallback.Decoder = ""
			args = buildFFmpegSegmentCommand(fallback, absPath, tmpPath, start, duration, info)
			logDetail(fallback, "FFMPEG", "seg cmd: %s %s", fallback.FFmpegPath, strings.Join(args, " "))
			err = runFFmpeg(fallback, args)
		}
		if !localCfg.ForceEncoder && err != nil && !localCfg.SegmentByKey && isHardwareEncoder(localCfg.Encoder) {
			_ = os.Remove(tmpPath)
			markHardwareDisabled(localCfg.Encoder)
			logSimple(cfg, "FFMPEG", "segment hw failed, retrying with libx264: %v", err)
			fallback := localCfg
			fallback.Encoder = "libx264"
			fallback.Decoder = ""
			args = buildFFmpegSegmentCommand(fallback, absPath, tmpPath, start, duration, info)
			logDetail(fallback, "FFMPEG", "seg cmd: %s %s", fallback.FFmpegPath, strings.Join(args, " "))
			err = runFFmpeg(fallback, args)
		}
		if err == nil {
			if renameErr := os.Rename(tmpPath, segmentPath); renameErr != nil {
				err = renameErr
			}
		} else {
			_ = os.Remove(tmpPath)
		}
		j.err = err
		segJobsMu.Lock()
		delete(segJobs, segmentPath)
		segJobsMu.Unlock()
	}()

	<-j.done
	return j.err
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

			applyHardwareFallback(&cfg, "hls")
			logSimple(cfg, "FFMPEG", "start path=%s encoder=%s bitrate=%s", absPath, cfg.Encoder, cfg.Bitrate)
			if err := os.MkdirAll(cacheDir, 0o755); err != nil {
				j.err = err
			jobsMu.Lock()
			delete(jobs, cacheDir)
			jobsMu.Unlock()
			logSimple(cfg, "FFMPEG", "start failed: %v", err)
			return
		}
		_ = writePlaylistMeta(cacheDir, playlistMeta{
			Duration: info.Duration,
			Segment:  cfg.SegmentDuration,
		})
		cmd := buildFFmpegCommand(cfg, absPath, cacheDir, playlistPath, info)
		logDetail(cfg, "FFMPEG", "cmd: %s %s", cfg.FFmpegPath, strings.Join(cmd, " "))
			if err := runFFmpeg(cfg, cmd); err != nil {
				if !cfg.SegmentByKey && isHardwareEncoder(cfg.Encoder) && cfg.Decoder != "" {
					logSimple(cfg, "FFMPEG", "hardware decode failed, retrying with software decode: %v", err)
					fallback := cfg
					fallback.Decoder = ""
					cmd = buildFFmpegCommand(fallback, absPath, cacheDir, playlistPath, info)
					logDetail(fallback, "FFMPEG", "cmd: %s %s", fallback.FFmpegPath, strings.Join(cmd, " "))
					if err2 := runFFmpeg(fallback, cmd); err2 == nil {
						logSimple(fallback, "FFMPEG", "done (fallback): %s", absPath)
						return
					} else {
						err = err2
					}
				}
				if !cfg.ForceEncoder && !cfg.SegmentByKey && isHardwareEncoder(cfg.Encoder) {
					markHardwareDisabled(cfg.Encoder)
					logSimple(cfg, "FFMPEG", "hardware failed, retrying with libx264: %v", err)
					fallback := cfg
					fallback.Encoder = "libx264"
					cmd = buildFFmpegCommand(fallback, absPath, cacheDir, playlistPath, info)
					logDetail(fallback, "FFMPEG", "cmd: %s %s", fallback.FFmpegPath, strings.Join(cmd, " "))
					if err2 := runFFmpeg(fallback, cmd); err2 == nil {
						logSimple(fallback, "FFMPEG", "done (fallback): %s", absPath)
						return
					} else {
						err = err2
					}
				}
				j.err = err
				jobsMu.Lock()
				delete(jobs, cacheDir)
			jobsMu.Unlock()
			logSimple(cfg, "FFMPEG", "failed: %v", err)
			return
		}
		_ = markPlaylistDone(cacheDir)
		logSimple(cfg, "FFMPEG", "done: %s", absPath)
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
	if cfg.SegmentByKey {
		args = append(args, "-c:v", "copy")
		if bsf := annexBFilter(info.VideoCodec); bsf != "" {
			args = append(args, "-bsf:v", bsf)
		}
	} else {
		args = append(args,
			"-c:v", cfg.Encoder,
			"-b:v", cfg.Bitrate,
		)
		if strings.Contains(cfg.Encoder, "videotoolbox") {
			args = append(args, "-allow_sw", "1")
			args = append(args, "-pix_fmt", "nv12")
		}
		if !isHardwareEncoder(cfg.Encoder) {
			if cfg.ForceKeyFrames {
				args = append(args, "-flags", "+cgop")
				args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", cfg.SegmentDuration))
			}
			if cfg.DisableSceneCut {
				args = append(args, "-sc_threshold", "0")
			}
		}
		if info.FPS > 0 {
			gop := int(math.Round(info.FPS * float64(cfg.SegmentDuration)))
			if gop > 0 {
				args = append(args, "-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop))
			}
		}
	}

	if info.HasAudio {
		if cfg.PreferAudioCopy && info.AudioCodec == "aac" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", cfg.AudioBitrate, "-af", "aresample=async=1:min_hard_comp=0.1:first_pts=0")
			if info.AudioCh > 0 {
				args = append(args, "-ac", strconv.Itoa(info.AudioCh))
			}
			if info.AudioLayout != "" {
				args = append(args, "-channel_layout", info.AudioLayout)
			}
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

func buildFFmpegSegmentCommand(cfg Config, inputPath, outputPath string, startSec, durSec float64, info VideoInfo) []string {
	coarse := startSec
	fine := 0.0
	if cfg.SegmentSeekAcc {
		coarse = math.Floor(startSec)
		if coarse < 0 {
			coarse = 0
		}
		fine = startSec - coarse
		if fine < 0 {
			fine = 0
		}
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-ss", formatSeconds(coarse),
	}
	if cfg.Decoder != "" {
		args = append(args, "-hwaccel", cfg.Decoder)
	}
	args = append(args, "-i", inputPath)
	if cfg.SegmentSeekAcc {
		args = append(args, "-ss", formatSeconds(fine))
	}
	args = append(args, "-t", formatSeconds(durSec))
	if cfg.SegmentByKey {
		args = append(args, "-c:v", "copy")
		if bsf := annexBFilter(info.VideoCodec); bsf != "" {
			args = append(args, "-bsf:v", bsf)
		}
	} else {
		args = append(args,
			"-c:v", cfg.Encoder,
			"-b:v", cfg.Bitrate,
		)
		if strings.Contains(cfg.Encoder, "videotoolbox") {
			args = append(args, "-allow_sw", "1")
			//args = append(args, "-pix_fmt", "nv12")
		}
		if !isHardwareEncoder(cfg.Encoder) {
			if cfg.ForceKeyFrames {
				args = append(args, "-flags", "+cgop")
				args = append(args, "-force_key_frames", "0")
			}
			if cfg.DisableSceneCut {
				args = append(args, "-sc_threshold", "0")
			}
		}
	}

	if info.HasAudio {
		if cfg.PreferAudioCopy && info.AudioCodec == "aac" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", cfg.AudioBitrate, "-af", "aresample=async=1:min_hard_comp=0.1:first_pts=0")
			if info.AudioCh > 0 {
				args = append(args, "-ac", strconv.Itoa(info.AudioCh))
			}
			if info.AudioLayout != "" {
				args = append(args, "-channel_layout", info.AudioLayout)
			}
		}
	} else {
		args = append(args, "-an")
	}

	args = append(args,
		"-avoid_negative_ts", "make_zero",
		"-reset_timestamps", "1",
		"-f", "mpegts",
		outputPath,
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

	cmd := exec.Command(cfg.FFprobePath, "-v", "error", "-show_entries", "format=duration", "-show_entries", "stream=codec_type,codec_name,avg_frame_rate,channels,channel_layout", "-of", "json", absPath)
	out, err := cmd.Output()
	if err != nil {
		return VideoInfo{}, err
	}
	var parsed struct {
		Streams []struct {
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			AvgFrameRate  string `json:"avg_frame_rate"`
			Channels      int    `json:"channels"`
			ChannelLayout string `json:"channel_layout"`
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
			if info.AudioCh == 0 {
				info.AudioCh = s.Channels
			}
			if info.AudioLayout == "" {
				info.AudioLayout = s.ChannelLayout
			}
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec = s.CodecName
			}
			if info.FPS == 0 && s.AvgFrameRate != "" {
				info.FPS = parseRational(s.AvgFrameRate)
			}
		}
	}
	if cfg.MaxKeyframeGap > 0 {
		info.MaxKFGap = probeMaxKeyframeGap(cfg, absPath)
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

func formatSeconds(v float64) string {
	if v < 0 {
		v = 0
	}
	return fmt.Sprintf("%.3f", v)
}

func applySegmentByKey(cfg *Config, info VideoInfo, emitLog bool) {
	if !cfg.SegmentByKey {
		return
	}
	if isKeyframeCopyCodec(info.VideoCodec) {
		return
	}
	if emitLog {
		logSimple(*cfg, "PLAY", "segment_by_keyframe disabled codec=%s", info.VideoCodec)
	}
	cfg.SegmentByKey = false
}

func applySegmentMode(cfg *Config, info VideoInfo, emitLog bool) {
	if cfg.MaxKeyframeGap > 0 && info.MaxKFGap > float64(cfg.MaxKeyframeGap) {
		if emitLog {
			logSimple(*cfg, "PLAY", "max_keyframe_interval exceeded gap=%.3fs limit=%ds -> keyframe mode", info.MaxKFGap, cfg.MaxKeyframeGap)
		}
		cfg.SegmentOnDemand = false
		cfg.SegmentByKey = true
	}
	if cfg.SegmentOnDemand && cfg.SegmentByKey {
		if emitLog {
			logSimple(*cfg, "PLAY", "segment_by_keyframe disabled for on_demand")
		}
		cfg.SegmentByKey = false
	}
	applySegmentByKey(cfg, info, emitLog)
}

func isKeyframeCopyCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "h264", "avc1", "hevc", "h265", "hev1":
		return true
	default:
		return false
	}
}

func annexBFilter(codec string) string {
	switch strings.ToLower(codec) {
	case "h264", "avc1":
		return "h264_mp4toannexb"
	case "hevc", "h265", "hev1":
		return "hevc_mp4toannexb"
	default:
		return ""
	}
}

func probeMaxKeyframeGap(cfg Config, absPath string) float64 {
	cmd := exec.Command(cfg.FFprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries", "frame=pkt_pts_time",
		"-of", "csv=p=0",
		absPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(string(out), "\n")
	var prevSet bool
	var prev float64
	var maxGap float64
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, err := strconv.ParseFloat(line, 64)
		if err != nil {
			continue
		}
		if prevSet {
			gap := v - prev
			if gap > maxGap {
				maxGap = gap
			}
		}
		prev = v
		prevSet = true
	}
	return maxGap
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

var bitrateRe = regexp.MustCompile(`^\d+(k|m|K|M)?$`)

func parseBitrateParam(r *http.Request, def string) (string, error) {
	v := strings.TrimSpace(r.URL.Query().Get("bitrate"))
	if v == "" {
		return def, nil
	}
	if !bitrateRe.MatchString(v) {
		return "", fmt.Errorf("invalid bitrate")
	}
	return v, nil
}

func logLine(module, msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	log.Printf("[%s][%s] %s", ts, module, msg)
}

func logSimple(cfg Config, module, format string, args ...interface{}) {
	if !cfg.LogSimple && !cfg.LogDetail {
		return
	}
	logLine(module, fmt.Sprintf(format, args...))
}

func logDetail(cfg Config, module, format string, args ...interface{}) {
	if !cfg.LogDetail {
		return
	}
	logLine(module, fmt.Sprintf(format, args...))
}

func fatalf(module, format string, args ...interface{}) {
	logLine(module, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func withAuth(cfg Config, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(cfg, w, r) {
			return
		}
		h(w, r)
	}
}

func checkAuth(cfg Config, w http.ResponseWriter, r *http.Request) bool {
	if !cfg.BasicAuthOn {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok ||
		subtle.ConstantTimeCompare([]byte(user), []byte(cfg.BasicAuthUser)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.BasicAuthPass)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="RTT"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
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

func detectDecoder(ffmpegPath string) (decoder string) {
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
	return decoder
}

func availableEncoders(ffmpegPath string) map[string]bool {
	out := map[string]bool{}
	if data, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output(); err == nil {
		text := string(data)
		for _, enc := range []string{
			"hevc_videotoolbox", "hevc_nvenc", "hevc_qsv", "hevc_amf", "libx265",
			"h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_amf", "libx264",
		} {
			if strings.Contains(text, enc) {
				out[enc] = true
			}
		}
	}
	return out
}

func pickEncoder(prefer string, ffmpegPath string) string {
	p := strings.ToLower(strings.TrimSpace(prefer))
	if p == "" || p == "auto" {
		p = "h265"
	}
	avail := availableEncoders(ffmpegPath)
	choose := func(candidates []string, fallback string) string {
		for _, c := range candidates {
			if avail[c] {
				return c
			}
		}
		return fallback
	}
	switch p {
	case "h265", "hevc":
		return choose([]string{"hevc_videotoolbox", "hevc_nvenc", "hevc_qsv", "hevc_amf"}, "libx265")
	case "h264", "avc":
		return choose([]string{"h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_amf"}, "libx264")
	default:
		return prefer
	}
}

func isHardwareEncoder(encoder string) bool {
	switch encoder {
	case "h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_amf",
		"hevc_videotoolbox", "hevc_nvenc", "hevc_qsv", "hevc_amf":
		return true
	default:
		return false
	}
}

func init() {
	flag.CommandLine.SetOutput(io.Discard)
	log.SetFlags(0)
}
