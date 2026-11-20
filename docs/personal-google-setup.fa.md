# راه‌اندازی اسکیرک با پروژهٔ شخصی گوگل

[English](personal-google-setup.md)

این مراحل را صاحب خروجی یک‌بار انجام می‌دهد. فایل‌های صندوق درایو به حسابی تعلق دارند که دسترسی می‌دهد و درخواست‌ها از سهمیهٔ پروژهٔ شما استفاده می‌کنند. افرادی که پروفایل را دریافت می‌کنند آن را مستقیماً در اسکیرک وارد می‌کنند.

## ۱. پروژه بسازید و درایو را فعال کنید

![ساخت پروژه، انتخاب آن و فعال‌کردن Google Drive API](images/personal-google-enable-drive.png)

با حسابی که قرار است مالک صندوق باشد وارد Google Cloud شوید. **Create project** را بزنید و یک نام انتخاب کنید. در انتخابگر پروژه همان پروژه را باز کنید. در صفحهٔ **Google Drive API** دکمهٔ **Enable** را بزنید. باقی مراحل را در همین پروژه انجام دهید. [ساخت پروژه](https://console.cloud.google.com/projectcreate) · [فعال‌سازی Drive API](https://console.cloud.google.com/apis/library/drive.googleapis.com)

## ۲. دسترسی را مشخص کنید

![تنظیم Branding، انتخاب External و ذخیرهٔ دسترسی drive.file](images/personal-google-configure-consent.png)

۱. در **Google Auth Platform → Branding** اگر **Get started** را دیدید آن را بزنید. یک نام برای برنامه و یک ایمیل پشتیبانی وارد کنید. برای حساب شخصی در بخش مخاطبان **External** را انتخاب کنید. ایمیل تماس را وارد کنید و شرایط گوگل را بخوانید. [Branding](https://console.cloud.google.com/auth/branding)

۲. در **Data Access → Add or Remove Scopes** دسترسی `https://www.googleapis.com/auth/drive.file` را انتخاب کنید و **Update** و سپس **Save** را بزنید. اگر در فهرست نبود، فعال بودن Drive API را بررسی کنید و در صورت نیاز از **Manually add scopes** استفاده کنید. اسکیرک با این دسترسی فایل‌های صندوق خودش را مدیریت می‌کند. [Data Access](https://console.cloud.google.com/auth/scopes) · [توضیح دسترسی‌ها](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)

![نمونهٔ دسترسی ذخیره‌شده](images/personal-google-drive-scope.jpg)

## ۳. برنامه را پیش از ورود منتشر کنید

![محل Publish app و تأیید وضعیت انتشار](images/personal-google-publish-app.png)

در **Audience** وضعیت **Publishing status** را ببینید. اگر **Testing** است، **Publish app** و سپس **Confirm** را بزنید تا **In production** نمایش داده شود. سپس به اسکیرک اجازهٔ دسترسی بدهید. مجوز درایو که در حالت آزمایشی صادر می‌شود پس از هفت روز منقضی می‌شود. انتشار برنامه این علت انقضا را برطرف می‌کند. لغو دسترسی، استفاده‌نکردن طولانی یا تغییر سیاست حساب هنوز می‌تواند باعث منقضی‌شدن مجوز شود. [Audience](https://console.cloud.google.com/auth/audience) · [شرایط انقضای گوگل](https://developers.google.com/identity/protocols/oauth2#expiration)

![نمونهٔ وضعیت In production](images/personal-google-audience-status.jpg)

انتشار، مخاطبان مجاز را تغییر می‌دهد. اگر گوگل برای برند یا تأیید پروژه مدرک بیشتری درخواست کرد، مراحل همان پروژه را دنبال کنید. جریان ورود Desktop از نشانی محلی استفاده می‌کند و برای بازگشت مرورگر به برنامه به دامنهٔ عمومی نیاز ندارد. [شرایط استفادهٔ شخصی](https://support.google.com/cloud/answer/13464323) · [ورود برنامه‌های Desktop](https://developers.google.com/identity/protocols/oauth2/native-app)

## ۴. یک Desktop app بسازید

![انتخاب Desktop app](images/personal-google-desktop-option.jpg)

در مرورگر رایانهٔ خودتان به **Clients → Create client** بروید، نوع **Desktop app** را انتخاب کنید، نامی وارد کنید و **Create** را بزنید. این پنجره را باز نگه دارید. گوگل مقدار کامل **Client secret** را هنگام ساخت نشان می‌دهد. در مرحلهٔ بعد هم این مقدار و هم **Client ID** را از همین پنجره کپی می‌کنید. [Clients](https://console.cloud.google.com/auth/clients) · [راهنمای گوگل](https://support.google.com/cloud/answer/15549257)

## ۵. خروجی را راه‌اندازی کنید

بعد از نصب اسکیرک، ترمینال خروجی را باز کنید. اگر خروجی روی سرور راه دور است، ابتدا با SSH به آن وصل شوید. برای ساخت کیت جدید روی لینوکس با systemd، دستور زیر را اجرا کنید. دستورهای مک، ویندوز و اجرای مستقیم در ترمینال در راهنمای نصب آمده‌اند. [راهنمای نصب](install.fa.md)

```sh
skirk setup init --out skirk-kit
```

۱. در منوی ورود اطلاعات، Enter را بزنید تا گزینهٔ ۱ (**Paste**) انتخاب شود. **Client ID** را از پنجرهٔ گوگل کپی کنید، در ترمینال جای‌گذاری کنید و Enter را بزنید. در پرسش بعدی، **Client secret** را جای‌گذاری کنید و دوباره Enter را بزنید. هنگام جای‌گذاری، کلید روی صفحه نمایش داده نمی‌شود. اسکیرک فایل اعتبارنامه را روی خروجی ذخیره می‌کند.

۲. پیوندی را که اسکیرک نشان می‌دهد در مرورگر خودتان باز کنید. با حسابی که قرار است مالک صندوق درایو باشد دسترسی را تأیید کنید. در اتصال SSH ممکن است مرورگر در پایان روی صفحه‌ای با نشانی localhost بماند که باز نمی‌شود. **کل نشانی** را از نوار آدرس کپی کنید و در همان ترمینالی که منتظر پاسخ است جای‌گذاری کنید. این نشانی محرمانه است.

۳. فایل اعتبارنامه و `skirk-kit/exit.json` را روی خروجی نگه دارید. `skirk` را اجرا کنید، گزینهٔ **11. Show client profile and QR code** را بزنید و برای باز کردن فایل پیشنهادی `client.skirk`، Enter را بزنید. کد را در برنامهٔ گوشی اسکن کنید یا متن یک‌خطی پروفایل را کپی کنید. کد یا متن را فقط به‌صورت خصوصی در اختیار افرادی بگذارید که اجازهٔ استفاده از خروجی و صندوق درایو را دارند. [اتصال گوشی یا رایانه](install.fa.md#اتصال-گوشی-یا-رایانه)

اگر از قبل فایل JSON مربوط به همین Desktop app را روی خروجی دارید، می‌توانید در منو گزینهٔ فایل را انتخاب کنید یا مسیر آن را با `--oauth-client-file` بدهید.

## تمدید مجوز

اگر خطای `invalid_grant` می‌بینید یا کیت را در حالت آزمایشی ساخته‌اید، ابتدا وضعیت را به **In production** تغییر دهید. در پوشهٔ خروجی با همان حساب و همان Desktop client دستور زیر را اجرا کنید. پس از تمدید، سرویس را دوباره راه‌اندازی و پروفایل به‌روزشده را در کلاینت‌ها وارد کنید. پوشهٔ درایو و هویت تونل حفظ می‌شوند.

```sh
skirk setup renew-oauth --kit skirk-kit
```

اگر کیت قدیمی کلید اصلی را ذخیره نکرده است، مسیر فایل اعتبارنامهٔ همان Desktop app را با `--oauth-client-file` بدهید.
