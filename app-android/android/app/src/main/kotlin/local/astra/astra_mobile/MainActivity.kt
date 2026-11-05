package local.astra.astra_mobile

import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodChannel

class MainActivity : FlutterActivity() {

    private var bridge: NativeBridge? = null

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)

        val nativeBridge = NativeBridge(this)
        bridge = nativeBridge

        MethodChannel(
            flutterEngine.dartExecutor.binaryMessenger,
            NativeBridge.METHOD_CHANNEL
        ).setMethodCallHandler(nativeBridge)

        EventChannel(
            flutterEngine.dartExecutor.binaryMessenger,
            NativeBridge.NETWORK_CHANNEL
        ).setStreamHandler(NetworkMonitor(applicationContext))
    }

    override fun onRequestPermissionsResult(
        requestCode: Int,
        permissions: Array<out String>,
        grantResults: IntArray
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        bridge?.onRequestPermissionsResult(requestCode, grantResults)
    }
}
