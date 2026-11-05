package local.astra.astra_mobile

import android.Manifest
import android.annotation.TargetApi
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.media.MediaScannerConnection
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.provider.MediaStore
import android.webkit.MimeTypeMap
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.io.File
import java.io.IOException
import java.util.concurrent.Executors

/**
 * 壳的原生能力：下载落盘、打开文件、外链跳转、联网状态。
 *
 * 全部手写 Platform Channel，不引第三方插件（本机 flutter pub get 可能没网）。
 *
 * 下载落盘分两条路：
 * - API 29+：MediaStore.Downloads（不需要任何存储权限，落公共「下载」目录，卸载 App 仍在）；
 * - API 24~28：写 getExternalStoragePublicDirectory(DOWNLOADS)，需要 WRITE_EXTERNAL_STORAGE，
 *   首次保存时才申请，拒绝就明确回「没有权限」而不是静默失败。
 */
class NativeBridge(private val activity: Activity) : MethodChannel.MethodCallHandler {

    companion object {
        const val METHOD_CHANNEL = "astra/native"
        const val NETWORK_CHANNEL = "astra/native/network"
        private const val REQUEST_WRITE_STORAGE = 0xA57A
        private const val MAX_NAME_LENGTH = 120
    }

    private val appContext: Context get() = activity.applicationContext
    private val io = Executors.newSingleThreadExecutor()
    private val main = Handler(Looper.getMainLooper())
    private var pendingPermissionSave: (() -> Unit)? = null

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            "saveFile" -> handleSave(call, result)
            "openFile" -> result.success(mapOf("ok" to openFile(call.argument<String>("uri"))))
            "openExternal" -> result.success(mapOf("ok" to openExternal(call.argument<String>("url"))))
            "isNetworkAvailable" -> result.success(mapOf("ok" to isOnline()))
            "deviceInfo" -> result.success(deviceInfo())
            else -> result.notImplemented()
        }
    }

    /** 由 MainActivity 转发，承接存储权限申请结果。 */
    fun onRequestPermissionsResult(requestCode: Int, grantResults: IntArray) {
        if (requestCode != REQUEST_WRITE_STORAGE) return
        val action = pendingPermissionSave
        pendingPermissionSave = null
        action?.invoke()
    }

    // ------------------------------------------------------------------ 保存

    private fun handleSave(call: MethodCall, result: MethodChannel.Result) {
        val name = sanitize(call.argument<String>("name") ?: "download")
        val bytes = call.argument<ByteArray>("bytes")
        if (bytes == null || bytes.isEmpty()) {
            result.success(error("文件内容为空"))
            return
        }
        val mime = call.argument<String>("mime")?.takeIf { it.isNotBlank() } ?: guessMime(name)

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            writeToMediaStore(name, mime, bytes, result)
            return
        }
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M || hasWritePermission()) {
            writeToDownloadsDir(name, mime, bytes, result)
            return
        }
        if (pendingPermissionSave != null) {
            result.success(error("上一次保存还没结束"))
            return
        }
        pendingPermissionSave = {
            if (hasWritePermission()) {
                writeToDownloadsDir(name, mime, bytes, result)
            } else {
                result.success(
                    mapOf(
                        "ok" to false,
                        "needsPermission" to true,
                        "error" to "未授予存储权限"
                    )
                )
            }
        }
        activity.requestPermissions(
            arrayOf(Manifest.permission.WRITE_EXTERNAL_STORAGE),
            REQUEST_WRITE_STORAGE
        )
    }

    @TargetApi(Build.VERSION_CODES.Q)
    private fun writeToMediaStore(
        name: String,
        mime: String,
        bytes: ByteArray,
        result: MethodChannel.Result
    ) {
        io.execute {
            try {
                val resolver = appContext.contentResolver
                val values = ContentValues().apply {
                    put(MediaStore.Downloads.DISPLAY_NAME, name)
                    put(MediaStore.Downloads.MIME_TYPE, mime)
                    put(MediaStore.Downloads.IS_PENDING, 1)
                }
                val collection = MediaStore.Downloads.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
                val uri = resolver.insert(collection, values)
                    ?: throw IOException("无法在下载目录创建条目")
                try {
                    resolver.openOutputStream(uri)?.use { it.write(bytes) }
                        ?: throw IOException("无法写入下载目录")
                    val published = ContentValues().apply {
                        put(MediaStore.Downloads.IS_PENDING, 0)
                    }
                    resolver.update(uri, published, null, null)
                } catch (e: Exception) {
                    runCatching { resolver.delete(uri, null, null) }
                    throw e
                }
                reply(
                    result,
                    mapOf(
                        "ok" to true,
                        "uri" to uri.toString(),
                        "displayPath" to "下载/$name",
                        "size" to bytes.size
                    )
                )
            } catch (e: Exception) {
                reply(result, error(e.message ?: "保存失败"))
            }
        }
    }

    private fun writeToDownloadsDir(
        name: String,
        mime: String,
        bytes: ByteArray,
        result: MethodChannel.Result
    ) {
        io.execute {
            try {
                @Suppress("DEPRECATION")
                val dir = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
                if (!dir.exists() && !dir.mkdirs()) throw IOException("下载目录不可用")
                val file = uniqueFile(dir, name)
                file.writeBytes(bytes)
                // 写完后让 MediaStore 收录，别的 App（文件管理器/预览器）才看得到。
                MediaScannerConnection.scanFile(
                    appContext,
                    arrayOf(file.absolutePath),
                    arrayOf(mime)
                ) { _, uri ->
                    reply(
                        result,
                        mapOf(
                            "ok" to true,
                            "uri" to (uri?.toString() ?: ""),
                            "path" to file.absolutePath,
                            "displayPath" to "下载/${file.name}",
                            "size" to bytes.size
                        )
                    )
                }
            } catch (e: Exception) {
                reply(result, error(e.message ?: "保存失败"))
            }
        }
    }

    private fun uniqueFile(dir: File, name: String): File {
        val base = File(dir, name)
        if (!base.exists()) return base
        val dot = name.lastIndexOf('.')
        val stem = if (dot > 0) name.substring(0, dot) else name
        val ext = if (dot > 0) name.substring(dot) else ""
        var index = 1
        while (index < 1000) {
            val candidate = File(dir, "$stem ($index)$ext")
            if (!candidate.exists()) return candidate
            index++
        }
        return File(dir, "$stem-${System.currentTimeMillis()}$ext")
    }

    private fun hasWritePermission(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.M ||
            activity.checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) ==
            PackageManager.PERMISSION_GRANTED

    // ------------------------------------------------------------------ 打开

    private fun openFile(raw: String?): Boolean {
        if (raw.isNullOrBlank()) return false
        val uri = Uri.parse(raw)
        val mime = appContext.contentResolver.getType(uri) ?: guessMime(raw)
        return try {
            val intent = Intent(Intent.ACTION_VIEW).apply {
                setDataAndType(uri, mime)
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            appContext.startActivity(intent)
            true
        } catch (e: ActivityNotFoundException) {
            false
        } catch (e: Exception) {
            false
        }
    }

    /** 外链 / tel: / mailto: 一律交给系统处理。 */
    private fun openExternal(raw: String?): Boolean {
        if (raw.isNullOrBlank()) return false
        return try {
            val intent = Intent(Intent.ACTION_VIEW, Uri.parse(raw)).apply {
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            }
            appContext.startActivity(intent)
            true
        } catch (e: ActivityNotFoundException) {
            false
        } catch (e: Exception) {
            false
        }
    }

    // ------------------------------------------------------------------ 杂项

    private fun connectivityManager(): ConnectivityManager? =
        appContext.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager

    @Suppress("DEPRECATION")
    fun isOnline(): Boolean {
        val cm = connectivityManager() ?: return false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            val network = cm.activeNetwork ?: return false
            val caps = cm.getNetworkCapabilities(network) ?: return false
            // 不用 VALIDATED：内网常有「连着 Wi-Fi 但没有外网」的场景，
            // 用 VALIDATED 会把可用的局域网服务器误判成断网。
            return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        }
        val info = cm.activeNetworkInfo ?: return false
        return info.isConnected
    }

    private fun deviceInfo(): Map<String, Any?> = mapOf(
        "sdk" to Build.VERSION.SDK_INT,
        "release" to Build.VERSION.RELEASE,
        "model" to "${Build.MANUFACTURER} ${Build.MODEL}".trim(),
        "packageName" to appContext.packageName
    )

    private fun guessMime(name: String): String {
        val ext = name.substringAfterLast('.', "").lowercase()
        if (ext.isEmpty()) return "application/octet-stream"
        return MimeTypeMap.getSingleton().getMimeTypeFromExtension(ext)
            ?: "application/octet-stream"
    }

    private fun sanitize(raw: String): String {
        val cleaned = raw
            .replace(Regex("[\\\\/:*?\"<>|]"), "_")
            .filter { it.code >= 0x20 && it.code != 0x7f }
            .trim()
            .trimStart('.')
        val safe = cleaned.ifEmpty { "download" }
        if (safe.length <= MAX_NAME_LENGTH) return safe
        val dot = safe.lastIndexOf('.')
        return if (dot > 0 && safe.length - dot <= 12) {
            safe.substring(0, MAX_NAME_LENGTH - (safe.length - dot)) + safe.substring(dot)
        } else {
            safe.substring(0, MAX_NAME_LENGTH)
        }
    }

    private fun error(message: String): Map<String, Any?> =
        mapOf("ok" to false, "error" to message)

    private fun reply(result: MethodChannel.Result, payload: Map<String, Any?>) {
        main.post { result.success(payload) }
    }
}

/**
 * 联网状态变化流（EventChannel）。取 ConnectivityManager 的默认网络回调，
 * 不用轮询，断开/恢复都能立刻上报。
 */
class NetworkMonitor(context: Context) : EventChannel.StreamHandler {
    private val appContext: Context = context.applicationContext
    private val main = Handler(Looper.getMainLooper())
    private var sink: EventChannel.EventSink? = null
    private var callback: ConnectivityManager.NetworkCallback? = null

    override fun onListen(arguments: Any?, events: EventChannel.EventSink?) {
        sink = events
        emit()
        val cm = connectivityManager() ?: return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) = emit()
            override fun onLost(network: Network) = emit()
            override fun onCapabilitiesChanged(
                network: Network,
                capabilities: NetworkCapabilities
            ) = emit()
        }
        callback = cb
        runCatching {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                cm.registerDefaultNetworkCallback(cb)
            } else {
                val request = NetworkRequest.Builder()
                    .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                    .build()
                cm.registerNetworkCallback(request, cb)
            }
        }
    }

    override fun onCancel(arguments: Any?) {
        val cb = callback
        if (cb != null) {
            runCatching { connectivityManager()?.unregisterNetworkCallback(cb) }
        }
        callback = null
        sink = null
    }

    private fun connectivityManager(): ConnectivityManager? =
        appContext.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager

    @Suppress("DEPRECATION")
    private fun isOnline(): Boolean {
        val cm = connectivityManager() ?: return false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            val network = cm.activeNetwork ?: return false
            val caps = cm.getNetworkCapabilities(network) ?: return false
            return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        }
        return cm.activeNetworkInfo?.isConnected == true
    }

    private fun emit() {
        val online = isOnline()
        main.post { sink?.success(online) }
    }
}
