import 'dart:async';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:media_kit/media_kit.dart';
import 'package:media_kit/src/models/player_log.dart';
import 'package:media_kit/src/models/player_stream.dart';
import 'package:media_kit/src/player/platform_player.dart';
import 'package:video_player/video_player.dart';
import 'diary_service.dart';

/// 模拟与 media_kit.PlayerStream 接口对齐的流集合
class LunaPlayerStreams implements PlayerStream {
  final StreamController<Duration> positionController = StreamController<Duration>.broadcast();
  final StreamController<Duration> durationController = StreamController<Duration>.broadcast();
  final StreamController<Duration> bufferController = StreamController<Duration>.broadcast();
  final StreamController<bool> playingController = StreamController<bool>.broadcast();
  final StreamController<bool> bufferingController = StreamController<bool>.broadcast();
  final StreamController<bool> completedController = StreamController<bool>.broadcast();
  final StreamController<String> errorController = StreamController<String>.broadcast();
  final StreamController<double> volumeController = StreamController<double>.broadcast();
  final StreamController<double> rateController = StreamController<double>.broadcast();
  final StreamController<VideoParams> videoParamsController = StreamController<VideoParams>.broadcast();
  final StreamController<PlayerLog> logController = StreamController<PlayerLog>.broadcast();
  final StreamController<double> pitchController = StreamController<double>.broadcast();

  @override
  Stream<Duration> get position => positionController.stream;

  @override
  Stream<Duration> get duration => durationController.stream;

  @override
  Stream<Duration> get buffer => bufferController.stream;

  @override
  Stream<bool> get playing => playingController.stream;

  @override
  Stream<bool> get buffering => bufferingController.stream;

  @override
  Stream<bool> get completed => completedController.stream;

  @override
  Stream<String> get error => errorController.stream;

  @override
  Stream<double> get volume => volumeController.stream;

  @override
  Stream<double> get rate => rateController.stream;

  @override
  Stream<VideoParams> get videoParams => videoParamsController.stream;

  @override
  Stream<PlayerLog> get log => logController.stream;

  @override
  Stream<double> get pitch => pitchController.stream;

  void dispose() {
    positionController.close();
    durationController.close();
    bufferController.close();
    playingController.close();
    bufferingController.close();
    completedController.close();
    errorController.close();
    volumeController.close();
    rateController.close();
    videoParamsController.close();
    logController.close();
    pitchController.close();
  }

  @override
  dynamic noSuchMethod(Invocation invocation) {
    if (invocation.isGetter) {
      return const Stream<dynamic>.empty();
    }
    return null;
  }
}

/// 采用和 LunaTV-Mobile 完全相同架构的 ExoPlayer (AndroidX Media3) 播放器封装，
/// 同时对上层红果鉴播控 UI (PlayerControls & TelevisionControls) 暴露完全兼容的 Player 契约。
class LunaExoPlayer implements Player {
  LunaExoPlayer({this.probeSource}) {
    state = const PlayerState().copyWith(volume: 100.0);
  }

  /// 可选的原生代理探针读取器，用于播放失败后记录代理实际往返。
  final Future<List<String>> Function()? probeSource;

  VideoPlayerController? _controller;
  VoidCallback? _valueListener;
  Timer? _positionPollTimer;

  int _openGeneration = 0;
  bool _disposed = false;

  final ValueNotifier<int> revision = ValueNotifier<int>(0);

  @override
  PlayerState state = const PlayerState();

  @override
  final LunaPlayerStreams stream = LunaPlayerStreams();

  @override
  PlatformPlayer? get platform => null;

  @override
  Future<int> get handle => Future<int>.value(0);

  VideoPlayerController? get controller => _controller;

  @override
  Future<void> open(Playable playable, {bool play = true}) async {
    if (_disposed) return;
    if (playable is! Media) return;

    final media = playable;
    final url = media.uri;
    if (url.isEmpty) return;

    final myGen = ++_openGeneration;
    _positionPollTimer?.cancel();

    // 切换新节目时，旧控制器先静音/暂停，彻底防止新旧双重音频重叠
    final old = _controller;
    if (old != null) {
      if (_valueListener != null) {
        try {
          old.removeListener(_valueListener!);
        } catch (_) {}
        _valueListener = null;
      }
      try {
        await old.pause();
      } catch (_) {}
    }

    state = state.copyWith(completed: false);
    _safeAdd(stream.completedController, false);

    final Map<String, String> headers = <String, String>{};
    try {
      if (media.httpHeaders != null) {
        headers.addAll(Map<String, String>.from(media.httpHeaders!));
      }
    } catch (_) {}
    headers.removeWhere((key, _) => key.toLowerCase() == 'accept-encoding');

    // 针对防盗链 CDN 注入标准移动端 UA
    if (!headers.keys.any((k) => k.toLowerCase() == 'user-agent')) {
      headers['User-Agent'] =
          'Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36';
    }

    final lowerUrl = url.toLowerCase();
    final pathOnly = lowerUrl.split('?').first.split('#').first;
    bool isHls = pathOnly.endsWith('.m3u8') ||
        pathOnly.endsWith('.m3u') ||
        (headers['accept']?.toLowerCase().contains('mpegurl') ?? false);

    final uri = Uri.tryParse(url);
    if (uri == null && !url.startsWith('/')) {
      DiaryService.add('[ExoPlayer] 错误: 无效的播放地址 $url');
      throw Exception('无效的播放地址: $url');
    }

    // 先导探测：如果尚未显式判定为 HLS，向网络地址快速嗅探前置响应头与内容，智能识别
    if (!isHls && uri != null && (uri.scheme == 'http' || uri.scheme == 'https')) {
      try {
        final client = HttpClient()..connectionTimeout = const Duration(seconds: 3);
        final req = await client.getUrl(uri);
        headers.forEach((k, v) => req.headers.set(k, v));
        req.headers.set('Range', 'bytes=0-512');
        final resp = await req.close();
        final cType = resp.headers.contentType?.toString().toLowerCase() ?? '';
        final chunks = await resp.take(1).toList();
        final firstChunk = chunks.isNotEmpty ? String.fromCharCodes(chunks.first) : '';
        DiaryService.add('[Sniff] 探测结果: status=${resp.statusCode}, contentType=$cType, prefix=${firstChunk.length > 20 ? firstChunk.substring(0, 20) : firstChunk}');
        if (cType.contains('mpegurl') || firstChunk.contains('#EXTM3U')) {
          isHls = true;
          DiaryService.add('[Sniff] 自动识别为 HLS 流，注入 formatHint: VideoFormat.hls');
        }
        client.close(force: true);
      } catch (e) {
        DiaryService.add('[Sniff] 探测警告 (非致命): $e');
      }
    }

    VideoFormat? formatHint = isHls ? VideoFormat.hls : null;

    DiaryService.add('[ExoPlayer] open(gen=$myGen): url=$url, isHls=$isHls, formatHint=$formatHint, headers=${headers.keys.toList()}');

    VideoPlayerController c;
    if (url.startsWith('/') || (uri != null && uri.scheme == 'file')) {
      final filePath = uri != null && uri.scheme == 'file' ? uri.toFilePath() : url;
      DiaryService.add('[ExoPlayer] 本地文件播放: $filePath');
      c = VideoPlayerController.file(
        File(filePath),
        videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
      );
    } else {
      c = VideoPlayerController.networkUrl(
        uri!,
        formatHint: formatHint,
        httpHeaders: headers,
        videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
      );
    }

    try {
      DiaryService.add('[ExoPlayer] c.initialize() 开始 (formatHint=$formatHint)...');
      await c.initialize();
      DiaryService.add(
          '[ExoPlayer] c.initialize() 成功! duration=${c.value.duration}, size=${c.value.size}, isInitialized=${c.value.isInitialized}');
    } catch (e, stack) {
      DiaryService.add('[ExoPlayer] 首次 initialize 失败: $e');
      DiaryService.add('[ExoPlayer] controller 状态: ${c.value.errorDescription ?? "无附加信息"}');
      // 失败瞬间回查本机代理，记录播放器实际会看到的响应，便于定位是代理
      // 返回了错误状态还是播放器自身解析失败。
      await _probeProxy(uri, headers);
      await _dumpNativeProbe();
      // 如果首次尝试失败，且为网络视频，则原地使用交替格式（HLS <-> MP4）自愈重试
      if (uri != null && (uri.scheme == 'http' || uri.scheme == 'https')) {
        final alternateFormat = formatHint == VideoFormat.hls ? null : VideoFormat.hls;
        DiaryService.add('[ExoPlayer] 启动自愈重试，切换格式为 formatHint=$alternateFormat...');
        try {
          await c.dispose();
        } catch (_) {}

        final retryHeaders = Map<String, String>.from(headers);
        if (alternateFormat == VideoFormat.hls) {
          retryHeaders['Accept'] = 'application/vnd.apple.mpegurl,application/x-mpegURL,*/*';
        }
        c = VideoPlayerController.networkUrl(
          uri,
          formatHint: alternateFormat,
          httpHeaders: retryHeaders,
          videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
        );
        try {
          await c.initialize();
          formatHint = alternateFormat;
          DiaryService.add('[ExoPlayer] 自愈重试成功! duration=${c.value.duration}');
        } catch (retryErr, retryStack) {
          DiaryService.add('[ExoPlayer] 自愈重试依然失败! 异常: $retryErr\n堆栈: $retryStack');
          if (myGen == _openGeneration) {
            _safeAdd(stream.errorController, retryErr.toString());
          }
          try {
            await c.dispose();
          } catch (_) {}
          rethrow;
        }
      } else {
        DiaryService.add('[ExoPlayer] c.initialize() 失败! 异常: $e\n堆栈: $stack');
        if (myGen == _openGeneration) {
          _safeAdd(stream.errorController, e.toString());
        }
        try {
          await c.dispose();
        } catch (_) {}
        rethrow;
      }
    }

    if (_disposed || myGen != _openGeneration) {
      try {
        await c.dispose();
      } catch (_) {}
      return;
    }

    if (old != null) {
      try {
        await old.dispose();
      } catch (_) {}
    }

    if (myGen != _openGeneration) {
      try {
        await c.dispose();
      } catch (_) {}
      return;
    }

    _controller = c;
    final dur = c.value.duration;
    final w = c.value.size.width.round();
    final h = c.value.size.height.round();
    final videoParams = VideoParams(w: w, h: h, dw: w, dh: h);

    state = state.copyWith(
      duration: dur,
      position: Duration.zero,
      playing: play,
      buffering: c.value.isBuffering,
      width: w > 0 ? w : 1920,
      height: h > 0 ? h : 1080,
      videoParams: videoParams,
    );

    _safeAdd(stream.durationController, dur);
    _safeAdd(stream.positionController, Duration.zero);
    _safeAdd(stream.playingController, play);
    _safeAdd(stream.bufferingController, c.value.isBuffering);
    _safeAdd(stream.videoParamsController, videoParams);

    _valueListener = () => _onControllerValue(c);
    c.addListener(_valueListener!);

    // 定时轮询真实播放位置 (与 LunaTV-Mobile 对齐)
    _startPositionPolling();

    if (media.start != null && media.start! > Duration.zero) {
      try {
        await c.seekTo(media.start!);
        state = state.copyWith(position: media.start!);
        _safeAdd(stream.positionController, media.start!);
      } catch (_) {}
    }

    if (play) {
      try {
        await c.play();
      } catch (_) {}
    }
    revision.value++;
  }

  /// 播放失败后回查本机代理：分别模拟播放器的无 Range 请求与开放 Range 请求，
  /// 状态码异常时记录响应体（Go 侧错误信息），用于区分代理故障与解码故障。
  Future<void> _probeProxy(Uri? uri, Map<String, String> headers) async {
    if (uri == null || (uri.scheme != 'http' && uri.scheme != 'https')) return;
    for (final range in <String?>[null, 'bytes=0-']) {
      try {
        final client = HttpClient()..connectionTimeout = const Duration(seconds: 5);
        final request = await client.getUrl(uri);
        headers.forEach((key, value) {
          if (key.toLowerCase() != 'accept-encoding') {
            request.headers.set(key, value);
          }
        });
        if (range != null) request.headers.set('Range', range);
        final response = await request.close().timeout(const Duration(seconds: 8));
        final body = await response
            .take(1)
            .toList()
            .timeout(const Duration(seconds: 5), onTimeout: () => []);
        final head = body.isEmpty
            ? ''
            : String.fromCharCodes(
                body.first.take(24),
              ).replaceAll(RegExp(r'[\x00-\x1f]'), '.');
        DiaryService.add(
          '[Probe] range=${range ?? "无"} status=${response.statusCode} '
          'type=${response.headers.contentType} '
          'len=${response.headers.contentLength} '
          'range=${response.headers.value("content-range")} head=$head',
        );
        client.close(force: true);
      } catch (error) {
        DiaryService.add('[Probe] range=${range ?? "无"} 回查失败: $error');
      }
    }
  }

  /// 记录原生代理侧最近的请求与响应，直接反映播放器拿到的字节数。
  Future<void> _dumpNativeProbe() async {
    final source = probeSource;
    if (source == null) return;
    try {
      final entries = await source();
      if (entries.isEmpty) {
        DiaryService.add('[Probe] 原生代理无记录（播放器可能未发起请求）');
        return;
      }
      for (final entry in entries) {
        DiaryService.add('[Probe] $entry');
      }
    } catch (error) {
      DiaryService.add('[Probe] 读取原生记录失败: $error');
    }
  }

  void _startPositionPolling() {
    _positionPollTimer?.cancel();
    _positionPollTimer = Timer.periodic(const Duration(milliseconds: 300), (_) {
      final c = _controller;
      if (c == null || !c.value.isInitialized) return;
      final pos = c.value.position;
      if (pos != state.position) {
        state = state.copyWith(position: pos);
        _safeAdd(stream.positionController, pos);
      }
    });
  }

  void _onControllerValue(VideoPlayerController c) {
    if (_disposed || _controller != c) return;
    final v = c.value;

    bool stateChanged = false;
    if (v.isPlaying != state.playing) {
      DiaryService.add('[ExoPlayer] isPlaying 变为: ${v.isPlaying}');
      state = state.copyWith(playing: v.isPlaying);
      _safeAdd(stream.playingController, v.isPlaying);
      stateChanged = true;
    }
    if (v.isBuffering != state.buffering) {
      DiaryService.add('[ExoPlayer] isBuffering 变为: ${v.isBuffering}');
      state = state.copyWith(buffering: v.isBuffering);
      _safeAdd(stream.bufferingController, v.isBuffering);
      stateChanged = true;
    }
    if (v.hasError) {
      DiaryService.add('[ExoPlayer] controller error: ${v.errorDescription}');
    }
    final dur = v.duration;
    if (dur > Duration.zero && dur != state.duration) {
      state = state.copyWith(duration: dur);
      _safeAdd(stream.durationController, dur);
      stateChanged = true;
    }
    final pos = v.position;
    if (pos != state.position) {
      state = state.copyWith(position: pos);
      _safeAdd(stream.positionController, pos);
    }
    if (v.buffered.isNotEmpty) {
      final lastRange = v.buffered.last;
      if (lastRange.end != state.buffer) {
        state = state.copyWith(buffer: lastRange.end);
        _safeAdd(stream.bufferController, state.buffer);
      }
    }
    if (v.isCompleted != state.completed) {
      state = state.copyWith(completed: v.isCompleted);
      _safeAdd(stream.completedController, v.isCompleted);
      stateChanged = true;
    }

    if (stateChanged) {
      revision.value++;
    }
  }

  @override
  Future<void> play() async {
    final c = _controller;
    if (c == null) return;
    try {
      await c.play();
    } catch (_) {}
  }

  @override
  Future<void> pause() async {
    final c = _controller;
    if (c == null) return;
    try {
      await c.pause();
    } catch (_) {}
  }

  @override
  Future<void> playOrPause() async {
    final c = _controller;
    if (c == null) return;
    if (c.value.isPlaying) {
      await pause();
    } else {
      await play();
    }
  }

  @override
  Future<void> seek(Duration target) async {
    final c = _controller;
    if (c == null) return;
    try {
      await c.seekTo(target);
      state = state.copyWith(position: target);
      _safeAdd(stream.positionController, target);
      if (state.completed) {
        state = state.copyWith(completed: false);
        _safeAdd(stream.completedController, false);
      }
    } catch (_) {}
  }

  @override
  Future<void> setRate(double rate) async {
    final r = rate.clamp(0.25, 4.0);
    state = state.copyWith(rate: r);
    _safeAdd(stream.rateController, r);
    final c = _controller;
    if (c == null) return;
    try {
      await c.setPlaybackSpeed(r);
    } catch (_) {}
  }

  @override
  Future<void> setVolume(double volume) async {
    final v = volume.clamp(0.0, 100.0);
    state = state.copyWith(volume: v);
    _safeAdd(stream.volumeController, v);
    final c = _controller;
    if (c == null) return;
    try {
      await c.setVolume(v / 100.0);
    } catch (_) {}
  }

  @override
  Future<void> stop() async {
    final c = _controller;
    if (c == null) return;
    try {
      await c.pause();
      await c.seekTo(Duration.zero);
    } catch (_) {}
    state = state.copyWith(
      playing: false,
      buffering: false,
      completed: false,
      position: Duration.zero,
    );
    _safeAdd(stream.playingController, false);
    _safeAdd(stream.bufferingController, false);
    _safeAdd(stream.positionController, Duration.zero);
    revision.value++;
  }

  @override
  Future<void> dispose() async {
    _disposed = true;
    _positionPollTimer?.cancel();
    final c = _controller;
    final listener = _valueListener;
    if (c != null && listener != null) {
      try {
        c.removeListener(listener);
      } catch (_) {}
    }
    _valueListener = null;
    if (c != null) {
      try {
        await c.pause();
      } catch (_) {}
      try {
        await c.dispose();
      } catch (_) {}
      _controller = null;
    }
    stream.dispose();
  }

  void _safeAdd<T>(StreamController<T> ctl, T value) {
    if (!ctl.isClosed) ctl.add(value);
  }

  @override
  dynamic noSuchMethod(Invocation invocation) {
    if (invocation.isGetter) return null;
    if (invocation.isMethod) return Future<void>.value();
    return null;
  }
}

/// 采用和 LunaTV-Mobile 完全相同架构的 ExoPlayer 视图组件
class LunaExoVideoView extends StatelessWidget {
  const LunaExoVideoView({
    super.key,
    required this.player,
    this.fit = BoxFit.contain,
    this.controls,
  });

  final LunaExoPlayer player;
  final BoxFit fit;
  final Widget Function(BuildContext)? controls;

  @override
  Widget build(BuildContext context) {
    return AnimatedBuilder(
      animation: player.revision,
      builder: (context, _) {
        final c = player.controller;
        final hasVideo = c != null && c.value.isInitialized;
        final rawRatio = hasVideo ? c.value.aspectRatio : 16 / 9;
        final ratio = (rawRatio > 0 && !rawRatio.isNaN && !rawRatio.isInfinite)
            ? rawRatio
            : 16 / 9;

        return Stack(
          fit: StackFit.expand,
          children: [
            if (hasVideo)
              Center(
                child: AspectRatio(
                  aspectRatio: ratio,
                  child: VideoPlayer(c),
                ),
              )
            else
              const ColoredBox(color: Colors.black),
            if (controls != null) controls!(context),
          ],
        );
      },
    );
  }
}
