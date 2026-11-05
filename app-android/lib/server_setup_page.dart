import 'dart:async';

import 'package:flutter/material.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'theme.dart';
import 'web_view_page.dart';
import 'widgets/splash_view.dart';

/// 首启 / 修改服务器地址页。
///
/// - [firstRun] = true：没有可用地址，连上后直接替换进入 WebView，返回到此页会退出 App；
/// - [firstRun] = false：从 WebView / 错误页点「换地址」进来的编辑态，连接成功后
///   把新地址 pop 回给调用方（由调用方重建 WebView，历史干净）。
class ServerSetupPage extends StatefulWidget {
  const ServerSetupPage({super.key, this.initialUrl, this.firstRun = false});

  final bool firstRun;
  final String? initialUrl;

  @override
  State<ServerSetupPage> createState() => _ServerSetupPageState();
}

class _ServerSetupPageState extends State<ServerSetupPage> {
  late final TextEditingController _controller =
      TextEditingController(text: widget.initialUrl ?? '');
  String? _error;
  bool _saving = false;
  Timer? _draftTimer;

  @override
  void initState() {
    super.initState();
    final String initial = widget.initialUrl ?? '';
    if (initial.isNotEmpty) {
      // 预填旧地址时全选，便于直接输入覆盖。
      _controller.selection =
          TextSelection(baseOffset: 0, extentOffset: initial.length);
    } else {
      _restoreDraft();
    }
    _controller.addListener(_scheduleDraftSave);
  }

  @override
  void dispose() {
    _draftTimer?.cancel();
    _controller.removeListener(_scheduleDraftSave);
    _controller.dispose();
    super.dispose();
  }

  /// 输入即存草稿：用户切走/App 被系统杀掉，下次回来地址还在。
  void _scheduleDraftSave() {
    _draftTimer?.cancel();
    _draftTimer = Timer(const Duration(milliseconds: 500), () async {
      final SharedPreferences prefs = await SharedPreferences.getInstance();
      final String text = _controller.text.trim();
      if (text.isEmpty) {
        await prefs.remove(kServerUrlDraftKey);
      } else {
        await prefs.setString(kServerUrlDraftKey, text);
      }
    });
  }

  Future<void> _restoreDraft() async {
    final SharedPreferences prefs = await SharedPreferences.getInstance();
    final String? draft = prefs.getString(kServerUrlDraftKey);
    if (!mounted || draft == null || draft.isEmpty || _controller.text.isNotEmpty) {
      return;
    }
    _controller.text = draft;
    _controller.selection =
        TextSelection(baseOffset: 0, extentOffset: draft.length);
  }

  Future<void> _save() async {
    final String raw = _controller.text.trim();
    final String? normalized = normalizeUrl(raw);
    if (normalized == null) {
      setState(() {
        _error = '地址格式不正确，需以 http:// 或 https:// 开头，'
            '例如 http://192.168.31.5:7788';
      });
      return;
    }
    setState(() {
      _error = null;
      _saving = true;
    });
    final SharedPreferences prefs = await SharedPreferences.getInstance();
    await prefs.setString(kServerUrlKey, normalized);
    if (!mounted) return;
    if (widget.firstRun) {
      Navigator.of(context).pushReplacement(MaterialPageRoute<void>(
        builder: (_) => WebViewPage(serverUrl: normalized),
      ));
    } else {
      Navigator.of(context).pop(normalized);
    }
  }

  @override
  Widget build(BuildContext context) {
    return PopScope(
      // 首启页空手退出会让用户下次仍是白板，先留在这一页；
      // 编辑态允许返回（等价于取消）。
      canPop: !widget.firstRun,
      child: Scaffold(
        appBar: AppBar(
          title: Text(widget.firstRun ? '连接 Astra 控制面' : '服务器地址'),
        ),
        body: SafeArea(
          child: ListView(
            padding: const EdgeInsets.fromLTRB(24, 8, 24, 24),
            keyboardDismissBehavior: ScrollViewKeyboardDismissBehavior.onDrag,
            children: <Widget>[
              if (widget.firstRun) ...<Widget>[
                const SizedBox(height: 16),
                const Center(child: AstraLogo(size: 64)),
                const SizedBox(height: 24),
              ],
              const Text(
                '输入 Astra 控制面的地址，手机与服务器需在同一网络。',
                style: TextStyle(color: Colors.white70, height: 1.6),
              ),
              const SizedBox(height: 20),
              TextField(
                controller: _controller,
                keyboardType: TextInputType.url,
                textInputAction: TextInputAction.go,
                autocorrect: false,
                enableSuggestions: false,
                autofillHints: const <String>[AutofillHints.url],
                autofocus: widget.firstRun,
                decoration: InputDecoration(
                  border: const OutlineInputBorder(),
                  labelText: '服务器地址',
                  hintText: 'http://192.168.31.5:7788',
                  errorText: _error,
                  errorMaxLines: 3,
                  prefixIcon: const Icon(Icons.dns_outlined, size: 20),
                ),
                onSubmitted: (_) => _save(),
              ),
              const SizedBox(height: 12),
              const Text(
                '局域网地址可以直接写 IP:端口，会自动补 http://。',
                style: TextStyle(fontSize: 12, color: Colors.white38),
              ),
              const SizedBox(height: 24),
              FilledButton(
                onPressed: _saving ? null : _save,
                child: Text(_saving ? '保存中…' : '连接'),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

/// 校验并规范化地址：无 scheme 时自动补 http://，无效返回 null。
String? normalizeUrl(String raw) {
  String input = raw.trim();
  if (input.isEmpty) return null;
  if (!input.startsWith('http://') && !input.startsWith('https://')) {
    input = 'http://$input';
  }
  final Uri? uri = Uri.tryParse(input);
  if (uri == null ||
      (uri.scheme != 'http' && uri.scheme != 'https') ||
      uri.host.isEmpty) {
    return null;
  }
  return input;
}
