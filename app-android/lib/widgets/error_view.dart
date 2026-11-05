import 'package:flutter/material.dart';

import '../bridge.dart';
import '../theme.dart';

/// 全屏错误页：图标 + 人话原因 + 三个自救按钮。
///
/// 放在 [RefreshIndicator] 里使用（外层是可滚动的 ListView/ScrollView），
/// 所以这里用 [SingleChildScrollView] + 最小高度约束，小屏/键盘弹出时也不溢出。
class ErrorView extends StatelessWidget {
  const ErrorView({
    super.key,
    required this.failure,
    required this.serverUrl,
    required this.onRetry,
    required this.onChangeAddress,
    required this.onCopyDiagnostic,
    this.networkDown = false,
  });

  final LoadFailure failure;
  final String serverUrl;
  final VoidCallback onRetry;
  final VoidCallback onChangeAddress;
  final VoidCallback onCopyDiagnostic;
  final bool networkDown;

  IconData get _icon => switch (failure.kind) {
        LoadFailureKind.unreachable => Icons.wifi_off_rounded,
        LoadFailureKind.serverError => Icons.dns_outlined,
        LoadFailureKind.badAddress => Icons.link_off_rounded,
        LoadFailureKind.unknown => Icons.error_outline_rounded,
      };

  @override
  Widget build(BuildContext context) {
    return ColoredBox(
      color: kDarkBg,
      child: LayoutBuilder(
        builder: (BuildContext context, BoxConstraints constraints) {
          return SingleChildScrollView(
            physics: const AlwaysScrollableScrollPhysics(),
            child: ConstrainedBox(
              constraints: BoxConstraints(minHeight: constraints.maxHeight),
              child: Center(
                child: Padding(
                  padding: const EdgeInsets.symmetric(horizontal: 28, vertical: 32),
                  child: Column(
                    mainAxisSize: MainAxisSize.min,
                    crossAxisAlignment: CrossAxisAlignment.stretch,
                    children: <Widget>[
                      Center(
                        child: Container(
                          width: 76,
                          height: 76,
                          decoration: BoxDecoration(
                            color: Colors.white.withValues(alpha: 0.05),
                            shape: BoxShape.circle,
                          ),
                          child: Icon(_icon, size: 36, color: Colors.white38),
                        ),
                      ),
                      const SizedBox(height: 20),
                      Text(
                        failure.title,
                        textAlign: TextAlign.center,
                        style: const TextStyle(
                          fontSize: 19,
                          fontWeight: FontWeight.w600,
                          color: Colors.white,
                        ),
                      ),
                      const SizedBox(height: 10),
                      Text(
                        failure.hint,
                        textAlign: TextAlign.center,
                        style: const TextStyle(
                          fontSize: 13,
                          height: 1.6,
                          color: Colors.white54,
                        ),
                      ),
                      const SizedBox(height: 18),
                      _InfoChip(
                        icon: Icons.link_rounded,
                        text: failure.url?.isNotEmpty == true ? failure.url! : serverUrl,
                      ),
                      if (failure.codeLine != null) ...<Widget>[
                        const SizedBox(height: 8),
                        _InfoChip(
                          icon: Icons.terminal_rounded,
                          text: failure.codeLine!,
                        ),
                      ],
                      if (networkDown) ...<Widget>[
                        const SizedBox(height: 8),
                        const _InfoChip(
                          icon: Icons.signal_cellular_off_rounded,
                          text: '设备当前没有网络连接',
                          tint: Color(0xFFFFB74D),
                        ),
                      ],
                      const SizedBox(height: 28),
                      FilledButton.icon(
                        onPressed: onRetry,
                        icon: const Icon(Icons.refresh_rounded, size: 20),
                        label: const Text('重试'),
                      ),
                      const SizedBox(height: 12),
                      OutlinedButton.icon(
                        onPressed: onChangeAddress,
                        icon: const Icon(Icons.edit_outlined, size: 18),
                        label: const Text('换地址'),
                      ),
                      const SizedBox(height: 4),
                      TextButton.icon(
                        onPressed: onCopyDiagnostic,
                        icon: const Icon(Icons.copy_rounded, size: 16),
                        label: const Text('复制诊断信息'),
                        style: TextButton.styleFrom(
                          foregroundColor: Colors.white54,
                          minimumSize: const Size.fromHeight(44),
                        ),
                      ),
                      const SizedBox(height: 8),
                      const Text(
                        '下拉页面也可以重试',
                        textAlign: TextAlign.center,
                        style: TextStyle(fontSize: 11, color: Colors.white24),
                      ),
                    ],
                  ),
                ),
              ),
            ),
          );
        },
      ),
    );
  }
}

class _InfoChip extends StatelessWidget {
  const _InfoChip({required this.icon, required this.text, this.tint});

  final IconData icon;
  final String text;
  final Color? tint;

  @override
  Widget build(BuildContext context) {
    final Color color = tint ?? Colors.white38;
    return Row(
      mainAxisAlignment: MainAxisAlignment.center,
      children: <Widget>[
        Icon(icon, size: 13, color: color),
        const SizedBox(width: 6),
        Flexible(
          child: Text(
            text,
            maxLines: 3,
            overflow: TextOverflow.ellipsis,
            style: TextStyle(
              fontSize: 12,
              height: 1.4,
              color: color,
              fontFeatures: const <FontFeature>[FontFeature.tabularFigures()],
            ),
          ),
        ),
      ],
    );
  }
}
