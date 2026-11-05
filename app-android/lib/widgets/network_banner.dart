import 'package:flutter/material.dart';

/// 顶部网络状态条：断网时常驻，恢复后短暂显示「已恢复」再自动收起。
class NetworkBanner extends StatelessWidget {
  const NetworkBanner({
    super.key,
    required this.visible,
    this.offline = true,
  });

  final bool visible;

  /// true = 断网（红/橙）；false = 刚恢复（绿）。
  final bool offline;

  @override
  Widget build(BuildContext context) {
    final Color bg = offline ? const Color(0xFF5A2A1E) : const Color(0xFF1E3A24);
    final Color fg = offline ? const Color(0xFFFFB74D) : const Color(0xFF81C784);
    final IconData icon =
        offline ? Icons.wifi_off_rounded : Icons.wifi_rounded;
    final String label = offline ? '网络已断开，恢复后会自动重新加载' : '网络已恢复';

    return AnimatedSwitcher(
      duration: const Duration(milliseconds: 180),
      child: !visible
          ? const SizedBox(width: double.infinity, key: ValueKey<String>('hidden'))
          : Material(
              key: ValueKey<String>(offline ? 'offline' : 'online'),
              color: bg,
              child: Padding(
                padding: const EdgeInsets.fromLTRB(16, 8, 16, 8),
                child: Row(
                  children: <Widget>[
                    Icon(icon, size: 15, color: fg),
                    const SizedBox(width: 8),
                    Expanded(
                      child: Text(
                        label,
                        style: TextStyle(fontSize: 12.5, color: fg),
                      ),
                    ),
                  ],
                ),
              ),
            ),
    );
  }
}
