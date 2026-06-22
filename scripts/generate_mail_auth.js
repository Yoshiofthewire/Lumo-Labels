const { firefox } = require('playwright');
const fs = require('fs');

(async () => {
  const browser = await firefox.launch({
    headless: false // IMPORTANT: must be visible for login
  });

  const context = await browser.newContext();
  const page = await context.newPage();

  console.log('Opening Proton Mail…');
  await page.goto('https://mail.proton.me', {
    waitUntil: 'domcontentloaded'
  });

  console.log('');
  console.log('👉 Log in manually in the browser window.');
  console.log('👉 Once you see Proton Mail inbox, come back here.');
  console.log('👉 Press ENTER in this terminal to save session.');
  console.log('');

  // wait for user to press Enter
  await new Promise(resolve => process.stdin.once('data', resolve));

  // save the full browser storage state (cookies + localStorage)
  const stateFile = 'mail_auth.json';
  await context.storageState({ path: stateFile });
  console.log(`✅ Browser storage state saved to ${stateFile}`);

  // --- extract Proton API tokens from the saved cookies ---
  const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  const cookies = state.cookies || [];

  // We want the mail.proton.me session (ClientID=WebMail), not the account one.
  // The REFRESH-<uid> cookie value is URL-encoded JSON containing the RefreshToken.
  const refreshCookie = cookies.find(c =>
    c.name.startsWith('REFRESH-') && c.domain === 'mail.proton.me'
  );

  if (!refreshCookie) {
    console.error('❌ Could not find a REFRESH-* cookie for mail.proton.me.');
    console.error('   Make sure you are fully logged in to Proton Mail before pressing ENTER.');
    await browser.close();
    process.exit(1);
  }

  const uid = refreshCookie.name.slice('REFRESH-'.length);

  let refreshToken;
  try {
    const decoded = JSON.parse(decodeURIComponent(refreshCookie.value));
    refreshToken = decoded.RefreshToken;
  } catch {
    console.error('❌ Failed to decode REFRESH cookie value.');
    await browser.close();
    process.exit(1);
  }

  if (!refreshToken) {
    console.error('❌ RefreshToken missing from decoded REFRESH cookie.');
    await browser.close();
    process.exit(1);
  }

  // AUTH-<uid> cookie value is the AccessToken
  const authCookie = cookies.find(c =>
    c.name === `AUTH-${uid}` && c.domain === 'mail.proton.me'
  );
  if (!authCookie) {
    console.error(`❌ Could not find AUTH-${uid} cookie for mail.proton.me.`);
    await browser.close();
    process.exit(1);
  }
  const accessToken = authCookie.value;

  // Capture ALL cookies from any proton.me domain — including the parent-domain
  // .proton.me cookies (e.g. Session-Id) that the Proton refresh API endpoint at
  // api.proton.me/auth/v4/refresh requires. Capturing only mail.proton.me cookies
  // caused refreshes to fail with 400 because the shared Session-Id was missing
  // from the jar when go-proton-api sent the request to api.proton.me.
  const sessionCookies = cookies
    .filter(c =>
      c.domain === 'mail.proton.me' ||
      c.domain === '.proton.me' ||
      c.domain === 'proton.me' ||
      c.domain === 'account.proton.me'
    )
    .map(c => ({ name: c.name, value: c.value, domain: c.domain, path: c.path || '/' }));

  // Log which domains contributed cookies so missing ones are immediately visible.
  const byDomain = {};
  sessionCookies.forEach(c => { byDomain[c.domain] = (byDomain[c.domain] || 0) + 1; });

  const bootstrap = {
    uid,
    accessToken,
    refreshToken,
    cookies: sessionCookies,
    updatedAt: new Date().toISOString()
  };

  const bootstrapFile = 'proton-bootstrap.json';
  fs.writeFileSync(bootstrapFile, JSON.stringify(bootstrap, null, 2));

  console.log('');
  console.log(`✅ Proton bootstrap tokens saved to ${bootstrapFile}`);
  console.log(`   UID:          ${uid}`);
  console.log(`   AccessToken:  ${accessToken.slice(0, 8)}…`);
  console.log(`   RefreshToken: ${refreshToken.slice(0, 8)}…`);
  console.log(`   Cookies:      ${sessionCookies.length} session cookie(s)`);
  console.log('   Cookie domains:');
  Object.entries(byDomain).forEach(([d, n]) => console.log(`     ${d}: ${n} cookie(s)`));
  const hasParentDomain = sessionCookies.some(c => c.domain === '.proton.me' || c.domain === 'proton.me');
  if (!hasParentDomain) {
    console.warn('');
    console.warn('⚠️  WARNING: No .proton.me parent-domain cookies were captured.');
    console.warn('   The Proton refresh endpoint needs the Session-Id from .proton.me.');
    console.warn('   Without it, token refreshes will fail with 400 after ~24h.');
    console.warn('   Try: fully reload Proton Mail in the browser before pressing Enter.');
  }
  console.log('');
  console.log('📤 Next step: upload proton-bootstrap.json via the Config page');
  console.log('   Config → Proton Authentication → Browser Login Bootstrap → Upload');
  console.log('');

  await browser.close();
})();
