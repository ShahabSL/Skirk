# Set up Skirk with your Google Cloud project

[فارسی](personal-google-setup.fa.md)

Set up Google once for the exit. The Google account you authorize owns the Drive mailbox, and Drive API usage counts against your project's quota. People you share the Skirk client profile with import it in their Skirk app. Google sign-in happens during exit setup.

The illustrated steps show the controls to use with generic labels.

## 1. Create a project and enable Drive

![Create a project, select it, open Google Drive API, and click Enable.](images/personal-google-enable-drive.png)

1. Sign in to Google Cloud with the account that will own the mailbox. Enter a project name such as **Skirk Personal**, then click **Create**. [Create a project](https://console.cloud.google.com/projectcreate)
2. Select that project using the project picker. Open **Google Drive API** and click **Enable**. [Google Drive API](https://console.cloud.google.com/apis/library/drive.googleapis.com)

Use the same project for every Console step below.

## 2. Configure consent and Drive access

![Open Branding and choose Get started, set the audience to External, then select drive.file in Data Access and save it.](images/personal-google-configure-consent.png)

![Google Auth Platform shows drive.file in the saved scope table.](images/personal-google-drive-scope.jpg)

1. Under **Google Auth Platform → Branding**, click **Get started** if prompted. Enter an app name, choose your **User support email**, and click **Next**. [Branding](https://console.cloud.google.com/auth/branding)
2. Choose **External** for a personal Google account. Enter a contact email, review Google's policy, and finish creating the configuration if you agree. **Internal** is for apps limited to an eligible Google Workspace or Cloud Identity organization. [Google's consent setup guide](https://developers.google.com/workspace/guides/configure-oauth-consent)
3. Open **Data Access → Add or Remove Scopes**. Select `https://www.googleapis.com/auth/drive.file`, click **Update**, then **Save**. If it is missing from the list, confirm that Google Drive API is enabled in the selected project or use **Manually add scopes**. Skirk uses this per-file scope to create and manage its mailbox files. Google classifies it as non-sensitive. [Data Access](https://console.cloud.google.com/auth/scopes) · [Scope selection](https://support.google.com/cloud/answer/15549135) · [Drive scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)

## 3. Publish before signing in

![In Audience, confirm the status is Testing, click Publish app, click Confirm, and check for In production.](images/personal-google-publish-app.png)

![Google Auth Platform shows In production under Publishing status.](images/personal-google-audience-status.jpg)

Open **Audience** and check **Publishing status**. While it shows **Testing**, click **Publish app**, click **Confirm**, and check that the status changes to **In production**. [Audience](https://console.cloud.google.com/auth/audience) · [Google's published screenshot of the Publish app control](https://developers.google.com/static/google-ads/api/images/brand-verification/3a_consumer-publish-app.png)

**The seven-day expiry starts when you authorize Drive access while an External app is in Testing.** Authorize the kit after the project shows **In production** to avoid that Testing-specific expiry. Google can still invalidate a refresh token after revocation, long inactivity, or policy changes. [Publishing status](https://support.google.com/cloud/answer/15549945) · [Refresh token expiration](https://developers.google.com/identity/protocols/oauth2#expiration)

Publishing and verification are separate. Google describes a verification exception for limited personal use. Follow any verification or branding requirements shown for your project. Publishing changes who can authorize the app. Google's Drive data-use policies still apply. [Personal-use verification guidance](https://support.google.com/cloud/answer/13464323) · [Workspace developer policy](https://developers.google.com/workspace/workspace-api-user-data-developer-policy)

## 4. Create a Desktop app client

![Desktop app in the Google OAuth application-type selector.](images/personal-google-desktop-option.jpg)

In your computer's browser, open **Clients → Create client**. Choose **Desktop app**, enter a name, and click **Create**. Leave the window open. Google shows the full **Client secret** when you create it, and you'll copy both **Client ID** and **Client secret** in the next step. [Clients](https://console.cloud.google.com/auth/clients) · [Google's client secret guidance](https://support.google.com/cloud/answer/15549257)

## 5. Create the Skirk kit

Once Skirk is installed, open a terminal on the exit. Connect to it using SSH if it is a remote server. For a new kit on Linux with systemd, run the command below. For macOS, Windows, or a foreground exit, see the platform commands in the installation guide. [Install Skirk](install.md#configure-the-exit)

```sh
skirk setup init --out skirk-kit
```

1. At Skirk’s credential menu, press Enter for **1. Paste**. Copy **Client ID** from the Google window, paste it into Skirk, and press Enter. At the next prompt paste **Client secret** and press Enter. The secret stays hidden as you paste. Skirk saves the credentials on the exit.
2. Open Skirk’s approval link in your browser. Sign in with the Google account that will own the mailbox and approve access. If you are using SSH and your browser ends on a localhost address that fails to load, copy the **entire address** from the address bar and paste it into the waiting Skirk prompt. Keep the address private. [Google's desktop authorization flow](https://developers.google.com/identity/protocols/oauth2/native-app)
3. Keep the saved credentials and `skirk-kit/exit.json` on the exit. Privately send the one-line contents of `skirk-kit/client.skirk` to anyone you trust to use your exit and mailbox. They paste that profile into the Skirk app. [Connect a device](install.md#connect-a-device)

If you already have a Google Desktop app JSON file on the exit, you can select the file option in the wizard or supply its path with `--oauth-client-file`.

## Renew access and check quota

- **Already authorized in Testing or seeing `invalid_grant`.** Check that the project shows **In production**, then run `skirk setup renew-oauth --kit skirk-kit` with the same Google account and OAuth client ID. If the kit lacks its original client secret, supply the saved credential file with `--oauth-client-file`. Restart the exit and import the renewed `client.skirk` on clients whose grant has expired. Renewal keeps the existing tunnel and mailbox. [Skirk renewal details](setup.md#renew-an-expiring-personal-oauth-grant)
- **Google says the app is being tested.** For a long-running kit, publish the project first and authorize again. To run a short test in **Testing**, add the exact signing-in account under **Audience → Test users**. Access granted there will still expire after seven days. [Google's audience guidance](https://support.google.com/cloud/answer/15549945)
- **Do I need a domain?** The Desktop app flow uses a loopback callback. The exit can use this flow without a public callback URL. If Google asks for a domain for branding or verification, follow the requirements shown for your project. [Desktop callbacks](https://developers.google.com/identity/protocols/oauth2/native-app#redirect-uri_loopback) · [Google branding guidance](https://support.google.com/cloud/answer/15549049)
- **Estimating capacity for family or friends.** Check the Drive API quota for your project. Google applies limits per project and per user within the project. Devices using the same kit use the mailbox owner's Google authorization. Concurrent usage depends on traffic and API costs, so the project quota is a starting point for testing capacity. [Drive API usage limits](https://developers.google.com/workspace/drive/api/guides/limits)
