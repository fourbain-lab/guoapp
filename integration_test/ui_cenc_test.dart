import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/local_store.dart';
import 'package:duanju_app/models.dart';
import 'package:duanju_app/player_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:media_kit/media_kit.dart';
import 'package:media_kit_video/media_kit_video.dart';
import 'package:shared_preferences/shared_preferences.dart';

const fixtureBase = String.fromEnvironment('FIXTURE_BASE_URL');

/// Mounts the real PlayerScreen for the CENC fixture and checks that the app
/// renders a video surface and advances, which is what the user sees.
void main() {
  final binding = IntegrationTestWidgetsFlutterBinding.ensureInitialized();

  testWidgets('player screen renders and plays the CENC episode', (tester) async {
    MediaKit.ensureInitialized();
    final observed = Player(
      configuration: const PlayerConfiguration(
        bufferSize: 32 * 1024 * 1024,
        logLevel: MPVLogLevel.error,
      ),
    );
    final repository = _CencRepository();
    await repository.initialize();
    final store = LocalStore(await SharedPreferences.getInstance());
    final detail = await repository.detail(_CencRepository.drama);

    await tester.pumpWidget(
      MaterialApp(
        home: PlayerScreen(
          detail: detail,
          initialIndex: 0,
          repository: repository,
          store: store,
          playerFactory: () => observed,
        ),
      ),
    );

    Future<void> until(bool Function() ready, String step) async {
      final timer = Stopwatch()..start();
      while (!ready()) {
        if (timer.elapsed > const Duration(seconds: 45)) {
          fail('Timed out: $step');
        }
        await tester.pump(const Duration(milliseconds: 200));
      }
    }

    await until(() => find.byType(Video).evaluate().isNotEmpty, 'video surface');
    await until(
      () =>
          observed.state.position.inMilliseconds > 500 &&
          observed.state.duration.inSeconds >= 18,
      'cenc playback advanced',
    );
    expect(find.text('暂时无法播放'), findsNothing);

    binding.reportData ??= {};
    binding.reportData!['uiCenc'] = {
      'positionMs': observed.state.position.inMilliseconds,
      'durationMs': observed.state.duration.inMilliseconds,
    };
    // ignore: avoid_print
    print('[UiCenc] surface=true pos=${observed.state.position} '
        'dur=${observed.state.duration}');
  }, timeout: const Timeout(Duration(minutes: 4)));
}

class _CencRepository extends NativeRepository {
  static final drama = Drama(
    id: 'hongguo:900001',
    source: 'hongguo',
    title: 'CENC 界面验证',
    episodes: 1,
  );

  @override
  Future<DramaDetail> detail(Drama drama) async => DramaDetail(drama, [
    Episode({
      'id': '1',
      'source': 'hongguo',
      'currentEpisode': 1,
      'title': '第1集',
      'videoUrl': '$fixtureBase/encrypted.mp4',
      'referer': '$fixtureBase/',
    }, 1),
  ]);

  @override
  Future<PlaybackPlan> resolve(
    Drama drama,
    Episode episode, {
    int quality = 0,
  }) async {
    final plan = await super.resolve(drama, episode, quality: quality);
    return PlaybackPlan(
      url: plan.url,
      local: plan.local,
      headers: plan.headers,
      decryptionKey: '00112233445566778899aabbccddeeff',
      session: plan.session,
      routeIndex: plan.routeIndex,
      routeCount: plan.routeCount,
    );
  }
}
