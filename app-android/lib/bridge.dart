import 'dart:async';
import 'dart:io' show Platform;

import 'package:flutter/services.dart';

/// 主框架加载失败的原因分类，决定错误页的人话文案与自查建议。
enum LoadFailureKind {
  /// 连不上：拒绝连接、DNS 解析失败、超时、TLS 失败。
  unreachable,

  /// 连上了但服务端返回 5xx。
  serverError,

  /// 地址本身不合法（用户填的串解析不出 host）。
  badAddress,

  /// 其它（WebView 内核报的未知错误）。
  unknown,
}

class LoadFailure {
  const LoadFailure({
    required this.kind,
    this.code,
    this.description,
    this.statusCode,
    this.url,
  });

  final LoadFailureKind kind;

  /// WebView / Chromium 的 net error code（如 -6 = ERR_CONNECTION_REFUSED）。
  final int? code;
  final String? description;
  final int? statusCode;
  final String? url;

  String get title => switch (kind) {
        LoadFailureKind.unreachable => '连不上服务器',
        LoadFailureKind.serverError => '服务器出错了',
        LoadFailureKind.badAddress => '地址格式不对',
        LoadFailureKind.unknown => '页面没能打开',
      };

  /// 标题下面的一句话解释：只说用户能动手的部分。
  String get hint => switch (kind) {
        LoadFailureKind.unreachable =>
          '手机和服务器不在同一个网络，或者服务端没启动。\n确认地址里的 IP 和端口后重试。',
        LoadFailureKind.serverError =>
          '服务端进程还活着，但这个请求返回了 HTTP ${statusCode ?? 500}。\n稍等几秒重试；一直这样的话需要看服务端日志。',
        LoadFailureKind.badAddress => '这个地址不像一个 HTTP 地址。\n点「换地址」重新填写，例如 http://192.168.31.5:7788。',
        LoadFailureKind.unknown => 'WebView 没能把页面渲染出来。\n可以先重试，或换一个地址试试。',
      };

  /// 错误码 → 一句人话，用于展示在副标题位置。
  String? get codeLine {
    if (statusCode != null) return 'HTTP $statusCode';
    if (code == null) return null;
    final String? meaning = switch (code) {
      -2 => 'ERR_NAME_NOT_RESOLVED（域名解析失败）',
      -6 => 'ERR_CONNECTION_REFUSED（端口没人监听）',
      -7 => 'ERR_TIMED_OUT（连接超时）',
      -8 => 'ERR_UNKNOWN_HOST（主机不可达）',
      -21 => 'ERR_NETWORK_CHANGED（网络切换中断）',
      -100 => 'ERR_CONNECTION_CLOSED（连接被关闭）',
      -101 => 'ERR_CONNECTION_RESET（连接被重置）',
      -102 => 'ERR_CONNECTION_REFUSED（拒绝连接）',
      -105 => 'ERR_NAME_NOT_RESOLVED（DNS 解析失败）',
      -106 => 'ERR_INTERNET_DISCONNECTED（设备离线）',
      -109 => 'ERR_ADDRESS_UNREACHABLE（地址不可达）',
      -118 => 'ERR_CONNECTION_TIMED_OUT（连接超时）',
      -202 => 'ERR_CERT_AUTHORITY_INVALID（证书不被信任）',
      _ => null,
    };
    return meaning == null ? '错误码 $code' : '$meaning · $code';
  }
}

/// 一次保存的结果。[ok] 为 true 时 [uri] 可用于打开。
class SaveFileResult {
  const SaveFileResult({
    required this.ok,
    this.uri,
    this.path,
    this.displayPath,
    this.error,
    this.needsPermission = false,
  });

  final bool ok;
  final String? uri;
  final String? path;
  final String? displayPath;
  final String? error;
  final bool needsPermission;

  static SaveFileResult fromMap(Map<String, dynamic> map) => SaveFileResult(
        ok: map['ok'] == true,
        uri: map['uri'] as String?,
        path: map['path'] as String?,
        displayPath: map['displayPath'] as String?,
        error: map['error'] as String?,
        needsPermission: map['needsPermission'] == true,
      );
}

/// 原生能力桥。全部走 Platform Channel 手写实现，不引第三方插件。
///
/// - 下载落盘 / 打开文件 / 外链跳转 / 网络状态：Android 原生 [NativeBridge.kt] 实现；
/// - 其它平台（iOS/桌面）没有实现时，调用会静默降级（返回 false / 空流），壳仍然可用。
class NativeBridge {
  NativeBridge._();

  static const MethodChannel _methods = MethodChannel('astra/native');
  static const EventChannel _networkEvents = EventChannel('astra/native/network');

  /// 原生不可用（非 Android 或通道未注册）时返回的兜底描述。
  static String _platformLabel = 'unknown';

  static Future<Map<String, dynamic>?> _invoke(
    String method, [
    Map<String, Object?>? arguments,
  ]) async {
    try {
      final Map<Object?, Object?>? raw =
          await _methods.invokeMethod<Map<Object?, Object?>>(method, arguments);
      return raw?.map((Object? k, Object? v) => MapEntry('$k', v));
    } on MissingPluginException {
      return null;
    } on PlatformException catch (e) {
      return <String, dynamic>{'ok': false, 'error': e.message ?? e.code};
    }
  }

  static Future<SaveFileResult> saveFile({
    required String name,
    required Uint8List bytes,
    String? mime,
  }) async {
    final Map<String, dynamic>? raw = await _invoke('saveFile', <String, Object?>{
      'name': name,
      'bytes': bytes,
      'mime': mime,
    });
    if (raw == null) {
      return const SaveFileResult(
        ok: false,
        error: '当前平台不支持原生保存，请用系统浏览器打开该链接下载',
      );
    }
    return SaveFileResult.fromMap(raw);
  }

  /// 保存成功后调「打开」Intent；返回 false 表示没有能处理它的应用。
  static Future<bool> openFile(String uri) async {
    final Map<String, dynamic>? raw =
        await _invoke('openFile', <String, Object?>{'uri': uri});
    return raw?['ok'] == true;
  }

  /// 交给系统浏览器 / 拨号 / 邮件。
  static Future<bool> openExternal(String url) async {
    final Map<String, dynamic>? raw =
        await _invoke('openExternal', <String, Object?>{'url': url});
    return raw?['ok'] == true;
  }

  static Future<bool> isNetworkAvailable() async {
    final Map<String, dynamic>? raw = await _invoke('isNetworkAvailable');
    return raw?['ok'] == true;
  }

  /// 连接状态变化流（true = 有网）。原生不可用时返回空流，不报错。
  static Stream<bool> networkChanges() {
    try {
      return _networkEvents
          .receiveBroadcastStream()
          .map((dynamic event) => event == true)
          .handleError((Object _) {});
    } on MissingPluginException {
      return const Stream<bool>.empty();
    }
  }

  /// 诊断信息里用的平台描述，如 `Android 14 (SDK 34) · Xiaomi 23127PN0CC`。
  static Future<String> platformLabel() async {
    if (_platformLabel != 'unknown') return _platformLabel;
    final Map<String, dynamic>? raw = await _invoke('deviceInfo');
    if (raw != null) {
      final Object? sdk = raw['sdk'];
      final Object? release = raw['release'];
      final Object? model = raw['model'];
      final StringBuffer buf = StringBuffer('Android');
      if (release != null) buf.write(' $release');
      if (sdk != null) buf.write(' (SDK $sdk)');
      if (model != null && '$model'.isNotEmpty) buf.write(' · $model');
      _platformLabel = buf.toString();
    } else {
      _platformLabel = '${Platform.operatingSystem} ${Platform.operatingSystemVersion}';
    }
    return _platformLabel;
  }
}

/// 注入到 Web 页面里的壳胶水（页面加载完执行一次，不修改 web 源码）。
///
/// 提供三件事：
/// 1. `window.AstraBridge` —— Web 端可主动调用的扩展点（保存、外链、toast）；
/// 2. `<a download>` 点击接管 —— 把 blob:/data:/同源 http 下载读成 base64 交给壳落盘；
/// 3. 下拉刷新手势 + 滚动位置回报 —— 顶部下拉超过阈值时通知壳 reload。
const String kAstraShimJs = r'''
(function () {
  if (window.__astraShellVersion) { return; }
  window.__astraShellVersion = 1;

  var channel = window.AstraNative || null;

  function post(payload) {
    if (!channel || typeof channel.postMessage !== 'function') { return; }
    try { channel.postMessage(JSON.stringify(payload)); } catch (e) {}
  }

  // ---------- 1. 对外扩展点 ----------
  function guessName(a) {
    var d = a ? (a.getAttribute('download') || '') : '';
    if (d) { return d; }
    try {
      var u = new URL(a.getAttribute('href'), location.href);
      var seg = u.pathname.split('/').filter(Boolean).pop() || 'download';
      return decodeURIComponent(seg);
    } catch (e) { return 'download'; }
  }

  function sendBlob(blob, name) {
    var reader = new FileReader();
    reader.onloadend = function () {
      var text = String(reader.result || '');
      var comma = text.indexOf(',');
      if (comma < 0) { post({ type: 'saveFailed', message: '解析文件内容失败' }); return; }
      post({
        type: 'saveFile',
        name: name,
        mime: blob.type || '',
        size: blob.size,
        base64: text.slice(comma + 1)
      });
    };
    reader.onerror = function () { post({ type: 'saveFailed', message: '读取文件内容失败' }); };
    reader.readAsDataURL(blob);
  }

  function fetchAsBlob(url, name) {
    post({ type: 'saving', name: name });
    fetch(url, { credentials: 'include', cache: 'no-store' })
      .then(function (r) {
        if (!r.ok) { throw new Error('HTTP ' + r.status); }
        return r.blob();
      })
      .then(function (b) { sendBlob(b, name); })
      .catch(function (e) { post({ type: 'saveFailed', message: String((e && e.message) || e) }); });
  }

  var extra = {
    version: 1,
    saveFile: function (name, base64, mime) {
      post({ type: 'saveFile', name: String(name || 'download'), base64: String(base64 || ''), mime: String(mime || '') });
    },
    saveBlob: function (blob, name) { sendBlob(blob, String(name || 'download')); },
    saveUrl: function (url, name) { fetchAsBlob(String(url), String(name || 'download')); },
    openExternal: function (url) { post({ type: 'openExternal', url: String(url || '') }); },
    toast: function (message) { post({ type: 'toast', message: String(message || '') }); }
  };
  var api = window.AstraBridge || {};
  for (var k in extra) {
    if (typeof api[k] === 'undefined') { api[k] = extra[k]; }
  }
  window.AstraBridge = api;

  // ---------- 2. 下载链接接管 ----------
  document.addEventListener('click', function (ev) {
    var target = ev.target;
    var a = target && target.closest ? target.closest('a[href][download], a[href][data-astra-download]') : null;
    if (!a) { return; }
    if (a.getAttribute('data-astra-bypass') !== null) { return; }
    var href = a.getAttribute('href') || '';
    var name = guessName(a);
    if (href.indexOf('blob:') === 0 || href.indexOf('data:') === 0) {
      ev.preventDefault();
      ev.stopPropagation();
      fetchAsBlob(href, name);
      return;
    }
    if (/^https?:/i.test(href)) {
      var sameOrigin = false;
      try { sameOrigin = new URL(href, location.href).origin === location.origin; } catch (e) {}
      ev.preventDefault();
      ev.stopPropagation();
      if (sameOrigin) {
        // 同源带 cookie 拉下来交给壳；跨域读不了 body，交给系统浏览器下。
        fetchAsBlob(href, name);
      } else {
        post({ type: 'openExternal', url: href });
      }
      return;
    }
    // 其它协议（tel:/mailto: 等）不拦，交给原生导航策略。
  }, true);

  // ---------- 3. 滚动位置回报 ----------
  var lastAtTop = null;
  function reportScroll() {
    var se = document.scrollingElement || document.documentElement;
    var y = Math.max(0, Math.round((se && se.scrollTop) || window.scrollY || 0));
    var atTop = y <= 0;
    if (atTop !== lastAtTop) {
      lastAtTop = atTop;
      post({ type: 'scroll', y: y, atTop: atTop });
    }
  }
  window.addEventListener('scroll', reportScroll, { passive: true, capture: true });
  document.addEventListener('touchstart', reportScroll, { passive: true, capture: true });
  window.addEventListener('load', reportScroll);
  reportScroll();

  // ---------- 4. 下拉刷新 ----------
  // 说明：Android 平台视图（WebView）会吃掉 Flutter 的手势竞技场，
  // 所以下拉手势只能在页面内识别，识别成功后回传给壳触发 reload。
  var TRIGGER = 72;
  var MAX_PULL = 96;
  var startY = 0;
  var pulling = false;
  var pullDist = 0;
  var busy = false;
  var pill = null;
  var pillLabel = null;

  function insideScrolledBox(el) {
    var node = el;
    while (node && node.nodeType === 1 && node !== document.documentElement) {
      var st = null;
      try { st = getComputedStyle(node); } catch (e) { st = null; }
      if (st && /(auto|scroll|overlay)/.test(st.overflowY) &&
          node.scrollHeight - node.clientHeight > 4 && node.scrollTop > 0) {
        return true;
      }
      node = node.parentElement;
    }
    return false;
  }

  function atTopOfPage(el) {
    var se = document.scrollingElement || document.documentElement;
    if (se && se.scrollTop > 0) { return false; }
    if (window.scrollY > 0) { return false; }
    return !insideScrolledBox(el);
  }

  function ensurePill() {
    if (pill && pill.parentNode) { return pill; }
    var style = document.createElement('style');
    style.textContent = '@keyframes __astraSpin{to{transform:rotate(360deg)}}' +
      '#__astra_pull{position:fixed;left:50%;top:0;display:flex;align-items:center;gap:8px;' +
      'padding:7px 14px;border-radius:0 0 14px 14px;background:rgba(27,29,34,.94);color:#e8eaee;' +
      'font:500 12px/1.3 -apple-system,Roboto,"PingFang SC",sans-serif;box-shadow:0 2px 12px rgba(0,0,0,.4);' +
      'z-index:2147483647;pointer-events:none;transform:translate(-50%,-100%);opacity:0;' +
      'transition:transform .12s ease-out,opacity .12s}' +
      '#__astra_pull .__sp{display:inline-block;width:12px;height:12px;border-radius:50%;' +
      'border:2px solid #4C8DFF;border-top-color:transparent;box-sizing:border-box}';
    (document.head || document.documentElement).appendChild(style);

    pill = document.createElement('div');
    pill.id = '__astra_pull';
    var dot = document.createElement('span');
    dot.className = '__sp';
    pillLabel = document.createElement('span');
    pillLabel.textContent = '下拉刷新';
    pill.appendChild(dot);
    pill.appendChild(pillLabel);
    (document.body || document.documentElement).appendChild(pill);
    return pill;
  }

  function showPill(dist, text) {
    var el = ensurePill();
    if (text && pillLabel) { pillLabel.textContent = text; }
    // pill 高约 32px：拉到 32 就完全露出，再多只做轻微拉伸。
    var offset = Math.min(dist, MAX_PULL) - 32;
    el.style.opacity = '1';
    el.style.transform = 'translate(-50%,' + offset + 'px)';
  }

  function hidePill() {
    if (!pill) { return; }
    pill.style.opacity = '0';
    pill.style.transform = 'translate(-50%,-100%)';
  }

  function resetPull() {
    pulling = false;
    pullDist = 0;
  }

  document.addEventListener('touchstart', function (ev) {
    if (busy || ev.touches.length !== 1) { return; }
    if (!atTopOfPage(ev.target)) { return; }
    startY = ev.touches[0].clientY;
    pulling = true;
    pullDist = 0;
  }, { passive: true, capture: true });

  document.addEventListener('touchmove', function (ev) {
    if (!pulling || ev.touches.length !== 1) { return; }
    var dy = ev.touches[0].clientY - startY;
    if (dy <= 0) { pullDist = 0; hidePill(); return; }
    if (!atTopOfPage(ev.target)) { resetPull(); hidePill(); return; }
    pullDist = Math.min(MAX_PULL, dy * 0.5);
    showPill(pullDist, pullDist >= TRIGGER ? '松开刷新' : '下拉刷新');
    if (ev.cancelable && dy > 8) { ev.preventDefault(); }
  }, { passive: false, capture: true });

  document.addEventListener('touchend', function () {
    if (!pulling) { return; }
    var dist = pullDist;
    resetPull();
    if (dist >= TRIGGER) {
      busy = true;
      showPill(MAX_PULL, '正在刷新…');
      post({ type: 'pullRefresh' });
      // 兜底：10 秒后无论是否真的刷新了都收起，避免卡住。
      setTimeout(function () { busy = false; hidePill(); }, 10000);
    } else {
      hidePill();
    }
  }, { passive: true, capture: true });

  document.addEventListener('touchcancel', function () {
    resetPull();
    if (!busy) { hidePill(); }
  }, { passive: true, capture: true });
})();
''';
