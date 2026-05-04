package local

// stealthScript is injected via Page.addScriptToEvaluateOnNewDocument
// before any page JS runs. It patches the most common automation
// tells that off-the-shelf bot detectors (Cloudflare's challenge,
// PerimeterX, Akamai, etc.) check for. None of these are foolproof
// individually — together they cover the ~80% of sites that use
// stock detection libraries.
//
// Each patch is wrapped in a try/catch so a failure on one
// (e.g. WebGL not available in some sandbox) doesn't abort the
// whole script and leak even more tells.
//
// Drift watch: when Chrome ships a new version, some of these
// indicators may shift. The /TestStealth* probe tests in this
// package read the same properties and assert they look human;
// they break loudly when something regresses.
const stealthScript = `
(() => {
  const safe = (fn) => { try { fn(); } catch (_) {} };

  // 1. navigator.webdriver — the single biggest tell. Real Chrome
  //    has this property absent (or undefined); WebDriver-controlled
  //    Chrome sets it to true. Defining it as a non-writable
  //    'undefined' getter passes both presence and value checks.
  safe(() => {
    Object.defineProperty(Navigator.prototype, 'webdriver', {
      get: () => undefined,
      configurable: true,
    });
  });

  // 2. window.chrome — vanilla Chrome exposes a populated chrome
  //    object (chrome.app, chrome.runtime, etc). Headless Chrome
  //    historically missed chrome.runtime. Fake the minimum shape
  //    detectors check for.
  safe(() => {
    if (!window.chrome) {
      window.chrome = {};
    }
    if (!window.chrome.runtime) {
      window.chrome.runtime = {
        OnInstalledReason: { INSTALL: 'install' },
        OnRestartRequiredReason: { APP_UPDATE: 'app_update' },
        PlatformOs: { LINUX: 'linux', MAC: 'mac', WIN: 'win' },
        connect: () => {},
        sendMessage: () => {},
      };
    }
  });

  // 3. navigator.plugins — empty plugins[] array is a strong tell on
  //    modern Chrome which still exposes Chrome PDF Viewer + a few
  //    others. Fake a realistic minimal set with the right prototype.
  safe(() => {
    const fakePlugin = (name, filename, description) => {
      const p = Object.create(Plugin.prototype);
      Object.defineProperties(p, {
        name:        { value: name },
        filename:    { value: filename },
        description: { value: description },
        length:      { value: 1 },
      });
      return p;
    };
    const plugins = [
      fakePlugin('PDF Viewer',         'internal-pdf-viewer', 'Portable Document Format'),
      fakePlugin('Chrome PDF Viewer',  'internal-pdf-viewer', 'Portable Document Format'),
      fakePlugin('Chromium PDF Viewer','internal-pdf-viewer', 'Portable Document Format'),
    ];
    Object.defineProperty(Navigator.prototype, 'plugins', {
      get: () => plugins,
      configurable: true,
    });
  });

  // 4. navigator.languages — empty array or single-language returns
  //    flag automation. Default Chrome returns ['en-US', 'en'].
  safe(() => {
    Object.defineProperty(Navigator.prototype, 'languages', {
      get: () => ['en-US', 'en'],
      configurable: true,
    });
  });

  // 5. Notifications permission — headless Chrome returns 'denied'
  //    where headed Chrome returns 'default' without a user prompt.
  //    Detectors call permissions.query and check for that mismatch.
  safe(() => {
    if (navigator.permissions && navigator.permissions.query) {
      const orig = navigator.permissions.query.bind(navigator.permissions);
      navigator.permissions.query = (params) => {
        if (params && params.name === 'notifications') {
          return Promise.resolve({ state: Notification.permission || 'default' });
        }
        return orig(params);
      };
    }
  });

  // 6. WebGL vendor / renderer — Chromium's SwiftShader software GL
  //    in headless mode reports "Google Inc." / "Google SwiftShader",
  //    a dead giveaway versus real GPU vendor strings. Override the
  //    two parameters detectors actually read (37445=VENDOR,
  //    37446=RENDERER) with realistic values.
  safe(() => {
    const patch = (proto) => {
      const orig = proto.getParameter;
      proto.getParameter = function (parameter) {
        if (parameter === 37445) return 'Intel Inc.';
        if (parameter === 37446) return 'Intel Iris OpenGL Engine';
        return orig.call(this, parameter);
      };
    };
    if (window.WebGLRenderingContext) patch(WebGLRenderingContext.prototype);
    if (window.WebGL2RenderingContext) patch(WebGL2RenderingContext.prototype);
  });

  // 7. Strip 'puppeteer'/'chromedp'/'__nightmare' from any error
  //    stack traces some detectors capture. We can't intercept all
  //    stack reads, but Function.prototype.toString is the common
  //    one used to sniff for native vs proxied functions.
  safe(() => {
    const origToString = Function.prototype.toString;
    Function.prototype.toString = function () {
      const r = origToString.call(this);
      return r.replace(/(puppeteer|chromedp|__nightmare|playwright)/gi, '');
    };
  });
})();
`
