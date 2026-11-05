import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

/// 品牌色 / 基础底色。与 Web 端深色主题保持一致，避免壳与页面切换时闪色。
const Color kBrandColor = Color(0xFF4C8DFF);
const Color kDarkBg = Color(0xFF121212);
const Color kDarkSurface = Color(0xFF1B1D22);

const String kServerUrlKey = 'server_url';
const String kServerUrlDraftKey = 'server_url_draft';

ThemeData buildAstraTheme() {
  final ColorScheme scheme = ColorScheme.fromSeed(
    seedColor: kBrandColor,
    brightness: Brightness.dark,
  ).copyWith(surface: kDarkBg);

  return ThemeData(
    brightness: Brightness.dark,
    scaffoldBackgroundColor: kDarkBg,
    colorScheme: scheme,
    appBarTheme: const AppBarTheme(
      backgroundColor: kDarkBg,
      surfaceTintColor: Colors.transparent,
      elevation: 0,
      centerTitle: false,
    ),
    snackBarTheme: const SnackBarThemeData(
      behavior: SnackBarBehavior.floating,
      backgroundColor: kDarkSurface,
      contentTextStyle: TextStyle(color: Colors.white, fontSize: 14),
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        minimumSize: const Size.fromHeight(48),
        backgroundColor: kBrandColor,
        foregroundColor: Colors.white,
      ),
    ),
    outlinedButtonTheme: OutlinedButtonThemeData(
      style: OutlinedButton.styleFrom(
        minimumSize: const Size.fromHeight(48),
        foregroundColor: Colors.white,
        side: const BorderSide(color: Colors.white24),
      ),
    ),
  );
}

/// 深色沉浸式系统栏样式：edge-to-edge 下状态栏/导航栏都透明，靠内容自己的背景色托底。
///
/// Android 15（API 35）强制 edge-to-edge 后 `statusBarColor` 会被忽略，
/// 所以这里统一用 transparent + 亮色图标，页面自己负责 statusBar 区域的安全区。
const SystemUiOverlayStyle kOverlayStyle = SystemUiOverlayStyle(
  statusBarColor: Colors.transparent,
  statusBarIconBrightness: Brightness.light,
  statusBarBrightness: Brightness.dark,
  systemNavigationBarColor: Colors.transparent,
  systemNavigationBarDividerColor: Colors.transparent,
  systemNavigationBarIconBrightness: Brightness.light,
  systemNavigationBarContrastEnforced: false,
  systemStatusBarContrastEnforced: false,
);
