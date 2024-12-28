const express = require('express');
const { execSync, exec, spawn } = require('child_process');
const path = require('path');
const fs = require('fs');
const retrieve = require("retrieve-keyframes").get
const { URL } = require('url');

const app = express();
const port = 8082;

var bitrate = '4567k';
var decoder = '';
var encoder = 'h264';
var runtimeDir = process.cwd();
var subsetSubtitlesCache = {};
const maxTranscodingTasks = 10;

var currentTranscodingTasks = 0;

// 初始化运行时目录
function init() {
	const args = process.argv.slice(2);

	// 尝试使用第一个参数作为运行时目录
	if (args.length > 0 && args[0] !== '--st') {
		runtimeDir = args[0];

		try {
			const stats = fs.statSync(runtimeDir);
			if (!stats.isDirectory()) {
				console.warn(`提供的运行时目录 "${runtimeDir}" 不是一个有效的目录, 使用当前工作目录代替.`);
				runtimeDir = process.cwd();
			}
		} catch (err) {
			console.warn(`提供的运行时目录 "${runtimeDir}" 不存在, 使用当前工作目录代替.`);
			runtimeDir = process.cwd();
		}

	} else {
		runtimeDir = process.cwd();
	}

	console.log(`当前运行目录: ${runtimeDir}`);
	if (!runtimeDir) {
		console.error("未检测到有效的运行时目录.");
		process.exit(1);
	}
}
// 同步检测硬件加速支持
function detectHardwareAcceleration() {
	try {
		const cmdDec = 'ffmpeg -hide_banner -hwaccels';
		const decodersOutput = execSync(cmdDec, { encoding: 'utf-8' });
		const availableDecoders = [
			{ name: 'videotoolbox', log: '检测到 Apple VideoToolbox 解码器支持.' },
			{ name: 'cuda', log: '检测到 NVIDIA CUDA 解码器支持.' },
			{ name: 'qsv', log: '检测到 Intel QSV 解码器支持.' },
			{ name: 'amf', log: '检测到 AMD AMF 解码器支持.' },
		];

		for (const decoderInfo of availableDecoders) {
			if (decodersOutput.includes(decoderInfo.name)) {
				decoder = decoderInfo.name;
				console.log(decoderInfo.log);
				break;
			}
		}

		if (decoder === '') {
			console.log('未检测到硬件解码器, 使用默认的软件解码器.');
		}

	} catch (err) {
		console.error(`无法执行 FFmpeg 以检查解码器支持: ${err.message}`);
		console.log('使用默认的软件解码器.');
	}

	try {
		const cmdEnc = 'ffmpeg -hide_banner -encoders';
		const encodersOutput = execSync(cmdEnc, { encoding: 'utf-8' });
		const availableEncoders = [
			{ name: 'h264_videotoolbox', log: '检测到 Apple VideoToolbox 编码器支持.' },
			{ name: 'h264_nvenc', log: '检测到 NVIDIA NVENC 编码器支持.' },
			{ name: 'h264_qsv', log: '检测到 Intel QSV 编码器支持.' },
			{ name: 'h264_amf', log: '检测到 AMD AMF 编码器支持.' },
		];

		for (const encoderInfo of availableEncoders) {
			if (encodersOutput.includes(encoderInfo.name)) {
				encoder = encoderInfo.name;
				console.log(encoderInfo.log);
				break;
			}
		}

		if (encoder === 'h264') {
			console.log('未检测到硬件编码器, 使用默认的软件编码器.');
		}
	} catch (err) {
		console.error(`无法执行 FFmpeg 以检查编码器支持: ${err.message}`);
		console.log('使用默认的软件编码器.');
	}
}

// 安全构造文件路径
function safeFilePath(baseDir, relativePath) {
	try {
		const absolutePath = fs.realpathSync(path.join(baseDir, relativePath));
		if (!absolutePath.startsWith(baseDir)) {
			return Promise.reject('非法路径访问.');
		}
		return Promise.resolve(absolutePath)
	}
	catch (err) {
		return Promise.reject(`路径解析失败: ${err.message}`);
	}
}


function round(num, precision = 4) {
	return (Math.round(num * Math.pow(10, precision)) / Math.pow(10, precision));
}

function roundDown(num, precision = 4) {
	return (Math.floor(num * Math.pow(10, precision)) / Math.pow(10, precision));
}

function roundUp(num, precision = 4) {
	return (Math.ceil(num * Math.pow(10, precision)) / Math.pow(10, precision));
}

// 获取视频文件信息
function getVideoInfo(videoPath) {
	return new Promise((resolve, reject) => {
		const cmd = `ffprobe -v error -show_entries format=duration -show_entries stream=index,codec_type,duration,time_base -of json ${videoPath}`;
		exec(cmd, (err, stdout, stderr) => {
			if (err) {
				console.error(`无法获取视频信息: ${err.message}`);
				reject(`无法获取视频信息, 见控制台日志.`);
				return;
			}

			try {
				const info = JSON.parse(stdout);
				let duration = -1;
				let formatDuration = -1;
				let hasAudio = 0;
				let timeBase = 0.001;

				// 提取 format duration
				if (info.format && info.format.duration) {
					formatDuration = info.format.duration;
				}
				
				// 提取 stream 信息
				if (info.streams) {
					for (const stream of info.streams) {
						if (stream.index === 0 && stream.codec_type === "video") {
							if (stream.duration) {
								duration = stream.duration;
							}
							if (stream.time_base) {
								const [numerator, denominator] = stream.time_base.split('/').map(Number);
								if (!isNaN(numerator) && !isNaN(denominator)) {
									timeBase = (numerator / denominator);
								}
							}
						}
						if (stream.index === 1 && stream.codec_type === "audio") {
							hasAudio = 1;
						}
					}
				}

				// 使用 format duration 作为备选
				if (duration === -1 && formatDuration !== -1) {
					duration = formatDuration;
				}

				if (duration !== -1) {
					duration = parseFloat(duration);
					if (isNaN(duration)) {
						duration = -1;
					} else {
						duration = round(duration);
					}
				}

				if (duration === -1) {
					reject("无法获取视频长度.");
					return;
				}

				resolve({ duration, hasAudio, timeBase });
			} catch (parseError) {
				console.error("解析 JSON 出错:", parseError);
				reject("解析视频信息出错, 见控制台日志.");
				return;
			}
		});
	});
}

function getVideoKeyframesTimestamp(videoPath, timeBase) {
	return new Promise((resolve, reject) => {
		retrieve(videoPath, (videoPath.match(/\.mkv/) ? "mkv" : "mp4"), function (err, frames) {
			if (err) {
				console.error(`无法获取视频关键帧: ${err.message}`);
				reject(`无法获取视频关键帧, 见控制台日志.`);
				return;
			}
			let videoTimestamps = [];
			frames.forEach((v) => {
				videoTimestamps.push(roundUp(v.pts * timeBase));
			});
			resolve(videoTimestamps);
		});
	});
}

function readSubtitle(videoPath) {
	return new Promise((resolve, reject) => {
		// 获取视频文件所在的目录和文件名 (不带扩展名).
		const videoDir = path.dirname(videoPath);
		const videoName = path.basename(videoPath, path.extname(videoPath));
		const videoExt = path.extname(videoPath);

		let subtitleFileContent = null;

		// 检查字幕文件是否存在.

		fs.readFile(path.join(videoDir, `${videoName}.ass`), 'utf-8', function (err, data) {
			if (!err) {
				resolve({ videoName, data });
				return;
			}
			fs.readFile(path.join(videoDir, `${videoName}${videoExt}.ass`), 'utf-8', function (err, data) {
				if (!err) {
					resolve({ videoName, data });
					return;
				}
				reject(null);
			});
		});
	});
}

function prepareSubsetSubtitle(videoName, subtitleContent) {
	return new Promise((resolve, reject) => {
		resolve();
	});
}

// 生成 M3U8 播放列表
function generatePlaylist(videoPath, videoDuration, videoHasAudio, videoTimestamps) {
	const encodedVideoPath = encodeURIComponent(videoPath);
	const baseURL = `/video/rttSegment?path=${encodedVideoPath}&audio=${videoHasAudio}`;
	let playlist = `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2333\n#EXT-X-PLAYLIST-TYPE:VOD\n`;
	for (let i = 0; i < videoTimestamps.length; i++) {
		if (videoTimestamps.length === (i + 1)) {
			let segmentDuration = round(videoDuration - videoTimestamps[i]);
			playlist += `#EXTINF:${segmentDuration},\n${baseURL}&start=${videoTimestamps[i]}&duration=${segmentDuration}\n`;
			break;
		}
		let segmentDuration = round(videoTimestamps[i + 1] - videoTimestamps[i]);
		playlist += `#EXTINF:${segmentDuration},\n${baseURL}&start=${videoTimestamps[i]}&duration=${segmentDuration}\n`;
	}
	playlist += "#EXT-X-ENDLIST\n";
	return playlist;
}

// 实时转码
function transcode(videoPath, startTime, durationTime, videoHasAudio) {
	return new Promise((resolve, reject) => {
		// 检查是否达到最大转码任务数
		if (currentTranscodingTasks >= maxTranscodingTasks) {
			reject("转码任务已满, 请稍后重试.");
			return;
		}
		currentTranscodingTasks++;

		const startTimeStr = String(round(startTime));
		const delayTimeStr = String(round(startTime / 2));
		const durationTimeStr = String(round(durationTime));
		console.log(startTime);
		console.log(startTimeStr);
		console.log(startTime/2);
		console.log(delayTimeStr);
		console.log(durationTime);
		console.log(durationTimeStr);
		let args = [
			'-ss', startTimeStr,
			'-t', durationTimeStr,
			'-accurate_seek',
			'-i', videoPath,
			'-map', '0:v:0',
			'-c:v', encoder,
			'-b:v', String(bitrate),
			'-bsf:v', 'h264_mp4toannexb',
			'-avoid_negative_ts', 'make_zero',
			'-start_at_zero',
			'-muxdelay', delayTimeStr,
			'-muxpreload', delayTimeStr,
			'-f', 'mpegts',
			'pipe:1'
		];

		if (decoder !== '') {
			args.unshift('-hwaccel', decoder);
		}

		if (videoHasAudio) {
			args.splice(args.indexOf('-bsf:v') + 2, 0,
				'-map', '0:a:0',
				'-c:a', 'aac',
				'-b:a', '256k'
			);
		}

		const ffmpegProcess = spawn('ffmpeg', args, { encoding: 'buffer' });

		const outputBuffer = [];
		const errorBuffer = [];

		ffmpegProcess.stdout.on('data', (data) => {
			outputBuffer.push(data);
		});

		ffmpegProcess.stderr.on('data', (data) => {
			errorBuffer.push(data);
		});

		ffmpegProcess.on('close', (code) => {
			currentTranscodingTasks--;
			const err = Buffer.concat(errorBuffer).toString();
			if (code !== 0) {
				console.error(`转码失败 (Code: ${code}): ${err || 'FFmpeg 执行失败'}`);
				return reject(`转码失败, 见控制台日志.`);
			}
			resolve(Buffer.concat(outputBuffer));
		});

		ffmpegProcess.on('error', (err) => {
			currentTranscodingTasks--;
			console.error(`转码过程发生错误: ${err}`);
			return reject(`转码失败, 见控制台日志.`);
		});
	});
}

// 处理默认请求
app.get('/', (req, res) => {
	res.status(404).send("404 Not Found.");
});

// 处理播放列表请求
app.get('/video/rttPlaylist', async (req, res) => {
	const videoPath = req.query.path;

	if (!videoPath) {
		return res.status(400).send("缺少必要参数.");
	}
	try {
		const absPath = await safeFilePath(runtimeDir, videoPath);

		if (!fs.existsSync(absPath)) {
			return res.status(404).send("视频文件不存在.");
		}

		const { duration, hasAudio, timeBase } = await getVideoInfo(absPath);
		const timestamps = await getVideoKeyframesTimestamp(absPath, timeBase);
		readSubtitle(videoPath).then(function ({ videoName, subtitleContent }) {
			prepareSubsetSubtitle(videoName.toLowerCase(), subtitleContent);
		}).catch(function () {
		});
		const playlist = generatePlaylist(videoPath, duration, hasAudio, timestamps);
		res.set('Content-Type', 'application/vnd.apple.mpegurl');
		res.send(playlist);
	} catch (err) {
		res.status(403).send(err);
	}
});

// 处理分片请求
app.get('/video/rttSegment', async (req, res) => {
	const videoPath = req.query.path;
	const videoStartTimeStr = req.query.start;
	const videoDurationTimeStr = req.query.duration;
	const videoHasAudio = req.query.audio === '1';

	if (!videoPath || !videoStartTimeStr || !videoDurationTimeStr) {
		return res.status(400).send("缺少必要参数.");
	}
	videoStartTime = parseFloat(videoStartTimeStr);
	videoDurationTime = parseFloat(videoDurationTimeStr);
	if (isNaN(videoStartTime) || isNaN(videoDurationTime)) {
		return res.status(400).send("无效的时间参数.");
	}

	try {
		const absPath = await safeFilePath(runtimeDir, videoPath);
		const buffer = await transcode(absPath, videoStartTime, videoDurationTime, videoHasAudio);
		res.set('Content-Type', 'video/MP2T');
		res.set('Content-Length', String(buffer.length));
		res.send(buffer);
	} catch (err) {
		res.status(500).send(err);
	}
});

// 启动服务器
init();
detectHardwareAcceleration();

app.listen(port, () => {
	console.log(`服务已启动, 监听端口 ${port}...`);
});
