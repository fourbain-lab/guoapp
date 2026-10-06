import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/models.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:media_kit/media_kit.dart';

const fixtureBase = String.fromEnvironment('FIXTURE_BASE_URL');

/// Resolves each synthetic fixture through the native core, then plays the
/// resulting plan through media_kit exactly as the app does, including the
/// decryption key for the CENC fixture.
void main() {
  final binding = IntegrationTestWidgetsFlutterBinding.ensureInitialized();

  testWidgets('resolve and play every fixture through media_kit', (tester) async {
    MediaKit.ensureInitialized();
    final repository = NativeRepository();
    await repository.initialize();

    const drama = Drama(
      id: 'hongguo:700001',
      source: 'hongguo',
      title: '设备播放验证',
      episodes: 3,
    );
    final fixtures = {
      1: ('clear.mp4', ''),
      2: ('index.m3u8', ''),
      3: ('encrypted.mp4', '00112233445566778899aabbccddeeff'),
    };

    final report = <Map<String, Object?>>[];
    for (final entry in fixtures.entries) {
      final number = entry.key;
      final (file, key) = entry.value;
      final episode = Episode({
        'id': '$number',
        'source': 'hongguo',
        'currentEpisode': number,
        'title': '第$number集',
        'videoUrl': '$fixtureBase/$file',
        'referer': '$fixtureBase/',
      }, number);

      String? resolveError;
      PlaybackPlan? plan;
      try {
        plan = await repository.resolve(drama, episode);
      } catch (error) {
        resolveError = error.toString();
      }
      if (plan == null) {
        // ignore: avoid_print
        print('[PlanProbe] ep$number ($file) resolve FAILED: $resolveError');
        report.add({
          'episode': number,
          'file': file,
          'resolveError': resolveError,
          'playable': false,
        });
        continue;
      }

      // ignore: avoid_print
      print('[PlanProbe] ep$number ($file) resolved url=${plan.url} '
          'local=${plan.local} keyFromPlan=${plan.decryptionKey.length}');

      final effectiveKey = key.isNotEmpty ? key : plan.decryptionKey;
      final player = Player(
        configuration: const PlayerConfiguration(bufferSize: 8 * 1024 * 1024),
      );
      String? playError;
      try {
        final platform = player.platform;
        if (platform is NativePlayer) {
          await platform.setProperty(
            'demuxer-lavf-o',
            [
              'seg_max_retry=3',
              'strict=experimental',
              'allowed_extensions=ALL',
              'protocol_whitelist=[http,https,tcp,tls,crypto,data,file]',
              if (effectiveKey.isNotEmpty) 'decryption_key=$effectiveKey',
            ].join(','),
          );
        }
        await player.open(
          Media(plan.url, httpHeaders: plan.headers),
          play: true,
        );
        final deadline = DateTime.now().add(const Duration(seconds: 30));
        while (DateTime.now().isBefore(deadline)) {
          if (player.state.position.inMilliseconds > 300) break;
          await Future<void>.delayed(const Duration(milliseconds: 250));
        }
      } catch (error) {
        playError = error.toString();
      }
      // Headless runs have no video surface, so width stays null; real progress
      // and a known duration are the reliable signals that decoding worked.
      final playable =
          playError == null &&
          player.state.duration.inMilliseconds > 0 &&
          player.state.position.inMilliseconds > 300;
      // ignore: avoid_print
      print('[PlanProbe] ep$number playable=$playable err=$playError '
          'width=${player.state.width} dur=${player.state.duration} '
          'pos=${player.state.position}');
      report.add({
        'episode': number,
        'file': file,
        'playable': playable,
        'playError': playError,
        'width': player.state.width,
        'durationMs': player.state.duration.inMilliseconds,
        'positionMs': player.state.position.inMilliseconds,
      });
      await player.dispose();
      await repository.release(plan.session);
    }

    binding.reportData ??= {};
    binding.reportData!['planProbe'] = report;
    final allPlayable = report.every((row) => row['playable'] == true);
    expect(allPlayable, isTrue,
        reason: 'every fixture must play through media_kit: $report');
  }, timeout: const Timeout(Duration(minutes: 5)));
}
