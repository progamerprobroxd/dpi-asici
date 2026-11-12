# DPI Aşıcı: Android uygulaması (tek tık VPN)

Uygulama telefonda **yerel bir VPN** olarak çalışır. Proxy/port girmezsin, Wi-Fi ve mobil veride çalışır.
Trafik başka bir sunucuya gitmez; cihazda parçalanıp normal yoldan çıkar. IP adresini gizlemez, trafiği şifrelemez.

APK'yı bu bilgisayarda derleyemediğim için GitHub'ın ücretsiz sunucusunda derletiyoruz (5-10 dk).

## 1. GitHub hesabı ve boş depo
1. https://github.com adresinde hesap aç (ücretsiz).
2. **New repository** → ad: `dpi-asici` → **Private** (özel) seç → Create.
   Özel olması önemli: projedeki imza anahtarı herkese açık olmamalı.

## 2. Erişim anahtarı (token) al
GitHub → Settings → Developer settings → Personal access tokens → **Tokens (classic)** → Generate new token.
`repo` ve `workflow` kutularını işaretle, oluştur, çıkan `ghp_...` yazısını kopyala.

## 3. Dosyaları yükle (Termux'ta)
zip dosyasını telefonun İndirilenler klasörüne kaydet, sonra Termux'ta sırayla:
```
pkg install -y git unzip
cd ~
unzip -o ~/storage/downloads/dpi-asici-android.zip
cd dpi-android
git config --global user.name "adın"
git config --global user.email "mailin@ornek.com"
git init -b main
git add -A
git commit -m "ilk sürüm"
git remote add origin https://github.com/KULLANICI_ADIN/dpi-asici.git
git push -u origin main
```
Kullanıcı adı sorulunca GitHub kullanıcı adını, parola sorulunca **token'ı** yapıştır.

## 4. APK'yı indir
Depoda **Actions** sekmesi → "APK derle" çalışıyor. Yeşil tik olunca depo ana sayfasında
sağdaki **Releases** → en yeni `build-1` → `app-release.apk` dosyasına dokun, indir, kur
(Chrome "bilinmeyen kaynak" izni isteyebilir).

Derleme **kırmızı çarpı** olursa Actions'taki başarısız adımın son ~40 satırını bana at, düzeltirim.

## Kullanım
Uygulamayı aç → büyük düğmeye dokun → VPN iznini onayla. Bitti. Kapatmak için tekrar dokun.
Xiaomi/MIUI'de: uygulama bilgisi → Pil → **Kısıtlama yok**, ve son uygulamalarda kartı kilitle.
