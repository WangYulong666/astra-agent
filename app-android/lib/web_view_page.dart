import 'dart:async';
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:webview_flutter/webview_flutter.dart';

import 'bridge.dart';
import 'server_setup_page.dart';
import 'theme.dart';
import 'widgets/error_view.dart';
import 'widgets/network_banner.dart';

/// WebView 主页面。
///
/// 三件事按「壳该做的事」分工：
/// - 页面本身（业务 UI）全部由服务端 Web 渲染，壳只负责装它的容器；
/// - 加载失败 / 断网 / 下载 / 外链 / 返回键由壳接管，保证用户不会卡死；
/// - 与 Web 的唯一耦合是注入的 `window.AstraBridge`（见 [kAstraShimJs]），Web 不改也能用。
class WebViewPage extends StatefulWidget {
  const WebViewPage({super.key, required this.serverUrl, this.initialFailure});

  final String serverUrl;

  /// 启动探测阶段（闪屏）已经失败的结论，直接进错误页，避免再白屏转圈。
  final LoadFailure? initialFailure;

  @override
  State<WebViewPage> createState() => _WebViewPageState();
}

class _WebViewPageState extends State<WebViewPage> {
  late final WebViewController _controller;
  late String _serverUrl;
  late Uri _serverOrigin;

  /// 最近一次主框架导航的 URL（重试 / 下拉刷新都以它为基准，而不是永远回首页）。
  String _currentUrl = '';

  LoadFailure? _failure;
  bool _loading = true;
  double _progress = 0;

  /// 是否已经成功渲染过一页。首屏才用整屏转圈，
  /// 之后的站内跳转只留顶部细进度条，避免每次点链接都闪一下。
  bool _hasContent = false;

  /// JS 回报的滚动位置（是否在顶部），用于给下拉刷新兜底判断。
  bool _atTop = true;

  bool _offline = false;
  DateTime? _recoveredAt;
  Timer? _offlineDebounce;
  Timer? _recoveredTimer;
  StreamSubscription<bool>? _networkSub;

  DateTime? _lastBackAt;

  @override
  void initState() {
    super.initState();
    _serverUrl = widget.serverUrl;
    _serverOrigin = Uri.parse(_serverUrl);
    _failure = widget.initialFailure;

    _controller = _buildController();
    _networkSub = NativeBridge.networkChanges().listen(
      _onNetworkChanged,
      onError: (Object _) {},
    );

    if (_failure == null) {
      _loading = true;
      _loadUrl(_serverUrl);
    } else {
      _loading = false;
    }
  }

  @override
  void dispose() {
    _networkSub?.cancel();
    _offlineDebounce?.cancel();
    _recoveredTimer?.cancel();
    super.dispose();
  }

  WebViewController _buildController() {
    return WebViewController()
      ..setJavaScriptMode(JavaScriptMode.unrestricted)
      ..setBackgroundColor(kDarkBg)
      ..addJavaScriptChannel(
        'AstraNative',
        onMessageReceived: _onChannelMessage,
      )
      ..setNavigationDelegate(NavigationDelegate(
        onProgress: (int value) {
          if (!mounted) return;
          final double next = value.clamp(0, 100) / 100;
          // 进度回调很密（0~100 逐级），只在肉眼可辨的变化上重建，避免拖慢页面。
          if ((next - _progress).abs() < 0.04 && next != 1) return;
          setState(() => _progress = next);
        },
        onPageStarted: (String url) {
          if (!mounted) return;
          setState(() {
            _currentUrl = url;
            _loading = true;
            _progress = 0;
            _failure = null;
          });
        },
        onPageFinished: (String url) {
          if (!mounted) return;
          if (url.isNotEmpty && url != 'about:blank') _currentUrl = url;
          setState(() {
            _loading = false;
            _progress = 1;
            _hasContent = true;
          });
          _injectShim();
        },
        onWebResourceError: _onResourceError,
        onHttpError: _onHttpError,
        onNavigationRequest: _onNavigationRequest,
      ));
  }

  // ---------------------------------------------------------------- 加载与错误

  Future<void> _loadUrl(String url) async {
    try {
      await _controller.loadRequest(Uri.parse(url));
    } on Object {
      // loadRequest 本身失败（极端情况），由 onWebResourceError 兜底。
    }
  }

  Future<void> _injectShim() async {
    try {
      await _controller.runJavaScript(kAstraShimJs);
    } on Object {
      // 页面可能在注入瞬间被销毁，忽略。
    }
  }

  /// 重试 / 下拉刷新统一入口：优先重载当前页，没有当前页才回首页。
  Future<void> _reload() async {
    if (!mounted) return;
    setState(() {
      _failure = null;
      _loading = true;
      _progress = 0;
    });
    await _loadUrl(_reloadTarget());
  }

  String _reloadTarget() {
    if (_currentUrl.isEmpty || _currentUrl == 'about:blank') return _serverUrl;
    return _currentUrl;
  }

  bool _isBlankPage() =>
      _currentUrl.isEmpty || _currentUrl == 'about:blank';

  void _onResourceError(WebResourceError error) {
    // 只处理主框架：子资源失败（某张图 404）不该顶掉整个页面。
    if (error.isForMainFrame == false) return;
    if (!mounted) return;
    setState(() {
      _failure = LoadFailure(
        kind: _classify(error),
        code: error.errorCode,
        description: error.description,
        url: _reloadTarget(),
      );
      _loading = false;
      _progress = 1;
    });
  }

  void _onHttpError(HttpResponseError error) {
    final int status = error.response?.statusCode ?? 0;
    if (status < 500) return; // 4xx 交给 Web 自己的页面
    // webview_flutter 的 onHttpError 不区分主框架和子资源，
    // 只把「当前文档地址本身」的 5xx 当致命错误，避免某个接口 500 顶掉整页。
    final Uri? uri = error.request?.uri;
    if (uri != null && !_isCurrentDocument(uri)) return;
    if (!mounted) return;
    setState(() {
      _failure = LoadFailure(
        kind: LoadFailureKind.serverError,
        statusCode: status,
        url: _reloadTarget(),
      );
      _loading = false;
      _progress = 1;
    });
  }

  bool _isCurrentDocument(Uri uri) {
    final Uri? current = Uri.tryParse(_currentUrl);
    if (current == null || !current.hasAuthority) return true;
    return uri.host == current.host &&
        uri.port == current.port &&
        uri.path == current.path;
  }

  LoadFailureKind _classify(WebResourceError error) {
    final WebResourceErrorType? type = error.errorType;
    switch (type) {
      case WebResourceErrorType.badUrl:
      case WebResourceErrorType.unsupportedScheme:
        return LoadFailureKind.badAddress;
      case WebResourceErrorType.hostLookup:
      case WebResourceErrorType.connect:
      case WebResourceErrorType.timeout:
      case WebResourceErrorType.io:
      case WebResourceErrorType.failedSslHandshake:
      case WebResourceErrorType.redirectLoop:
      case WebResourceErrorType.proxyAuthentication:
        return LoadFailureKind.unreachable;
      default:
        break;
    }
    // Android 的 errorType 可能为 null，退化成按 Chromium net error code 判断。
    final int code = error.errorCode;
    if (code == -300 || code == -301 || code == -302 || code == -303) {
      return LoadFailureKind.badAddress;
    }
    if (code < 0) return LoadFailureKind.unreachable;
    return LoadFailureKind.unknown;
  }

  // ---------------------------------------------------------------- 导航与协议

  NavigationDecision _onNavigationRequest(NavigationRequest request) {
    final Uri? uri = Uri.tryParse(request.url);
    if (uri == null || uri.scheme.isEmpty) return NavigationDecision.prevent;

    final String scheme = uri.scheme.toLowerCase();
    if (scheme == 'http' || scheme == 'https') {
      if (_isSameServer(uri)) return NavigationDecision.navigate;
      // 非本服务器：交给系统浏览器，别把用户困在 WebView 里。
      NativeBridge.openExternal(request.url);
      return NavigationDecision.prevent;
    }

    // tel: / mailto: / sms: / geo: / market: / 其它已注册协议 → 系统处理。
    NativeBridge.openExternal(request.url);
    return NavigationDecision.prevent;
  }

  /// 同源判定用 host + 生效端口，子路径一律算内部（包括 /api、/ws）。
  bool _isSameServer(Uri uri) {
    if (uri.host != _serverOrigin.host) return false;
    final int port = uri.hasPort
        ? uri.port
        : (uri.scheme == 'https' ? 443 : 80);
    final int serverPort = _serverOrigin.hasPort
        ? _serverOrigin.port
        : (_serverOrigin.scheme == 'https' ? 443 : 80);
    return port == serverPort;
  }

  // ---------------------------------------------------------------- JS 通道

  Future<void> _onChannelMessage(JavaScriptMessage message) async {
    Map<String, dynamic> data;
    try {
      final Object? decoded = jsonDecode(message.message);
      if (decoded is! Map<String, dynamic>) return;
      data = decoded;
    } on FormatException {
      return;
    }

    switch (data['type']) {
      case 'scroll':
        _atTop = data['atTop'] == true;
      case 'pullRefresh':
        // 只有在顶部才刷新，避免和页面内滚动打架。
        if (_atTop) await _reload();
      case 'saveFile':
        await _saveFromWeb(data);
      case 'saveFailed':
        _toast('保存失败：${data['message'] ?? '未知原因'}');
      case 'toast':
        final String text = (data['message'] as String?) ?? '';
        if (text.isNotEmpty) _toast(text);
      case 'openExternal':
        final String url = (data['url'] as String?) ?? '';
        if (url.isNotEmpty) await NativeBridge.openExternal(url);
      default:
        break;
    }
  }

  Future<void> _saveFromWeb(Map<String, dynamic> data) async {
    final String rawName = (data['name'] as String?)?.trim() ?? '';
    final String name = sanitizeFileName(rawName.isEmpty ? 'download' : rawName);
    final String body = (data['base64'] as String?) ?? '';
    if (body.isEmpty) {
      _toast('保存失败：文件内容为空');
      return;
    }
    Uint8List bytes;
    try {
      bytes = base64Decode(body);
    } on FormatException {
      _toast('保存失败：文件内容损坏');
      return;
    }
    final String? rawMime = (data['mime'] as String?)?.trim();
    final String? mime = (rawMime == null || rawMime.isEmpty) ? null : rawMime;

    _toast('正在保存 $name…', duration: const Duration(seconds: 1));
    final SaveFileResult result =
        await NativeBridge.saveFile(name: name, bytes: bytes, mime: mime);
    if (!mounted) return;

    if (!result.ok) {
      final String message = result.needsPermission
          ? '没有存储权限，无法保存到「下载」目录'
          : '保存失败：${result.error ?? '未知原因'}';
      _toast(message, duration: const Duration(seconds: 5));
      return;
    }

    final String where = result.displayPath ?? name;
    _toast(
      '已保存到「$where」',
      duration: const Duration(seconds: 6),
      action: SnackBarAction(
        label: '打开',
        onPressed: () async {
          final String uri = result.uri ?? '';
          if (uri.isEmpty) {
            _toast('文件位置不可打开，请到「下载」中查看');
            return;
          }
          final bool opened = await NativeBridge.openFile(uri);
          if (!opened && mounted) _toast('没有能打开此类型文件的应用');
        },
      ),
    );
  }

  // ---------------------------------------------------------------- 网络

  void _onNetworkChanged(bool online) {
    if (!mounted) return;
    if (!online) {
      // WiFi 切蜂窝会先报 onLost 再报 onAvailable，短暂抖动不该闪提示条。
      _offlineDebounce?.cancel();
      _offlineDebounce = Timer(const Duration(milliseconds: 700), () {
        if (!mounted) return;
        setState(() {
          _offline = true;
          _recoveredAt = null;
        });
      });
      return;
    }

    _offlineDebounce?.cancel();
    if (!_offline) return; // 本来就在线，忽略重复回调

    // 恢复时只在「用户其实看不到内容」的情况下自动重载，不打断正在输入的表单。
    final bool shouldReload = _failure != null || _isBlankPage();
    setState(() {
      _offline = false;
      _recoveredAt = DateTime.now();
    });
    _toast(shouldReload ? '网络已恢复，正在重新加载' : '网络已恢复');
    if (shouldReload) _reload();

    _recoveredTimer?.cancel();
    _recoveredTimer = Timer(const Duration(seconds: 3), () {
      if (mounted) setState(() => _recoveredAt = null);
    });
  }

  // ---------------------------------------------------------------- 返回键

  Future<void> _handleBack() async {
    bool canGoBack = false;
    try {
      canGoBack = await _controller.canGoBack();
    } on Object {
      canGoBack = false;
    }
    if (canGoBack) {
      await _controller.goBack();
      return;
    }
    final DateTime now = DateTime.now();
    if (_lastBackAt != null &&
        now.difference(_lastBackAt!) <= const Duration(seconds: 2)) {
      await SystemNavigator.pop();
      return;
    }
    _lastBackAt = now;
    _toast('再按一次退出');
  }

  Future<void> _changeAddress() async {
    final String? result = await Navigator.of(context).push<String>(
      MaterialPageRoute<String>(
        builder: (_) => ServerSetupPage(initialUrl: _serverUrl),
      ),
    );
    if (!mounted || result == null || result == _serverUrl) return;
    // 换服务器等于换了一个 App：重建 WebView，历史与缓存都不跨站。
    Navigator.of(context).pushReplacement(MaterialPageRoute<void>(
      builder: (_) => WebViewPage(serverUrl: result),
    ));
  }

  Future<void> _copyDiagnostic() async {
    final String platform = await NativeBridge.platformLabel();
    final LoadFailure? failure = _failure;
    final StringBuffer buf = StringBuffer()
      ..writeln('Astra 诊断信息')
      ..writeln('时间: ${DateTime.now().toString().split('.').first}')
      ..writeln('服务器: $_serverUrl')
      ..writeln('当前页: ${_reloadTarget()}')
      ..writeln('平台: $platform');
    if (failure != null) {
      buf.writeln('类型: ${failure.kind.name}');
      if (failure.statusCode != null) buf.writeln('HTTP: ${failure.statusCode}');
      if (failure.code != null) buf.writeln('错误码: ${failure.code}');
      if (failure.description != null) buf.writeln('描述: ${failure.description}');
    }
    if (_offline) buf.writeln('网络: 设备离线');
    await Clipboard.setData(ClipboardData(text: buf.toString().trimRight()));
    if (mounted) _toast('诊断信息已复制');
  }

  // ---------------------------------------------------------------- UI

  void _toast(
    String message, {
    SnackBarAction? action,
    Duration duration = const Duration(seconds: 3),
  }) {
    final ScaffoldMessengerState? messenger = ScaffoldMessenger.maybeOf(context);
    if (messenger == null) return;
    messenger.removeCurrentSnackBar();
    messenger.showSnackBar(
      SnackBar(content: Text(message), action: action, duration: duration),
    );
  }

  @override
  Widget build(BuildContext context) {
    return AnnotatedRegion<SystemUiOverlayStyle>(
      value: kOverlayStyle,
      child: PopScope(
        canPop: false,
        onPopInvokedWithResult: (bool didPop, Object? _) {
          if (!didPop) _handleBack();
        },
        child: Scaffold(
          resizeToAvoidBottomInset: true,
          appBar: _buildAppBar(),
          body: Column(
            children: <Widget>[
              NetworkBanner(
                visible: _offline || _recoveredAt != null,
                offline: _offline,
              ),
              // 固定 2px 高度，进度条出现/消失都不会顶动页面。
              SizedBox(
                height: 2,
                child: (_loading && _failure == null)
                    ? LinearProgressIndicator(
                        value: _progress > 0 && _progress < 1 ? _progress : null,
                        minHeight: 2,
                        backgroundColor: Colors.white10,
                        valueColor: const AlwaysStoppedAnimation<Color>(kBrandColor),
                      )
                    : null,
              ),
              Expanded(child: _buildContent(context)),
            ],
          ),
        ),
      ),
    );
  }

  PreferredSizeWidget _buildAppBar() {
    final String host = _serverOrigin.authority.isEmpty
        ? _serverUrl
        : _serverOrigin.authority;
    return AppBar(
      titleSpacing: 16,
      title: Column(
        mainAxisAlignment: MainAxisAlignment.center,
        crossAxisAlignment: CrossAxisAlignment.start,
        children: <Widget>[
          const Text(
            'Astra',
            style: TextStyle(fontSize: 16, fontWeight: FontWeight.w600, height: 1.15),
          ),
          Text(
            host,
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: const TextStyle(fontSize: 11, color: Colors.white38, height: 1.25),
          ),
        ],
      ),
      actions: <Widget>[
        IconButton(
          icon: const Icon(Icons.refresh_rounded, size: 22),
          tooltip: '刷新',
          onPressed: _reload,
        ),
        IconButton(
          icon: const Icon(Icons.settings_outlined, size: 21),
          tooltip: '服务器地址',
          onPressed: _changeAddress,
        ),
        const SizedBox(width: 4),
      ],
    );
  }

  Widget _buildContent(BuildContext context) {
    final LoadFailure? failure = _failure;
    if (failure != null) {
      // 错误页本身可滚动 → 下拉即重试（这里是真正的 RefreshIndicator）。
      return RefreshIndicator(
        onRefresh: _reload,
        color: kBrandColor,
        backgroundColor: kDarkSurface,
        child: ErrorView(
          failure: failure,
          serverUrl: _serverUrl,
          networkDown: _offline,
          onRetry: _reload,
          onChangeAddress: _changeAddress,
          onCopyDiagnostic: _copyDiagnostic,
        ),
      );
    }

    return Stack(
      children: <Widget>[
        // WebView 是 Android 平台视图：底部导航栏区域留出来，
        // 避免页面最下方的按钮被手势条盖住。
        Padding(
          padding: EdgeInsets.only(
            bottom: MediaQuery.viewPaddingOf(context).bottom,
          ),
          child: WebViewWidget(controller: _controller),
        ),
        if (_loading && _progress == 0 && !_hasContent)
          const ColoredBox(
            color: kDarkBg,
            child: Center(
              child: SizedBox(
                width: 28,
                height: 28,
                child: CircularProgressIndicator(strokeWidth: 2.5),
              ),
            ),
          ),
      ],
    );
  }
}

/// 清洗下载文件名：去掉路径分隔符/控制字符，限长，保证有扩展名。
String sanitizeFileName(String raw) {
  String name = raw
      .replaceAll(RegExp(r'[\\/:*?"<>|]'), '_')
      .replaceAll(RegExp(r'[\x00-\x1f\x7f]'), '')
      .trim();
  name = name.replaceFirst(RegExp(r'^\.+'), '');
  if (name.isEmpty) name = 'download';
  if (name.length > 120) {
    final int dot = name.lastIndexOf('.');
    if (dot > 0 && name.length - dot <= 12) {
      name = name.substring(0, 120 - (name.length - dot)) + name.substring(dot);
    } else {
      name = name.substring(0, 120);
    }
  }
  if (!name.contains('.')) name = '$name.bin';
  return name;
}
