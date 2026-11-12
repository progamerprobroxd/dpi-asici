package com.dpiasici.app

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.VpnService
import android.net.VpnService.Builder
import android.os.Build
import android.os.ParcelFileDescriptor
import go.core.Core

/**
 * Tek tıkla açılan yerel VPN. Tüm trafik TUN arayüzünden Go çekirdeğine gider;
 * çekirdek TLS ClientHello'yu parçalayıp DNS'i DoH ile çözer. Uzak bir sunucu yoktur.
 */
class DpiVpnService : VpnService() {

    companion object {
        const val ACTION_START = "com.dpiasici.app.START"
        const val ACTION_STOP = "com.dpiasici.app.STOP"
        private const val CHANNEL = "vpn"
        private const val NOTIF_ID = 1

        @Volatile var running = false
        @Volatile var error = ""
    }

    private var tun: ParcelFileDescriptor? = null
    private var cm: ConnectivityManager? = null
    private var netCallback: ConnectivityManager.NetworkCallback? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopVpn()
            stopSelf()
            return START_NOT_STICKY
        }
        showNotification()
        if (!running) startVpn()
        return START_STICKY
    }

    private fun startVpn() {
        try {
            val mode = getSharedPreferences("ayar", MODE_PRIVATE).getString("mode", "auto") ?: "auto"
            val b = Builder()
                .setSession("DPI Aşıcı")
                .setMtu(1500)
                // Yalnızca IPv4: IPv6'yı yakalarsak IPv4-only ağlarda uygulamalar yedek yola dönemez.
                // Android, IPv4-only VPN'de IPv6'yı kapatır; uygulamalar IPv4'e geçer.
                .addAddress("10.111.0.2", 32)
                .addRoute("0.0.0.0", 0)
                .addDnsServer("1.1.1.1")
                .addDnsServer("1.0.0.1")
                // Uygulamanın kendi trafiği (DoH ve çıkış bağlantıları) VPN'e girmez; döngü olmaz.
                .addDisallowedApplication(packageName)
            if (Build.VERSION.SDK_INT >= 29) b.setMetered(false)

            val pfd = b.establish() ?: throw IllegalStateException("VPN arayüzü açılamadı")
            tun = pfd
            Core.start(pfd.fd.toLong(), 1500L, mode)
            running = true
            error = ""
            watchNetwork()
        } catch (t: Throwable) {
            error = "Başlatılamadı: " + (t.message ?: t.javaClass.simpleName)
            stopVpn()
            stopSelf()
        }
    }

    private fun stopVpn() {
        unwatchNetwork()
        try { Core.stop() } catch (e: Throwable) {}
        try { tun?.close() } catch (e: Exception) {}
        tun = null
        running = false
        try { stopForeground(STOP_FOREGROUND_REMOVE) } catch (e: Exception) {}
    }

    /** Wi-Fi ↔ mobil veri geçişinde öğrenilen yöntemleri sıfırlar. */
    private fun watchNetwork() {
        val c = getSystemService(ConnectivityManager::class.java) ?: return
        val cb = object : ConnectivityManager.NetworkCallback() {
            private var first = true
            override fun onAvailable(network: Network) {
                if (first) { first = false; return }
                try { Core.networkChanged() } catch (e: Throwable) {}
            }
        }
        try {
            c.registerDefaultNetworkCallback(cb)
            cm = c
            netCallback = cb
        } catch (e: Exception) {}
    }

    private fun unwatchNetwork() {
        val cb = netCallback ?: return
        try { cm?.unregisterNetworkCallback(cb) } catch (e: Exception) {}
        netCallback = null
    }

    private fun showNotification() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL, "VPN durumu", NotificationManager.IMPORTANCE_LOW)
        )
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, DpiVpnService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE
        )
        @Suppress("DEPRECATION")
        val n = Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_notif)
            .setContentTitle("DPI Aşıcı açık")
            .setContentText("Bağlantılar parçalanarak gönderiliyor.")
            .setContentIntent(open)
            .addAction(R.drawable.ic_notif, "Kapat", stop)
            .setOngoing(true)
            .build()
        startForeground(NOTIF_ID, n)
    }

    /** Kullanıcı sistem ayarlarından VPN'i kapatırsa. */
    override fun onRevoke() {
        stopVpn()
        stopSelf()
    }

    override fun onDestroy() {
        stopVpn()
        super.onDestroy()
    }
}
