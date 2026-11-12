package com.dpiasici.app

import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import android.telephony.TelephonyManager
import android.webkit.JavascriptInterface
import android.webkit.WebView
import android.webkit.WebViewClient
import go.core.Core
import org.json.JSONObject
import java.util.Locale

/** Arayüz: assets/index.html. Bu sınıf yalnızca düğmeyi VPN servisine bağlar. */
class MainActivity : Activity() {

    private lateinit var web: WebView
    private val tr = Locale("tr", "TR")
    private val prefs by lazy { getSharedPreferences("ayar", MODE_PRIVATE) }
    @Volatile private var activityError = ""

    private companion object {
        const val REQ_VPN = 1
        const val REQ_NOTIF = 2
        val MODE_RE = Regex("^(auto|s[0-5])$")
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        window.statusBarColor = Color.parseColor("#101426")
        window.navigationBarColor = Color.parseColor("#101426")

        web = WebView(this)
        web.setBackgroundColor(Color.parseColor("#101426"))
        web.overScrollMode = WebView.OVER_SCROLL_NEVER
        web.settings.javaScriptEnabled = true
        web.settings.allowFileAccess = false
        web.settings.allowContentAccess = false
        web.webViewClient = object : WebViewClient() {
            // Sayfa dışına çıkışa izin verme.
            override fun shouldOverrideUrlLoading(view: WebView, url: String?) = true
        }
        web.addJavascriptInterface(Bridge(), "Android")
        setContentView(web)
        web.loadUrl("file:///android_asset/index.html")
    }

    // Ekran görünmezken sayfadaki zamanlayıcıları durdur: pil ve ısı için.
    override fun onPause() {
        web.onPause()
        web.pauseTimers()
        super.onPause()
    }

    override fun onResume() {
        super.onResume()
        web.onResume()
        web.resumeTimers()
    }

    override fun onDestroy() {
        web.removeJavascriptInterface("Android")
        web.destroy()
        super.onDestroy()
    }

    private fun startVpn() {
        activityError = ""
        DpiVpnService.error = ""
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission("android.permission.POST_NOTIFICATIONS") != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf("android.permission.POST_NOTIFICATIONS"), REQ_NOTIF)
        }
        val consent = VpnService.prepare(this)
        if (consent != null) {
            startActivityForResult(consent, REQ_VPN)
        } else {
            launchService()
        }
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == REQ_VPN) {
            if (resultCode == RESULT_OK) launchService()
            else activityError = "VPN izni verilmedi. Tekrar dokun ve izin ver."
        }
    }

    private fun launchService() {
        startForegroundService(Intent(this, DpiVpnService::class.java).setAction(DpiVpnService.ACTION_START))
    }

    private fun stopVpn() {
        startService(Intent(this, DpiVpnService::class.java).setAction(DpiVpnService.ACTION_STOP))
    }

    private fun operatorName(): String {
        val raw = try {
            getSystemService(TelephonyManager::class.java)?.networkOperatorName ?: ""
        } catch (e: Exception) { "" }
        return raw.trim().split(" ").filter { it.isNotEmpty() }.joinToString(" ") { w ->
            if (w.length <= 2) w.uppercase(tr)
            else w.substring(0, 1).uppercase(tr) + w.substring(1).lowercase(tr)
        }
    }

    // Bu uygulama VPN dışında tutulduğu için etkin ağ her zaman gerçek (fiziksel) ağdır.
    private fun netInfo(): JSONObject {
        val o = JSONObject()
        var type = "none"
        try {
            val cm = getSystemService(ConnectivityManager::class.java)
            val caps = cm?.getNetworkCapabilities(cm.activeNetwork)
            if (caps != null && caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) {
                type = when {
                    caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) ||
                        caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "wifi"
                    caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "mobile"
                    else -> "wifi"
                }
            }
        } catch (e: Exception) {}
        o.put("type", type)
        if (type == "mobile") o.put("operator", operatorName())
        return o
    }

    /** JavaScript tarafının çağırdığı küçük köprü. */
    private inner class Bridge {

        @JavascriptInterface
        fun state(): String {
            val o = JSONObject()
            o.put("running", DpiVpnService.running)
            o.put("mode", prefs.getString("mode", "auto"))
            o.put("error", DpiVpnService.error.ifEmpty { activityError })
            o.put("net", netInfo())
            try {
                o.put("stats", JSONObject(Core.stats()))
            } catch (t: Throwable) {
                o.put("stats", JSONObject())
                if (DpiVpnService.error.isEmpty() && activityError.isEmpty()) {
                    o.put("error", "Çekirdek yüklenemedi: " + (t.message ?: t.javaClass.simpleName))
                }
            }
            return o.toString()
        }

        @JavascriptInterface
        fun toggle() {
            runOnUiThread { if (DpiVpnService.running) stopVpn() else startVpn() }
        }

        @JavascriptInterface
        fun setMode(m: String) {
            if (!MODE_RE.matches(m)) return
            prefs.edit().putString("mode", m).apply()
            if (DpiVpnService.running) {
                try { Core.setMode(m) } catch (e: Throwable) {}
            }
        }

        @JavascriptInterface
        fun batterySettings() {
            runOnUiThread {
                try {
                    startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
                } catch (e: Exception) {
                    try {
                        startActivity(
                            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName"))
                        )
                    } catch (e: Exception) {}
                }
            }
        }
    }
}
