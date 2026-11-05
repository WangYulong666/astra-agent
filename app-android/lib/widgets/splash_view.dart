import 'package:flutter/material.dart';

import '../theme.dart';

/// 品牌 Logo：纯代码绘制（无图片资源），深色底上的「A」字标。
class AstraLogo extends StatelessWidget {
  const AstraLogo({super.key, this.size = 88});

  final double size;

  @override
  Widget build(BuildContext context) {
    return Container(
      width: size,
      height: size,
      decoration: BoxDecoration(
        borderRadius: BorderRadius.circular(size * 0.28),
        gradient: const LinearGradient(
          begin: Alignment.topLeft,
          end: Alignment.bottomRight,
          colors: <Color>[Color(0xFF6FA8FF), kBrandColor, Color(0xFF2F63C4)],
        ),
        boxShadow: <BoxShadow>[
          BoxShadow(
            color: kBrandColor.withValues(alpha: 0.35),
            blurRadius: size * 0.4,
            spreadRadius: size * 0.04,
          ),
        ],
      ),
      alignment: Alignment.center,
      child: Text(
        'A',
        style: TextStyle(
          fontSize: size * 0.52,
          fontWeight: FontWeight.w700,
          height: 1,
          color: Colors.white,
        ),
      ),
    );
  }
}

/// 启动闪屏：品牌色 + logo + 「正在连接…」。
///
/// 只在冷启动探测服务器时出现，最长 [maxWaitSeconds] 秒后由调用方跳转。
class SplashView extends StatelessWidget {
  const SplashView({super.key, this.message = '正在连接…', this.showProgress = true});

  final String message;
  final bool showProgress;

  @override
  Widget build(BuildContext context) {
    return ColoredBox(
      color: kDarkBg,
      child: Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            const AstraLogo(),
            const SizedBox(height: 28),
            const Text(
              'Astra',
              style: TextStyle(
                fontSize: 24,
                fontWeight: FontWeight.w600,
                letterSpacing: 2,
                color: Colors.white,
              ),
            ),
            const SizedBox(height: 10),
            Text(
              message,
              style: const TextStyle(fontSize: 13, color: Colors.white54),
            ),
            const SizedBox(height: 32),
            SizedBox(
              width: 120,
              child: showProgress
                  ? ClipRRect(
                      borderRadius: BorderRadius.circular(2),
                      child: const LinearProgressIndicator(
                        minHeight: 2,
                        backgroundColor: Colors.white12,
                        valueColor: AlwaysStoppedAnimation<Color>(kBrandColor),
                      ),
                    )
                  : const SizedBox(height: 2),
            ),
          ],
        ),
      ),
    );
  }
}
