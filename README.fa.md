# Skirk

[English](README.md)

<p align="center">
  <img src="assets/logo.png" alt="Skirk logo" width="160">
</p>

Skirk یک ابزار Go-first برای تست شبکه‌های محدود است. روی دستگاه کلاینت یک
پروکسی SOCKS5، پروکسی HTTP اختیاری، یا VPN اندروید می‌دهد و ترافیک TCP را به
صورت رمزگذاری‌شده از مسیر Google Drive `appDataFolder` به یک دستگاه خروجی
می‌رساند. دستگاه خروجی همان جایی است که اینترنت معمولی دارد و درخواست‌ها را
به مقصد واقعی وصل می‌کند.

استفاده از Skirk فقط برای کار قانونی، با اجازه، روی اکانت و شبکه‌ای است که
خودتان حق استفاده از آن را دارید. این پروژه وابسته به Google، Google Cloud،
Google Drive، Cloudflare، GitHub، Microsoft، Android یا هیچ ارائه‌دهنده‌ی
دیگری نیست. قبل از استفاده یا انتشار، حتما [DISCLAIMER.md](DISCLAIMER.md) را
بخوانید.

## چه چیزهایی لازم دارید

- یک دستگاه خروجی با اینترنت عادی. VPS بهتر است، چون همیشه روشن می‌ماند؛ ولی
  لپ‌تاپ یا سرور خانگی هم تا وقتی آنلاین باشد کار می‌کند.
- یک اکانت Google برای mailbox داخل Drive.
- یک پروفایل `skirk:...` که می‌توانید برای دستگاه‌های کلاینت بفرستید.

کلاینت‌ها لازم نیست وارد Google شوند، `gcloud` نصب کنند، یا پروژه Google Cloud
داشته باشند. راه‌اندازی فقط یک بار روی دستگاه خروجی انجام می‌شود و در آخر یک
متن یک‌خطی می‌دهد که برای کلاینت فرستاده می‌شود.
همان پروفایل را می‌شود روی چند دستگاه import کرد. هر کلاینت برای خودش یک
شناسه محلی می‌سازد و هر بار اتصال هم یک شناسه اجرای تازه دارد، برای همین
جواب‌های Drive بین دو دستگاه قاطی نمی‌شود.

## شروع سریع

روی دستگاه خروجی Skirk را نصب کنید:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

یک kit بسازید:

```bash
skirk setup init --out skirk-kit
```

اگر نیاز به ورود Google باشد، Skirk لینک/کد ورود را نشان می‌دهد. روی Linux اگر
Google Cloud CLI نصب نباشد، Skirk می‌تواند آن را زیر `~/google-cloud-sdk` نصب
کند. برای اینکه quota روی پروژه خودتان حساب شود، بهتر است OAuth client خودتان
را بدهید:

```bash
skirk setup init --out skirk-kit --reset-google-login --oauth-client-file ./oauth-client.json
```

بعد exit را روشن کنید:

```bash
skirk serve-exit --config skirk-kit/exit.json
```

فایل `skirk-kit/client.skirk` همان پروفایل یک‌خطی کلاینت است. آن را برای
کلاینت بفرستید.

روی کلاینت Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"

read -r SKIRK_CLIENT_CONFIG
# پروفایل skirk:... را paste کنید، Enter بزنید، بعد:
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080
```

برای تست:

```bash
curl --socks5-hostname 127.0.0.1:18080 http://example.com/
```

اگر برنامه‌ای که استفاده می‌کنید گزینه `socks5h` دارد، همان را انتخاب کنید تا
DNS هم از مسیر exit رد شود.

## کلاینت‌ها

روی Linux یا سرورهای بدون محیط گرافیکی، همان CLI کافی است:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080
```

اگر روی یک Linux همیشه از همین کلاینت استفاده می‌کنید، بهتر است یک client ID
ثابت بدهید. این مقدار secret نیست؛ فقط این دستگاه را از بقیه دستگاه‌هایی که
همان پروفایل را دارند جدا می‌کند:

```bash
skirk serve-client --config client.skirk --listen 127.0.0.1:18080 --client-id my-laptop
```

روی Windows، نسخه portable دسکتاپ را از Release دانلود کنید. برنامه همان
پروفایل یک‌خطی `skirk:` را import می‌کند و sidecar داخلی Skirk را بالا
می‌آورد. فعلا مسیر Windows proxy-first است؛ یعنی مرورگر یا برنامه را روی
SOCKS5 `127.0.0.1:18080` تنظیم کنید.

روی Android، برنامه Android را نصب کنید، پروفایل یک‌خطی را import کنید، حالت
`VPN` را انتخاب کنید و `Connect` بزنید. Android دفعه اول اجازه VPN می‌خواهد.
حالت `Proxy` هم هست، ولی فقط وقتی مناسب است که خود برنامه یا دستگاه دیگری در
LAN بتواند SOCKS5 را مستقیم تنظیم کند.

جزئیات بیشتر در [docs/clients.md](docs/clients.md) آمده است.

## تست در شبکه محدود

پروفایل‌های کلاینت به صورت پیش‌فرض از `google_front` استفاده می‌کنند؛ یعنی
ترافیک Google API را از مسیری با ظاهر Google عبور می‌دهند. exit معمولا
`direct` است، چون خودش اینترنت عادی دارد.

اگر شبکه محدود روی سیستم شما به شکل یک SOCKS محلی در دسترس است:

```bash
skirk serve-client \
  --config "$SKIRK_CLIENT_CONFIG" \
  --listen 127.0.0.1:18080 \
  --route-mode google_front \
  --upstream-proxy socks5h://127.0.0.1:11093
```

برای تست سرعت روی اینترنت عادی، `--upstream-proxy` را حذف کنید. اگر خواستید
مستقیم به Google API وصل شوید:

```bash
skirk serve-client --config "$SKIRK_CLIENT_CONFIG" --listen 127.0.0.1:18080 --route-mode direct
```

## بنچمارک و لاگ

وقتی exit روشن است، latency، throughput و مصرف تقریبی Drive API را این‌طور
اندازه بگیرید:

```bash
skirk bench-live --config skirk-kit/client.skirk --samples 5
```

تست از مسیر شبکه محدود:

```bash
skirk bench-live \
  --config skirk-kit/client.skirk \
  --upstream-proxy socks5h://127.0.0.1:11093 \
  --route-mode google_front \
  --samples 3
```

برای تست حجم بالاتر:

```bash
skirk bench-live --config skirk-kit/client.skirk --bulk-url http://example.com/big.bin
```

لاگ‌ها در هر دقیقه تعداد عملیات Drive، quota تخمینی، خطاها، حجم پاسخ‌ها و
زمان عملیات‌ها را نشان می‌دهند. اگر OAuth client خودتان را استفاده می‌کنید،
Google Cloud Console منبع دقیق‌تر برای quota پروژه است.

## قطع کردن و پاکسازی

در حالت عادی، runtime فایل‌های mailbox را بعد از پردازش حذف می‌کند. `serve-exit`
هم یک janitor خودکار دارد که آبجکت‌های قدیمی `muxv4/`، مسیر legacy `muxv3/`،
`control/` و `data/` را که بیشتر از ۲۴ ساعت مانده‌اند پاک می‌کند.

پاکسازی دستی به صورت dry-run:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h
```

حذف واقعی:

```bash
skirk cleanup --config skirk-kit/exit.json --older-than 2h --delete
```

برای revoke کردن OAuth token داخل config:

```bash
skirk revoke --config skirk-kit/exit.json --revoke-oauth
```

بعد فایل‌های ساخته‌شده را پاک کنید:

```bash
rm -rf skirk-kit
```

اگر پروفایل کلاینت leak شد، OAuth را revoke کنید و kit جدید بسازید. با
`client.skirk`، `client.json` و `exit.json` مثل پسورد رفتار کنید.

## امکانات پیشرفته

اگر می‌خواهید خروجی exit از یک پروکسی دیگر رد شود، مثلا WARP/wireproxy:

```bash
skirk serve-exit --config skirk-kit/exit.json --exit-proxy socks5h://127.0.0.1:40000
```

اگر علاوه بر SOCKS5، پروکسی HTTP/HTTPS هم می‌خواهید:

```bash
skirk serve-client \
  --config skirk-kit/client.skirk \
  --listen 127.0.0.1:18080 \
  --http-proxy-listen 127.0.0.1:18081
```

`burst polling` یک حالت آزمایشی است و پیش‌فرض نیست:

```bash
skirk serve-client --config skirk-kit/client.skirk --burst-poll --burst-poll-ms 25
```

در تست‌های اولیه، بهبود latency آن کم و ناپایدار بود؛ برای همین مسیر پایدار
همان adaptive polling معمولی است.

## مستندات

- [راهنمای نصب](docs/install.md)
- [راهنمای راه‌اندازی](docs/setup.md)
- [راهنمای کلاینت‌ها](docs/clients.md)
- [حالت‌های transport](docs/skirk_modes.md)
- [نکات CLI](docs/go_skirk.md)
- [راهنمای release](docs/release.md)
- [سیاست امنیتی](SECURITY.md)
- [سلب مسئولیت حقوقی](DISCLAIMER.md)
