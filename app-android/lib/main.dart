import 'dart:async';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'bridge.dart';
import 'server_setup_page.dart';
import 'theme.dart';
import 'web_view_page.dart';
import 'widgets/splash_view.dart';

/// 启动探测的最长等待时间：超过就直接进错误页，而不是一直转圈。
const Duration _kProbeTimeout = Duration(seconds: 4);

/// 闪屏总时长上限（含 DNS/连接/读取的全部阶段）。
const Duration _kSplashTimeout = Duration(seconds: 5);

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  // edge-to-edge：Android 15 上系统栏不再单独着色，由页面内容托底。
  await SystemChrome.setEnabledSystemUIMode(SystemUiMode.edgeToEdge);
  SystemChrome.setSystemUIOverlayStyle(kOverlayStyle);
  runApp(const AstraApp());
}

class AstraApp extends StatelessWidget {
  const AstraApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Astra',
      debugShowCheckedModeBanner: false,
      theme: buildAstraTheme(),
      home: const BootPage(),
    );
  }
}

/// 启动闪屏 + 首连探测。
///
/// 有已保存地址 → 闪屏里探测 `/healthz`（最多 [_kProbeTimeout]）→
/// 通就进 WebView，不通就带着失败结论进错误页（用户能重试/换地址）。
/// 没地址 → 直接进设置页。
class BootPage extends StatefulWidget {
  const BootPage({super.key});

  @override
  State<BootPage> createState() => _BootPageState();
}

class _BootPageState extends State<BootPage> {
  @override
  void initState() {
    super.initState();
    _boot();
  }

  Future<void> _boot() async {
    final SharedPreferences prefs = await SharedPreferences.getInstance();
    final String? saved = prefs.getString(kServerUrlKey);
    if (!mounted) return;

    final String? normalized = saved == null ? null : normalizeUrl(saved);
    if (normalized == null) {
      _goToSetup();
      return;
    }

    // 闪屏最多停 5 秒：到点就算探测还没回来，也带着「超时」结论进错误页。
    final LoadFailure? failure = await _probe(normalized).timeout(
      _kSplashTimeout,
      onTimeout: () => LoadFailure(
        kind: LoadFailureKind.unreachable,
        description: '探测超时（5 秒无响应）',
        url: normalized,
      ),
    );
    if (!mounted) return;
    Navigator.of(context).pushReplacement(MaterialPageRoute<void>(
      builder: (_) => WebViewPage(
        serverUrl: normalized,
        initialFailure: failure,
      ),
    ));
  }

  void _goToSetup() {
    Navigator.of(context).pushReplacement(MaterialPageRoute<void>(
      builder: (_) => const ServerSetupPage(firstRun: true),
    ));
  }

  /// 打一次 `/healthz`：只要能拿到 HTTP 响应就认为「服务器可达」，
  /// 页面具体的 5xx / 渲染问题交给 WebView 的导航回调处理。
  Future<LoadFailure?> _probe(String baseUrl) async {
    final Uri base = Uri.parse(baseUrl);
    final String prefix = base.path.replaceAll(RegExp(r'/+$'), '');
    final Uri target = base.replace(path: '$prefix/healthz');
    final HttpClient client = HttpClient()..connectionTimeout = _kProbeTimeout;
    final Stopwatch watch = Stopwatch()..start();
    try {
      final HttpClientRequest request =
          await client.getUrl(target).timeout(_kProbeTimeout);
      request.headers.set(HttpHeaders.acceptHeader, 'application/json');
      final HttpClientResponse response =
          await request.close().timeout(_kProbeTimeout);
      await response.drain<void>().timeout(_kProbeTimeout);
      if (response.statusCode >= 500) {
        return LoadFailure(
          kind: LoadFailureKind.serverError,
          statusCode: response.statusCode,
          url: baseUrl,
        );
      }
      return null;
    } on TimeoutException {
      return LoadFailure(
        kind: LoadFailureKind.unreachable,
        description: '探测超时（${watch.elapsed.inSeconds + 1} 秒无响应）',
        url: baseUrl,
      );
    } on SocketException catch (e) {
      return LoadFailure(
        kind: LoadFailureKind.unreachable,
        description: e.message.isEmpty ? e.osError?.message : e.message,
        url: baseUrl,
      );
    } on HandshakeException catch (e) {
      return LoadFailure(
        kind: LoadFailureKind.unreachable,
        description: 'TLS 握手失败：${e.message}',
        url: baseUrl,
      );
    } on Object catch (e) {
      return LoadFailure(
        kind: LoadFailureKind.unknown,
        description: '$e',
        url: baseUrl,
      );
    } finally {
      client.close(force: true);
    }
  }

  @override
  Widget build(BuildContext context) {
    return const AnnotatedRegion<SystemUiOverlayStyle>(
      value: kOverlayStyle,
      child: Scaffold(body: SplashView()),
    );
  }
}
