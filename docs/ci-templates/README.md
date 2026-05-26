# CI Workflow Templates — Manual Installation Required

این پوشه شامل workflow‌هایی است که نیاز به **نصب دستی** در `.github/workflows/`
دارند. علت این جداسازی محدودیت امنیتی استاندارد گیت‌هاب است:

> GitHub App‌هایی که توسط ابزارهای automation استفاده می‌شوند (مثل سندباکس
> توسعه AI) به‌طور پیش‌فرض permission مخصوص `workflows` ندارند. این
> permission فقط با تأیید دستی مالک repo قابل اعطا است. در نتیجه فایل‌های
> داخل `.github/workflows/` فقط توسط کاربر انسانی قابل ایجاد یا ویرایش‌اند.

این الگو در پروژه سابقه دارد: `docs/step23-ci-workflow-patch.md` و
`docs/step25-ci-workflow-patch.md`. این پوشه برای Step 27 (و هر استپ
آتی که چندین فایل workflow را همزمان لمس کند) ساخته شده است.

---

## فایل‌های موجود

### `build-android-apk.yml` — ساخت APK رسمی **MasterDnsPro**

این workflow یک APK گرافیکی اندروید با نام **MasterDnsPro** می‌سازد، آن
را با keystore شما امضا می‌کند، و به‌عنوان artifact `android-apk` به job
release اصلی تحویل می‌دهد. (Step 27 در `PLAN.md`.)

### `install.sh` — اسکریپت نصب خودکار (ترجیحی)

به‌جای اعمال دستی همه‌ی تغییرات، یک اسکریپت idempotent در همین پوشه
هست که همه‌ی ۵ تغییر لازم را با یک دستور انجام می‌دهد:

```bash
# روی ماشین شخصی شما (نه سندباکس)، از ریشه‌ی repo:
git checkout main                      # یا genspark_ai_developer برای PR
bash docs/ci-templates/install.sh
git diff                               # تغییرات را مرور کنید
git add -A
git commit -m "step 27 patch: install Android APK build pipeline + bump Go 1.21→1.25"
git push
```

**اسکریپت چه می‌کند؟** (همه به‌صورت idempotent — تکرار اجرا بی‌ضرر است)

| گام | تغییر |
|---|---|
| ۱ | کپی `build-android-apk.yml` به `.github/workflows/` |
| ۲ | تغییر `go-version: "1.21"` → `"1.25"` در `build-go.yml` و `build-test.yml` |
| ۳ | افزودن job میانی `build-android:` بالای job `release` در `build-go.yml` |
| ۴ | افزودن `build-android` به `needs:` و قید `if: ${{ always() && needs.build.result == 'success' }}` به job `release` |
| ۵ | گسترش `find` در ساخت SHA256SUMS به `*.apk` و افزودن `release_assets/**/*.apk` به `files:` در `softprops/action-gh-release` |

پس از اجرا، اسکریپت با `python3 yaml.safe_load` صحت syntax همه workflow‌ها
را بررسی می‌کند و در پایان دستورهای commit/push را نمایش می‌دهد.

### نصب کاملاً دستی (اگر `install.sh` را نمی‌خواهید اجرا کنید)

برای راهنمای گام‌به‌گام (در ۵ commit مجزا از طریق UI گیت‌هاب) به فایل
[`../step27-ci-workflow-patch.md`](../step27-ci-workflow-patch.md) مراجعه
کنید.

---

## دلیل `if: always()` در job release

workflow APK در حالت **Soft-skip** کار می‌کند — اگر variable
`ANDROID_APP_REPO` ست نشده باشد، با warning موفقانه exit می‌شود (نه
fail). بدون این `if`، شکست soft-skip باعث drop شدن release دسکتاپ
می‌شود.

به‌طور مشابه، اگر هرکدام از ۴ secret امضا (`ANDROID_SIGNING_KEY`,
`ANDROID_KEY_ALIAS`, `ANDROID_KEYSTORE_PASS`, `ANDROID_KEY_PASS`) ست
نشده باشد، APK ناامضا upload می‌شود با warning مشهود — همچنان release
دسکتاپ منتشر می‌شود.

---

## چه چیزهایی باید **خارج از سندباکس** انجام شوند

این لیست در PLAN.md (Step 27.5 و 27.6) هم آمده، اینجا برای دسترسی سریع
تکرار می‌شود:

### 1. Variable‌های repo (`Settings → Secrets and variables → Actions → Variables`)

| نام | مقدار | الزامی؟ |
|---|---|---|
| `ANDROID_APP_REPO` | `hamijafayi-debug/MasterDnsPro-Android` (یا اسم فورک نهایی شما) | ✅ بله — بدون این، workflow soft-skip می‌شود |
| `ANDROID_APP_REF` | `main` (یا branch/tag دیگر) | ❌ پیش‌فرض `main` |

### 2. Secret‌های امضای APK (`Settings → Secrets and variables → Actions → Secrets`)

| نام | محتوا |
|---|---|
| `ANDROID_SIGNING_KEY` | base64 از فایل `release.jks` (بدون newline) |
| `ANDROID_KEY_ALIAS` | alias کلید (مثلاً `masterdnspro`) |
| `ANDROID_KEYSTORE_PASS` | رمز keystore |
| `ANDROID_KEY_PASS` | رمز کلید |

اگر هرکدام موجود نباشد، APK ناامضا upload می‌شود (با warning مشهود).

### 3. تولید keystore (یک‌بار، لوکال)

```bash
keytool -genkey -v -keystore release.jks -keyalg RSA \
  -keysize 2048 -validity 10000 -alias masterdnspro
# محتوای keystore را به base64 تبدیل کنید:
base64 -i release.jks | tr -d '\n' > keystore_b64.txt
# محتوای keystore_b64.txt را در ANDROID_SIGNING_KEY بگذارید
```

⚠️ **هرگز** فایل `release.jks` یا `keystore_b64.txt` را commit نکنید.

### 4. ساخت فورک Android (یک‌بار)

WhiteDNS از `iampedii` **هیچ‌کدام از ۲۶ استپ بهبود ما را پشتیبانی
نمی‌کند** چون به هسته‌ی StormDNS بسته است. باید:

1. fork از `https://github.com/iampedii/WhiteDNS` به اکانت `hamijafayi-debug`.
2. خواندن و رعایت `TRADEMARK.MD` آپ‌استریم (تغییر اسم/آیکون/برند).
3. swap submodule هسته از StormDNS به MasterDnsVPN webapp:
   ```bash
   git submodule deinit -f third_party/StormDNS
   git rm -f third_party/StormDNS
   git submodule add https://github.com/hamijafayi-debug/MasterDnsVPN.git third_party/MasterDnsVPN
   git commit -am "swap core: StormDNS → MasterDnsVPN (26-step support)"
   ```
4. در `app/build.gradle.kts` تغییر go module/package از `stormdns-go`
   به `masterdnsvpn-go`.
5. تغییر برند به **MasterDnsPro**: `AndroidManifest.xml`، `strings.xml`،
   آیکون‌ها، رنگ‌ها.
6. push به branch `main` فورک.
7. مقدار variable `ANDROID_APP_REPO` در همین webapp repo را به آدرس
   فورک ست کنید.

---

## چرا ساختار به این شکل است؟

| لایه | نقش | محل |
|---|---|---|
| webapp (همین repo) | هسته‌ی Go با ۲۶ استپ بهبود | `internal/`, `cmd/` |
| فورک Android | اپ Kotlin/Compose با UI گرافیکی | repo جداگانه (`ANDROID_APP_REPO`) |
| `build-android-apk.yml` | چسب CI بین آن دو | همین فایل (پس از نصب) |

این جداسازی به این دلیل است که اپ Android کد build خاص خودش (Gradle،
NDK، Compose) دارد که شایسته نیست با هسته‌ی Go در یک repo ترکیب شود.
به‌جای آن، فورک Android این repo (webapp) را به‌عنوان submodule consume
می‌کند و workflow ما هر دو را در زمان build کنار هم می‌چیند.

---

## معیار سنجش (پس از نصب کامل)

- [ ] `bash docs/ci-templates/install.sh` اجرا شده.
- [ ] commit + push تغییرات.
- [ ] variable `ANDROID_APP_REPO` ست شده.
- [ ] هر ۴ secret امضا ست شده.
- [ ] فورک Android ساخته شده، submodule swap شده، برند MasterDnsPro.
- [ ] اولین Release: فایل `MasterDnsPro_Android_Universal.apk` در release
      هست و SHA256SUMS.txt شامل آن است.
