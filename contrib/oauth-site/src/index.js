const origin = 'https://skirk.getephemra.com';

const css = `
:root{color-scheme:dark;--bg:#0c1113;--panel:#151c1d;--panel2:#1a2223;--line:#34413f;--text:#edf1e9;--muted:#aab9b1;--accent:#c7f08d;--accent2:#86c9b1;--max:1120px}
*{box-sizing:border-box}html{scroll-behavior:smooth}body{margin:0;background:radial-gradient(circle at 82% 0%,#213833 0,transparent 37%),var(--bg);color:var(--text);font:16px/1.65 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
a{color:inherit}a:focus-visible{outline:3px solid var(--accent);outline-offset:4px;border-radius:3px}a:hover{color:var(--accent)}.shell{width:min(var(--max),calc(100% - 40px));margin-inline:auto}.skip-link{position:absolute;left:-10000px;top:8px;z-index:10;background:var(--accent);color:#162118;padding:8px}.skip-link:focus{left:8px}
.topbar{display:flex;align-items:center;justify-content:space-between;gap:24px;padding:23px 0;border-bottom:1px solid var(--line)}.brand{display:inline-flex;align-items:center;gap:11px;font-size:19px;font-weight:780;letter-spacing:-.04em;text-decoration:none}.mark{display:grid;place-items:center;width:30px;height:30px;border:1px solid var(--accent);border-radius:8px;color:var(--accent);font:700 17px/1 ui-monospace,monospace}.nav{display:flex;align-items:center;gap:24px;flex-wrap:wrap}.nav a,.footer a{color:var(--muted);font-size:13px;text-decoration:none}.nav a[aria-current=page]{color:var(--accent)}
.eyebrow,.terminal,.kicker,.step-number,.meta{font:600 12px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace;letter-spacing:.08em}.eyebrow,.kicker{color:var(--accent)}.terminal,.meta{color:var(--muted)}.hero{display:grid;grid-template-columns:minmax(0,1.25fr) minmax(280px,.75fr);align-items:end;gap:72px;padding:96px 0 90px}.hero h1{max-width:720px;margin:14px 0 24px;font-size:clamp(48px,7vw,94px);line-height:1.03;letter-spacing:-.075em}.hero h1 span{color:var(--accent)}.lead{max-width:650px;margin:0;color:var(--muted);font-size:clamp(17px,1.7vw,21px);line-height:1.65}.buttons{display:flex;flex-wrap:wrap;gap:12px;margin-top:34px}.button{display:inline-flex;align-items:center;justify-content:center;min-height:48px;padding:10px 18px;border:1px solid var(--line);border-radius:9px;font-weight:650;font-size:14px;text-decoration:none;transition:background .15s ease,border-color .15s ease}.button.primary{background:var(--accent);border-color:var(--accent);color:#162118}.button.primary:hover{background:#defbb4;color:#162118}.button.secondary:hover{border-color:var(--accent);background:var(--panel)}
.route{border:1px solid var(--line);border-radius:16px;background:linear-gradient(145deg,#1d302b,var(--panel));padding:25px;box-shadow:0 18px 60px #0005}.route .meta{margin:0 0 29px}.route-line{display:flex;align-items:center;gap:11px;flex-wrap:wrap;font:650 15px/1.9 ui-monospace,monospace}.route-line span{color:var(--accent)}.route-footer{display:flex;justify-content:space-between;gap:12px;margin-top:29px;padding-top:18px;border-top:1px solid var(--line);color:var(--muted);font-size:12px}
.section{padding:58px 0;border-top:1px solid var(--line)}.section h2,.card h2{margin:8px 0 0;font-size:clamp(26px,3vw,38px);line-height:1.2;letter-spacing:-.045em}.steps{display:grid;grid-template-columns:repeat(3,1fr);gap:14px;margin:32px 0 0;padding:0;list-style:none}.step,.card{min-width:0;border:1px solid var(--line);border-radius:13px;background:var(--panel);padding:25px}.step-number{color:var(--accent2)}.step h3{font-size:18px;letter-spacing:-.02em;margin:18px 0 7px}.step p,.card p{margin:0;color:var(--muted);font-size:14px}.cards{display:grid;grid-template-columns:repeat(2,1fr);gap:14px;margin:24px 0 74px}.card{background:var(--panel2)}.card p{margin:16px 0 22px}.card a{font-size:14px;font-weight:650;text-underline-offset:5px}.card code,.document code{padding:2px 5px;border:1px solid var(--line);border-radius:4px;font-size:.91em;overflow-wrap:anywhere}
.document{max-width:790px;padding:70px 0 95px}.document h1{margin:13px 0 7px;font-size:clamp(34px,5vw,56px);line-height:1.12;letter-spacing:-.06em}.document .date{margin:0 0 38px;color:var(--muted);font-size:13px}.document .summary{border-left:3px solid var(--accent);padding:3px 0 3px 20px;margin:0 0 42px;color:var(--text);font-size:18px}.document h2{margin:37px 0 10px;padding-top:23px;border-top:1px solid var(--line);font-size:21px;line-height:1.3;letter-spacing:-.025em}.document p{color:var(--muted);margin:0 0 16px}.document a{color:var(--text);text-underline-offset:4px}.document a:hover{color:var(--accent)}.document-end{display:flex;justify-content:space-between;gap:18px;flex-wrap:wrap;margin-top:48px;padding-top:24px;border-top:1px solid var(--line);font-size:14px}
.footer{display:flex;justify-content:space-between;align-items:center;gap:24px;flex-wrap:wrap;border-top:1px solid var(--line);padding:22px 0 36px;color:var(--muted);font-size:12px}.footer nav{display:flex;gap:22px;flex-wrap:wrap}
@media(max-width:840px){.hero{grid-template-columns:1fr;gap:36px;padding:70px 0}.route{max-width:580px}.steps{grid-template-columns:1fr}.cards{grid-template-columns:1fr}}@media(max-width:560px){.shell{width:min(var(--max),calc(100% - 30px))}.topbar{align-items:flex-start;flex-direction:column;gap:14px}.nav{gap:17px}.hero{padding:54px 0 65px}.hero h1{font-size:clamp(44px,13vw,67px)}.route{padding:20px}.route-footer{flex-direction:column}.document{padding-top:50px}.document h2{font-size:19px}}
@media(prefers-reduced-motion:reduce){html{scroll-behavior:auto}.button{transition:none}}
`;

const home = `
<main id="main" class="shell">
  <section class="hero" aria-labelledby="hero-title">
    <div>
      <p class="eyebrow">OPEN SOURCE · SELF-HOSTED EXIT</p>
      <h1 id="hero-title">A route through<br><span>your Drive.</span></h1>
      <p class="lead">Skirk is an encrypted TCP transport through a Google Drive mailbox. Run an exit on a machine you control, then connect from a client when ordinary routes are blocked.</p>
      <div class="buttons"><a class="button primary" href="https://github.com/ShahabSL/Skirk">View source and setup ↗</a><a class="button secondary" href="/privacy">How Google access works →</a></div>
    </div>
    <aside class="route" aria-label="Skirk transport route"><p class="meta">ROUTE / ONE SESSION</p><div class="route-line">client <span>→</span> Drive <span>→</span> exit <span>→</span> destination</div><div class="route-footer"><span>encrypted mailbox objects</span><span>no inbound exit port</span></div></aside>
  </section>
  <section class="section" aria-labelledby="flow-title"><p class="kicker">THE TRANSPORT</p><h2 id="flow-title">How traffic moves</h2><ol class="steps">
    <li class="step"><span class="step-number">01 / CLIENT</span><h3>Accept traffic</h3><p>A local SOCKS or HTTP proxy, or a supported VPN frontend, accepts application traffic.</p></li>
    <li class="step"><span class="step-number">02 / DRIVE</span><h3>Exchange frames</h3><p>Skirk writes encrypted stream frames as files in an app-created Drive mailbox.</p></li>
    <li class="step"><span class="step-number">03 / EXIT</span><h3>Reach the destination</h3><p>Your exit reads frames, connects to destinations, and sends responses back through the mailbox.</p></li>
  </ol></section>
  <section class="cards" aria-label="Platforms and Google access">
    <div class="card"><p class="kicker">CLIENTS</p><h2>Use it where you are.</h2><p>The repository includes a Go CLI, desktop frontends for Windows, macOS, and Linux, and an Android client. The iOS packet-tunnel app remains a prototype.</p><a href="https://github.com/ShahabSL/Skirk/releases">See release assets ↗</a></div>
    <div class="card"><p class="kicker">PERMISSION</p><h2>Your Drive account.</h2><p>Skirk asks for the limited <code>drive.file</code> OAuth scope to create and manage its own mailbox files. Traffic payloads are encrypted before upload. Your exit can see destinations.</p><a href="/privacy">Read the data-use details →</a></div>
  </section>
</main>`;

const privacy = `
<main id="main" class="shell"><article class="document">
  <p class="terminal">$ cat PRIVACY.md</p><h1>Privacy and Google Drive data use</h1><p class="date">Last updated November 14, 2026</p>
  <p class="summary">Skirk is software you run with an exit you control. It uses your Google Drive account as a mailbox for encrypted transport objects. This page explains what the software handles and what the exit operator can see.</p>
  <h2>Google account permission</h2><p>Skirk requests <code>https://www.googleapis.com/auth/drive.file</code>. This scope lets the app create and manage files it creates or that you explicitly share with it. Skirk uses it to create a visible app-owned mailbox folder, list mailbox objects, upload encrypted objects, download them, and delete objects after use or cleanup. It does not request full-Drive access or scan unrelated Drive files.</p>
  <h2>What is stored</h2><p>Message payloads in the mailbox are encrypted before upload. File names and Drive metadata can still reveal operational details such as session, direction, client and run identifiers, sequence numbers, object sizes, and timestamps. An object can remain in Drive after a crash or interrupted cleanup until the operator runs cleanup or removes the mailbox.</p><p>The generated client profile contains a Google OAuth refresh token and a tunnel secret. Skirk saves these in local configuration or platform storage on devices where you import the profile. Treat the profile as a password. Do not post it, include it in bug reports, or share it outside the people you intend to authorize.</p>
  <h2>What the exit can see</h2><p>The exit decrypts Skirk frames and opens the requested destination connection. Its operator can see destination addresses and timing; for non-TLS application protocols it may also see plaintext traffic. Destination services see the exit network address. Skirk is a transport, so application traffic privacy still depends on the application protocol, including HTTPS where appropriate.</p>
  <h2>Who receives data</h2><p>Google processes the mailbox objects and OAuth grant as part of Google Drive. The exit operator controls the exit machine and its outbound connections. In Skirk's normal self-hosted path, there is no Skirk-operated relay or account server between your client and your exit. This informational website is hosted by Cloudflare, which processes ordinary website requests; it does not receive your Skirk profile or Drive mailbox contents.</p>
  <h2>Retention and removal</h2><p>The client and exit remove consumed mailbox objects, and the exit periodically cleans up stale objects. You can remove your local profile, delete the Skirk-created mailbox from Drive, and revoke the app's Google Account access. The repository documents <a href="https://github.com/ShahabSL/Skirk/blob/main/docs/setup.md">cleanup and revoke commands ↗</a>. Revoking access stops future token refreshes but does not delete objects already in Drive.</p>
  <h2>Changes and contact</h2><p>This disclosure will be updated when Skirk changes how it uses Google data. For questions, open an issue in the <a href="https://github.com/ShahabSL/Skirk/issues">Skirk repository ↗</a>.</p>
  <div class="document-end"><a href="/">← Skirk overview</a><a href="/terms">Terms and license →</a></div>
</article></main>`;

const terms = `
<main id="main" class="shell"><article class="document">
  <p class="terminal">$ cat TERMS.md</p><h1>Terms and license</h1><p class="date">Last updated November 14, 2026</p>
  <p class="summary">Skirk is open-source software for a self-hosted client and exit. It is not a hosted connectivity service. These notes explain the software license and what operating it involves; the repository license is the controlling text for the code.</p>
  <h2>Software license</h2><p>Skirk is distributed under the <a href="https://github.com/ShahabSL/Skirk/blob/main/LICENSE">MIT License ↗</a>. Review the license for its permission and warranty terms. Third-party components keep their own licenses, which are identified in the repository.</p>
  <h2>Running Skirk</h2><p>You choose the Google account and OAuth project, run or choose the exit machine, and decide which client devices receive the generated profile. The profile carries a Google refresh token and tunnel secret; anyone who has it may be able to use your mailbox and exit until you revoke or replace it. Google Drive quotas, storage, account rules, and any cloud or network charges associated with your setup still apply.</p>
  <h2>Availability</h2><p>Skirk depends on Google Drive, OAuth authorization, the exit machine, and the networks between them. Connections can fail or slow down, and authorization can expire or be revoked. No speed, uptime, or indefinite authorization is promised. Consult the <a href="https://github.com/ShahabSL/Skirk/blob/main/docs/setup.md">setup guide ↗</a> for recovery and cleanup.</p>
  <h2>Questions</h2><p>Use the <a href="https://github.com/ShahabSL/Skirk/issues">project issue tracker ↗</a>. For how Skirk uses Google Drive data, read the <a href="/privacy">privacy disclosure</a>.</p>
  <div class="document-end"><a href="/">← Skirk overview</a><a href="/privacy">Privacy →</a></div>
</article></main>`;

const pages = {
  '/': {
    title: 'Skirk — encrypted transport through Google Drive',
    description: 'Open-source TCP transport through an encrypted Google Drive mailbox with a self-hosted exit.',
    body: home,
  },
  '/privacy': {
    title: 'Skirk privacy and Google Drive data use',
    description: 'What Skirk accesses in Google Drive, what the exit can see, and how to remove access.',
    body: privacy,
  },
  '/terms': {
    title: 'Skirk terms and license',
    description: 'Skirk software license and responsibilities for running a self-hosted exit.',
    body: terms,
  },
};

function document(path, page) {
  const current = (link) => path === link ? ' aria-current="page"' : '';
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="theme-color" content="#0c1113"><title>${page.title}</title><meta name="description" content="${page.description}"><link rel="canonical" href="${origin}${path}"><meta property="og:title" content="${page.title}"><meta property="og:description" content="${page.description}"><meta property="og:url" content="${origin}${path}"><meta property="og:type" content="website"><link rel="icon" href="/favicon.svg" type="image/svg+xml"><style>${css}</style></head>
<body><a class="skip-link" href="#main">Skip to content</a><header class="shell topbar"><a class="brand" href="/"><span class="mark" aria-hidden="true">S</span> Skirk</a><nav class="nav" aria-label="Main navigation"><a href="/"${current('/')}>Overview</a><a href="/privacy"${current('/privacy')}>Privacy</a><a href="/terms"${current('/terms')}>Terms</a><a href="https://github.com/ShahabSL/Skirk">GitHub ↗</a></nav></header>${page.body}<footer class="shell footer"><span>SKIRK / SHAHAB LAVASANI</span><nav aria-label="Footer navigation"><a href="/privacy">Privacy</a><a href="/terms">Terms and license</a><a href="https://github.com/ShahabSL/Skirk">Source ↗</a></nav></footer></body></html>`;
}

const headers = {
  'content-type': 'text/html; charset=utf-8',
  'cache-control': 'public, max-age=300',
  'content-security-policy': "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
  'referrer-policy': 'strict-origin-when-cross-origin',
  'x-content-type-options': 'nosniff',
};

export default {
  fetch(request) {
    const url = new URL(request.url);
    const method = request.method;
    if (method !== 'GET' && method !== 'HEAD') {
      return new Response('Method not allowed', { status: 405, headers: { allow: 'GET, HEAD' } });
    }

    const redirects = {
      '/skirk': '/',
      '/skirk/': '/',
      '/skirk/privacy': '/privacy',
      '/skirk/terms': '/terms',
      '/privacy/': '/privacy',
      '/terms/': '/terms',
    };
    const target = redirects[url.pathname];
    if (target) {
      return Response.redirect(origin + target, 308);
    }
    if (url.pathname === '/favicon.svg') {
      const icon = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#0c1113"/><path d="M43 20H27c-6 0-9 3-9 8 0 4 3 6 8 7l12 2c5 1 8 3 8 7 0 6-5 10-12 10H18" fill="none" stroke="#c7f08d" stroke-width="7" stroke-linecap="round"/></svg>';
      return new Response(method === 'HEAD' ? null : icon, { headers: { 'content-type': 'image/svg+xml', 'cache-control': 'public, max-age=86400', 'x-content-type-options': 'nosniff' } });
    }
    const page = pages[url.pathname];
    if (!page) {
      return new Response(method === 'HEAD' ? null : 'Not found', { status: 404, headers: { 'content-type': 'text/plain; charset=utf-8', 'x-content-type-options': 'nosniff' } });
    }
    return new Response(method === 'HEAD' ? null : document(url.pathname, page), { headers });
  },
};
