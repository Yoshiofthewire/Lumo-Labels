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

  // Collect the session cookies needed for the refresh endpoint
  const SESSION_COOKIE_NAMES = new Set([
    'Session-Id', `AUTH-${uid}`, `REFRESH-${uid}`, 'Domain', 'Tag'
  ]);
  const sessionCookies = cookies
    .filter(c => c.domain === 'mail.proton.me' && SESSION_COOKIE_NAMES.has(c.name))
    .map(c => ({ name: c.name, value: c.value, domain: c.domain, path: '/' }));

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
  console.log('');
  console.log('📤 Next step: upload proton-bootstrap.json via the Config page');
  console.log('   Config → Proton Authentication → Browser Login Bootstrap → Upload');
  console.log('');

  await browser.close();
})();
